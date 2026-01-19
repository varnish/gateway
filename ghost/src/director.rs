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
use regex::Regex;
use varnish::ffi::VCL_BACKEND;
use varnish::vcl::{Backend, Buffer, Ctx, HttpHeaders, StrOrBytes, VclDirector, VclError};

use crate::backend_pool::BackendPool;
use crate::config::{Config, ConfigV2, PathMatch, PathMatchType};
use crate::not_found_backend::{NotFoundBackend, NotFoundBody};

/// Wrapper for VCL_BACKEND pointer that implements Send + Sync
///
/// This is safe because Varnish runs single-threaded per worker,
/// and the backend pointer is valid for the lifetime of the director.
struct SendSyncBackend(VCL_BACKEND);

unsafe impl Send for SendSyncBackend {}
unsafe impl Sync for SendSyncBackend {}

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

/// Compiled path match for efficient matching
#[derive(Debug, Clone)]
pub enum PathMatchCompiled {
    Exact(String),
    PathPrefix(String),
    Regex(String), // Store pattern as string; compile on demand with caching
}

impl PathMatchCompiled {
    /// Create from config PathMatch
    fn from_config(pm: &PathMatch) -> Self {
        match pm.match_type {
            PathMatchType::Exact => PathMatchCompiled::Exact(pm.value.clone()),
            PathMatchType::PathPrefix => PathMatchCompiled::PathPrefix(pm.value.clone()),
            PathMatchType::RegularExpression => PathMatchCompiled::Regex(pm.value.clone()),
        }
    }

    /// Check if this path match matches the given path
    fn matches(&self, path: &str, regex_cache: &RwLock<HashMap<String, Arc<Regex>>>) -> bool {
        match self {
            PathMatchCompiled::Exact(value) => path == value,
            PathMatchCompiled::PathPrefix(prefix) => matches_path_prefix(prefix, path),
            PathMatchCompiled::Regex(pattern) => {
                // Try to get from cache first (read lock)
                {
                    let cache = regex_cache.read();
                    if let Some(re) = cache.get(pattern) {
                        return re.is_match(path);
                    }
                }

                // Cache miss - compile and cache (write lock)
                let mut cache = regex_cache.write();
                // Double-check in case another thread compiled it
                if let Some(re) = cache.get(pattern) {
                    return re.is_match(path);
                }

                // Compile regex
                match Regex::new(pattern) {
                    Ok(re) => {
                        let is_match = re.is_match(path);
                        cache.insert(pattern.clone(), Arc::new(re));
                        is_match
                    }
                    Err(_) => false, // Invalid regex doesn't match
                }
            }
        }
    }
}

/// Route entry with optional path matching (v2)
#[derive(Debug, Clone)]
pub struct RouteEntry {
    pub path_match: Option<PathMatchCompiled>,
    pub backends: Vec<WeightedBackendRef>,
    pub priority: i32,
}

/// Routing state for v2 configuration with path-based routing
#[derive(Debug, Clone)]
pub struct RoutingStateV2 {
    /// Exact hostname matches with path-based routes
    pub exact: HashMap<String, Vec<RouteEntry>>,
    /// Wildcard hostname patterns with path-based routes (in order)
    pub wildcards: Vec<(String, Vec<RouteEntry>)>,
    /// Default fallback backends
    pub default: Option<Vec<WeightedBackendRef>>,
    /// Regex compilation cache (shared, lock-protected)
    pub regex_cache: Arc<RwLock<HashMap<String, Arc<Regex>>>>,
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

/// Build v2 routing state from configuration with path-based routing
pub fn build_routing_state_v2(
    config: &ConfigV2,
    backend_pool: &mut BackendPool,
    ctx: &mut Ctx,
) -> Result<RoutingStateV2, VclError> {
    let mut exact = HashMap::new();
    let mut wildcards = Vec::new();

    // Process vhosts
    for (hostname, vhost) in &config.vhosts {
        let mut route_entries = Vec::new();

        // Process each route in the vhost
        for route in &vhost.routes {
            let mut backend_refs = Vec::new();

            // Create backend refs for each backend in the route
            for backend in &route.backends {
                let key = backend_pool.get_or_create(ctx, &backend.address, backend.port)?;
                backend_refs.push(WeightedBackendRef {
                    key,
                    weight: backend.weight,
                });
            }

            let path_match = route
                .path_match
                .as_ref()
                .map(|pm| PathMatchCompiled::from_config(pm));

            route_entries.push(RouteEntry {
                path_match,
                backends: backend_refs,
                priority: route.priority,
            });
        }

        // Sort routes by priority (descending - higher priority first)
        route_entries.sort_by(|a, b| b.priority.cmp(&a.priority));

        // Add default_backends as lowest priority route if present
        if !vhost.default_backends.is_empty() {
            let mut default_refs = Vec::new();
            for backend in &vhost.default_backends {
                let key = backend_pool.get_or_create(ctx, &backend.address, backend.port)?;
                default_refs.push(WeightedBackendRef {
                    key,
                    weight: backend.weight,
                });
            }
            route_entries.push(RouteEntry {
                path_match: None,
                backends: default_refs,
                priority: 0,
            });
        }

        // Categorize into exact or wildcard
        if hostname.starts_with("*.") {
            wildcards.push((hostname.clone(), route_entries));
        } else {
            exact.insert(hostname.clone(), route_entries);
        }
    }

    // Process global default
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

    Ok(RoutingStateV2 {
        exact,
        wildcards,
        default,
        regex_cache: Arc::new(RwLock::new(HashMap::new())),
    })
}

/// Routing state that can be either v1 or v2
#[derive(Debug, Clone)]
pub enum AnyRoutingState {
    V1(Arc<RoutingState>),
    V2(Arc<RoutingStateV2>),
}

/// Ghost director implementation
pub struct GhostDirector {
    /// Routing state (wrapped for hot-reload)
    routing: RwLock<AnyRoutingState>,
    /// Backend pool (owned by this director)
    backends: RwLock<BackendPool>,
    /// Path to config file (for reload)
    config_path: PathBuf,
    /// Synthetic 404 backend for undefined vhosts (stored backend must outlive this director)
    not_found_backend: SendSyncBackend,
}

impl GhostDirector {
    /// Create a new Ghost director with v1 routing
    ///
    /// Returns (director, not_found_backend). The Backend must be kept alive
    /// for the lifetime of the director.
    pub fn new(
        ctx: &mut Ctx,
        routing: Arc<RoutingState>,
        backends: BackendPool,
        config_path: PathBuf,
    ) -> Result<(Self, Backend<NotFoundBackend, NotFoundBody>), VclError> {
        // Create synthetic 404 backend
        let not_found_backend = Backend::new(ctx, "ghost", "ghost_404", NotFoundBackend, false)?;
        let not_found_ptr = SendSyncBackend(not_found_backend.vcl_ptr());

        let director = Self {
            routing: RwLock::new(AnyRoutingState::V1(routing)),
            backends: RwLock::new(backends),
            config_path,
            not_found_backend: not_found_ptr,
        };

        Ok((director, not_found_backend))
    }

    /// Create a new Ghost director with v2 routing
    pub fn new_v2(
        ctx: &mut Ctx,
        routing: Arc<RoutingStateV2>,
        backends: BackendPool,
        config_path: PathBuf,
    ) -> Result<(Self, Backend<NotFoundBackend, NotFoundBody>), VclError> {
        let not_found_backend = Backend::new(ctx, "ghost", "ghost_404", NotFoundBackend, false)?;
        let not_found_ptr = SendSyncBackend(not_found_backend.vcl_ptr());

        let director = Self {
            routing: RwLock::new(AnyRoutingState::V2(routing)),
            backends: RwLock::new(backends),
            config_path,
            not_found_backend: not_found_ptr,
        };

        Ok((director, not_found_backend))
    }

    /// Reload configuration from disk
    ///
    /// Detects config version and loads appropriate routing state
    pub fn reload(&self, ctx: &mut Ctx) -> Result<(), String> {
        use std::fs;

        // Read config file to detect version
        let content = fs::read_to_string(&self.config_path)
            .map_err(|e| format!("Failed to read config: {}", e))?;

        // Parse just to get version
        let version_check: serde_json::Value = serde_json::from_str(&content)
            .map_err(|e| format!("Failed to parse config JSON: {}", e))?;

        let version = version_check
            .get("version")
            .and_then(|v| v.as_u64())
            .ok_or_else(|| "Config missing version field".to_string())?;

        let mut backend_pool = self.backends.write();

        let new_routing = match version {
            1 => {
                // Load v1 config
                let config = crate::config::load(&self.config_path)?;
                let routing = build_routing_state(&config, &mut backend_pool, ctx)
                    .map_err(|e| format!("Failed to build v1 routing state: {}", e))?;
                AnyRoutingState::V1(Arc::new(routing))
            }
            2 => {
                // Load v2 config
                let config = crate::config::load_v2(&self.config_path)?;
                let routing = build_routing_state_v2(&config, &mut backend_pool, ctx)
                    .map_err(|e| format!("Failed to build v2 routing state: {}", e))?;
                AnyRoutingState::V2(Arc::new(routing))
            }
            _ => {
                return Err(format!("Unsupported config version: {}", version));
            }
        };

        drop(backend_pool);

        // Atomically update routing
        let mut guard = self.routing.write();
        *guard = new_routing;

        Ok(())
    }
}

impl VclDirector for GhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<VCL_BACKEND> {
        // Get bereq for Host and URL extraction
        let bereq = ctx.http_bereq.as_ref()?;

        // Get Host header
        let host = get_host_header(bereq)?;

        // Get routing state
        let routing_guard = self.routing.read();
        let routing = routing_guard.clone();
        drop(routing_guard);

        // Match based on routing version
        let backend_refs = match &routing {
            AnyRoutingState::V1(v1_routing) => {
                // V1: hostname-only matching
                match match_host(v1_routing, &host) {
                    Some(refs) => refs,
                    None => {
                        // Return 404 backend for undefined vhosts
                        return Some(self.not_found_backend.0);
                    }
                }
            }
            AnyRoutingState::V2(v2_routing) => {
                // V2: hostname + path matching
                // Extract URL from bereq
                let url = bereq
                    .header("url")
                    .and_then(|u| match u {
                        StrOrBytes::Utf8(s) => Some(s),
                        StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                    })
                    .unwrap_or("/");

                let path = extract_path(url);

                match match_host_and_path(v2_routing, &host, path) {
                    Some(refs) => refs,
                    None => {
                        // Return 404 backend for undefined vhosts
                        return Some(self.not_found_backend.0);
                    }
                }
            }
        };

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

        let (total_vhosts, has_default) = match &*routing {
            AnyRoutingState::V1(v1) => {
                (v1.exact.len() + v1.wildcards.len(), v1.default.is_some())
            }
            AnyRoutingState::V2(v2) => {
                (v2.exact.len() + v2.wildcards.len(), v2.default.is_some())
            }
        };

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

/// Match hostname and path against v2 routing state
///
/// Returns the list of backend references for the matched route.
/// Matching priority:
/// 1. Exact hostname > wildcard hostname > default
/// 2. Within matched vhost, iterate routes by priority
/// 3. First route whose path matches wins
fn match_host_and_path<'a>(
    routing: &'a RoutingStateV2,
    host: &str,
    path: &str,
) -> Option<&'a [WeightedBackendRef]> {
    let host = host.to_lowercase();

    // 1. Try exact hostname match
    if let Some(routes) = routing.exact.get(&host) {
        if let Some(backends) = match_routes(routes, path, &routing.regex_cache) {
            return Some(backends);
        }
    }

    // 2. Try wildcard hostname match
    for (pattern, routes) in &routing.wildcards {
        if matches_wildcard(pattern, &host) {
            if let Some(backends) = match_routes(routes, path, &routing.regex_cache) {
                return Some(backends);
            }
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

/// Match path against a list of route entries (already sorted by priority)
fn match_routes<'a>(
    routes: &'a [RouteEntry],
    path: &str,
    regex_cache: &RwLock<HashMap<String, Arc<Regex>>>,
) -> Option<&'a [WeightedBackendRef]> {
    for route in routes {
        // Check if path matches this route
        let path_matches = match &route.path_match {
            Some(pm) => pm.matches(path, regex_cache),
            None => true, // No path match = matches all paths
        };

        if path_matches && !route.backends.is_empty() {
            return Some(&route.backends);
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

/// Extract path from URL (without query string)
fn extract_path(url: &str) -> &str {
    // Remove query string and fragment
    let path_end = url.find('?').or_else(|| url.find('#')).unwrap_or(url.len());
    let path = &url[..path_end];

    // Path should start with /
    if path.is_empty() {
        "/"
    } else {
        path
    }
}

/// Match path prefix according to Gateway API semantics
///
/// Gateway API PathPrefix matching is element-wise, not simple string prefix:
/// - "/api" matches "/api" and "/api/v2" but NOT "/api2"
/// - Matching is done on path element boundaries (/)
fn matches_path_prefix(prefix: &str, path: &str) -> bool {
    if prefix == "/" {
        // Root prefix matches everything
        return true;
    }

    if path == prefix {
        // Exact match
        return true;
    }

    // Check if path starts with prefix followed by /
    if path.starts_with(prefix) {
        let remainder = &path[prefix.len()..];
        // Must be followed by / for element-wise matching
        return remainder.starts_with('/');
    }

    false
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

    #[test]
    fn test_extract_path() {
        assert_eq!(extract_path("/api/users"), "/api/users");
        assert_eq!(extract_path("/api/users?foo=bar"), "/api/users");
        assert_eq!(extract_path("/api/users#fragment"), "/api/users");
        assert_eq!(extract_path("/api/users?foo=bar#fragment"), "/api/users");
        assert_eq!(extract_path(""), "/");
    }

    #[test]
    fn test_matches_path_prefix() {
        // Root prefix matches everything
        assert!(matches_path_prefix("/", "/"));
        assert!(matches_path_prefix("/", "/api"));
        assert!(matches_path_prefix("/", "/api/users"));

        // Exact match
        assert!(matches_path_prefix("/api", "/api"));

        // Prefix with trailing slash
        assert!(matches_path_prefix("/api", "/api/users"));
        assert!(matches_path_prefix("/api", "/api/v2/users"));

        // Should NOT match if not on element boundary
        assert!(!matches_path_prefix("/api", "/api2"));
        assert!(!matches_path_prefix("/api", "/apiusers"));

        // Different path
        assert!(!matches_path_prefix("/api", "/web"));
    }

    #[test]
    fn test_path_match_compiled_exact() {
        let pm = PathMatchCompiled::Exact("/api/v2/users".to_string());
        let cache = RwLock::new(HashMap::new());

        assert!(pm.matches("/api/v2/users", &cache));
        assert!(!pm.matches("/api/v2/user", &cache));
        assert!(!pm.matches("/api/v2/users/123", &cache));
    }

    #[test]
    fn test_path_match_compiled_prefix() {
        let pm = PathMatchCompiled::PathPrefix("/api".to_string());
        let cache = RwLock::new(HashMap::new());

        assert!(pm.matches("/api", &cache));
        assert!(pm.matches("/api/users", &cache));
        assert!(pm.matches("/api/v2/users", &cache));
        assert!(!pm.matches("/api2", &cache));
        assert!(!pm.matches("/web", &cache));
    }

    #[test]
    fn test_path_match_compiled_regex() {
        let pm = PathMatchCompiled::Regex(r"^/files/\d+$".to_string());
        let cache = RwLock::new(HashMap::new());

        assert!(pm.matches("/files/123", &cache));
        assert!(pm.matches("/files/456", &cache));
        assert!(!pm.matches("/files/abc", &cache));
        assert!(!pm.matches("/files/", &cache));
        assert!(!pm.matches("/files/123/extra", &cache));

        // Verify regex is cached
        assert_eq!(cache.read().len(), 1);
    }

    #[test]
    fn test_path_match_compiled_invalid_regex() {
        let pm = PathMatchCompiled::Regex(r"[invalid(".to_string());
        let cache = RwLock::new(HashMap::new());

        // Invalid regex should not match anything
        assert!(!pm.matches("/anything", &cache));
    }

    #[test]
    fn test_match_routes() {
        let cache = RwLock::new(HashMap::new());

        let routes = vec![
            RouteEntry {
                path_match: Some(PathMatchCompiled::Exact("/api/v2/users".to_string())),
                backends: vec![WeightedBackendRef {
                    key: "backend1".to_string(),
                    weight: 100,
                }],
                priority: 10000,
            },
            RouteEntry {
                path_match: Some(PathMatchCompiled::PathPrefix("/api".to_string())),
                backends: vec![WeightedBackendRef {
                    key: "backend2".to_string(),
                    weight: 100,
                }],
                priority: 1040,
            },
            RouteEntry {
                path_match: None, // default route
                backends: vec![WeightedBackendRef {
                    key: "backend3".to_string(),
                    weight: 100,
                }],
                priority: 0,
            },
        ];

        // Exact match wins
        let result = match_routes(&routes, "/api/v2/users", &cache);
        assert!(result.is_some());
        assert_eq!(result.unwrap()[0].key, "backend1");

        // Prefix match
        let result = match_routes(&routes, "/api/v1/users", &cache);
        assert!(result.is_some());
        assert_eq!(result.unwrap()[0].key, "backend2");

        // Default route
        let result = match_routes(&routes, "/web", &cache);
        assert!(result.is_some());
        assert_eq!(result.unwrap()[0].key, "backend3");
    }
}
