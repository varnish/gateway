//! Ghost VMOD - Gateway API routing for Varnish.
//!
//! Routes requests by hostname and path to weighted backends using Varnish
//! native directors. Configuration is hot-reloaded from `ghost.json` without
//! restarting Varnish. See `README.md` for architecture details.

use parking_lot::RwLock;
use std::ffi::CStr;
use std::path::PathBuf;
use std::sync::Arc;

use varnish::ffi::{vrt_ctx, VCL_STRING};
use varnish::vcl::{Ctx, Director, StrOrBytes, VclError};

// VRT_r_local_socket is declared in vrt_obj.h but not included in varnish-rs bindings.
// It returns the name of the Varnish listener socket (e.g., "http-80") for the current request.
unsafe extern "C" {
    fn VRT_r_local_socket(ctx: *const vrt_ctx) -> VCL_STRING;
}

/// Get the local socket name from the Varnish context.
/// Returns `None` in backend context where the session isn't available.
fn local_socket<'a>(ctx: &'a Ctx<'a>) -> Option<&'a str> {
    let raw = unsafe { VRT_r_local_socket(ctx.raw) };
    let cstr = <Option<&CStr>>::from(raw)?;
    let s = cstr.to_str().ok()?;
    if s.is_empty() {
        None
    } else {
        Some(s)
    }
}

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
/// See `README.md` for VCL usage examples.
#[varnish::vmod(docs = "API.md")]
mod ghost {
    use super::*;
    use varnish::ffi::VCL_BACKEND;

    /// Initialize ghost with a configuration file path.
    ///
    /// Must be called in `vcl_init` before creating any ghost backends.
    /// The config file is not loaded here — it will be loaded when `ghost_backend` is created.
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
    /// Routes requests to upstream servers based on the Host header and loaded
    /// configuration. Performs weighted random backend selection per-vhost.
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

        /// Route the request in `vcl_recv` context.
        ///
        /// Performs full routing (hostname -> vhost -> route -> backend) using
        /// `req` headers and `local.socket` for listener-aware routing.
        /// Returns a concrete backend, not a director.
        /// Sets `X-Gateway-Listener` and `X-Gateway-Route` headers on the request.
        ///
        /// # Safety
        ///
        /// Must be called from VCL context with a valid `Ctx` that has an active
        /// client request (`http_req`). The returned `VCL_BACKEND` pointer is only
        /// valid for the lifetime of the current VCL transaction.
        pub unsafe fn recv(&self, ctx: &mut Ctx) -> VCL_BACKEND {
            // Copy listener to owned String to avoid borrow conflict:
            // local_socket() borrows ctx immutably, http_req.as_mut() needs mutable.
            let listener_owned = local_socket(ctx).map(|s| s.to_string());
            let fallback = self.director.as_ref().vcl_ptr();

            let req = match ctx.http_req.as_mut() {
                Some(r) => r,
                None => return fallback,
            };

            let result = self
                .ghost_director
                .route_request(req, listener_owned.as_deref());
            for (tag, msg) in result.log_msgs {
                ctx.log(tag, &msg);
            }

            // TODO: set hash_ignore_busy via C API when varnish-rs exposes it.
            // For now, pass mode (the default) already prevents coalescing.

            // Signal pass via header instead of ctx.set_pass() so that
            // user VCL concatenated after the preamble vcl_recv still runs.
            // The postamble vcl_recv checks this header and calls return(pass).
            if result.pass {
                if let Some(req) = ctx.http_req.as_mut() {
                    let _ = req.set_header("X-Ghost-Pass", "true");
                }
            }

            // Set X-Gateway-Listener and X-Gateway-Route headers for user VCL
            // Re-borrow req from ctx since the previous borrow ended after route_request
            if let Some(req) = ctx.http_req.as_mut() {
                if let Some(ref listener) = listener_owned {
                    let _ = req.set_header("X-Gateway-Listener", listener);
                }
                if let Some(ref name) = result.route_name {
                    let _ = req.set_header("X-Gateway-Route", name);
                }
            }

            match result.backend {
                Some(backend_ref) => backend_ref.vcl_ptr(),
                None => fallback,
            }
        }

        /// Get the VCL backend (director) for use in `vcl_backend_fetch`.
        ///
        /// Returns the ghost director which resolves backends in backend
        /// context. For listener-aware routing, use `recv()` in `vcl_recv` instead.
        ///
        /// # Safety
        ///
        /// Must be called from VCL context. The returned `VCL_BACKEND` pointer is
        /// only valid for the lifetime of the current VCL transaction.
        pub unsafe fn backend(&self) -> VCL_BACKEND {
            self.director.as_ref().vcl_ptr()
        }

        /// Reload the configuration from disk.
        ///
        /// Reads `ghost.json`, builds new routing state, and atomically swaps it in.
        /// Existing backends are preserved for connection reuse.
        /// Returns `true` on success, `false` on failure (see `last_error()`).
        pub fn reload(&self, ctx: &mut Ctx) -> bool {
            self.ghost_director.reload(ctx).is_ok()
        }

        /// Get the last reload error message, or empty string if no error.
        pub fn last_error(&self) -> String {
            self.ghost_director.last_error().unwrap_or_default()
        }
    }
}
