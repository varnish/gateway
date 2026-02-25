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
mod internal_error_backend;
mod not_found_backend;
mod redirect_backend;
mod stats;
mod vhost_director;

use backend_pool::BackendPool;
use config::ResponseHeaderFilter;
use director::{GhostDirector, SharedGhostDirector};
use internal_error_backend::{InternalErrorBackend, InternalErrorBody};
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
    // Keep internal_error_backend alive for the lifetime of this ghost_backend
    _internal_error_backend: varnish::vcl::Backend<InternalErrorBackend, InternalErrorBody>,
}

/// Ghost VMOD - Gateway API routing for Varnish.
///
/// Routes requests by hostname and path to weighted backends using Varnish native
/// directors. Configuration is hot-reloaded from `ghost.json` without restarting Varnish.
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
///     ghost.init("/etc/varnish/ghost.json");
///     new router = ghost.ghost_backend();
/// }
///
/// sub vcl_recv {
///     if (req.url == "/.varnish-ghost/reload" && (client.ip == "127.0.0.1" || client.ip == "::1")) {
///         return (pass);
///     }
/// }
///
/// sub vcl_backend_fetch {
///     set bereq.backend = router.backend();
/// }
/// ```
#[varnish::vmod(docs = "API.md")]
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

    /// Pre-routing hook for `vcl_recv`. Currently a no-op, reserved for future use.
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
            Some(StrOrBytes::Bytes(b)) => match std::str::from_utf8(b) {
                Ok(s) => s.to_string(),
                Err(_) => return,
            },
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

        // Set headers — must unset first since set_header() appends
        for action in &filter.set {
            resp.unset_header(&action.name);
            let _ = resp.set_header(&action.name, &action.value);
        }

        // Add headers (appends to existing value per Gateway API spec)
        // Must unset+set to avoid duplicate header slots
        for action in &filter.add {
            match resp.header(&action.name) {
                Some(existing) => {
                    let existing_str = match existing {
                        StrOrBytes::Utf8(s) => s.to_string(),
                        StrOrBytes::Bytes(b) => String::from_utf8_lossy(b).to_string(),
                    };
                    let combined = format!("{},{}", existing_str, action.value);
                    resp.unset_header(&action.name);
                    let _ = resp.set_header(&action.name, &combined);
                }
                None => {
                    let _ = resp.set_header(&action.name, &action.value);
                }
            }
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
        /// Must be called after `ghost.init()` has been called. If the config
        /// file (ghost.json) already exists on disk, routing state is loaded
        /// immediately. Otherwise the director starts empty and backends will
        /// be populated on the first `reload()` call from chaperone.
        ///
        /// # Errors
        ///
        /// Returns an error if `ghost.init()` has not been called first.
        pub fn new(ctx: &mut Ctx, #[vcl_name] name: &str) -> Result<Self, VclError> {
            // Get config path from global state
            let config_path = {
                let state_guard = STATE.read();
                let state = state_guard.as_ref().ok_or_else(|| {
                    VclError::new("ghost.backend: ghost.init() must be called first".to_string())
                })?;
                state.config_path.clone()
            };

            // Start with empty routing state
            use std::collections::HashMap;
            let empty_directors = director::VhostDirectorMap {
                exact: HashMap::new(),
                wildcards: Vec::new(),
            };
            let backend_pool = BackendPool::new();

            let (ghost_director_impl, not_found_backend, redirect_backend, internal_error_backend) =
                GhostDirector::new(ctx, Arc::new(empty_directors), backend_pool, config_path)?;

            // Pre-load config if the file already exists on disk.
            // On initial startup the file won't exist yet (chaperone hasn't
            // generated it), so reload() returns an empty config — same as before.
            // On VCL reload the file is already populated, so we get routing
            // state immediately with no empty window between vcl.use and the
            // next ghost reload from chaperone.
            if let Err(e) = ghost_director_impl.reload(ctx) {
                // Non-fatal: chaperone will trigger reload once ghost.json is ready
                ctx.log(
                    varnish::vcl::LogTag::Error,
                    format!("ghost init: pre-load skipped: {}", e),
                );
            }

            let ghost_director = Arc::new(ghost_director_impl);
            let shared_director = SharedGhostDirector(Arc::clone(&ghost_director));
            let director = Director::new(ctx, "ghost", name, shared_director)?;

            Ok(ghost_backend {
                director,
                ghost_director,
                _not_found_backend: not_found_backend,
                _redirect_backend: redirect_backend,
                _internal_error_backend: internal_error_backend,
            })
        }

        /// Get the VCL backend for use in `vcl_backend_fetch`.
        ///
        /// Matches the Host header against configured virtual hosts, selects a
        /// backend by weighted random, and returns a native Varnish backend.
        ///
        /// # Example
        ///
        /// ```vcl
        /// sub vcl_backend_fetch {
        ///     set bereq.backend = router.backend();
        /// }
        /// ```
        ///
        /// # Safety
        ///
        /// The returned `VCL_BACKEND` pointer is only valid for the lifetime of
        /// this director. Callers must ensure the director is not dropped while
        /// the backend pointer is in use.
        pub unsafe fn backend(&self) -> VCL_BACKEND {
            self.director.as_ref().vcl_ptr()
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

        /// Get the last reload error message, or empty string if no error.
        pub fn last_error(&self) -> String {
            self.ghost_director.last_error().unwrap_or_default()
        }
    }
}
