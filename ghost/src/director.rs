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
use parking_lot::RwLock;
use regex::Regex;
use varnish::ffi::VCL_BACKEND;
use varnish::vcl::{Backend, Buffer, Ctx, HttpHeaders, LogTag, StrOrBytes, VclDirector, VclError};

use crate::backend_pool::BackendPool;
use crate::config::{Config, HeaderMatch, MatchType, PathMatch, PathMatchType, QueryParamMatch};
use crate::not_found_backend::{NotFoundBackend, NotFoundBody};
use crate::redirect_backend::{RedirectBackend, RedirectBody};
use crate::vhost_director::VhostDirector;

/// Wrapper for VCL_BACKEND pointer that implements Send + Sync
///
/// SAFETY: VCL_BACKEND is an opaque Varnish handle designed for multi-threaded use.
/// While individual Varnish workers are single-threaded, we use Arc and atomic
/// operations because multiple workers may access the same director concurrently
/// (via shared VCL state). The raw pointer is managed by Varnish's backend
/// infrastructure which provides its own synchronization guarantees.
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
                // Compile regex at runtime during config reload.
                // Note: In debug mode, regex compilation from Varnish worker threads
                // causes a crash due to threading/TLS conflicts. This only affects
                // debug builds; release builds work correctly.
                let re = Regex::new(&pm.value)
                    .map_err(|e| format!("Invalid regex pattern '{}': {}", pm.value, e))?;
                Ok(PathMatchCompiled::Regex(Arc::new(re)))
            }
        }
    }

    /// Check if this path match matches the given path
    pub fn matches(&self, path: &str) -> bool {
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
    /// Works with borrowed data - no allocations
    pub fn matches(&self, bereq: &HttpHeaders) -> bool {
        let header_value = match bereq.header(match self {
            HeaderMatchCompiled::Exact { name, .. } => name,
            HeaderMatchCompiled::Regex { name, .. } => name,
        }) {
            Some(v) => v,
            None => return false,
        };

        match self {
            HeaderMatchCompiled::Exact { value, .. } => match header_value {
                StrOrBytes::Utf8(s) => s == value,
                StrOrBytes::Bytes(b) => b == value.as_bytes(),
            },
            HeaderMatchCompiled::Regex { regex, .. } => match header_value {
                StrOrBytes::Utf8(s) => regex.is_match(s),
                StrOrBytes::Bytes(b) => {
                    // Only allocate if we have non-UTF8 bytes that need regex matching
                    std::str::from_utf8(b)
                        .map(|s| regex.is_match(s))
                        .unwrap_or(false)
                }
            },
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
    pub fn matches(&self, query_string: &str) -> bool {
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
    pub filters: Option<Arc<crate::config::RouteFilters>>,
    pub backends: Vec<WeightedBackendRef>,
    pub priority: i32,
}

/// Map of vhost directors for two-tier routing
#[derive(Debug, Clone)]
pub struct VhostDirectorMap {
    /// Exact hostname matches to vhost directors
    pub exact: HashMap<String, Arc<VhostDirector>>,
    /// Wildcard hostname patterns to vhost directors (in order)
    pub wildcards: Vec<(String, Arc<VhostDirector>)>,
}

/// Build vhost directors from configuration
///
/// Creates a VhostDirector for each vhost in the config. Each director handles
/// route matching and backend selection for its hostname.
pub fn build_vhost_directors(
    config: &Config,
    backend_pool: &mut BackendPool,
    ctx: &mut Ctx,
    redirect_backend: VCL_BACKEND,
) -> Result<VhostDirectorMap, VclError> {
    let mut exact = HashMap::new();
    let mut wildcards = Vec::new();

    // First pass: build routes and populate backend pool
    let mut vhost_routes: HashMap<String, Vec<RouteEntry>> = HashMap::new();

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

            let filters = route.filters.as_ref().map(|f| Arc::new(f.clone()));

            route_entries.push(RouteEntry {
                path_match,
                method: route.method.clone(),
                headers,
                query_params,
                filters,
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
                filters: None,
                backends: default_refs,
                priority: 0,
            });
        }

        // Store routes for second pass
        vhost_routes.insert(hostname.clone(), route_entries);
    }

    // Second pass: create VhostDirectors with shared backend pool
    // Now that all backends are created, wrap the pool in Arc
    let backend_pool_arc = Arc::new(backend_pool.clone());

    for (hostname, route_entries) in vhost_routes {
        // Create VhostDirector for this vhost
        let vhost_director = Arc::new(VhostDirector::new(
            hostname.clone(),
            route_entries,
            Arc::clone(&backend_pool_arc),
            redirect_backend,
        ));

        // Categorize into exact or wildcard
        if hostname.starts_with("*.") {
            wildcards.push((hostname, vhost_director));
        } else {
            exact.insert(hostname, vhost_director);
        }
    }

    Ok(VhostDirectorMap { exact, wildcards })
}

/// Collect all backend keys referenced in vhost directors
fn collect_referenced_backends_from_directors(directors: &VhostDirectorMap) -> std::collections::HashSet<String> {
    let mut keys = std::collections::HashSet::new();

    // Collect from exact matches
    for director in directors.exact.values() {
        for key in director.backend_keys() {
            keys.insert(key);
        }
    }

    // Collect from wildcards
    for (_, director) in &directors.wildcards {
        for key in director.backend_keys() {
            keys.insert(key);
        }
    }

    keys
}

/// Ghost director implementation
pub struct GhostDirector {
    /// Vhost directors (atomic swap for lock-free reads)
    vhost_directors: ArcSwap<VhostDirectorMap>,
    /// Backend pool (atomic swap for lock-free reads)
    backends: ArcSwap<BackendPool>,
    /// Path to config file (for reload)
    config_path: PathBuf,
    /// Synthetic 404 backend for undefined vhosts (stored backend must outlive this director)
    not_found_backend: SendSyncBackend,
    /// Synthetic redirect backend for RequestRedirect filters (stored backend must outlive this director)
    redirect_backend: SendSyncBackend,
    /// Last reload error message (for debugging)
    last_error: RwLock<Option<String>>,
}

/// Return type for GhostDirector::new() containing all necessary components
pub type GhostDirectorCreationResult = (
    GhostDirector,
    Backend<NotFoundBackend, NotFoundBody>,
    Backend<RedirectBackend, RedirectBody>,
);

impl GhostDirector {
    /// Create a new Ghost director with vhost directors
    ///
    /// Returns (director, not_found_backend, redirect_backend). The Backends must be kept alive
    /// for the lifetime of the director.
    pub fn new(
        ctx: &mut Ctx,
        vhost_directors: Arc<VhostDirectorMap>,
        backends: BackendPool,
        config_path: PathBuf,
    ) -> Result<GhostDirectorCreationResult, VclError> {
        // Create synthetic 404 backend
        let not_found_backend = Backend::new(ctx, "ghost", "ghost_404", NotFoundBackend, false)?;
        let not_found_ptr = SendSyncBackend(not_found_backend.vcl_ptr());

        // Create synthetic redirect backend
        let redirect_backend = Backend::new(ctx, "ghost", "ghost_redirect", RedirectBackend, false)?;
        let redirect_ptr = SendSyncBackend(redirect_backend.vcl_ptr());

        let director = Self {
            vhost_directors: ArcSwap::new(Arc::clone(&vhost_directors)),
            backends: ArcSwap::new(Arc::new(backends)),
            config_path,
            not_found_backend: not_found_ptr,
            redirect_backend: redirect_ptr,
            last_error: RwLock::new(None),
        };

        Ok((director, not_found_backend, redirect_backend))
    }

    /// Reload configuration from disk
    pub fn reload(&self, ctx: &mut Ctx) -> Result<(), String> {
        // Clone current backend pool for modification
        let current_backends = self.backends.load();
        let mut backend_pool = (**current_backends).clone();

        // Load config
        let config = crate::config::load(&self.config_path).map_err(|e| {
            let error_msg = format!("Ghost reload failed: {}", e);
            // Log to VSL for visibility in varnishlog
            ctx.log(LogTag::Error, &error_msg);
            // Store for VCL access
            *self.last_error.write() = Some(error_msg.clone());
            error_msg
        })?;

        // Build new vhost directors
        let new_directors = build_vhost_directors(&config, &mut backend_pool, ctx, self.redirect_backend.0).map_err(|e| {
            let error_msg = format!("Ghost reload failed: {}", e);
            // Log to VSL for visibility in varnishlog
            ctx.log(LogTag::Error, &error_msg);
            // Store for VCL access
            *self.last_error.write() = Some(error_msg.clone());
            error_msg
        })?;

        // Collect all backend keys referenced in the new directors
        let referenced_keys = collect_referenced_backends_from_directors(&new_directors);

        // Clean up unreferenced backends from the pool
        backend_pool.retain_only(&referenced_keys);

        // Atomic swap of vhost_directors and backends
        self.vhost_directors.store(Arc::new(new_directors));
        self.backends.store(Arc::new(backend_pool));

        // Clear error on success
        *self.last_error.write() = None;

        Ok(())
    }

    /// Get the last reload error message (if any)
    pub fn last_error(&self) -> Option<String> {
        self.last_error.read().clone()
    }

    /// JSON output format for backend.list -j
    fn list_json(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        let directors = self.vhost_directors.load();
        let backends = self.backends.load();

        let mut all_backends = Vec::new();

        // Collect from exact matches
        for director in directors.exact.values() {
            // Build the JSON object directly
            let selections = director.stats().backend_selections();
            let total = director.stats().total_requests();

            let backend_objs = crate::format::format_backend_selections_json(&selections, total);

            let obj = serde_json::json!({
                "name": format!("ghost.{}", director.hostname()),
                "type": "vhost_director",
                "admin": "auto",
                "health": if director.healthy(ctx).0 { "healthy" } else { "sick" },
                "routes": director.backend_keys().len(),
                "total_requests": total,
                "last_request": director.stats().last_request().map(|t| {
                    use crate::format::format_timestamp;
                    format_timestamp(Some(t))
                }),
                "backends": backend_objs
            });

            all_backends.push(obj);
        }

        // Collect from wildcards
        for (_, director) in &directors.wildcards {
            let selections = director.stats().backend_selections();
            let total = director.stats().total_requests();

            let backend_objs = crate::format::format_backend_selections_json(&selections, total);

            let obj = serde_json::json!({
                "name": format!("ghost.{}", director.hostname()),
                "type": "vhost_director",
                "admin": "auto",
                "health": if director.healthy(ctx).0 { "healthy" } else { "sick" },
                "routes": director.backend_keys().len(),
                "total_requests": total,
                "last_request": director.stats().last_request().map(|t| {
                    use crate::format::format_timestamp;
                    format_timestamp(Some(t))
                }),
                "backends": backend_objs
            });

            all_backends.push(obj);
        }

        let output = serde_json::json!({
            "backends": all_backends,
            "total_vhosts": directors.exact.len() + directors.wildcards.len(),
            "total_backends": backends.len()
        });

        let json_str = serde_json::to_string(&output).unwrap_or_else(|_| "{}".to_string());
        let _ = vsb.write(&json_str);
    }
}

impl VclDirector for GhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<VCL_BACKEND> {
        // Get bereq for Host header extraction
        let bereq = ctx.http_bereq.as_ref()?;

        // Get Host header
        let host = get_host_header(bereq)?;

        // Load vhost directors (lock-free atomic load)
        let directors = self.vhost_directors.load();

        // Match hostname to vhost director
        let vhost_director = match match_hostname(&directors, &host) {
            Some(dir) => dir,
            None => {
                // No vhost found for this hostname
                // Return 404 backend
                return Some(self.not_found_backend.0);
            }
        };

        // Delegate to vhost director for route matching and backend selection
        match vhost_director.resolve(ctx) {
            Some(backend) => Some(backend),
            None => {
                // No backend found for this path/method/headers
                // Return 404 backend
                Some(self.not_found_backend.0)
            }
        }
    }

    fn healthy(&self, ctx: &mut Ctx) -> (bool, SystemTime) {
        // Aggregate health across all vhost directors
        // Report healthy if any vhost has backends
        let directors = self.vhost_directors.load();

        // Check if any vhost director is healthy
        for director in directors.exact.values() {
            let (healthy, _) = director.healthy(ctx);
            if healthy {
                return (true, SystemTime::now());
            }
        }

        for (_, director) in &directors.wildcards {
            let (healthy, _) = director.healthy(ctx);
            if healthy {
                return (true, SystemTime::now());
            }
        }

        // No healthy vhosts
        (false, SystemTime::now())
    }

    fn release(&self) {
        // Backends are dropped when director is dropped
    }

    fn list(&self, ctx: &mut Ctx, vsb: &mut Buffer, detailed: bool, json: bool) {
        let directors = self.vhost_directors.load();

        if json {
            self.list_json(ctx, vsb);
        } else if detailed {
            // Detailed: each vhost gets detailed output
            for director in directors.exact.values() {
                director.list(ctx, vsb, true, false);
            }
            for (_, director) in &directors.wildcards {
                director.list(ctx, vsb, true, false);
            }
        } else {
            // Brief: table header + rows
            let header = "Backend name                      Admin    Health    Requests\n";
            let _ = vsb.write(&header);

            for director in directors.exact.values() {
                director.list(ctx, vsb, false, false);
            }
            for (_, director) in &directors.wildcards {
                director.list(ctx, vsb, false, false);
            }
        }
    }
}

/// Match hostname to vhost director
///
/// Returns the vhost director for the matched hostname.
/// Matching priority: exact hostname > wildcard hostname
fn match_hostname<'a>(
    directors: &'a VhostDirectorMap,
    host: &str,
) -> Option<&'a Arc<VhostDirector>> {
    let host = host.to_lowercase();

    // 1. Try exact hostname match
    if let Some(director) = directors.exact.get(&host) {
        return Some(director);
    }

    // 2. Try wildcard hostname match
    for (pattern, director) in &directors.wildcards {
        if matches_wildcard(pattern, &host) {
            return Some(director);
        }
    }

    None
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

}
