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

use parking_lot::RwLock;
use std::path::PathBuf;
use std::sync::Arc;

use varnish::vcl::{Ctx, Director, VclError};

mod backend_pool;
mod config;
mod director;

use backend_pool::BackendPool;
use director::{build_routing_state, GhostDirector, SharedGhostDirector};

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
///   "version": 1,
///   "vhosts": {
///     "api.example.com": {
///       "backends": [
///         {"address": "10.0.0.1", "port": 8080, "weight": 100},
///         {"address": "10.0.0.2", "port": 8080, "weight": 100}
///       ]
///     },
///     "*.staging.example.com": {
///       "backends": [
///         {"address": "10.0.2.1", "port": 8080, "weight": 100}
///       ]
///     }
///   },
///   "default": {
///     "backends": [
///       {"address": "10.0.99.1", "port": 80, "weight": 100}
///     ]
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

        // Validate config file exists and is parseable
        let _config = config::load(&config_path)
            .map_err(|e| VclError::new(format!("ghost.init: {}", e)))?;

        let state = GhostState { config_path };

        let mut guard = STATE.write();
        *guard = Some(Arc::new(state));

        Ok(())
    }

    /// Pre-routing hook for `vcl_recv`.
    ///
    /// This function is reserved for future URL rewriting and pre-routing logic.
    /// Currently returns `None` (no action). Reload handling has moved to the
    /// director for cleaner separation.
    ///
    /// # Returns
    ///
    /// - `None` - no action, continue normal request processing
    ///
    /// # Future Use
    ///
    /// This will be used for:
    /// - URL normalization/rewriting
    /// - Authentication checks
    /// - Rate limiting intercepts
    #[allow(unused_variables)]
    pub fn recv(ctx: &mut Ctx) -> Option<String> {
        // Placeholder for future URL rewriting logic
        None
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
        /// a director that manages native Varnish backends for all endpoints
        /// in the configuration.
        ///
        /// # Errors
        ///
        /// Returns an error if `ghost.init()` has not been called first, or if
        /// any backend creation fails.
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

            // Load configuration
            let config = config::load(&config_path)
                .map_err(|e| VclError::new(format!("ghost.backend: failed to load config: {}", e)))?;

            // Create backend pool for this director
            let mut backend_pool = BackendPool::new();

            // Build routing state and create backends
            let routing = build_routing_state(&config, &mut backend_pool, ctx)?;

            // Create director (Arc-wrapped so we can clone for reload access)
            let ghost_director = Arc::new(GhostDirector::new(Arc::new(routing), backend_pool, config_path));
            let shared_director = SharedGhostDirector(Arc::clone(&ghost_director));
            let director = Director::new(ctx, "ghost", name, shared_director)?;

            Ok(ghost_backend {
                director,
                ghost_director,
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
        /// # Returns
        ///
        /// - `true` on success
        /// - `false` on failure
        ///
        /// # Example
        ///
        /// ```vcl
        /// sub vcl_recv {
        ///     if (req.url == "/.varnish-ghost/reload" && (client.ip == "127.0.0.1" || client.ip == "::1")) {
        ///         if (router.reload()) {
        ///             return (synth(200, "OK"));
        ///         } else {
        ///             return (synth(500, "Reload failed"));
        ///         }
        ///     }
        /// }
        /// ```
        pub fn reload(&self, ctx: &mut Ctx) -> bool {
            self.ghost_director.reload(ctx).is_ok()
        }

        /// Check if the request's Host header matches any configured vhost.
        ///
        /// This can be used in `vcl_recv` to generate a 404 response for
        /// unconfigured hosts before attempting backend selection.
        ///
        /// # Returns
        ///
        /// - `true` if the Host header matches a configured vhost (exact, wildcard, or default)
        /// - `false` if no match is found
        ///
        /// # Example
        ///
        /// ```vcl
        /// sub vcl_recv {
        ///     if (!router.has_vhost()) {
        ///         return (synth(404, "vhost not found"));
        ///     }
        /// }
        /// ```
        pub fn has_vhost(&self, ctx: &mut Ctx) -> bool {
            self.ghost_director.has_vhost(ctx)
        }
    }
}
