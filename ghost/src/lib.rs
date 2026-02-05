//! Ghost VMOD - Gateway API routing for Varnish
//!
//! A purpose-built Varnish vmod for Kubernetes Gateway API implementation.
//! Handles backend management, request routing, and configuration hot-reloading.
//!
//! ## Native Backend Architecture
//!
//! Ghost uses Varnish native backends with the director pattern for routing.
//! This provides:
//!
//! - Battle-tested HTTP client and connection pooling from Varnish
//! - Lower latency and memory usage vs async Rust HTTP
//! - Simpler code with fewer dependencies
//! - Better integration with Varnish ecosystem
//!
//! ## Stack Usage Requirements
//!
//! Rust code, especially in debug builds, requires more stack space than typical C code.
//! Varnish's default thread pool stack size (80kB) is often insufficient for ghost.
//!
//! **Recommended configuration**: Increase Varnish's thread pool stack to 160kB:
//!
//! ```bash
//! varnishd -p thread_pool_stack=160k ...
//! ```
//!
//! This is particularly important when using regex-based routing rules, which have
//! higher stack requirements. Production deployments should always use this setting.

use parking_lot::RwLock;
use std::path::PathBuf;
use std::sync::Arc;

use varnish::vcl::{Ctx, Director, StrOrBytes, VclError};

mod backend_pool;
mod config;
mod director;
pub mod format;
mod not_found_backend;
mod redirect_backend;
mod stats;
mod vhost_director;

use backend_pool::BackendPool;
use config::ResponseHeaderFilter;
use director::{GhostDirector, SharedGhostDirector};
use not_found_backend::{NotFoundBackend, NotFoundBody};
use redirect_backend::{RedirectBackend, RedirectBody};

/// Header name for passing matched route filters to vcl_deliver
const FILTER_CONTEXT_HEADER: &str = "X-Ghost-Filter-Context";

// Run VTC tests
varnish::run_vtc_tests!("tests/*.vtc");

/// Global state for the ghost VMOD (routing config path only)
struct GhostState {
    config_path: PathBuf,
}

/// Global state storage (config path only, routing is in director instances)
static STATE: RwLock<Option<Arc<GhostState>>> = RwLock::new(None);

/// The ghost backend - wraps our director
#[allow(non_camel_case_types)]
pub struct ghost_backend {
    director: Director<SharedGhostDirector>,
    ghost_director: Arc<GhostDirector>,
    // Keep not_found_backend alive for the lifetime of this ghost_backend
    _not_found_backend: varnish::vcl::Backend<NotFoundBackend, NotFoundBody>,
    // Keep redirect_backend alive for the lifetime of this ghost_backend
    _redirect_backend: varnish::vcl::Backend<RedirectBackend, RedirectBody>,
}

/// Ghost VMOD - Gateway API routing for Varnish
///
/// Ghost is a purpose-built Varnish VMOD for Kubernetes Gateway API implementation.
/// It handles virtual host routing, backend selection, and configuration hot-reloading.
///
/// ## Features
///
/// - **Virtual host routing**: Route requests based on the Host header
/// - **Exact hostname matching**: `api.example.com`
/// - **Wildcard hostname matching**: `*.staging.example.com` (single label only, per Gateway API spec)
/// - **Weighted backend selection**: Distribute traffic across backends by weight
/// - **Hot configuration reload**: Update routing without restarting Varnish
/// - **Default backend fallback**: Catch-all for unmatched requests
/// - **Native backends**: Uses Varnish's built-in HTTP client for optimal performance
///
/// ## Minimal VCL Example
///
/// ```vcl
/// vcl 4.1;
///
/// import ghost;
///
/// backend dummy { .host = "127.0.0.1"; .port = "80"; }
///
/// sub vcl_init {
///     # Initialize ghost with the configuration file path
///     ghost.init("/etc/varnish/ghost.json");
///
///     # Create the ghost backend router
///     new router = ghost.ghost_backend();
/// }
///
/// sub vcl_recv {
///     # Intercept reload requests (localhost only) and bypass cache
///     if (req.url == "/.varnish-ghost/reload" && (client.ip == "127.0.0.1" || client.ip == "::1")) {
///         return (pass);
///     }
/// }
///
/// sub vcl_backend_fetch {
///     # Use ghost for backend selection based on Host header
///     # Ghost handles reload requests internally, returning 200/500 status
///     set bereq.backend = router.backend();
/// }
///
/// sub vcl_backend_error {
///     # Handle cases where ghost director returns no backend
///     if (beresp.status == 503) {
///         set beresp.http.Content-Type = "application/json";
///         synthetic({"{"error": "Backend selection failed"}"});
///     }
///     return (deliver);
/// }
/// ```
///
/// ## Configuration File Format (ghost.json)
///
/// ```json
/// {
///   "version": 2,
///   "vhosts": {
///     "api.example.com": {
///       "routes": [
///         {
///           "path_match": {"type": "PathPrefix", "value": "/api"},
///           "backends": [
///             {"address": "10.0.0.1", "port": 8080, "weight": 100},
///             {"address": "10.0.0.2", "port": 8080, "weight": 100}
///           ],
///           "priority": 100
///         }
///       ]
///     },
///     "*.staging.example.com": {
///       "routes": [
///         {
///           "backends": [
///             {"address": "10.0.2.1", "port": 8080, "weight": 100}
///           ],
///           "priority": 100
///         }
///       ],
///       "default_backends": [
///         {"address": "10.0.99.1", "port": 80, "weight": 100}
///       ]
///     }
///   }
/// }
/// ```
///
/// ## Error Handling
///
/// When no backend is found (no vhost match and no default), the director returns `None`,
/// causing Varnish to trigger `vcl_backend_error` with status 503. Handle this in your VCL
/// to provide appropriate error responses.
///
/// ## Hot Reload
///
/// Trigger a configuration reload by sending:
///
/// ```bash
/// curl -i http://localhost/.varnish-ghost/reload
/// ```
///
/// Returns HTTP 200 on success, HTTP 500 on failure (with error in `x-ghost-error` header).
/// The reload happens within the director, creating new backends as needed while preserving
/// existing connections for unchanged backends.
#[varnish::vmod(docs = "README.md")]
mod ghost {
    use super::*;
    use varnish::ffi::VCL_BACKEND;

    /// Initialize ghost with a configuration file path.
    ///
    /// This function must be called in `vcl_init` before creating any ghost backends.
    /// It loads and validates the JSON configuration file.
    ///
    /// # Arguments
    ///
    /// * `path` - Absolute path to the ghost configuration JSON file
    ///
    /// # Errors
    ///
    /// Returns an error if the configuration file cannot be read or contains invalid JSON.
    ///
    /// # Example
    ///
    /// ```vcl
    /// sub vcl_init {
    ///     ghost.init("/etc/varnish/ghost.json");
    /// }
    /// ```
    pub fn init(path: &str) -> Result<(), VclError> {
        let config_path = PathBuf::from(path);

        // Don't load config here - it may not exist yet during startup.
        // Config will be loaded when ghost_backend is created in vcl_init,
        // after chaperone has generated the initial ghost.json file.
        // This avoids race conditions during pod startup.

        let state = GhostState { config_path };

        let mut guard = STATE.write();
        *guard = Some(Arc::new(state));

        Ok(())
    }

    /// Pre-routing hook for `vcl_recv`.
    ///
    /// Reserved for future use in vcl_recv for request inspection and potential
    /// modification. Currently returns None (no action).
    ///
    /// # Future Use Cases
    ///
    /// This will enable:
    /// - URL normalization/rewriting (may require &mut Ctx for bereq modification)
    /// - Authentication checks (read-only)
    /// - Rate limiting decisions (read-only with external state)
    ///
    /// # Note
    ///
    /// The ctx parameter is currently unused but kept for API stability.
    /// Future implementations may need mutable access for request modification.
    #[allow(unused_variables)]
    pub fn recv(ctx: &Ctx) -> Option<String> {
        // Placeholder for future URL rewriting logic
        None
    }

    /// Deliver hook for response header modification.
    ///
    /// Call this in `vcl_deliver` to apply ResponseHeaderModifier filters.
    /// Reads filter context from response headers (copied from bereq in vcl_backend_response).
    pub fn deliver(ctx: &mut Ctx) {
        // Get mutable response for both reading and modifying
        let resp = match ctx.http_resp.as_mut() {
            Some(r) => r,
            None => return,
        };

        // Read filter context from response header
        let filter_json = match resp.header(FILTER_CONTEXT_HEADER) {
            Some(StrOrBytes::Utf8(s)) => s.to_string(),
            Some(StrOrBytes::Bytes(b)) => {
                match std::str::from_utf8(b) {
                    Ok(s) => s.to_string(),
                    Err(_) => return,
                }
            }
            None => return,
        };

        // Remove filter context header (internal only, don't leak to client)
        resp.unset_header(FILTER_CONTEXT_HEADER);

        // Deserialize filter
        let filter: ResponseHeaderFilter = match serde_json::from_str(&filter_json) {
            Ok(f) => f,
            Err(_) => return,
        };

        // Remove headers
        for name in &filter.remove {
            resp.unset_header(name);
        }

        // Set headers
        for action in &filter.set {
            let _ = resp.set_header(&action.name, &action.value);
        }

        // Add headers
        for action in &filter.add {
            let _ = resp.set_header(&action.name, &action.value);
        }
    }

    /// Ghost backend object for request routing.
    ///
    /// The ghost backend routes requests to upstream servers based on the
    /// Host header and the loaded configuration. It performs weighted random
    /// selection when multiple backends are available for a virtual host.
    ///
    /// Uses the director pattern with Varnish native backends for optimal
    /// performance and connection pooling.
    ///
    /// # Example
    ///
    /// ```vcl
    /// sub vcl_init {
    ///     ghost.init("/etc/varnish/ghost.json");
    ///     new router = ghost.ghost_backend();
    /// }
    ///
    /// sub vcl_backend_fetch {
    ///     set bereq.backend = router.backend();
    /// }
    /// ```
    impl ghost_backend {
        /// Create a new ghost backend instance.
        ///
        /// Must be called after `ghost.init()` has been called. This creates
        /// a director with empty routing state. Backends will be populated
        /// on the first `reload()` call after chaperone generates ghost.json.
        ///
        /// # Errors
        ///
        /// Returns an error if `ghost.init()` has not been called first.
        pub fn new(ctx: &mut Ctx, #[vcl_name] name: &str) -> Result<Self, VclError> {
            // Get config path from global state
            let config_path = {
                let state_guard = STATE.read();
                let state = state_guard
                    .as_ref()
                    .ok_or_else(|| {
                        VclError::new("ghost.backend: ghost.init() must be called first".to_string())
                    })?;
                state.config_path.clone()
            };

            // Don't load configuration here - it may not exist yet during startup.
            // Start with empty vhost directors. The first reload() call from chaperone
            // will populate backends after ghost.json is generated.
            use std::collections::HashMap;
            let vhost_directors = director::VhostDirectorMap {
                exact: HashMap::new(),
                wildcards: Vec::new(),
            };

            // Create empty backend pool
            let backend_pool = BackendPool::new();

            // Create director (Arc-wrapped so we can clone for reload access)
            let (ghost_director_impl, not_found_backend, redirect_backend) =
                GhostDirector::new(ctx, Arc::new(vhost_directors), backend_pool, config_path)?;
            let ghost_director = Arc::new(ghost_director_impl);
            let shared_director = SharedGhostDirector(Arc::clone(&ghost_director));
            let director = Director::new(ctx, "ghost", name, shared_director)?;

            Ok(ghost_backend {
                director,
                ghost_director,
                _not_found_backend: not_found_backend,
                _redirect_backend: redirect_backend,
            })
        }

        /// Get the VCL backend for use in `vcl_backend_fetch`.
        ///
        /// When this backend is used, ghost will:
        /// 1. Match the request's Host header against configured virtual hosts
        /// 2. Select a backend using weighted random selection
        /// 3. Return a native Varnish backend pointer for the selected endpoint
        /// 4. Varnish handles the actual HTTP request and connection pooling
        ///
        /// If no backend is found, returns `None` which causes `vcl_backend_error`
        /// with status 503.
        ///
        /// # Safety
        ///
        /// This function returns a raw VCL_BACKEND pointer that must only be used
        /// within VCL backend fetch context. The pointer is valid for the lifetime
        /// of the ghost_backend object.
        ///
        /// # Example
        ///
        /// ```vcl
        /// sub vcl_backend_fetch {
        ///     set bereq.backend = router.backend();
        /// }
        /// ```
        pub unsafe fn backend(&self) -> VCL_BACKEND {
            self.director.vcl_ptr()
        }

        /// Reload the configuration for this ghost backend.
        ///
        /// Reloads the ghost.json configuration file and updates routing state.
        /// New backends are created as needed, existing backends are preserved
        /// for connection pooling.
        ///
        /// Errors are logged to VSL (varnishlog) and can be retrieved via `last_error()`.
        ///
        /// # Returns
        ///
        /// - `true` on success
        /// - `false` on failure (check `last_error()` for details)
        ///
        /// # Example
        ///
        /// ```vcl
        /// sub vcl_recv {
        ///     if (req.url == "/.varnish-ghost/reload" && (client.ip == "127.0.0.1" || client.ip == "::1")) {
        ///         if (router.reload()) {
        ///             return (synth(200, "OK"));
        ///         } else {
        ///             set req.http.X-Ghost-Error = router.last_error();
        ///             return (synth(500, "Reload failed"));
        ///         }
        ///     }
        /// }
        /// ```
        pub fn reload(&self, ctx: &mut Ctx) -> bool {
            self.ghost_director.reload(ctx).is_ok()
        }

        /// Get the last reload error message.
        ///
        /// Returns the error message from the most recent failed reload attempt,
        /// or an empty string if the last reload succeeded or no reload has been attempted.
        ///
        /// # Returns
        ///
        /// The error message as a string, or empty string if no error.
        ///
        /// # Example
        ///
        /// ```vcl
        /// sub vcl_recv {
        ///     if (req.url == "/.varnish-ghost/reload") {
        ///         if (!router.reload()) {
        ///             set req.http.X-Ghost-Error = router.last_error();
        ///             return (synth(500, req.http.X-Ghost-Error));
        ///         }
        ///         return (synth(200, "OK"));
        ///     }
        /// }
        /// ```
        pub fn last_error(&self) -> String {
            self.ghost_director.last_error().unwrap_or_default()
        }
    }
}
