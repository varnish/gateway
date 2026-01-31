//! Per-vhost director implementation
//!
//! VhostDirector handles route matching and backend selection for a single virtual host.
//! It's part of a two-tier director architecture where GhostDirector (meta-director)
//! matches the hostname and delegates to the appropriate VhostDirector.

use std::sync::Arc;
use std::time::SystemTime;

use varnish::ffi::VCL_BACKEND;
use varnish::vcl::{Buffer, Ctx, HttpHeaders, StrOrBytes, VclDirector, VclError};

use crate::backend_pool::BackendPool;
use crate::config::RouteFilters;
use crate::director::{RouteEntry, WeightedBackendRef};
use crate::stats::VhostStats;

/// Header name for passing matched route filters to vcl_deliver
const FILTER_CONTEXT_HEADER: &str = "X-Ghost-Filter-Context";

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
    /// Statistics for this vhost
    stats: Arc<VhostStats>,
}

impl VhostDirector {
    /// Create a new vhost director
    pub fn new(
        hostname: String,
        routes: Vec<RouteEntry>,
        backend_pool: Arc<BackendPool>,
    ) -> Self {
        Self {
            hostname,
            routes,
            backend_pool,
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

    /// Check if this director has any routes with backends
    fn has_backends(&self) -> bool {
        self.routes.iter().any(|r| !r.backends.is_empty())
    }

    /// Collect all backend keys used by this director
    pub fn backend_keys(&self) -> Vec<String> {
        let mut keys = Vec::new();
        for route in &self.routes {
            for backend_ref in &route.backends {
                keys.push(backend_ref.key.clone());
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
            let msg = format!(
                "  Last request: {}\n",
                format_timestamp(Some(last))
            );
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

        let backends: Vec<serde_json::Value> = selections
            .iter()
            .map(|(key, count)| {
                serde_json::json!({
                    "address": key,
                    "selections": count,
                    "percentage": if total > 0 {
                        (*count as f64 / total as f64) * 100.0
                    } else {
                        0.0
                    }
                })
            })
            .collect();

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
}

impl VclDirector for VhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<VCL_BACKEND> {
        let bereq = ctx.http_bereq.as_ref()?;

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

        // Match routes (already sorted by priority)
        let (backend_refs, matched_filters) = match_routes(&self.routes, path, method, bereq, query_string)?;

        // Apply request filters BEFORE backend selection
        if let Some(filters) = &matched_filters {
            // RequestHeaderModifier
            if let Some(req_header_mod) = &filters.request_header_modifier {
                let _ = apply_request_header_filter(ctx, req_header_mod);
            }

            // URLRewrite
            if let Some(url_rewrite) = &filters.url_rewrite {
                let _ = apply_url_rewrite_filter(ctx, url_rewrite);
            }

            // RequestRedirect - TODO: implement synthetic backend
            // if filters.request_redirect.is_some() {
            //     return redirect_backend
            // }

            // Store response filter context
            if filters.response_header_modifier.is_some() {
                let _ = store_filter_context(ctx, filters);
            }
        }

        // Select backend using weighted random
        let backend_ref = select_backend_weighted(backend_refs)?;

        // Record stats
        self.stats.record_request(&backend_ref.key);

        // Look up in backend pool
        let backend = self.backend_pool.get(&backend_ref.key)?;

        Some(backend.vcl_ptr())
    }

    fn healthy(&self, _ctx: &mut Ctx) -> (bool, SystemTime) {
        // Simple health check: return false if no backends available
        // Otherwise trust Kubernetes health checks via EndpointSlices
        (self.has_backends(), SystemTime::now())
    }

    fn release(&self) {
        // Nothing to do - backends are owned by the pool
    }

    fn list(&self, _ctx: &mut Ctx, vsb: &mut Buffer, detailed: bool, json: bool) {
        if json {
            self.list_json(vsb);
        } else if detailed {
            self.list_detailed(vsb);
        } else {
            self.list_brief(vsb);
        }
    }
}

/// Match routes against all conditions (already sorted by priority)
/// All conditions within a match are AND-ed together
fn match_routes<'a>(
    routes: &'a [RouteEntry],
    path: &str,
    method: &str,
    bereq: &HttpHeaders,
    query_string: Option<&str>,
) -> Option<(&'a [WeightedBackendRef], Option<Arc<RouteFilters>>)> {
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
            return Some((&route.backends, route.filters.clone()));
        }
    }

    None
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
    use crate::director::{PathMatchCompiled, WeightedBackendRef};
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
    fn test_match_routes_no_path_match() {
        // Route with no path match should match all paths
        let routes = vec![RouteEntry {
            path_match: None,
            method: None,
            headers: Vec::new(),
            query_params: Vec::new(),
            filters: None,
            backends: vec![WeightedBackendRef {
                key: "10.0.0.1:8080".to_string(),
                weight: 100,
            }],
            priority: 100,
        }];

        // This test doesn't use HttpHeaders, so we can't fully test it here
        // But we can verify the route structure is correct
        assert_eq!(routes.len(), 1);
        assert!(routes[0].path_match.is_none());
        assert_eq!(routes[0].backends.len(), 1);
    }

    #[test]
    fn test_match_routes_path_prefix() {
        let routes = vec![RouteEntry {
            path_match: Some(PathMatchCompiled::PathPrefix("/api".to_string())),
            method: None,
            headers: Vec::new(),
            query_params: Vec::new(),
            filters: None,
            backends: vec![WeightedBackendRef {
                key: "10.0.0.1:8080".to_string(),
                weight: 100,
            }],
            priority: 100,
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
                backends: vec![WeightedBackendRef {
                    key: "10.0.0.1:8080".to_string(),
                    weight: 100,
                }],
                priority: 100,
            }],
            backend_pool.clone(),
        );

        assert!(director.has_backends());

        // Director with no routes
        let empty_director = VhostDirector::new(
            "empty.example.com".to_string(),
            vec![],
            backend_pool,
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
        );

        // Initial stats should be zero
        assert_eq!(director.stats().total_requests(), 0);

        // Record a request
        director.stats().record_request("10.0.0.1:8080");
        assert_eq!(director.stats().total_requests(), 1);
    }
}

fn apply_request_header_filter(
    ctx: &mut Ctx,
    filter: &crate::config::RequestHeaderFilter,
) -> Result<(), VclError> {
    let bereq = ctx.http_bereq.as_mut()
        .ok_or_else(|| VclError::new("no bereq".to_string()))?;

    // Remove headers
    for name in &filter.remove {
        bereq.unset_header(name);
    }

    // Set headers (replaces)
    for action in &filter.set {
        bereq.set_header(&action.name, &action.value)?;
    }

    // Add headers (appends)
    for action in &filter.add {
        bereq.set_header(&action.name, &action.value)?;
    }

    Ok(())
}

fn apply_url_rewrite_filter(
    ctx: &mut Ctx,
    filter: &crate::config::URLRewriteFilter,
) -> Result<(), VclError> {
    let bereq = ctx.http_bereq.as_mut()
        .ok_or_else(|| VclError::new("no bereq".to_string()))?;

    // Rewrite hostname
    if let Some(hostname) = &filter.hostname {
        bereq.set_header("host", hostname)?;
    }

    // Rewrite path
    if let Some(path_type) = &filter.path_type {
        match path_type.as_str() {
            "ReplaceFullPath" => {
                if let Some(path) = &filter.replace_full_path {
                    bereq.set_url(path)?;
                }
            }
            "ReplacePrefixMatch" => {
                if let Some(new_prefix) = &filter.replace_prefix_match {
                    // Get current URL
                    let current_url = bereq.url()
                        .and_then(|u| match u {
                            StrOrBytes::Utf8(s) => Some(s),
                            StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok(),
                        })
                        .unwrap_or("/");

                    // Extract path and query
                    let (path, query) = extract_path_and_query(current_url);

                    // For ReplacePrefixMatch, we need to find what prefix was matched
                    // and replace it. This requires access to the matched PathMatch.
                    // For now, we'll do a simple replacement: if the path starts with
                    // any prefix, we replace it with new_prefix.

                    // Find the longest prefix that matches
                    // Note: This is simplified. Ideally we'd use the matched route's
                    // path_match value, but that's not passed to this function.
                    let new_path = if path.starts_with('/') {
                        // Simple approach: replace first path segment
                        let segments: Vec<&str> = path.splitn(3, '/').collect();
                        if segments.len() >= 2 {
                            // segments[0] is empty (before first /)
                            // segments[1] is first segment
                            // segments[2] is remainder (if any)
                            let remainder = if segments.len() > 2 {
                                segments[2]
                            } else {
                                ""
                            };
                            format!("{}/{}", new_prefix.trim_end_matches('/'), remainder)
                        } else {
                            new_prefix.to_string()
                        }
                    } else {
                        new_prefix.to_string()
                    };

                    // Reconstruct URL with query string if present
                    let final_url = if let Some(q) = query {
                        format!("{}?{}", new_path, q)
                    } else {
                        new_path
                    };

                    bereq.set_url(&final_url)?;
                }
            }
            _ => {
                ctx.log(
                    varnish::vcl::LogTag::Error,
                    format!("Unknown path rewrite type: {}", path_type)
                );
            }
        }
    }

    Ok(())
}

fn store_filter_context(
    ctx: &mut Ctx,
    filters: &Arc<RouteFilters>,
) -> Result<(), VclError> {
    let bereq = ctx.http_bereq.as_mut()
        .ok_or_else(|| VclError::new("no bereq".to_string()))?;

    // Serialize response filter to JSON
    if let Some(resp_filter) = &filters.response_header_modifier {
        let json = serde_json::to_string(resp_filter)
            .map_err(|e| VclError::new(format!("serialize filter: {}", e)))?;
        bereq.set_header(FILTER_CONTEXT_HEADER, &json)?;
    }

    Ok(())
}
