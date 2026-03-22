//! Per-vhost director implementation
//!
//! VhostDirector handles route matching and backend selection for a single virtual host.
//! It's part of a two-tier director architecture where GhostDirector (meta-director)
//! matches the hostname and delegates to the appropriate VhostDirector.

use std::sync::Arc;
use std::time::SystemTime;

use varnish::vcl::{
    BackendRef, Buffer, Ctx, HttpHeaders, LogTag, ProbeResult, StrOrBytes, VclDirector, VclError,
};

use crate::backend_pool::BackendPool;
use crate::config::RouteFilters;
use crate::director::{BypassHeaderCompiled, PathMatchCompiled, RouteEntry, WeightedBackendGroup};
use crate::redirect_backend::RedirectConfig;
use crate::stats::VhostStats;

/// Wrapper for BackendRef that implements Send + Sync
///
/// SAFETY: BackendRef wraps a VCL_BACKEND opaque Varnish handle designed for
/// multi-threaded use. While individual Varnish workers are single-threaded,
/// we use Arc and atomic operations because multiple workers may access the
/// same director concurrently (via shared VCL state). The raw pointer is
/// managed by Varnish's backend infrastructure which provides its own
/// synchronization guarantees.
#[derive(Debug)]
struct SendSyncBackendRef(BackendRef);

unsafe impl Send for SendSyncBackendRef {}
unsafe impl Sync for SendSyncBackendRef {}

/// Header name for passing matched route filters to vcl_deliver
const FILTER_CONTEXT_HEADER: &str = "X-Ghost-Filter-Context";

/// Result of route matching containing backend groups, filters, and match context
#[derive(Debug)]
pub struct RouteMatchResult<'a> {
    pub backend_groups: &'a [WeightedBackendGroup],
    pub filters: Option<Arc<RouteFilters>>,
    pub matched_path: Option<&'a PathMatchCompiled>,
    pub route_name: Option<&'a str>,
    pub cache_policy: Option<&'a crate::config::CachePolicy>,
    pub bypass_headers: &'a [crate::director::BypassHeaderCompiled],
}

/// Result returned by route_request to the caller (recv/resolve).
/// Contains the resolved backend plus directives that must be applied
/// via the Varnish C API (not headers).
pub struct RouteRequestResult {
    pub backend: Option<BackendRef>,
    pub route_name: Option<String>,
    pub log_msgs: Vec<(LogTag, String)>,
    /// Whether to bypass the cache entirely (return(pass) in VCL terms).
    pub pass: bool,
    // TODO: re-add hash_ignore_busy when varnish-rs exposes set_hash_ignore_busy().
}

/// Director for a single virtual host
///
/// Handles route matching (path, method, headers, query params) and backend selection
/// for all routes belonging to a single hostname.
#[derive(Debug)]
pub struct VhostDirector {
    /// Hostname this director handles (for debugging/observability)
    hostname: String,
    /// Routes for this vhost (already sorted by priority)
    routes: Vec<RouteEntry>,
    /// Shared backend pool (shared with GhostDirector)
    backend_pool: Arc<BackendPool>,
    /// Synthetic redirect backend for RequestRedirect filters
    redirect_backend: Option<SendSyncBackendRef>,
    /// Synthetic 500 backend for matched routes with no backends
    internal_error_backend: Option<SendSyncBackendRef>,
    /// Statistics for this vhost
    stats: Arc<VhostStats>,
}

impl VhostDirector {
    /// Create a new vhost director
    pub fn new(
        hostname: String,
        routes: Vec<RouteEntry>,
        backend_pool: Arc<BackendPool>,
        redirect_backend: Option<BackendRef>,
        internal_error_backend: Option<BackendRef>,
    ) -> Self {
        Self {
            hostname,
            routes,
            backend_pool,
            redirect_backend: redirect_backend.map(SendSyncBackendRef),
            internal_error_backend: internal_error_backend.map(SendSyncBackendRef),
            stats: Arc::new(VhostStats::new()),
        }
    }

    /// Get hostname for this director
    #[allow(dead_code)]
    pub fn hostname(&self) -> &str {
        &self.hostname
    }

    /// Get stats for this director
    #[allow(dead_code)]
    pub fn stats(&self) -> &Arc<VhostStats> {
        &self.stats
    }

    /// Check if this director has any routes with backends or filters
    /// Routes with redirect filters but no backends are considered "healthy"
    fn has_backends(&self) -> bool {
        self.routes.iter().any(|r| {
            r.backend_groups
                .iter()
                .any(|g| !g.backends.is_empty())
                || r.filters.is_some()
        })
    }

    /// Collect all backend keys used by this director
    pub fn backend_keys(&self) -> Vec<String> {
        let mut keys = Vec::new();
        for route in &self.routes {
            for group in &route.backend_groups {
                for key in &group.backends {
                    keys.push(key.clone());
                }
            }
        }
        keys
    }

    /// Brief output format for backend.list (single line per vhost)
    fn list_brief(&self, vsb: &mut Buffer) {
        let health = if self.has_backends() {
            "healthy"
        } else {
            "sick"
        };
        let msg = format!(
            "ghost.{:<30} auto     {:<8} {}\n",
            self.hostname,
            health,
            self.stats.total_requests()
        );
        let _ = vsb.write(&msg);
    }

    /// Detailed output format for backend.list -p (multi-line per vhost)
    fn list_detailed(&self, vsb: &mut Buffer) {
        use crate::format::format_timestamp;

        let msg = format!("Backend: ghost.{}\n", self.hostname);
        let _ = vsb.write(&msg);
        let admin = "  Admin: auto\n";
        let _ = vsb.write(&admin);

        let health = if self.has_backends() {
            "healthy"
        } else {
            "sick"
        };
        let msg = format!("  Health: {}\n", health);
        let _ = vsb.write(&msg);

        let msg = format!("  Routes: {}\n", self.routes.len());
        let _ = vsb.write(&msg);

        let total = self.stats.total_requests();
        let msg = format!("  Total requests: {}\n", total);
        let _ = vsb.write(&msg);

        if let Some(last) = self.stats.last_request() {
            let msg = format!("  Last request: {}\n", format_timestamp(Some(last)));
            let _ = vsb.write(&msg);
        }

        // Backend selection breakdown
        let selections = self.stats.backend_selections();
        if !selections.is_empty() {
            let backends_hdr = "  Backends:\n";
            let _ = vsb.write(&backends_hdr);
            for (key, count) in selections.iter() {
                let pct = if total > 0 {
                    format!("{:.1}%", (*count as f64 / total as f64) * 100.0)
                } else {
                    "0.0%".to_string()
                };
                let msg = format!("    {} - {} selections ({})\n", key, count, pct);
                let _ = vsb.write(&msg);
            }
        }

        let newline = "\n";
        let _ = vsb.write(&newline);
    }

    /// JSON output format for backend.list -j
    fn list_json(&self, vsb: &mut Buffer) {
        use crate::format::format_timestamp;

        let selections = self.stats.backend_selections();
        let total = self.stats.total_requests();

        let backends = crate::format::format_backend_selections_json(&selections, total);

        let obj = serde_json::json!({
            "name": format!("ghost.{}", self.hostname),
            "type": "vhost_director",
            "admin": "auto",
            "health": if self.has_backends() { "healthy" } else { "sick" },
            "routes": self.routes.len(),
            "total_requests": total,
            "last_request": self.stats.last_request().map(|t| format_timestamp(Some(t))),
            "backends": backends
        });

        let json_str = serde_json::to_string(&obj).unwrap_or_else(|_| "{}".to_string());
        let _ = vsb.write(&json_str);
    }

    /// Route a request using the given HTTP headers.
    ///
    /// This is the core routing logic extracted from resolve() so it can work
    /// with either req (vcl_recv) or bereq (vcl_backend_fetch) headers.
    /// The listener parameter comes from ctx.local_socket() and filters routes
    /// by which Varnish listener received the request.
    /// Log messages are collected and returned so the caller can emit them
    /// (avoids borrow conflicts between HttpHeaders and Ctx).
    pub fn route_request(
        &self,
        http: &mut HttpHeaders,
        listener: Option<&str>,
    ) -> RouteRequestResult {
        let mut log_msgs: Vec<(LogTag, String)> = Vec::new();

        // Extract request components (owned strings to avoid borrow conflicts)
        let (path_owned, query_string_owned, method_owned) = {
            let url = http
                .url()
                .and_then(|u| match u {
                    StrOrBytes::Utf8(s) => Some(s),
                    StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                })
                .unwrap_or("/");

            let (path, query_string) = extract_path_and_query(url);

            let method = http
                .method()
                .and_then(|m| match m {
                    StrOrBytes::Utf8(s) => Some(s),
                    StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                })
                .unwrap_or("GET");

            (
                path.to_string(),
                query_string.map(|s| s.to_string()),
                method.to_string(),
            )
        };

        // Match routes (already sorted by priority)
        let match_result = match match_routes(
            &self.routes,
            &path_owned,
            &method_owned,
            http,
            query_string_owned.as_deref(),
            listener,
        ) {
            Some(r) => r,
            None => return RouteRequestResult {
                backend: None,
                route_name: None,
                log_msgs,
                pass: true,
            },
        };
        let backend_groups = match_result.backend_groups;
        let matched_filters = match_result.filters.as_ref();
        let route_name = match_result.route_name.map(|s| s.to_string());

        // Apply request filters BEFORE backend selection
        if let Some(filters) = matched_filters {
            // RequestRedirect - takes precedence over all other filters
            if let Some(redirect_filter) = &filters.request_redirect {
                log_msgs.push((LogTag::Debug, "Applying request redirect filter".to_string()));

                // Extract original request components
                let (original_scheme, original_hostname, original_port) = {
                    let host_header = http
                        .header("Host")
                        .and_then(|h| match h {
                            StrOrBytes::Utf8(s) => Some(s),
                            StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                        })
                        .unwrap_or("localhost");
                    let (hostname, port_opt) = parse_host_and_port(host_header);

                    // Determine scheme from listener name (authoritative)
                    // Listeners are named "http-{port}" or "https-{port}"
                    let scheme = if listener.is_some_and(|l| l.starts_with("https")) {
                        "https"
                    } else {
                        "http"
                    };

                    let port =
                        port_opt.unwrap_or_else(|| if scheme == "https" { 443 } else { 80 });

                    (scheme.to_string(), hostname.to_string(), port)
                };

                // Extract matched prefix string (for ReplacePrefixMatch logic)
                let matched_path_str = match_result.matched_path.and_then(|pm| match pm {
                    PathMatchCompiled::PathPrefix(prefix) => Some(prefix.clone()),
                    PathMatchCompiled::Exact(path) => Some(path.clone()),
                    PathMatchCompiled::Regex(_) => None,
                });

                let redirect_config = RedirectConfig {
                    filter: redirect_filter.clone(),
                    original_scheme,
                    original_hostname,
                    original_port,
                    original_path: path_owned.clone(),
                    original_query: query_string_owned.clone().unwrap_or_default(),
                    matched_path: matched_path_str,
                };

                let config_json = match serde_json::to_string(&redirect_config) {
                    Ok(json) => json,
                    Err(e) => {
                        log_msgs.push((
                            LogTag::Error,
                            format!("Failed to serialize redirect config: {}", e),
                        ));
                        return RouteRequestResult {
                            backend: None,
                            route_name: route_name.clone(),
                            log_msgs,
                            pass: true,
                        };
                    }
                };

                if let Err(e) = http.set_header("X-Ghost-Redirect-Config", &config_json) {
                    log_msgs.push((
                        LogTag::Error,
                        format!("Failed to set redirect config header: {}", e),
                    ));
                    return RouteRequestResult {
                        backend: None,
                        route_name: route_name.clone(),
                        log_msgs,
                        pass: true,
                    };
                }

                return RouteRequestResult {
                    backend: self.redirect_backend.as_ref().map(|r| r.0.clone()),
                    route_name: route_name.clone(),
                    log_msgs,
                    pass: true,
                };
            }

            // Apply other filters only if NOT redirecting
            if let Some(req_header_mod) = &filters.request_header_modifier {
                let _ = apply_request_header_filter(http, req_header_mod);
            }

            if let Some(url_rewrite) = &filters.url_rewrite {
                log_msgs.push((LogTag::Debug, "Applying URL rewrite filter".to_string()));
                match apply_url_rewrite_filter(http, url_rewrite, match_result.matched_path) {
                    Ok(msgs) => log_msgs.extend(msgs),
                    Err(e) => {
                        log_msgs.push((LogTag::Error, format!("URL rewrite failed: {}", e)));
                    }
                }
            }

            if filters.response_header_modifier.is_some() {
                let _ = store_filter_context(http, filters);
            }
        }

        // Determine cache behavior from policy
        let pass = apply_cache_policy_headers(http, &match_result, &query_string_owned);

        // Select backend using two-level weighted random:
        // Level 1: pick a group by weight
        // Level 2: pick a random pod within the selected group
        let backend_key = match select_backend_from_groups(backend_groups) {
            Some(key) => key,
            None => {
                return RouteRequestResult {
                    backend: self.internal_error_backend.as_ref().map(|r| r.0.clone()),
                    route_name,
                    log_msgs,
                    pass,
                };
            }
        };

        // Record stats
        self.stats.record_request(backend_key);

        // Look up in backend pool
        let backend = match self.backend_pool.get(backend_key) {
            Some(b) => b,
            None => return RouteRequestResult {
                backend: None,
                route_name,
                log_msgs,
                pass,
            },
        };

        RouteRequestResult {
            backend: Some(AsRef::<BackendRef>::as_ref(&*backend).clone()),
            route_name,
            log_msgs,
            pass,
        }
    }
}

impl VclDirector for VhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<BackendRef> {
        let bereq = ctx.http_bereq.as_mut()?;
        let result = self.route_request(bereq, None);
        for (tag, msg) in result.log_msgs {
            ctx.log(tag, &msg);
        }
        result.backend
    }

    fn probe(&self, _ctx: &mut Ctx) -> ProbeResult {
        // Simple health check: return false if no backends available
        // Otherwise trust Kubernetes health checks via EndpointSlices
        ProbeResult {
            healthy: self.has_backends(),
            last_changed: SystemTime::now(),
        }
    }

    fn report(&self, _ctx: &mut Ctx, vsb: &mut Buffer) {
        self.list_brief(vsb);
    }

    fn report_details(&self, _ctx: &mut Ctx, vsb: &mut Buffer) {
        self.list_detailed(vsb);
    }

    fn report_json(&self, _ctx: &mut Ctx, vsb: &mut Buffer) {
        self.list_json(vsb);
    }

    fn report_details_json(&self, _ctx: &mut Ctx, vsb: &mut Buffer) {
        self.list_json(vsb);
    }
}

/// Match routes against all conditions (already sorted by priority)
/// All conditions within a match are AND-ed together.
/// The listener parameter filters routes by which Varnish listener received the request.
fn match_routes<'a>(
    routes: &'a [RouteEntry],
    path: &str,
    method: &str,
    http: &HttpHeaders,
    query_string: Option<&str>,
    listener: Option<&str>,
) -> Option<RouteMatchResult<'a>> {
    for route in routes {
        // Listener filter (empty = match all)
        if !route.listeners.is_empty() {
            match listener {
                Some(l) if route.listeners.iter().any(|rl| rl == l) => {}
                _ => continue,
            }
        }

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
        if !route.headers.iter().all(|hm| hm.matches(http)) {
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

        // All conditions matched - return the route even with empty backend groups.
        // The caller will return 500 for matched routes with no backends.
        return Some(RouteMatchResult {
            backend_groups: &route.backend_groups,
            filters: route.filters.clone(),
            matched_path: route.path_match.as_ref(),
            route_name: route.route_name.as_deref(),
            cache_policy: route.cache_policy.as_ref(),
            bypass_headers: &route.bypass_headers,
        });
    }

    None
}

/// Apply cache policy to the request. Returns whether to pass (bypass cache).
///
/// Sets bereq-bridging headers for values that need to reach vcl_backend_response:
/// - `X-Ghost-Default-TTL: <N>s` → set beresp.ttl when origin has no Cache-Control
/// - `X-Ghost-Forced-TTL: <N>s` → override beresp.ttl unconditionally
/// - `X-Ghost-Grace: <N>s` → set beresp.grace
/// - `X-Ghost-Keep: <N>s` → set beresp.keep
/// - `X-Ghost-Cache-Key-Extra: <data>` → additional hash_data() input
fn apply_cache_policy_headers(
    http: &mut HttpHeaders,
    match_result: &RouteMatchResult,
    query_string: &Option<String>,
) -> bool {
    let cache_policy = match match_result.cache_policy {
        Some(cp) => cp,
        None => {
            // No cache policy → pass-through mode (no caching)
            return true;
        }
    };

    // Check bypass rules using pre-compiled regexes from config load
    for bypass in match_result.bypass_headers {
        match bypass {
            BypassHeaderCompiled::Present { name } => {
                if http.header(name).is_some() {
                    return true;
                }
            }
            BypassHeaderCompiled::Regex { name, regex } => {
                if let Some(val) = http.header(name) {
                    let val_str = match val {
                        StrOrBytes::Utf8(s) => s,
                        StrOrBytes::Bytes(b) => match std::str::from_utf8(b) {
                            Ok(s) => s,
                            Err(_) => continue,
                        },
                    };
                    if regex.is_match(val_str) {
                        return true;
                    }
                }
            }
        }
    }

    // Set TTL headers (bridge to vcl_backend_response via bereq)
    if let Some(ttl) = cache_policy.default_ttl_seconds {
        let _ = http.set_header("X-Ghost-Default-TTL", &format!("{}s", ttl));
    }
    if let Some(ttl) = cache_policy.forced_ttl_seconds {
        let _ = http.set_header("X-Ghost-Forced-TTL", &format!("{}s", ttl));
    }

    // Set grace and keep (bridge to vcl_backend_response via bereq)
    if cache_policy.grace_seconds > 0 {
        let _ = http.set_header("X-Ghost-Grace", &format!("{}s", cache_policy.grace_seconds));
    }
    if cache_policy.keep_seconds > 0 {
        let _ = http.set_header("X-Ghost-Keep", &format!("{}s", cache_policy.keep_seconds));
    }

    // Cache key customization
    if let Some(cache_key) = &cache_policy.cache_key {
        // Build extra hash data from cache key headers
        let mut extra_parts: Vec<String> = Vec::new();

        for header_name in &cache_key.headers {
            if let Some(val) = http.header(header_name) {
                let val_str = match val {
                    StrOrBytes::Utf8(s) => s.to_string(),
                    StrOrBytes::Bytes(b) => String::from_utf8_lossy(b).to_string(),
                };
                extra_parts.push(format!("{}:{}", header_name, val_str));
            }
        }

        // Query parameter filtering: rewrite URL to include/exclude params
        if !cache_key.query_params_include.is_empty() || !cache_key.query_params_exclude.is_empty()
        {
            if let Some(qs) = query_string {
                let filtered = filter_query_params(
                    qs,
                    &cache_key.query_params_include,
                    &cache_key.query_params_exclude,
                );
                // Add filtered query string to hash
                extra_parts.push(format!("qs:{}", filtered));
            }
        }

        if !extra_parts.is_empty() {
            let _ = http.set_header("X-Ghost-Cache-Key-Extra", &extra_parts.join("|"));
        }
    }

    // Cache policy present → do not pass (enable caching)
    // TODO: hash_ignore_busy = !request_coalescing when varnish-rs exposes the API
    false
}

/// Filter query parameters based on include/exclude lists.
/// Returns the filtered query string.
fn filter_query_params(query_string: &str, include: &[String], exclude: &[String]) -> String {
    let params: Vec<(&str, &str)> = query_string
        .split('&')
        .filter_map(|part| {
            let mut split = part.splitn(2, '=');
            let key = split.next()?;
            let val = split.next().unwrap_or("");
            Some((key, val))
        })
        .collect();

    let filtered: Vec<String> = if !include.is_empty() {
        // Allowlist mode: only keep params in include list
        params
            .iter()
            .filter(|(k, _)| include.iter().any(|i| i == k))
            .map(|(k, v)| {
                if v.is_empty() {
                    k.to_string()
                } else {
                    format!("{}={}", k, v)
                }
            })
            .collect()
    } else if !exclude.is_empty() {
        // Denylist mode: remove params in exclude list
        params
            .iter()
            .filter(|(k, _)| !exclude.iter().any(|e| e == k))
            .map(|(k, v)| {
                if v.is_empty() {
                    k.to_string()
                } else {
                    format!("{}={}", k, v)
                }
            })
            .collect()
    } else {
        return query_string.to_string();
    };

    filtered.join("&")
}

/// Select a backend using two-level weighted random selection:
/// Level 1: pick a group by weight (skip weight-0 groups)
/// Level 2: uniform random within selected group
fn select_backend_from_groups(groups: &[WeightedBackendGroup]) -> Option<&str> {
    if groups.is_empty() {
        return None;
    }

    use rand::Rng;
    let mut rng = rand::thread_rng();

    // Level 1: pick a group by weight
    let total_weight: u32 = groups.iter().map(|g| g.weight).sum();

    if total_weight == 0 {
        return None;
    }

    let r = rng.gen_range(0..total_weight);
    let mut cumulative = 0u32;
    let mut selected_group = &groups[0];
    for group in groups {
        cumulative += group.weight;
        if r < cumulative {
            selected_group = group;
            break;
        }
    }

    if selected_group.backends.is_empty() {
        return None;
    }

    // Level 2: uniform random within selected group
    if selected_group.backends.len() == 1 {
        return Some(&selected_group.backends[0]);
    }

    let idx = rng.gen_range(0..selected_group.backends.len());
    Some(&selected_group.backends[idx])
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::director::{PathMatchCompiled, WeightedBackendGroup};
    use std::collections::HashMap;

    #[test]
    fn test_extract_path_and_query() {
        assert_eq!(extract_path_and_query("/api/users"), ("/api/users", None));
        assert_eq!(
            extract_path_and_query("/api/users?foo=bar"),
            ("/api/users", Some("foo=bar"))
        );
        assert_eq!(
            extract_path_and_query("/api/users#fragment"),
            ("/api/users", None)
        );
        assert_eq!(
            extract_path_and_query("/api/users?foo=bar#fragment"),
            ("/api/users", Some("foo=bar"))
        );
        assert_eq!(extract_path_and_query(""), ("/", None));
        assert_eq!(extract_path_and_query("?query"), ("/", Some("query")));
    }

    #[test]
    fn test_select_backend_from_groups_single() {
        let groups = vec![WeightedBackendGroup {
            weight: 100,
            backends: vec!["10.0.0.1:8080".to_string()],
        }];
        let selected = select_backend_from_groups(&groups).unwrap();
        assert_eq!(selected, "10.0.0.1:8080");
    }

    #[test]
    fn test_select_backend_from_groups_distribution() {
        // Two groups: 90% to group1 (2 pods), 10% to group2 (2 pods)
        let groups = vec![
            WeightedBackendGroup {
                weight: 90,
                backends: vec![
                    "10.0.0.1:8080".to_string(),
                    "10.0.0.2:8080".to_string(),
                ],
            },
            WeightedBackendGroup {
                weight: 10,
                backends: vec![
                    "10.0.0.3:8080".to_string(),
                    "10.0.0.4:8080".to_string(),
                ],
            },
        ];

        // Run many selections and check distribution
        let mut group1_count = 0;
        let mut group2_count = 0;
        for _ in 0..1000 {
            let selected = select_backend_from_groups(&groups).unwrap();
            if selected == "10.0.0.1:8080" || selected == "10.0.0.2:8080" {
                group1_count += 1;
            } else {
                group2_count += 1;
            }
        }

        // Allow for statistical variance (should be roughly 900:100)
        assert!(
            group1_count > 800,
            "group1 selected {} times, expected ~900",
            group1_count
        );
        assert!(
            group2_count < 200,
            "group2 selected {} times, expected ~100",
            group2_count
        );
    }

    #[test]
    fn test_select_backend_from_groups_empty() {
        let groups: Vec<WeightedBackendGroup> = vec![];
        assert!(select_backend_from_groups(&groups).is_none());
    }

    #[test]
    fn test_select_backend_from_groups_uniform_within_group() {
        // Single group with 2 pods — should distribute ~50/50
        let groups = vec![WeightedBackendGroup {
            weight: 100,
            backends: vec![
                "10.0.0.1:8080".to_string(),
                "10.0.0.2:8080".to_string(),
            ],
        }];

        let mut counts = HashMap::new();
        for _ in 0..1000 {
            let selected = select_backend_from_groups(&groups).unwrap();
            *counts.entry(selected.to_string()).or_insert(0) += 1;
        }

        let count_1 = *counts.get("10.0.0.1:8080").unwrap_or(&0);
        let count_2 = *counts.get("10.0.0.2:8080").unwrap_or(&0);

        // Each should get roughly 500 (allow wide margin)
        assert!(
            count_1 > 350 && count_1 < 650,
            "10.0.0.1 selected {} times, expected ~500",
            count_1
        );
        assert!(
            count_2 > 350 && count_2 < 650,
            "10.0.0.2 selected {} times, expected ~500",
            count_2
        );
    }

    #[test]
    fn test_match_routes_no_path_match() {
        // Route with no path match should match all paths
        let routes = vec![RouteEntry {
            path_match: None,
            method: None,
            headers: Vec::new(),
            query_params: Vec::new(),
            filters: None,
            backend_groups: vec![WeightedBackendGroup {
                weight: 100,
                backends: vec!["10.0.0.1:8080".to_string()],
            }],
            listeners: Vec::new(),
            route_name: None,
            priority: 100,
            rule_index: 0,
            cache_policy: None,
            bypass_headers: Vec::new(),
        }];

        // This test doesn't use HttpHeaders, so we can't fully test it here
        // But we can verify the route structure is correct
        assert_eq!(routes.len(), 1);
        assert!(routes[0].path_match.is_none());
        assert_eq!(routes[0].backend_groups.len(), 1);
    }

    #[test]
    fn test_match_routes_path_prefix() {
        let routes = vec![RouteEntry {
            path_match: Some(PathMatchCompiled::PathPrefix("/api".to_string())),
            method: None,
            headers: Vec::new(),
            query_params: Vec::new(),
            filters: None,
            backend_groups: vec![WeightedBackendGroup {
                weight: 100,
                backends: vec!["10.0.0.1:8080".to_string()],
            }],
            listeners: Vec::new(),
            route_name: None,
            priority: 100,
            rule_index: 0,
            cache_policy: None,
            bypass_headers: Vec::new(),
        }];

        // Verify route structure
        assert_eq!(routes.len(), 1);
        assert!(routes[0].path_match.is_some());
    }

    #[test]
    fn test_vhost_director_has_backends() {
        let backend_pool = Arc::new(BackendPool::new());

        // Director with routes that have backends
        let director = VhostDirector::new(
            "api.example.com".to_string(),
            vec![RouteEntry {
                path_match: None,
                method: None,
                headers: Vec::new(),
                query_params: Vec::new(),
                filters: None,
                backend_groups: vec![WeightedBackendGroup {
                    weight: 100,
                    backends: vec!["10.0.0.1:8080".to_string()],
                }],
                listeners: Vec::new(),
                route_name: None,
                priority: 100,
                rule_index: 0,
                cache_policy: None,
                bypass_headers: Vec::new(),
            }],
            backend_pool.clone(),
            None,
            None,
        );

        assert!(director.has_backends());

        // Director with no routes
        let empty_director = VhostDirector::new(
            "empty.example.com".to_string(),
            vec![],
            backend_pool,
            None,
            None,
        );

        assert!(!empty_director.has_backends());
    }

    #[test]
    fn test_vhost_director_stats() {
        let backend_pool = Arc::new(BackendPool::new());
        let director = VhostDirector::new(
            "api.example.com".to_string(),
            vec![],
            backend_pool,
            None,
            None,
        );

        // Initial stats should be zero
        assert_eq!(director.stats().total_requests(), 0);

        // Record a request
        director.stats().record_request("10.0.0.1:8080");
        assert_eq!(director.stats().total_requests(), 1);
    }

    #[test]
    fn test_replace_first_segment_heuristic() {
        // Basic case: replace first segment
        assert_eq!(
            replace_first_segment_heuristic("/v1/users", "/v2"),
            "/v2/users"
        );

        // Multiple segments (replaces first segment, preserves remainder)
        assert_eq!(
            replace_first_segment_heuristic("/api/v1/users/123", "/api/v2"),
            "/api/v2/v1/users/123"
        );

        // Root path
        assert_eq!(replace_first_segment_heuristic("/", "/v2"), "/v2");

        // Single segment (no remainder) - should not add trailing slash
        assert_eq!(replace_first_segment_heuristic("/v1", "/v2"), "/v2");

        // Trailing slash in new prefix is trimmed
        assert_eq!(
            replace_first_segment_heuristic("/v1/users", "/v2/"),
            "/v2/users"
        );
    }

    #[test]
    fn test_route_match_result_struct() {
        use crate::config::RouteFilters;

        let groups = vec![WeightedBackendGroup {
            weight: 100,
            backends: vec!["10.0.0.1:8080".to_string()],
        }];

        let path_match = PathMatchCompiled::PathPrefix("/api/v1".to_string());
        let filters = Arc::new(RouteFilters {
            request_header_modifier: None,
            response_header_modifier: None,
            request_redirect: None,
            url_rewrite: None,
        });

        let result = RouteMatchResult {
            backend_groups: &groups,
            filters: Some(filters.clone()),
            matched_path: Some(&path_match),
            route_name: Some("default/my-route"),
            cache_policy: None,
            bypass_headers: &[],
        };

        assert_eq!(result.backend_groups.len(), 1);
        assert!(result.filters.is_some());
        assert!(result.matched_path.is_some());
    }
}

fn apply_request_header_filter(
    http: &mut HttpHeaders,
    filter: &crate::config::RequestHeaderFilter,
) -> Result<(), VclError> {
    // Remove headers
    for name in &filter.remove {
        http.unset_header(name);
    }

    // Set headers (replaces) — must unset first since set_header() appends
    for action in &filter.set {
        http.unset_header(&action.name);
        http.set_header(&action.name, &action.value)?;
    }

    // Add headers (appends to existing value per Gateway API spec)
    // Must unset+set to avoid duplicate header slots
    for action in &filter.add {
        if let Some(existing) = http.header(&action.name) {
            let existing_str = match existing {
                StrOrBytes::Utf8(s) => s.to_string(),
                StrOrBytes::Bytes(b) => String::from_utf8_lossy(b).to_string(),
            };
            let combined = format!("{},{}", existing_str, action.value);
            http.unset_header(&action.name);
            http.set_header(&action.name, &combined)?;
        } else {
            http.set_header(&action.name, &action.value)?;
        }
    }

    Ok(())
}

/// Apply URL rewrite filter to HTTP headers.
/// Returns a list of log messages to be emitted by the caller.
fn apply_url_rewrite_filter(
    http: &mut HttpHeaders,
    filter: &crate::config::URLRewriteFilter,
    matched_path: Option<&PathMatchCompiled>,
) -> Result<Vec<(LogTag, String)>, VclError> {
    let mut log_msgs = Vec::new();

    // Rewrite hostname — must unset first since set_header() appends
    if let Some(hostname) = &filter.hostname {
        http.unset_header("host");
        http.set_header("host", hostname)?;
    }

    // Rewrite path
    if let Some(path_type) = &filter.path_type {
        match path_type.as_str() {
            "ReplaceFullPath" => {
                if let Some(path) = &filter.replace_full_path {
                    let current_url = http
                        .url()
                        .and_then(|u| match u {
                            StrOrBytes::Utf8(s) => Some(s),
                            StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                        })
                        .unwrap_or("/");
                    let (_, query) = extract_path_and_query(current_url);
                    let final_url = if let Some(q) = query {
                        format!("{}?{}", path, q)
                    } else {
                        path.to_string()
                    };
                    http.set_url(&final_url)?;
                }
            }
            "ReplacePrefixMatch" => {
                if let Some(new_prefix) = &filter.replace_prefix_match {
                    let current_url = http
                        .url()
                        .and_then(|u| match u {
                            StrOrBytes::Utf8(s) => Some(s),
                            StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                        })
                        .unwrap_or("/");

                    let (path, query) = extract_path_and_query(current_url);

                    let (new_path, log_msg) =
                        apply_replace_prefix_match(path, new_prefix, matched_path);

                    let final_url = if let Some(q) = query {
                        format!("{}?{}", new_path, q)
                    } else {
                        new_path
                    };

                    http.set_url(&final_url)?;

                    if let Some(msg) = log_msg {
                        log_msgs.push(msg);
                    }
                }
            }
            _ => {
                log_msgs.push((
                    LogTag::Error,
                    format!("Unknown path rewrite type: {}", path_type),
                ));
            }
        }
    }

    Ok(log_msgs)
}

/// Apply ReplacePrefixMatch path rewrite logic
///
/// Computes the new path based on the matched path type and returns both the new path
/// and an optional log message for debugging.
///
/// # Arguments
///
/// * `path` - The current request path (without query string)
/// * `new_prefix` - The new prefix to use for replacement
/// * `matched_path` - The PathMatch that was used to select this route (if any)
///
/// # Returns
///
/// A tuple of `(new_path, optional_log_message)` where the log message contains
/// a tag and message string for VSL logging.
fn apply_replace_prefix_match(
    path: &str,
    new_prefix: &str,
    matched_path: Option<&PathMatchCompiled>,
) -> (String, Option<(LogTag, String)>) {
    match matched_path {
        Some(PathMatchCompiled::PathPrefix(matched_prefix)) => {
            // Replace the matched prefix with new_prefix
            if path.starts_with(matched_prefix) {
                let remainder = &path[matched_prefix.len()..];
                let trimmed_new = new_prefix.trim_end_matches('/');

                let result = if remainder.is_empty() {
                    let r = trimmed_new.to_string();
                    if r.is_empty() {
                        "/".to_string()
                    } else {
                        r
                    }
                } else if remainder.starts_with('/') {
                    format!("{}{}", trimmed_new, remainder)
                } else {
                    format!("{}/{}", trimmed_new, remainder)
                };
                let log = format!(
                    "ReplacePrefixMatch: {} + {} -> {}",
                    matched_prefix, remainder, result
                );
                (result, Some((LogTag::Debug, log)))
            } else {
                let msg = format!(
                    "ReplacePrefixMatch: path {} doesn't start with matched prefix {}",
                    path, matched_prefix
                );
                (path.to_string(), Some((LogTag::Error, msg)))
            }
        }
        Some(PathMatchCompiled::Exact(_)) => {
            // Exact match: treat as ReplaceFullPath
            let msg =
                "ReplacePrefixMatch with Exact match - treating as full replacement".to_string();
            (new_prefix.to_string(), Some((LogTag::Debug, msg)))
        }
        Some(PathMatchCompiled::Regex(_)) => {
            // Regex: cannot extract matched portion, use heuristic
            let msg = "ReplacePrefixMatch with RegularExpression not supported - using heuristic"
                .to_string();
            (
                replace_first_segment_heuristic(path, new_prefix),
                Some((LogTag::Error, msg)),
            )
        }
        None => {
            // No path match: use heuristic
            let msg = "ReplacePrefixMatch without path match - using heuristic".to_string();
            (
                replace_first_segment_heuristic(path, new_prefix),
                Some((LogTag::Debug, msg)),
            )
        }
    }
}

/// Heuristic for replacing the first path segment when matched prefix is unknown
///
/// This is a **fallback behavior** used when `ReplacePrefixMatch` is configured but:
/// - No `PathMatch` is specified (route matches on method/headers/query params only), OR
/// - `PathMatch` uses `RegularExpression` (we can't extract the matched portion)
///
/// **NOT Gateway API Spec Compliant**: The Gateway API spec requires knowing the exact
/// matched prefix to perform the replacement. This heuristic provides reasonable behavior
/// in edge cases where the spec is ambiguous or silent.
///
/// # Behavior
///
/// Replaces the first path segment (everything between the first and second `/`) with
/// `new_prefix`, preserving the rest of the path.
///
/// # Examples
///
/// ```text
/// replace_first_segment_heuristic("/v1/users/123", "/v2")
///   -> "/v2/users/123"
///
/// replace_first_segment_heuristic("/api", "/v2")
///   -> "/v2"
///
/// replace_first_segment_heuristic("/v1/products", "/api/v2")
///   -> "/api/v2/products"
/// ```
///
/// # Implementation
///
/// 1. Split path on `/` into at most 3 parts: `["", "v1", "users/123"]`
/// 2. Replace the second part (first segment) with `new_prefix` (trimmed of trailing `/`)
/// 3. Append the remainder (third part, if any)
pub(crate) fn replace_first_segment_heuristic(path: &str, new_prefix: &str) -> String {
    if path.starts_with('/') {
        let segments: Vec<&str> = path.splitn(3, '/').collect();
        if segments.len() >= 2 {
            let remainder = if segments.len() > 2 { segments[2] } else { "" };
            let trimmed_new = new_prefix.trim_end_matches('/');
            if remainder.is_empty() {
                trimmed_new.to_string()
            } else {
                format!("{}/{}", trimmed_new, remainder)
            }
        } else {
            new_prefix.to_string()
        }
    } else {
        new_prefix.to_string()
    }
}

/// Parse hostname and port from Host header
///
/// Handles both regular `host:port` format and IPv6 `[::1]:port` format.
/// Returns (hostname, Some(port)) or (hostname, None) if no port specified.
fn parse_host_and_port(host_header: &str) -> (&str, Option<u16>) {
    // Handle IPv6: [::1]:8080 or [::1]
    if host_header.starts_with('[') {
        if let Some(bracket_end) = host_header.find(']') {
            let host = &host_header[0..=bracket_end];
            let port_part = &host_header[bracket_end + 1..];
            if let Some(port_str) = port_part.strip_prefix(':') {
                let port = port_str.parse::<u16>().ok();
                return (host, port);
            }
            return (host, None);
        }
    }

    // Handle regular host:port or just host
    if let Some(colon_pos) = host_header.rfind(':') {
        let host = &host_header[..colon_pos];
        let port_str = &host_header[colon_pos + 1..];
        if let Ok(port) = port_str.parse::<u16>() {
            return (host, Some(port));
        }
    }

    (host_header, None)
}

fn store_filter_context(http: &mut HttpHeaders, filters: &Arc<RouteFilters>) -> Result<(), VclError> {
    // Serialize response filter to JSON
    if let Some(resp_filter) = &filters.response_header_modifier {
        let json = serde_json::to_string(resp_filter)
            .map_err(|e| VclError::new(format!("serialize filter: {}", e)))?;
        http.set_header(FILTER_CONTEXT_HEADER, &json)?;
    }

    Ok(())
}
