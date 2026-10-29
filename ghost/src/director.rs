//! Director implementation for Ghost VMOD
//!
//! The GhostDirector handles host-based routing and weighted backend selection.
//! It implements the VclDirector trait to integrate with Varnish's director system.

use std::borrow::Cow;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::SystemTime;

use arc_swap::ArcSwap;
use parking_lot::{Mutex, RwLock};
use regex::Regex;
use varnish::vcl::{
    Backend, BackendRef, Buffer, Ctx, HttpHeaders, LogTag, ProbeResult, StrOrBytes, VclDirector,
    VclError,
};

use crate::backend_pool::BackendPool;
use crate::config::{
    BackendGroup, CachePolicy, Config, HeaderMatch, MatchType, PathMatch, PathMatchType,
    QueryParamMatch, Route,
};
use crate::internal_error_backend::{InternalErrorBackend, InternalErrorBody};
use crate::not_found_backend::{NotFoundBackend, NotFoundBody};
use crate::redirect_backend::{RedirectBackend, RedirectBody};
use crate::sync_wrapper::SendSyncBackendRef;
use crate::vhost_director;
use crate::vhost_director::VhostDirector;

/// A group of backends sharing a weight for correct weighted traffic distribution.
/// Selection is two-level: (1) pick a group by weight, (2) pick a random pod within the group.
#[derive(Debug, Clone)]
pub struct WeightedBackendGroup {
    /// Weight for group-level selection
    pub weight: u32,
    /// Backend pool keys ("address:port") within this group
    pub backends: Vec<String>,
}

/// Compile a regex, returning a single-line error that names the pattern.
///
/// The `regex` crate renders syntax errors over several lines (pattern, caret,
/// description). Only the final description line is kept, since the pattern
/// is already quoted, so the message fits on one VSL / log line.
fn compile_regex(pattern: &str) -> Result<Regex, String> {
    Regex::new(pattern).map_err(|e| {
        let msg = e.to_string();
        let summary = msg
            .lines()
            .map(str::trim)
            .rfind(|l| !l.is_empty())
            .unwrap_or("")
            .to_string();
        format!("invalid regex '{}': {}", pattern, summary)
    })
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
                let re = compile_regex(&pm.value)?;
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

/// Compiled bypass header rule for efficient per-request matching.
/// Pre-compiled at config load time to avoid regex compilation on every request.
#[derive(Debug, Clone)]
pub enum BypassHeaderCompiled {
    /// Header presence triggers bypass (no regex pattern specified)
    Present { name: String },
    /// Header value must match pre-compiled regex to trigger bypass
    Regex { name: String, regex: Arc<Regex> },
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
                let re = compile_regex(&hm.value)?;
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
                let re = compile_regex(&qpm.value)?;
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
    pub backend_groups: Vec<WeightedBackendGroup>,
    /// Listener names this route applies to (e.g., ["http-80"], ["https-443"]).
    /// Empty means match all listeners.
    pub listeners: Vec<String>,
    /// HTTPRoute namespace/name for X-Gateway-Route header.
    pub route_name: Option<String>,
    pub priority: i32,
    pub rule_index: i32,
    /// Cache policy from VarnishCachePolicy. None means pass-through (no caching).
    pub cache_policy: Option<crate::config::CachePolicy>,
    /// Pre-compiled bypass header rules (extracted from cache_policy at config load time).
    pub bypass_headers: Vec<BypassHeaderCompiled>,
}

/// Map of vhost directors for two-tier routing
#[derive(Debug, Clone)]
pub struct VhostDirectorMap {
    /// Exact hostname matches to vhost directors
    pub exact: HashMap<String, Arc<VhostDirector>>,
    /// Wildcard hostname patterns to vhost directors (in order)
    pub wildcards: Vec<(String, Arc<VhostDirector>)>,
}

impl VhostDirectorMap {
    /// Iterate over all vhost directors (exact matches first, then wildcards)
    pub fn all_directors(&self) -> impl Iterator<Item = &Arc<VhostDirector>> {
        self.exact
            .values()
            .chain(self.wildcards.iter().map(|(_, d)| d))
    }
}

/// Resolve a config `BackendGroup` into a `WeightedBackendGroup` by registering
/// its backends with the pool. External-proxy groups yield one synthetic
/// backend per upstream; native groups yield one entry per resolved pod IP.
fn resolve_backend_group(
    ctx: &mut Ctx,
    backend_pool: &mut BackendPool,
    group: &BackendGroup,
) -> Result<WeightedBackendGroup, VclError> {
    let mut backend_keys = Vec::new();
    if let Some(ref ep) = group.external_proxy {
        backend_keys.push(backend_pool.get_or_create_external(ctx, ep)?);
    } else {
        for backend in &group.backends {
            backend_keys.push(backend_pool.get_or_create(
                ctx,
                &backend.address,
                backend.port,
                group.backend_tls.as_ref(),
            )?);
        }
    }
    Ok(WeightedBackendGroup {
        weight: group.weight,
        backends: backend_keys,
    })
}

/// Match conditions and cache settings compiled from one config route.
#[derive(Debug)]
struct CompiledRoute {
    path_match: Option<PathMatchCompiled>,
    headers: Vec<HeaderMatchCompiled>,
    query_params: Vec<QueryParamMatchCompiled>,
    /// `None` when the route has no cache policy, or when its policy had to be
    /// dropped because a bypass rule could not be compiled.
    cache_policy: Option<CachePolicy>,
    bypass_headers: Vec<BypassHeaderCompiled>,
}

/// Pre-compile a cache policy's bypass header rules (avoids per-request compilation).
fn compile_bypass_headers(cp: &CachePolicy) -> Result<Vec<BypassHeaderCompiled>, String> {
    cp.bypass_headers
        .iter()
        .map(|bh| match &bh.value_regex {
            None => Ok(BypassHeaderCompiled::Present {
                name: bh.name.clone(),
            }),
            Some(pattern) => {
                let re = compile_regex(pattern)
                    .map_err(|e| format!("invalid bypass header '{}': {}", bh.name, e))?;
                Ok(BypassHeaderCompiled::Regex {
                    name: bh.name.clone(),
                    regex: Arc::new(re),
                })
            }
        })
        .collect()
}

/// Compile the regexes of one route.
///
/// Each route is compiled on its own so that a single pattern the `regex`
/// crate rejects (the operator validates with Go `regexp` semantics, which
/// differ) degrades only that route instead of failing the whole reload and
/// taking every vhost down with it. The degradation depends on what failed:
///
/// - **Match condition** (path, header or query param regex): returns `Err`
///   and the caller drops the route. Without the condition we cannot tell
///   which requests the route was meant to catch, so serving it at all would
///   either hijack traffic or guess. Dropping an invalid rule is what the
///   Gateway API prescribes for partially invalid routes; requests fall
///   through to the remaining routes of the vhost, or 404.
/// - **Cache policy bypass rule**: the route keeps routing, but its cache
///   policy is discarded so every request on it is passed (not cached).
///   Bypass rules exist to keep requests (e.g. authenticated ones) out of the
///   cache; when one can't be evaluated, caching nothing is the safe default
///   and matches ghost's behavior for routes without a policy. The second
///   tuple element carries the reason.
fn compile_route(route: &Route) -> Result<(CompiledRoute, Option<String>), String> {
    let path_match = route
        .path_match
        .as_ref()
        .map(PathMatchCompiled::from_config)
        .transpose()
        .map_err(|e| format!("invalid path match: {}", e))?;

    let headers = route
        .headers
        .iter()
        .map(HeaderMatchCompiled::from_config)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("invalid header match: {}", e))?;

    let query_params = route
        .query_params
        .iter()
        .map(QueryParamMatchCompiled::from_config)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("invalid query param match: {}", e))?;

    let (cache_policy, bypass_headers, warning) = match &route.cache_policy {
        None => (None, Vec::new(), None),
        Some(cp) => match compile_bypass_headers(cp) {
            Ok(bypass) => (Some(cp.clone()), bypass, None),
            Err(e) => (None, Vec::new(), Some(e)),
        },
    };

    Ok((
        CompiledRoute {
            path_match,
            headers,
            query_params,
            cache_policy,
            bypass_headers,
        },
        warning,
    ))
}

/// Compile every route of a vhost independently.
///
/// Returns the routes that survived compilation (paired with their config
/// entry) and one message per degraded route. See [`compile_route`] for how
/// each kind of failure degrades.
fn compile_vhost_routes<'a>(
    hostname: &str,
    routes: &'a [Route],
) -> (Vec<(&'a Route, CompiledRoute)>, Vec<String>) {
    let mut kept = Vec::with_capacity(routes.len());
    let mut problems = Vec::new();

    for (i, route) in routes.iter().enumerate() {
        let label = match &route.route_name {
            Some(name) => format!("vhost {} route {} ({})", hostname, i, name),
            None => format!("vhost {} route {}", hostname, i),
        };
        match compile_route(route) {
            Ok((compiled, None)) => kept.push((route, compiled)),
            Ok((compiled, Some(warning))) => {
                problems.push(format!(
                    "ghost: {}: {}; cache policy ignored, requests on this route are passed",
                    label, warning
                ));
                kept.push((route, compiled));
            }
            Err(e) => problems.push(format!("ghost: {}: {}; route dropped", label, e)),
        }
    }

    (kept, problems)
}

/// Report a degraded route both to VSL and to stderr. varnishd relays child
/// stderr to its own log ("Child said ..."), so the message shows up in the
/// chaperone pod logs too, not only in varnishlog.
fn report_degraded_route(ctx: &mut Ctx, msg: &str) {
    use std::io::Write;
    ctx.log(LogTag::Error, msg);
    let _ = writeln!(std::io::stderr(), "{}", msg);
}

/// Build vhost directors from configuration
///
/// Creates a VhostDirector for each vhost in the config. Each director handles
/// route matching and backend selection for its hostname.
///
/// Routes whose regexes fail to compile are degraded individually (see
/// [`compile_route`]) rather than failing the build; only backend creation
/// errors abort it.
pub fn build_vhost_directors(
    config: &Config,
    backend_pool: &mut BackendPool,
    ctx: &mut Ctx,
    redirect_backend: BackendRef,
    internal_error_backend: BackendRef,
) -> Result<VhostDirectorMap, VclError> {
    let mut exact = HashMap::new();
    let mut wildcards = Vec::new();

    // First pass: build routes and populate backend pool
    let mut vhost_routes: HashMap<String, Vec<RouteEntry>> = HashMap::new();

    // Process vhosts
    for (hostname, vhost) in &config.vhosts {
        let (compiled_routes, problems) = compile_vhost_routes(hostname, &vhost.routes);
        for msg in &problems {
            report_degraded_route(ctx, msg);
        }

        let mut route_entries = Vec::with_capacity(compiled_routes.len());

        // Resolve backends only for routes that survived compilation, so a
        // dropped route doesn't keep backends alive in the pool.
        for (route, compiled) in compiled_routes {
            let mut groups = Vec::new();
            for group in &route.backend_groups {
                groups.push(resolve_backend_group(ctx, backend_pool, group)?);
            }

            route_entries.push(RouteEntry {
                path_match: compiled.path_match,
                method: route.method.clone(),
                headers: compiled.headers,
                query_params: compiled.query_params,
                filters: route.filters.as_ref().map(|f| Arc::new(f.clone())),
                backend_groups: groups,
                listeners: route.listeners.clone(),
                route_name: route.route_name.clone(),
                priority: route.priority,
                rule_index: route.rule_index,
                cache_policy: compiled.cache_policy,
                bypass_headers: compiled.bypass_headers,
            });
        }

        // Sort routes by priority (descending), then by rule_index (ascending) for tiebreaking
        route_entries.sort_by(|a, b| {
            b.priority
                .cmp(&a.priority)
                .then_with(|| a.rule_index.cmp(&b.rule_index))
        });

        // Add default_backends as lowest priority route if present
        if !vhost.default_backends.is_empty() {
            let mut default_groups = Vec::new();
            for group in &vhost.default_backends {
                default_groups.push(resolve_backend_group(ctx, backend_pool, group)?);
            }
            route_entries.push(RouteEntry {
                path_match: None,
                method: None,
                headers: Vec::new(),
                query_params: Vec::new(),
                filters: None,
                backend_groups: default_groups,
                listeners: Vec::new(),
                route_name: None,
                priority: 0,
                rule_index: i32::MAX,
                cache_policy: None,
                bypass_headers: Vec::new(),
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
            Some(redirect_backend.clone()),
            Some(internal_error_backend.clone()),
        ));

        // Categorize into exact or wildcard
        if hostname.starts_with("*.") {
            wildcards.push((hostname, vhost_director));
        } else {
            exact.insert(hostname, vhost_director);
        }
    }

    // Sort wildcards by descending suffix length so more specific patterns match first
    // (e.g., *.bar.example.com before *.example.com)
    wildcards.sort_by_key(|w| std::cmp::Reverse(w.0.len()));

    Ok(VhostDirectorMap { exact, wildcards })
}

/// Collect all backend keys referenced in vhost directors
fn collect_referenced_backends_from_directors(
    directors: &VhostDirectorMap,
) -> std::collections::HashSet<String> {
    let mut keys = std::collections::HashSet::new();
    for director in directors.all_directors() {
        for key in director.backend_keys() {
            keys.insert(key);
        }
    }
    keys
}

/// Process-wide reload epoch, bumped once per externally triggered reload.
///
/// A VMOD `.so` is `dlopen`ed once per process, so every VCL importing ghost
/// shares this counter. That matters because the reload endpoint lives in
/// `vcl_recv` and therefore only ever reaches the *active* VCL's director —
/// instances living in labelled VCLs would otherwise never see a config change.
///
/// The fan-out is a pull, not a push: backends are registered against
/// `ctx->vcl`, so reloading another VCL's director from here would build its
/// backends against the wrong VCL and tie their lifetime to the wrong program.
/// Instead the handling instance publishes a new epoch and every other instance
/// catches up on its own next request, in its own context.
static RELOAD_EPOCH: AtomicU64 = AtomicU64::new(0);

/// Ghost director implementation
pub struct GhostDirector {
    /// Vhost directors (atomic swap for lock-free reads)
    vhost_directors: ArcSwap<VhostDirectorMap>,
    /// Backend pool (atomic swap for lock-free reads)
    backends: ArcSwap<BackendPool>,
    /// Path to config file (for reload)
    config_path: PathBuf,
    /// Synthetic 404 backend for undefined vhosts (stored backend must outlive this director)
    not_found_backend: SendSyncBackendRef,
    /// Synthetic redirect backend for RequestRedirect filters (stored backend must outlive this director)
    redirect_backend: SendSyncBackendRef,
    /// Synthetic 500 backend for matched routes with no backends
    internal_error_backend: SendSyncBackendRef,
    /// Last reload error message (for debugging)
    last_error: RwLock<Option<String>>,
    /// Epoch this instance has caught up to. See [`RELOAD_EPOCH`].
    seen_epoch: AtomicU64,
    /// Serializes catch-up reloads so concurrent workers reload at most once.
    sync_lock: Mutex<()>,
}

/// Bundle returned by [`GhostDirectorBundle::new`].
///
/// The three synthetic `Backend` values must outlive the director — clones of
/// their `BackendRef`s are stored inside it. The caller is expected to keep
/// the whole bundle alive (typically as fields on the owning VMOD object).
pub struct GhostDirectorBundle {
    pub director: GhostDirector,
    pub not_found: Backend<NotFoundBackend, NotFoundBody>,
    pub redirect: Backend<RedirectBackend, RedirectBody>,
    pub internal_error: Backend<InternalErrorBackend, InternalErrorBody>,
}

impl GhostDirectorBundle {
    /// Create a new Ghost director plus the synthetic backends it depends on.
    ///
    /// The synthetic backends inside the returned bundle must be kept alive for
    /// the lifetime of the director — the director only holds (Send + Sync)
    /// clones of their `BackendRef`s.
    pub fn new(
        ctx: &mut Ctx,
        vhost_directors: Arc<VhostDirectorMap>,
        backends: BackendPool,
        config_path: PathBuf,
    ) -> Result<Self, VclError> {
        // Create synthetic 404 backend
        let not_found_backend = Backend::new(ctx, "ghost", "ghost_404", NotFoundBackend, false)?;
        let not_found_ref = SendSyncBackendRef(not_found_backend.as_ref().clone());

        // Create synthetic redirect backend
        let redirect_backend =
            Backend::new(ctx, "ghost", "ghost_redirect", RedirectBackend, false)?;
        let redirect_ref = SendSyncBackendRef(redirect_backend.as_ref().clone());

        // Create synthetic 500 backend for matched routes with no backends
        let internal_error_backend =
            Backend::new(ctx, "ghost", "ghost_500", InternalErrorBackend, false)?;
        let internal_error_ref = SendSyncBackendRef(internal_error_backend.as_ref().clone());

        let director = GhostDirector {
            vhost_directors: ArcSwap::new(Arc::clone(&vhost_directors)),
            backends: ArcSwap::new(Arc::new(backends)),
            config_path,
            not_found_backend: not_found_ref,
            redirect_backend: redirect_ref,
            internal_error_backend: internal_error_ref,
            last_error: RwLock::new(None),
            seen_epoch: AtomicU64::new(RELOAD_EPOCH.load(Ordering::Acquire)),
            sync_lock: Mutex::new(()),
        };

        Ok(GhostDirectorBundle {
            director,
            not_found: not_found_backend,
            redirect: redirect_backend,
            internal_error: internal_error_backend,
        })
    }
}

impl GhostDirector {
    /// Reload configuration from disk into this instance only.
    ///
    /// Use [`GhostDirector::reload_and_publish`] for an externally triggered
    /// reload that other instances in this process must also pick up.
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
        let new_directors = build_vhost_directors(
            &config,
            &mut backend_pool,
            ctx,
            self.redirect_backend.0.clone(),
            self.internal_error_backend.0.clone(),
        )
        .map_err(|e| {
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
        self.seen_epoch
            .fetch_max(RELOAD_EPOCH.load(Ordering::Acquire), Ordering::AcqRel);

        Ok(())
    }

    /// Reload from disk and tell every other ghost instance in this process to
    /// do the same on its next request. Called by the reload endpoint.
    pub fn reload_and_publish(&self, ctx: &mut Ctx) -> Result<(), String> {
        self.reload(ctx)?;
        let published = RELOAD_EPOCH.fetch_add(1, Ordering::AcqRel) + 1;
        self.seen_epoch.fetch_max(published, Ordering::AcqRel);
        Ok(())
    }

    /// Catch up with a reload that another instance handled.
    ///
    /// Called on the request path, so the common case is one atomic load and a
    /// comparison. Only instances in VCLs the reload endpoint never reaches —
    /// labelled per-route VCLs — do any work here.
    pub fn sync(&self, ctx: &mut Ctx) {
        let epoch = RELOAD_EPOCH.load(Ordering::Acquire);
        if self.seen_epoch.load(Ordering::Acquire) >= epoch {
            return;
        }

        let _guard = self.sync_lock.lock();
        if self.seen_epoch.load(Ordering::Acquire) >= epoch {
            return;
        }

        // Record the attempt whether or not it worked. A config this instance
        // cannot load will not load on the next request either, and retrying
        // per request would re-read the file on every hit. The next successful
        // reload elsewhere publishes a new epoch and we try again then; the
        // failure itself is in last_error() meanwhile.
        //
        // fetch_max, not store: a successful reload() already recorded the
        // epoch current at its end, which may be newer than `epoch` if another
        // instance published meanwhile. Don't move seen_epoch backwards.
        let _ = self.reload(ctx);
        self.seen_epoch.fetch_max(epoch, Ordering::AcqRel);
    }

    /// Get the last reload error message (if any)
    pub fn last_error(&self) -> Option<String> {
        self.last_error.read().clone()
    }

    /// Full routing in client context: hostname match → vhost → route → backend.
    ///
    /// Used by the recv() VMOD method to route requests in vcl_recv using
    /// req headers and local_socket() for listener-aware routing.
    pub fn route_request(
        &self,
        http: &mut HttpHeaders,
        listener: Option<&str>,
    ) -> vhost_director::RouteRequestResult {
        let host = match get_host_header(http) {
            Some(h) => h,
            None => return vhost_director::RouteRequestResult::default(),
        };

        let directors = self.vhost_directors.load();
        let vhost = match match_hostname(&directors, &host) {
            Some(dir) => dir,
            None => {
                return vhost_director::RouteRequestResult {
                    backend: Some(self.not_found_backend.0.clone()),
                    ..Default::default()
                }
            }
        };

        let mut result = vhost.route_request(http, listener);
        if result.backend.is_none() {
            result.backend = Some(self.not_found_backend.0.clone());
        }
        result
    }

    /// JSON output format for backend.list -j
    fn list_json(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        let directors = self.vhost_directors.load();
        let backends = self.backends.load();

        let all_backends: Vec<_> = directors
            .all_directors()
            .map(|director| {
                let selections = director.stats().backend_selections();
                let total = director.stats().total_requests();
                let backend_objs =
                    crate::format::format_backend_selections_json(&selections, total);

                serde_json::json!({
                    "name": format!("ghost.{}", director.hostname()),
                    "type": "vhost_director",
                    "admin": "auto",
                    "health": if director.probe(ctx).healthy { "healthy" } else { "sick" },
                    "routes": director.route_count(),
                    "total_requests": total,
                    "last_request": director.stats().last_request().map(|t| {
                        use crate::format::format_timestamp;
                        format_timestamp(Some(t))
                    }),
                    "backends": backend_objs
                })
            })
            .collect();

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
    fn resolve(&self, ctx: &mut Ctx) -> Option<BackendRef> {
        let bereq = ctx.http_bereq.as_mut()?;
        let result = self.route_request(bereq, None);
        for (tag, msg) in result.log_msgs {
            ctx.log(tag, &msg);
        }
        result.backend
    }

    fn probe(&self, ctx: &mut Ctx) -> ProbeResult {
        let directors = self.vhost_directors.load();
        let healthy = directors.all_directors().any(|d| d.probe(ctx).healthy);
        ProbeResult {
            healthy,
            last_changed: SystemTime::now(),
        }
    }

    fn report(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        let directors = self.vhost_directors.load();

        let header = "Backend name                      Admin    Health    Requests\n";
        let _ = vsb.write(&header);

        for director in directors.all_directors() {
            director.report(ctx, vsb);
        }
    }

    fn report_details(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        let directors = self.vhost_directors.load();
        for director in directors.all_directors() {
            director.report_details(ctx, vsb);
        }
    }

    fn report_json(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        self.list_json(ctx, vsb);
    }

    fn report_details_json(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        self.list_json(ctx, vsb);
    }
}

/// Match hostname to vhost director
///
/// Returns the vhost director for the matched hostname.
/// Matching priority: exact hostname > wildcard hostname > catch-all ("*")
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

    // 3. Try catch-all ("*") — routes with no explicit hostnames
    if let Some(director) = directors.exact.get("*") {
        return Some(director);
    }

    None
}

/// Wrapper around Arc<GhostDirector> to implement VclDirector (orphan rules workaround)
pub struct SharedGhostDirector(pub Arc<GhostDirector>);

impl VclDirector for SharedGhostDirector {
    fn resolve(&self, ctx: &mut Ctx) -> Option<BackendRef> {
        self.0.resolve(ctx)
    }

    fn probe(&self, ctx: &mut Ctx) -> ProbeResult {
        self.0.probe(ctx)
    }

    fn report(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        self.0.report(ctx, vsb)
    }

    fn report_details(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        self.0.report_details(ctx, vsb)
    }

    fn report_json(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        self.0.report_json(ctx, vsb)
    }

    fn report_details_json(&self, ctx: &mut Ctx, vsb: &mut Buffer) {
        self.0.report_details_json(ctx, vsb)
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

    // Check that the matched prefix is non-empty (at least one label before the suffix)
    let prefix_len = host.len() - suffix.len();
    let prefix = &host[..prefix_len];

    // Prefix must be non-empty (multi-level subdomains are allowed per Gateway API spec)
    !prefix.is_empty()
}

/// Convert StrOrBytes to Cow<str> if possible
fn str_or_bytes_to_cow<'a>(sob: &'a StrOrBytes<'a>) -> Option<Cow<'a, str>> {
    match sob {
        StrOrBytes::Utf8(s) => Some(Cow::Borrowed(s)),
        StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok().map(Cow::Borrowed),
    }
}

/// Get Host header value (without port)
///
/// Handles regular hostnames, IPv4 addresses, and IPv6 bracketed addresses.
fn get_host_header(http: &HttpHeaders) -> Option<String> {
    let host_value = http.header("host")?;
    let host_str = str_or_bytes_to_cow(&host_value)?;
    Some(strip_port(&host_str).to_lowercase())
}

/// Strip port from a host string, handling IPv6 bracketed addresses.
pub(crate) fn strip_port(host: &str) -> &str {
    if host.starts_with('[') {
        // IPv6 bracketed: [::1]:8080 -> [::1]
        match host.find(']') {
            Some(end) => &host[..=end],
            None => host,
        }
    } else {
        // Regular host or host:port — use rfind to avoid splitting on IPv6 colons
        match host.rfind(':') {
            Some(pos) if host[pos + 1..].parse::<u16>().is_ok() => &host[..pos],
            _ => host,
        }
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
        assert!(matches_wildcard("*.example.com", "foo.bar.example.com"));
        assert!(!matches_wildcard("*.example.com", ".example.com"));
        assert!(!matches_wildcard("*.example.com", "example.com"));
    }

    #[test]
    fn test_backend_group_creation() {
        let group = WeightedBackendGroup {
            weight: 100,
            backends: vec!["10.0.0.1:8080".to_string(), "10.0.0.2:8080".to_string()],
        };
        assert_eq!(group.weight, 100);
        assert_eq!(group.backends.len(), 2);
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

    /// Patterns from issue #104 that Go `regexp` (and so the operator) accepts
    /// but the Rust `regex` crate rejects.
    const GO_ONLY_PATTERNS: &[&str] = &[
        r"^/[\w.-]{1,255}$",
        r"^[\w-]{300}$",
        r"^[[a-z]+$",
        r"[[]",
        r"\Q.\E",
        r"\0",
        r"\101",
        r"(",
    ];

    #[test]
    fn test_go_only_patterns_rejected_by_regex_crate() {
        for pattern in GO_ONLY_PATTERNS {
            assert!(
                Regex::new(pattern).is_err(),
                "expected regex crate to reject {:?}",
                pattern
            );
        }
    }

    #[test]
    fn test_compile_regex_error_is_single_line() {
        for pattern in GO_ONLY_PATTERNS {
            let err = compile_regex(pattern).expect_err("pattern should be rejected");
            assert!(!err.contains('\n'), "multi-line error: {:?}", err);
            assert!(
                err.starts_with(&format!("invalid regex '{}': ", pattern)),
                "unexpected error: {}",
                err
            );
            // Something beyond the prefix must describe the failure.
            assert!(err.len() > format!("invalid regex '{}': ", pattern).len());
        }
        assert!(compile_regex("^/ok$").is_ok());
    }

    fn route_from_json(json: serde_json::Value) -> Route {
        serde_json::from_value(json).expect("route JSON should deserialize")
    }

    fn backend_groups_json() -> serde_json::Value {
        serde_json::json!([{"weight": 100, "backends": [{"address": "10.0.0.1", "port": 8080}]}])
    }

    #[test]
    fn test_compile_vhost_routes_drops_only_routes_with_bad_match_regex() {
        let mut routes = vec![route_from_json(serde_json::json!({
            "route_name": "default/good-prefix",
            "path_match": {"type": "PathPrefix", "value": "/api"},
            "backend_groups": backend_groups_json(),
            "priority": 100,
        }))];
        for (i, pattern) in GO_ONLY_PATTERNS.iter().enumerate() {
            // Spread the bad patterns over all three match kinds.
            let route = match i % 3 {
                0 => serde_json::json!({
                    "route_name": format!("default/bad-path-{}", i),
                    "path_match": {"type": "RegularExpression", "value": pattern},
                    "backend_groups": backend_groups_json(),
                    "priority": 200,
                }),
                1 => serde_json::json!({
                    "route_name": format!("default/bad-header-{}", i),
                    "headers": [{"name": "X-Foo", "value": pattern, "type": "RegularExpression"}],
                    "backend_groups": backend_groups_json(),
                    "priority": 200,
                }),
                _ => serde_json::json!({
                    "route_name": format!("default/bad-query-{}", i),
                    "query_params": [{"name": "q", "value": pattern, "type": "RegularExpression"}],
                    "backend_groups": backend_groups_json(),
                    "priority": 200,
                }),
            };
            routes.push(route_from_json(route));
        }
        routes.push(route_from_json(serde_json::json!({
            "route_name": "default/good-regex",
            "path_match": {"type": "RegularExpression", "value": r"^/files/\d+$"},
            "headers": [{"name": "X-Env", "value": "^prod$", "type": "RegularExpression"}],
            "backend_groups": backend_groups_json(),
            "priority": 300,
        })));

        let (kept, problems) = compile_vhost_routes("api.example.com", &routes);

        let kept_names: Vec<_> = kept
            .iter()
            .map(|(r, _)| r.route_name.as_deref().unwrap())
            .collect();
        assert_eq!(
            kept_names,
            vec!["default/good-prefix", "default/good-regex"]
        );
        assert_eq!(problems.len(), GO_ONLY_PATTERNS.len());
        for msg in &problems {
            assert!(msg.contains("route dropped"), "unexpected message: {}", msg);
            assert!(
                msg.contains("api.example.com"),
                "unexpected message: {}",
                msg
            );
        }

        // The surviving regex route is fully compiled and usable.
        let (_, good) = &kept[1];
        assert!(good.path_match.as_ref().unwrap().matches("/files/42"));
        assert_eq!(good.headers.len(), 1);
    }

    #[test]
    fn test_compile_route_bad_bypass_regex_disables_caching_only() {
        let routes = vec![
            route_from_json(serde_json::json!({
                "route_name": "default/bad-bypass",
                "path_match": {"type": "PathPrefix", "value": "/private"},
                "backend_groups": backend_groups_json(),
                "priority": 100,
                "cache_policy": {
                    "default_ttl_seconds": 60,
                    "bypass_headers": [
                        {"name": "Authorization"},
                        {"name": "Cookie", "value_regex": r"\Qsession\E"}
                    ]
                }
            })),
            route_from_json(serde_json::json!({
                "route_name": "default/good-cache",
                "path_match": {"type": "PathPrefix", "value": "/public"},
                "backend_groups": backend_groups_json(),
                "priority": 100,
                "cache_policy": {
                    "default_ttl_seconds": 60,
                    "bypass_headers": [{"name": "Cookie", "value_regex": "session="}]
                }
            })),
        ];

        let (kept, problems) = compile_vhost_routes("www.example.com", &routes);

        // Both routes still route; only the bad one loses its cache policy.
        assert_eq!(kept.len(), 2);
        assert_eq!(problems.len(), 1);
        assert!(problems[0].contains("default/bad-bypass"));
        assert!(problems[0].contains("requests on this route are passed"));

        let (_, bad) = &kept[0];
        assert!(bad.cache_policy.is_none());
        assert!(bad.bypass_headers.is_empty());
        assert!(bad.path_match.as_ref().unwrap().matches("/private/x"));

        let (_, good) = &kept[1];
        assert!(good.cache_policy.is_some());
        assert_eq!(good.bypass_headers.len(), 1);
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
    fn test_wildcard_specificity_ordering() {
        // More specific wildcard (*.bar.example.com) should match before
        // less specific (*.example.com), regardless of insertion order
        let directors = VhostDirectorMap {
            exact: HashMap::new(),
            wildcards: vec![
                (
                    "*.example.com".to_string(),
                    Arc::new(VhostDirector::new(
                        "*.example.com".to_string(),
                        vec![],
                        Arc::new(crate::backend_pool::BackendPool::new()),
                        None,
                        None,
                    )),
                ),
                (
                    "*.bar.example.com".to_string(),
                    Arc::new(VhostDirector::new(
                        "*.bar.example.com".to_string(),
                        vec![],
                        Arc::new(crate::backend_pool::BackendPool::new()),
                        None,
                        None,
                    )),
                ),
            ],
        };

        // foo.bar.example.com should match *.bar.example.com (more specific)
        // but only if wildcards are sorted by specificity. Since VhostDirectorMap
        // is built by build_vhost_directors which sorts, let's simulate the sort:
        let mut wildcards = directors.wildcards;
        wildcards.sort_by_key(|w| std::cmp::Reverse(w.0.len()));
        let sorted_directors = VhostDirectorMap {
            exact: directors.exact,
            wildcards,
        };

        let matched = match_hostname(&sorted_directors, "foo.bar.example.com");
        assert!(matched.is_some());
        assert_eq!(matched.unwrap().hostname(), "*.bar.example.com");

        // foo.example.com should match *.example.com
        let matched = match_hostname(&sorted_directors, "foo.example.com");
        assert!(matched.is_some());
        assert_eq!(matched.unwrap().hostname(), "*.example.com");
    }

    #[test]
    fn test_strip_port_regular_hostname() {
        assert_eq!(strip_port("example.com"), "example.com");
        assert_eq!(strip_port("example.com:8080"), "example.com");
        assert_eq!(strip_port("example.com:443"), "example.com");
    }

    #[test]
    fn test_strip_port_ipv4() {
        assert_eq!(strip_port("127.0.0.1"), "127.0.0.1");
        assert_eq!(strip_port("127.0.0.1:8080"), "127.0.0.1");
    }

    #[test]
    fn test_strip_port_ipv6_bracketed() {
        assert_eq!(strip_port("[::1]"), "[::1]");
        assert_eq!(strip_port("[::1]:8080"), "[::1]");
        assert_eq!(strip_port("[2001:db8::1]"), "[2001:db8::1]");
        assert_eq!(strip_port("[2001:db8::1]:443"), "[2001:db8::1]");
    }

    #[test]
    fn test_strip_port_ipv6_no_brackets() {
        // Bare IPv6 without brackets is not a valid Host header format (RFC 2732
        // requires brackets). The behavior here is best-effort — ambiguous inputs
        // like "::1" may be misinterpreted, which is acceptable since they're malformed.
        // Non-ambiguous bare IPv6 (no trailing numeric segment) is preserved.
        assert_eq!(strip_port("2001:db8::ff"), "2001:db8::ff");
    }
}
