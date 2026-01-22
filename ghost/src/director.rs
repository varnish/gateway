//! Director implementation for Ghost VMOD
//!
//! The GhostDirector handles host-based routing and weighted backend selection.
//! It implements the VclDirector trait to integrate with Varnish's director system.

use std::borrow::Cow;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::SystemTime;

use arc_swap::ArcSwap;
use regex::Regex;
use varnish::ffi::VCL_BACKEND;
use varnish::vcl::{Backend, Buffer, Ctx, HttpHeaders, StrOrBytes, VclDirector, VclError};

use crate::backend_pool::BackendPool;
use crate::config::{Config, HeaderMatch, MatchType, PathMatch, PathMatchType, QueryParamMatch};
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


/// Compiled path match for efficient matching
#[derive(Debug, Clone)]
pub enum PathMatchCompiled {
    Exact(String),
    PathPrefix(String),
    Regex(Arc<Regex>), // Pre-compiled regex (immutable until next reload)
}

impl PathMatchCompiled {
    /// Create from config PathMatch
    fn from_config(pm: &PathMatch) -> Result<Self, String> {
        match pm.match_type {
            PathMatchType::Exact => Ok(PathMatchCompiled::Exact(pm.value.clone())),
            PathMatchType::PathPrefix => Ok(PathMatchCompiled::PathPrefix(pm.value.clone())),
            PathMatchType::RegularExpression => {
                // Compile regex at config load time
                let re = Regex::new(&pm.value)
                    .map_err(|e| format!("Invalid regex pattern '{}': {}", pm.value, e))?;
                Ok(PathMatchCompiled::Regex(Arc::new(re)))
            }
        }
    }

    /// Check if this path match matches the given path
    fn matches(&self, path: &str) -> bool {
        match self {
            PathMatchCompiled::Exact(value) => path == value,
            PathMatchCompiled::PathPrefix(prefix) => matches_path_prefix(prefix, path),
            PathMatchCompiled::Regex(re) => re.is_match(path),
        }
    }
}

/// Compiled header match for efficient matching
#[derive(Debug, Clone)]
pub enum HeaderMatchCompiled {
    Exact { name: String, value: String },
    Regex { name: String, regex: Arc<Regex> },
}

impl HeaderMatchCompiled {
    /// Create from config HeaderMatch
    fn from_config(hm: &HeaderMatch) -> Result<Self, String> {
        let name = hm.name.to_lowercase(); // Case-insensitive per HTTP spec
        match hm.match_type {
            MatchType::Exact => Ok(HeaderMatchCompiled::Exact {
                name,
                value: hm.value.clone(),
            }),
            MatchType::RegularExpression => {
                let re = Regex::new(&hm.value)
                    .map_err(|e| format!("Invalid regex '{}': {}", hm.value, e))?;
                Ok(HeaderMatchCompiled::Regex {
                    name,
                    regex: Arc::new(re),
                })
            }
        }
    }

    /// Check if this header match matches the given request
    fn matches(&self, bereq: &HttpHeaders) -> bool {
        match self {
            HeaderMatchCompiled::Exact { name, value } => {
                let header_value = match bereq.header(name) {
                    Some(v) => {
                        // Convert to String to avoid lifetime issues
                        match v {
                            StrOrBytes::Utf8(s) => s.to_string(),
                            StrOrBytes::Bytes(b) => {
                                match std::str::from_utf8(b) {
                                    Ok(s) => s.to_string(),
                                    Err(_) => return false,
                                }
                            }
                        }
                    }
                    None => return false,
                };
                &header_value == value
            }
            HeaderMatchCompiled::Regex { name, regex } => {
                let header_value = match bereq.header(name) {
                    Some(v) => {
                        // Convert to String to avoid lifetime issues
                        match v {
                            StrOrBytes::Utf8(s) => s.to_string(),
                            StrOrBytes::Bytes(b) => {
                                match std::str::from_utf8(b) {
                                    Ok(s) => s.to_string(),
                                    Err(_) => return false,
                                }
                            }
                        }
                    }
                    None => return false,
                };
                regex.is_match(&header_value)
            }
        }
    }
}

/// Compiled query parameter match for efficient matching
#[derive(Debug, Clone)]
pub enum QueryParamMatchCompiled {
    Exact { name: String, value: String },
    Regex { name: String, regex: Arc<Regex> },
}

impl QueryParamMatchCompiled {
    /// Create from config QueryParamMatch
    fn from_config(qpm: &QueryParamMatch) -> Result<Self, String> {
        match qpm.match_type {
            MatchType::Exact => Ok(QueryParamMatchCompiled::Exact {
                name: qpm.name.clone(),
                value: qpm.value.clone(),
            }),
            MatchType::RegularExpression => {
                let re = Regex::new(&qpm.value)
                    .map_err(|e| format!("Invalid regex '{}': {}", qpm.value, e))?;
                Ok(QueryParamMatchCompiled::Regex {
                    name: qpm.name.clone(),
                    regex: Arc::new(re),
                })
            }
        }
    }

    /// Check if this query param match matches the given query string
    fn matches(&self, query_string: &str) -> bool {
        let params = parse_query_string(query_string);
        let param_value = match self {
            QueryParamMatchCompiled::Exact { name, .. }
            | QueryParamMatchCompiled::Regex { name, .. } => match params.get(name.as_str()) {
                Some(v) => v,
                None => return false,
            },
        };

        match self {
            QueryParamMatchCompiled::Exact { value, .. } => param_value == value,
            QueryParamMatchCompiled::Regex { regex, .. } => regex.is_match(param_value),
        }
    }
}

/// Route entry with optional path matching (v2)
#[derive(Debug, Clone)]
pub struct RouteEntry {
    pub path_match: Option<PathMatchCompiled>,
    pub method: Option<String>,
    pub headers: Vec<HeaderMatchCompiled>,
    pub query_params: Vec<QueryParamMatchCompiled>,
    pub backends: Vec<WeightedBackendRef>,
    pub priority: i32,
}

/// Routing state for v2 configuration with path-based routing
#[derive(Debug, Clone)]
pub struct RoutingState {
    /// Exact hostname matches with path-based routes
    pub exact: HashMap<String, Vec<RouteEntry>>,
    /// Wildcard hostname patterns with path-based routes (in order)
    pub wildcards: Vec<(String, Vec<RouteEntry>)>,
    /// Default fallback backends
    pub default: Option<Vec<WeightedBackendRef>>,
}

/// Build routing state from configuration with path-based routing
pub fn build_routing_state(
    config: &Config,
    backend_pool: &mut BackendPool,
    ctx: &mut Ctx,
) -> Result<RoutingState, VclError> {
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

            let path_match = match route.path_match.as_ref() {
                Some(pm) => Some(
                    PathMatchCompiled::from_config(pm)
                        .map_err(|e| VclError::new(format!("Invalid path match: {}", e)))?,
                ),
                None => None,
            };

            // Compile header matches
            let headers: Result<Vec<_>, _> = route
                .headers
                .iter()
                .map(HeaderMatchCompiled::from_config)
                .collect();
            let headers = headers.map_err(|e| VclError::new(format!("Invalid header match: {}", e)))?;

            // Compile query param matches
            let query_params: Result<Vec<_>, _> = route
                .query_params
                .iter()
                .map(QueryParamMatchCompiled::from_config)
                .collect();
            let query_params = query_params.map_err(|e| VclError::new(format!("Invalid query param match: {}", e)))?;

            route_entries.push(RouteEntry {
                path_match,
                method: route.method.clone(),
                headers,
                query_params,
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
                method: None,
                headers: Vec::new(),
                query_params: Vec::new(),
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

    Ok(RoutingState {
        exact,
        wildcards,
        default: None,
    })
}

/// Collect all backend keys referenced in routing state
fn collect_referenced_backends(routing: &RoutingState) -> std::collections::HashSet<String> {
    let mut keys = std::collections::HashSet::new();

    // Collect from exact matches
    for routes in routing.exact.values() {
        for route in routes {
            for backend_ref in &route.backends {
                keys.insert(backend_ref.key.clone());
            }
        }
    }

    // Collect from wildcards
    for (_, routes) in &routing.wildcards {
        for route in routes {
            for backend_ref in &route.backends {
                keys.insert(backend_ref.key.clone());
            }
        }
    }

    // Collect from default
    if let Some(refs) = &routing.default {
        for backend_ref in refs {
            keys.insert(backend_ref.key.clone());
        }
    }

    keys
}

/// Ghost director implementation
pub struct GhostDirector {
    /// Routing state (atomic swap for lock-free reads)
    routing: ArcSwap<RoutingState>,
    /// Backend pool (atomic swap for lock-free reads)
    backends: ArcSwap<BackendPool>,
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
            routing: ArcSwap::new(Arc::clone(&routing)),
            backends: ArcSwap::new(Arc::new(backends)),
            config_path,
            not_found_backend: not_found_ptr,
        };

        Ok((director, not_found_backend))
    }

    /// Reload configuration from disk
    pub fn reload(&self, ctx: &mut Ctx) -> Result<(), String> {
        // Clone current backend pool for modification
        let current_backends = self.backends.load();
        let mut backend_pool = (**current_backends).clone();

        // Load config
        let config = crate::config::load(&self.config_path)?;
        let new_routing = build_routing_state(&config, &mut backend_pool, ctx)
            .map_err(|e| format!("Failed to build routing state: {}", e))?;

        // Collect all backend keys referenced in the new routing state
        let referenced_keys = collect_referenced_backends(&new_routing);

        // Clean up unreferenced backends from the pool
        backend_pool.retain_only(&referenced_keys);

        // Atomic swap of routing and backends
        self.routing.store(Arc::new(new_routing));
        self.backends.store(Arc::new(backend_pool));

        Ok(())
    }
}

impl VclDirector for GhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<VCL_BACKEND> {
        // Get bereq for Host and URL extraction
        let bereq = ctx.http_bereq.as_ref()?;

        // Get Host header
        let host = get_host_header(bereq)?;

        // Load routing state (lock-free atomic load)
        let routing = self.routing.load();

        // Extract URL from bereq
        let url = bereq
            .url()
            .and_then(|u| match u {
                StrOrBytes::Utf8(s) => Some(s),
                StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
            })
            .unwrap_or("/");

        // Extract path and query string
        let (path, query_string) = extract_path_and_query(url);

        // Extract method
        let method = bereq
            .method()
            .and_then(|m| match m {
                StrOrBytes::Utf8(s) => Some(s),
                StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
            })
            .unwrap_or("GET");

        // Hostname + path + method + headers + query matching
        let backend_refs = match match_host_and_path(&routing, &host, path, method, bereq, query_string) {
            Some(refs) => refs,
            None => {
                // Return 404 backend for undefined vhosts/paths
                return Some(self.not_found_backend.0);
            }
        };

        // Select backend using weighted random
        let backend_ref = select_backend_weighted(backend_refs)?;

        // Look up in backend pool (lock-free atomic load)
        let backends = self.backends.load();
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
        let routing = self.routing.load();
        let backends = self.backends.load();

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

/// Match hostname and path against routing state
///
/// Returns the list of backend references for the matched route.
/// Matching priority:
/// 1. Exact hostname > wildcard hostname > default
/// 2. Within matched vhost, iterate routes by priority
/// 3. First route whose path, method, headers, and query params match wins
fn match_host_and_path<'a>(
    routing: &'a RoutingState,
    host: &str,
    path: &str,
    method: &str,
    bereq: &HttpHeaders,
    query_string: Option<&str>,
) -> Option<&'a [WeightedBackendRef]> {
    let host = host.to_lowercase();

    // 1. Try exact hostname match
    if let Some(routes) = routing.exact.get(&host) {
        if let Some(backends) = match_routes(routes, path, method, bereq, query_string) {
            return Some(backends);
        }
    }

    // 2. Try wildcard hostname match
    for (pattern, routes) in &routing.wildcards {
        if matches_wildcard(pattern, &host) {
            if let Some(backends) = match_routes(routes, path, method, bereq, query_string) {
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

/// Match routes against all conditions (already sorted by priority)
/// All conditions within a match are AND-ed together
fn match_routes<'a>(
    routes: &'a [RouteEntry],
    path: &str,
    method: &str,
    bereq: &HttpHeaders,
    query_string: Option<&str>,
) -> Option<&'a [WeightedBackendRef]> {
    for route in routes {
        // Check path match
        if let Some(ref pm) = route.path_match {
            if !pm.matches(path) {
                continue;
            }
        }

        // Check method match
        if let Some(ref m) = route.method {
            if m != method {
                continue;
            }
        }

        // Check header matches (all must match - AND)
        if !route.headers.iter().all(|hm| hm.matches(bereq)) {
            continue;
        }

        // Check query param matches (all must match - AND)
        if let Some(qs) = query_string {
            if !route.query_params.iter().all(|qpm| qpm.matches(qs)) {
                continue;
            }
        } else if !route.query_params.is_empty() {
            // No query string but route requires query params
            continue;
        }

        // All conditions matched
        if !route.backends.is_empty() {
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

/// Extract path and query string from URL
/// Returns (path, Some(query_string)) or (path, None)
fn extract_path_and_query(url: &str) -> (&str, Option<&str>) {
    if let Some(q_idx) = url.find('?') {
        let path = &url[..q_idx];
        let query_part = &url[q_idx + 1..];
        // Strip fragment if present
        let query = if let Some(f_idx) = query_part.find('#') {
            &query_part[..f_idx]
        } else {
            query_part
        };
        let final_path = if path.is_empty() { "/" } else { path };
        (final_path, Some(query))
    } else {
        // No query string, but check for fragment
        let path_end = url.find('#').unwrap_or(url.len());
        let path = &url[..path_end];
        let final_path = if path.is_empty() { "/" } else { path };
        (final_path, None)
    }
}

/// Parse query string into key-value pairs
/// Returns first value only for duplicate keys (per Gateway API spec)
fn parse_query_string(query: &str) -> HashMap<&str, &str> {
    let mut params = HashMap::new();
    for pair in query.split('&') {
        if let Some(eq_idx) = pair.find('=') {
            let key = &pair[..eq_idx];
            let value = &pair[eq_idx + 1..];
            // Only insert if key doesn't exist (first value wins)
            params.entry(key).or_insert(value);
        }
    }
    params
}

/// Match path prefix according to Gateway API semantics
///
/// Gateway API PathPrefix matching is element-wise, not simple string prefix:
/// - "/api" matches "/api" and "/api/v2" but NOT "/api2"
/// - "/api/" matches "/api/" and "/api/v2" (prefix already includes trailing /)
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

    // Check if path starts with prefix
    if !path.starts_with(prefix) {
        return false;
    }

    let remainder = &path[prefix.len()..];

    // If prefix ends with /, we're already at element boundary
    if prefix.ends_with('/') {
        return true;
    }

    // Otherwise, remainder must start with / for element-wise matching
    remainder.starts_with('/')
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
    fn test_matches_path_prefix_with_trailing_slash() {
        // Prefix with trailing slash - matches production config pattern
        assert!(matches_path_prefix("/api/", "/api/"));
        assert!(matches_path_prefix("/api/", "/api/users"));
        assert!(matches_path_prefix("/api/", "/api/v2/products"));

        // More specific prefixes with trailing slashes
        assert!(matches_path_prefix("/api/v2/", "/api/v2/"));
        assert!(matches_path_prefix("/api/v2/", "/api/v2/products"));
        assert!(matches_path_prefix("/api/v2/", "/api/v2/users/123"));

        assert!(matches_path_prefix("/api/v1/", "/api/v1/"));
        assert!(matches_path_prefix("/api/v1/", "/api/v1/status"));
        assert!(matches_path_prefix("/api/v1/", "/api/v1/health/check"));

        // Should NOT match different paths
        assert!(!matches_path_prefix("/api/v2/", "/api/v1/products"));
        assert!(!matches_path_prefix("/api/v1/", "/api/v2/status"));
        assert!(!matches_path_prefix("/api/", "/web/api"));

        // Should NOT match if path doesn't include the trailing slash
        assert!(!matches_path_prefix("/api/v2/", "/api/v2"));
        assert!(!matches_path_prefix("/api/v1/", "/api/v1"));
    }

    #[test]
    fn test_path_match_compiled_exact() {
        let pm = PathMatchCompiled::Exact("/api/v2/users".to_string());

        assert!(pm.matches("/api/v2/users"));
        assert!(!pm.matches("/api/v2/user"));
        assert!(!pm.matches("/api/v2/users/123"));
    }

    #[test]
    fn test_path_match_compiled_prefix() {
        let pm = PathMatchCompiled::PathPrefix("/api".to_string());

        assert!(pm.matches("/api"));
        assert!(pm.matches("/api/users"));
        assert!(pm.matches("/api/v2/users"));
        assert!(!pm.matches("/api2"));
        assert!(!pm.matches("/web"));
    }

    #[test]
    fn test_path_match_compiled_regex() {
        let re = Regex::new(r"^/files/\d+$").unwrap();
        let pm = PathMatchCompiled::Regex(Arc::new(re));

        assert!(pm.matches("/files/123"));
        assert!(pm.matches("/files/456"));
        assert!(!pm.matches("/files/abc"));
        assert!(!pm.matches("/files/"));
        assert!(!pm.matches("/files/123/extra"));
    }

    #[test]
    fn test_path_match_from_config_invalid_regex() {
        let pm = PathMatch {
            match_type: PathMatchType::RegularExpression,
            value: r"[invalid(".to_string(),
        };

        // Invalid regex should return an error
        let result = PathMatchCompiled::from_config(&pm);
        assert!(result.is_err());
    }

    #[test]
    fn test_path_match_compiled_from_routes() {
        // Test PathMatchCompiled with different route types
        let exact = PathMatchCompiled::Exact("/api/v2/users".to_string());
        assert!(exact.matches("/api/v2/users"));
        assert!(!exact.matches("/api/v2/user"));

        let prefix = PathMatchCompiled::PathPrefix("/api".to_string());
        assert!(prefix.matches("/api"));
        assert!(prefix.matches("/api/users"));
        assert!(!prefix.matches("/api2"));

        // Test with None path_match (matches all)
        // This verifies the behavior used in RouteEntry with None path_match
    }

    #[test]
    fn test_collect_referenced_backends_empty() {
        // Empty routing state
        let routing = RoutingState {
            exact: HashMap::new(),
            wildcards: vec![],
            default: None,
        };

        let keys = collect_referenced_backends(&routing);
        assert_eq!(keys.len(), 0);
    }

    #[test]
    fn test_collect_referenced_backends_all_variants() {
        // Create routing state with multiple routes per vhost
        let routing = RoutingState {
            exact: {
                let mut map = HashMap::new();
                map.insert(
                    "api.example.com".to_string(),
                    vec![
                        RouteEntry {
                            path_match: Some(PathMatchCompiled::PathPrefix("/v2".to_string())),
                            method: None,
                            headers: Vec::new(),
                            query_params: Vec::new(),
                            backends: vec![WeightedBackendRef {
                                key: "10.0.0.1:8080".to_string(),
                                weight: 100,
                            }],
                            priority: 100,
                        },
                        RouteEntry {
                            path_match: Some(PathMatchCompiled::PathPrefix("/v1".to_string())),
                            method: None,
                            headers: Vec::new(),
                            query_params: Vec::new(),
                            backends: vec![WeightedBackendRef {
                                key: "10.0.0.2:8080".to_string(),
                                weight: 100,
                            }],
                            priority: 50,
                        },
                        RouteEntry {
                            path_match: None,
                            method: None,
                            headers: Vec::new(),
                            query_params: Vec::new(),
                            backends: vec![WeightedBackendRef {
                                key: "10.0.0.3:8080".to_string(),
                                weight: 100,
                            }],
                            priority: 0,
                        },
                    ],
                );
                map
            },
            wildcards: vec![(
                "*.staging.example.com".to_string(),
                vec![RouteEntry {
                    path_match: None,
                    method: None,
                    headers: Vec::new(),
                    query_params: Vec::new(),
                    backends: vec![WeightedBackendRef {
                        key: "10.0.2.1:8080".to_string(),
                        weight: 100,
                    }],
                    priority: 0,
                }],
            )],
            default: Some(vec![WeightedBackendRef {
                key: "10.0.99.1:80".to_string(),
                weight: 100,
            }]),
        };

        let keys = collect_referenced_backends(&routing);

        // Should have all 5 unique backend keys
        assert_eq!(keys.len(), 5);
        assert!(keys.contains("10.0.0.1:8080"));
        assert!(keys.contains("10.0.0.2:8080"));
        assert!(keys.contains("10.0.0.3:8080"));
        assert!(keys.contains("10.0.2.1:8080"));
        assert!(keys.contains("10.0.99.1:80"));
    }
}
