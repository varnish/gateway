//! Director implementation for Ghost VMOD
//!
//! The GhostDirector handles host-based routing and weighted backend selection.
//! It implements the VclDirector trait to integrate with Varnish's director system.

use std::borrow::Cow;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::SystemTime;

use parking_lot::RwLock;
use varnish::ffi::VCL_BACKEND;
use varnish::vcl::{Buffer, Ctx, HttpHeaders, StrOrBytes, VclDirector, VclError};

use crate::backend_pool::BackendPool;
use crate::config::Config;

/// Backend reference with weight for routing
#[derive(Debug, Clone)]
pub struct WeightedBackendRef {
    /// Backend pool key ("address:port")
    pub key: String,
    /// Weight for random selection
    pub weight: u32,
}

/// Routing state for the director
#[derive(Debug, Clone)]
pub struct RoutingState {
    /// Exact hostname matches
    pub exact: HashMap<String, Vec<WeightedBackendRef>>,
    /// Wildcard hostname patterns (in order)
    pub wildcards: Vec<(String, Vec<WeightedBackendRef>)>,
    /// Default fallback backends
    pub default: Option<Vec<WeightedBackendRef>>,
}

/// Build routing state from configuration
///
/// This creates the routing data structures and ensures all backends
/// exist in the backend pool.
pub fn build_routing_state(
    config: &Config,
    backend_pool: &mut BackendPool,
    ctx: &mut Ctx,
) -> Result<RoutingState, VclError> {
    let mut exact = HashMap::new();
    let mut wildcards = Vec::new();

    // Process vhosts
    for (hostname, vhost) in &config.vhosts {
        let mut backend_refs = Vec::new();

        // Create backend refs for each backend
        for backend in &vhost.backends {
            let key = backend_pool.get_or_create(ctx, &backend.address, backend.port)?;
            backend_refs.push(WeightedBackendRef {
                key,
                weight: backend.weight,
            });
        }

        // Categorize into exact or wildcard
        if hostname.starts_with("*.") {
            wildcards.push((hostname.clone(), backend_refs));
        } else {
            exact.insert(hostname.clone(), backend_refs);
        }
    }

    // Process default
    let default = if let Some(default_vhost) = &config.default {
        let mut backend_refs = Vec::new();
        for backend in &default_vhost.backends {
            let key = backend_pool.get_or_create(ctx, &backend.address, backend.port)?;
            backend_refs.push(WeightedBackendRef {
                key,
                weight: backend.weight,
            });
        }
        Some(backend_refs)
    } else {
        None
    };

    Ok(RoutingState {
        exact,
        wildcards,
        default,
    })
}

/// Ghost director implementation
pub struct GhostDirector {
    /// Routing state (wrapped for hot-reload)
    routing: RwLock<Arc<RoutingState>>,
    /// Backend pool (owned by this director)
    backends: RwLock<BackendPool>,
    /// Path to config file (for reload)
    config_path: PathBuf,
}

impl GhostDirector {
    /// Create a new Ghost director
    pub fn new(routing: Arc<RoutingState>, backends: BackendPool, config_path: PathBuf) -> Self {
        Self {
            routing: RwLock::new(routing),
            backends: RwLock::new(backends),
            config_path,
        }
    }

    /// Reload configuration from disk
    pub fn reload(&self, ctx: &mut Ctx) -> Result<(), String> {
        // Load new config
        let config = crate::config::load(&self.config_path)?;

        // Get mutable access to backends
        let mut backend_pool = self.backends.write();

        // Build new routing state (creates new backends as needed)
        let new_routing = build_routing_state(&config, &mut backend_pool, ctx)
            .map_err(|e| format!("Failed to build routing state: {}", e))?;

        drop(backend_pool);

        // Atomically update routing
        let mut guard = self.routing.write();
        *guard = Arc::new(new_routing);

        Ok(())
    }

    /// Check if the request's Host header matches any configured vhost
    pub fn has_vhost(&self, ctx: &mut Ctx) -> bool {
        // Get req for Host header extraction (in vcl_recv)
        let req = match ctx.http_req.as_ref() {
            Some(r) => r,
            None => return false,
        };

        // Get Host header
        let host = match get_host_header(req) {
            Some(h) => h,
            None => return false,
        };

        // Get routing state
        let routing_guard = self.routing.read();
        let routing = routing_guard.clone();
        drop(routing_guard);

        // Check if host matches any vhost
        match_host(&routing, &host).is_some()
    }
}

impl VclDirector for GhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<VCL_BACKEND> {
        // Get bereq for Host header extraction
        let bereq = ctx.http_bereq.as_ref()?;

        // Get Host header
        let host = get_host_header(bereq)?;

        // Get routing state
        let routing_guard = self.routing.read();
        let routing = routing_guard.clone();
        drop(routing_guard);

        // Match host using existing routing logic
        let backend_refs = match_host(&routing, &host)?;

        // Select backend using weighted random
        let backend_ref = select_backend_weighted(backend_refs)?;

        // Look up in backend pool
        let backends = self.backends.read();
        let backend = backends.get(&backend_ref.key)?;

        Some(backend.vcl_ptr())
    }

    fn healthy(&self, _ctx: &mut Ctx) -> (bool, SystemTime) {
        // Phase 1: Always report healthy
        // Phase 2: Check if any backend is healthy
        (true, SystemTime::now())
    }

    fn release(&self) {
        // Backends are dropped when director is dropped
    }

    fn list(&self, _ctx: &mut Ctx, vsb: &mut Buffer, _detailed: bool, _json: bool) {
        let routing = self.routing.read();
        let backends = self.backends.read();
        let total_vhosts = routing.exact.len() + routing.wildcards.len();
        let has_default = routing.default.is_some();

        let msg = format!(
            "{} vhosts, {} backends, default: {}",
            total_vhosts,
            backends.len(),
            if has_default { "yes" } else { "no" }
        );
        let _ = vsb.write(&msg);
    }
}

/// Wrapper around Arc<GhostDirector> to implement VclDirector (orphan rules workaround)
pub struct SharedGhostDirector(pub Arc<GhostDirector>);

impl VclDirector for SharedGhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<VCL_BACKEND> {
        self.0.resolve(ctx)
    }

    fn healthy(&self, ctx: &mut Ctx) -> (bool, SystemTime) {
        self.0.healthy(ctx)
    }

    fn release(&self) {
        self.0.release()
    }

    fn list(&self, ctx: &mut Ctx, vsb: &mut Buffer, detailed: bool, json: bool) {
        self.0.list(ctx, vsb, detailed, json)
    }
}

/// Match hostname against routing state
///
/// Returns the list of backend references for the matched vhost.
fn match_host<'a>(routing: &'a RoutingState, host: &str) -> Option<&'a [WeightedBackendRef]> {
    let host = host.to_lowercase();

    // 1. Exact match
    if let Some(refs) = routing.exact.get(&host) {
        if !refs.is_empty() {
            return Some(refs);
        }
    }

    // 2. Wildcard match
    for (pattern, refs) in &routing.wildcards {
        if matches_wildcard(pattern, &host) && !refs.is_empty() {
            return Some(refs);
        }
    }

    // 3. Default fallback
    if let Some(refs) = &routing.default {
        if !refs.is_empty() {
            return Some(refs);
        }
    }

    None
}

/// Check if a wildcard pattern matches a hostname
fn matches_wildcard(pattern: &str, host: &str) -> bool {
    if !pattern.starts_with("*.") {
        return false;
    }

    let suffix = &pattern[1..];
    if !host.ends_with(suffix) {
        return false;
    }

    // Check the matched part has no dots (single label only)
    let prefix_len = host.len() - suffix.len();
    let prefix = &host[..prefix_len];

    // Prefix must be non-empty and contain no dots
    !prefix.is_empty() && !prefix.contains('.')
}

/// Select a backend using weighted random selection
fn select_backend_weighted(refs: &[WeightedBackendRef]) -> Option<&WeightedBackendRef> {
    if refs.is_empty() {
        return None;
    }

    if refs.len() == 1 {
        return Some(&refs[0]);
    }

    // Calculate total weight
    let total_weight: u32 = refs.iter().map(|r| r.weight).sum();

    if total_weight == 0 {
        return None;
    }

    // Random selection
    use rand::Rng;
    let mut rng = rand::thread_rng();
    let r = rng.gen_range(0..total_weight);

    let mut cumulative = 0u32;
    for backend_ref in refs {
        cumulative += backend_ref.weight;
        if r < cumulative {
            return Some(backend_ref);
        }
    }

    // Fallback (shouldn't happen if weights are valid)
    Some(&refs[0])
}

/// Convert StrOrBytes to Cow<str> if possible
fn str_or_bytes_to_cow<'a>(sob: &'a StrOrBytes<'a>) -> Option<Cow<'a, str>> {
    match sob {
        StrOrBytes::Utf8(s) => Some(Cow::Borrowed(s)),
        StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok().map(Cow::Borrowed),
    }
}

/// Get Host header value (without port)
fn get_host_header(http: &HttpHeaders) -> Option<String> {
    let host_value = http.header("host")?;
    let host_str = str_or_bytes_to_cow(&host_value)?;
    // Strip port if present
    let host = host_str.split(':').next()?;
    Some(host.to_lowercase())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_matches_wildcard() {
        assert!(matches_wildcard("*.example.com", "foo.example.com"));
        assert!(matches_wildcard("*.example.com", "bar.example.com"));
        assert!(!matches_wildcard("*.example.com", "foo.bar.example.com"));
        assert!(!matches_wildcard("*.example.com", ".example.com"));
        assert!(!matches_wildcard("*.example.com", "example.com"));
    }

    #[test]
    fn test_backend_ref_creation() {
        let ref1 = WeightedBackendRef {
            key: "10.0.0.1:8080".to_string(),
            weight: 100,
        };
        assert_eq!(ref1.key, "10.0.0.1:8080");
        assert_eq!(ref1.weight, 100);
    }

    #[test]
    fn test_select_backend_weighted_single() {
        let refs = vec![WeightedBackendRef {
            key: "10.0.0.1:8080".to_string(),
            weight: 100,
        }];
        let selected = select_backend_weighted(&refs).unwrap();
        assert_eq!(selected.key, "10.0.0.1:8080");
    }

    #[test]
    fn test_select_backend_weighted_distribution() {
        let refs = vec![
            WeightedBackendRef {
                key: "10.0.0.1:8080".to_string(),
                weight: 90,
            },
            WeightedBackendRef {
                key: "10.0.0.2:8080".to_string(),
                weight: 10,
            },
        ];

        // Run many selections and check distribution
        let mut counts = HashMap::new();
        for _ in 0..1000 {
            let selected = select_backend_weighted(&refs).unwrap();
            *counts.entry(selected.key.clone()).or_insert(0) += 1;
        }

        let count_1 = *counts.get("10.0.0.1:8080").unwrap_or(&0);
        let count_2 = *counts.get("10.0.0.2:8080").unwrap_or(&0);

        // Allow for statistical variance (should be roughly 900:100)
        assert!(
            count_1 > 800,
            "10.0.0.1 selected {} times, expected ~900",
            count_1
        );
        assert!(
            count_2 < 200,
            "10.0.0.2 selected {} times, expected ~100",
            count_2
        );
    }

    #[test]
    fn test_select_backend_weighted_empty() {
        let refs: Vec<WeightedBackendRef> = vec![];
        assert!(select_backend_weighted(&refs).is_none());
    }

    #[test]
    fn test_match_host_exact() {
        let routing = RoutingState {
            exact: {
                let mut map = HashMap::new();
                map.insert(
                    "api.example.com".to_string(),
                    vec![WeightedBackendRef {
                        key: "10.0.0.1:8080".to_string(),
                        weight: 100,
                    }],
                );
                map
            },
            wildcards: vec![],
            default: None,
        };

        let result = match_host(&routing, "api.example.com");
        assert!(result.is_some());
        assert_eq!(result.unwrap()[0].key, "10.0.0.1:8080");
    }

    #[test]
    fn test_match_host_wildcard() {
        let routing = RoutingState {
            exact: HashMap::new(),
            wildcards: vec![(
                "*.example.com".to_string(),
                vec![WeightedBackendRef {
                    key: "10.0.0.1:8080".to_string(),
                    weight: 100,
                }],
            )],
            default: None,
        };

        let result = match_host(&routing, "foo.example.com");
        assert!(result.is_some());
        assert_eq!(result.unwrap()[0].key, "10.0.0.1:8080");
    }

    #[test]
    fn test_match_host_default() {
        let routing = RoutingState {
            exact: HashMap::new(),
            wildcards: vec![],
            default: Some(vec![WeightedBackendRef {
                key: "10.0.0.99:80".to_string(),
                weight: 100,
            }]),
        };

        let result = match_host(&routing, "unknown.example.com");
        assert!(result.is_some());
        assert_eq!(result.unwrap()[0].key, "10.0.0.99:80");
    }

    #[test]
    fn test_match_host_no_match() {
        let routing = RoutingState {
            exact: HashMap::new(),
            wildcards: vec![],
            default: None,
        };

        let result = match_host(&routing, "unknown.example.com");
        assert!(result.is_none());
    }
}
