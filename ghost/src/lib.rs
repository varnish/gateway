//! Ghost VMOD - Gateway API routing for Varnish
//!
//! A purpose-built Varnish vmod for Kubernetes Gateway API implementation.
//! Handles backend management, request routing, and configuration hot-reloading.
//!
//! ## Connection Pooling
//!
//! Ghost uses a background tokio runtime with an async reqwest client for proper
//! connection pooling. The runtime is created on VCL load and shared across all
//! backends via `#[shared_per_vcl]`. This means:
//!
//! - Connections are reused across requests
//! - Config reloads don't drop existing connections
//! - Pool parameters (idle timeout, max connections) are properly managed

use parking_lot::RwLock;
use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::mpsc::UnboundedSender;

use varnish::vcl::{Backend, Ctx, Event, HttpHeaders, StrOrBytes, VclBackend, VclError};

mod config;
mod response;
mod routing;
pub mod runtime;

use config::Config;
use response::ResponseBody;
use routing::MatchResult;
pub use runtime::BgThread;
use runtime::HttpRequest;

// Run VTC tests
varnish::run_vtc_tests!("tests/*.vtc");

/// Todo: Is this really needed?
/// Headers to filter out when forwarding requests to backends
const FILTERED_REQUEST_HEADERS: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
    "x-forwarded-for",
    "x-forwarded-host",
    "x-forwarded-proto",
];

/// Headers to filter out when returning responses
const FILTERED_RESPONSE_HEADERS: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
];

/// Global state for the ghost VMOD (routing config only, HTTP client is in BgThread)
struct GhostState {
    config_path: PathBuf,
    config: Config,
}

/// Global state storage (routing config only)
static STATE: RwLock<Option<Arc<GhostState>>> = RwLock::new(None);

/// The ghost backend - wraps our routing logic
#[allow(non_camel_case_types)]
pub struct ghost_backend {
    backend: Backend<GhostBackend, ResponseBody>,
}

/// Our VclBackend implementation that does the actual routing
struct GhostBackend {
    /// Channel sender to the background runtime for HTTP requests
    sender: UnboundedSender<HttpRequest>,
}

impl VclBackend<ResponseBody> for GhostBackend {
    fn get_response(&self, ctx: &mut Ctx) -> Result<Option<ResponseBody>, VclError> {
        // Get routing config state
        let state_guard = STATE.read();
        let state = state_guard
            .as_ref()
            .ok_or_else(|| VclError::new("ghost: not initialized".to_string()))?;

        // Get Host header from bereq
        let bereq = ctx
            .http_bereq
            .as_ref()
            .ok_or_else(|| VclError::new("ghost: no bereq available".to_string()))?;

        let host = get_host_header(bereq)
            .ok_or_else(|| VclError::new("ghost: no Host header in request".to_string()))?;

        // Get URL from bereq
        let url = get_url(bereq).unwrap_or_else(|| "/".to_string());

        // Match vhost
        let vhost = match routing::match_vhost(&state.config, &host) {
            MatchResult::Found(vhost) => vhost,
            MatchResult::NotFound => {
                return Ok(Some(synth_response(
                    ctx,
                    404,
                    "Not Found",
                    &format!(r#"{{"error": "no vhost match", "host": "{}"}}"#, host),
                )?));
            }
            MatchResult::NoBackends => {
                return Ok(Some(synth_response(
                    ctx,
                    503,
                    "Service Unavailable",
                    &format!(
                        r#"{{"error": "no backends available", "host": "{}"}}"#,
                        host
                    ),
                )?));
            }
        };

        // Select backend
        let target = routing::select_backend(vhost)
            .ok_or_else(|| VclError::new("ghost: failed to select backend".to_string()))?;

        // Build request URL
        let target_url = format!("http://{}:{}{}", target.address, target.port, url);

        // Parse method
        let method_str = get_method(bereq).unwrap_or_default();
        let method: reqwest::Method = method_str
            .parse()
            .unwrap_or(reqwest::Method::GET);

        // Collect headers (filtering hop-by-hop)
        let mut headers = collect_request_headers(bereq);
        headers.push(("X-Forwarded-Host".to_string(), host.clone()));

        // Drop state guard before blocking
        drop(state_guard);

        // Create oneshot channel for response
        let (response_tx, response_rx) = tokio::sync::oneshot::channel();

        // Build request for background runtime
        let request = HttpRequest {
            method,
            url: target_url,
            headers,
            response_tx,
        };

        // Send to background runtime
        self.sender
            .send(request)
            .map_err(|_| VclError::new("ghost: background runtime unavailable".to_string()))?;

        // Block waiting for response from async runtime
        let response = response_rx
            .blocking_recv()
            .map_err(|_| VclError::new("ghost: request was cancelled".to_string()))?
            .map_err(|e| VclError::new(format!("ghost: backend request failed: {}", e)))?;

        // Set response headers on beresp
        let beresp = ctx
            .http_beresp
            .as_mut()
            .ok_or_else(|| VclError::new("ghost: no beresp available".to_string()))?;

        beresp.set_status(response.status);

        // Copy response headers (filtering hop-by-hop)
        for (name, value) in &response.headers {
            if !FILTERED_RESPONSE_HEADERS
                .iter()
                .any(|h| h.eq_ignore_ascii_case(name))
            {
                let _ = beresp.set_header(name, value);
            }
        }

        // Get content-length if available
        let content_length = response.headers.iter().find_map(|(k, v)| {
            if k.eq_ignore_ascii_case("content-length") {
                v.parse().ok()
            } else {
                None
            }
        });

        // Return streaming response body via channel
        Ok(Some(ResponseBody::async_streaming(
            response.body_rx,
            content_length,
        )))
    }
}

/// Generate a synthetic response
fn synth_response(
    ctx: &mut Ctx,
    status: u16,
    reason: &str,
    body: &str,
) -> Result<ResponseBody, VclError> {
    let beresp = ctx
        .http_beresp
        .as_mut()
        .ok_or_else(|| VclError::new("ghost: no beresp available".to_string()))?;

    beresp.set_status(status);
    beresp.set_header("content-type", "application/json")?;
    beresp.set_header("x-ghost-error", reason)?;

    Ok(ResponseBody::buffered(body.as_bytes().to_vec()))
}

/// Convert StrOrBytes to String if possible
fn str_or_bytes_to_string(sob: &StrOrBytes) -> Option<String> {
    match sob {
        StrOrBytes::Utf8(s) => Some(s.to_string()),
        StrOrBytes::Bytes(b) => std::str::from_utf8(b).ok().map(|s| s.to_string()),
    }
}

/// Get Host header value (without port)
fn get_host_header(http: &HttpHeaders) -> Option<String> {
    // Use the header() method for case-insensitive lookup
    let host_value = http.header("host")?;
    let host_str = str_or_bytes_to_string(&host_value)?;
    // Strip port if present
    let host = host_str.split(':').next()?;
    Some(host.to_lowercase())
}

/// Get URL from HTTP request
fn get_url(http: &HttpHeaders) -> Option<String> {
    http.url().and_then(|s| str_or_bytes_to_string(&s))
}

/// Get method from HTTP request
fn get_method(http: &HttpHeaders) -> Option<String> {
    http.method().and_then(|s| str_or_bytes_to_string(&s))
}

/// Collect request headers into a Vec (filtering hop-by-hop headers)
fn collect_request_headers(http: &HttpHeaders) -> Vec<(String, String)> {
    let mut headers = Vec::new();
    for (name, value) in http {
        let name_lower = name.to_lowercase();
        if !FILTERED_REQUEST_HEADERS.iter().any(|h| *h == name_lower) {
            if let Some(v) = str_or_bytes_to_string(&value) {
                headers.push((name.to_string(), v));
            }
        }
    }
    headers
}

/// Reload configuration from disk (HTTP client is in BgThread, not recreated here)
fn reload_config() -> Result<(), String> {
    let state_guard = STATE.read();
    let current_state = state_guard.as_ref().ok_or("ghost not initialized")?;

    let config_path = current_state.config_path.clone();
    drop(state_guard);

    let config = config::load(&config_path)?;

    let new_state = GhostState { config_path, config };

    let mut guard = STATE.write();
    *guard = Some(Arc::new(new_state));

    Ok(())
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
///
/// ## Minimal VCL Example
///
/// ```vcl
/// vcl 4.1;
///
/// import ghost;
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
///     # Handle configuration reload requests
///     # Returns JSON response for /.varnish-ghost/reload
///     set req.http.x-ghost-reload = ghost.recv();
///     if (req.http.x-ghost-reload) {
///         return (synth(200, "Reload"));
///     }
/// }
///
/// sub vcl_synth {
///     # Return reload response
///     if (req.http.x-ghost-reload) {
///         set resp.http.content-type = "application/json";
///         set resp.body = req.http.x-ghost-reload;
///         return (deliver);
///     }
/// }
///
/// sub vcl_backend_fetch {
///     # Use ghost for backend selection based on Host header
///     set bereq.backend = router.backend();
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
/// ## Error Responses
///
/// - **404 Not Found**: No virtual host matched and no default configured
/// - **503 Service Unavailable**: Virtual host matched but has no backends
///
/// Both error responses include a JSON body with details.
///
/// ## Hot Reload
///
/// Trigger a configuration reload by sending:
///
/// ```bash
/// curl http://localhost/.varnish-ghost/reload
/// ```
///
/// Returns `{"status": "ok", "message": "configuration reloaded"}` on success.
#[varnish::vmod(docs = "README.md")]
mod ghost {
    use super::*;
    use varnish::ffi::VCL_BACKEND;

    /// VCL event handler - creates background runtime on VCL load.
    ///
    /// This is called automatically by Varnish when the VCL is loaded or discarded.
    /// It creates the background tokio runtime with the async HTTP client for
    /// connection pooling. The runtime is shared across all ghost backends in
    /// the VCL via `#[shared_per_vcl]`.
    #[event]
    pub fn event(
        #[shared_per_vcl] bg_thread: &mut Option<Box<BgThread>>,
        event: Event,
    ) {
        if let Event::Load = event {
            match BgThread::new() {
                Ok(bgt) => {
                    *bg_thread = Some(Box::new(bgt));
                }
                Err(e) => {
                    // Log error but don't crash - the vmod will fail gracefully
                    // when backends are used without initialization
                    eprintln!("ghost: failed to initialize background runtime: {}", e);
                }
            }
        }
        // BgThread is automatically dropped when VCL is discarded
    }

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
        let config =
            config::load(&config_path).map_err(|e| VclError::new(format!("ghost.init: {}", e)))?;

        let state = GhostState { config_path, config };

        let mut guard = STATE.write();
        *guard = Some(Arc::new(state));

        Ok(())
    }

    /// Handle reload requests in `vcl_recv`.
    ///
    /// Checks if the current request is a configuration reload request
    /// (path `/.varnish-ghost/reload`). If so, reloads the configuration
    /// from disk and returns a JSON status message.
    ///
    /// # Returns
    ///
    /// - `None` if this is a normal request (not a reload request)
    /// - `Some(json)` if this is a reload request, containing the status
    ///
    /// # Example
    ///
    /// ```vcl
    /// sub vcl_recv {
    ///     set req.http.x-ghost-reload = ghost.recv();
    ///     if (req.http.x-ghost-reload) {
    ///         return (synth(200, "Reload"));
    ///     }
    /// }
    ///
    /// sub vcl_synth {
    ///     if (req.http.x-ghost-reload) {
    ///         set resp.http.content-type = "application/json";
    ///         set resp.body = req.http.x-ghost-reload;
    ///         return (deliver);
    ///     }
    /// }
    /// ```
    pub fn recv(ctx: &mut Ctx) -> Option<String> {
        let req = ctx.http_req.as_ref()?;

        // Check for reload path
        let url = req.url()?;
        let url_str = str_or_bytes_to_string(&url)?;
        if url_str != "/.varnish-ghost/reload" {
            return None;
        }

        // Check for localhost (basic check - could be improved)
        // For now, we'll allow the reload from anywhere since this is Phase 1
        // TODO: Add proper localhost check in production

        // Reload config
        let result = reload_config();

        match result {
            Ok(()) => Some(r#"{"status": "ok", "message": "configuration reloaded"}"#.to_string()),
            Err(e) => Some(format!(
                r#"{{"status": "error", "message": "{}"}}"#,
                e.replace('"', "\\\"")
            )),
        }
    }

    /// Ghost backend object for request routing.
    ///
    /// The ghost backend routes requests to upstream servers based on the
    /// Host header and the loaded configuration. It performs weighted random
    /// selection when multiple backends are available for a virtual host.
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
        /// Must be called after `ghost.init()` has been called. The background
        /// runtime (created automatically on VCL load) provides connection pooling.
        ///
        /// # Errors
        ///
        /// Returns an error if `ghost.init()` has not been called first, or if
        /// the background runtime failed to initialize.
        pub fn new(
            ctx: &mut Ctx,
            #[vcl_name] name: &str,
            #[shared_per_vcl] bg_thread: &mut Option<Box<BgThread>>,
        ) -> Result<Self, VclError> {
            // Verify routing config is initialized
            {
                let state_guard = STATE.read();
                if state_guard.is_none() {
                    return Err(VclError::new(
                        "ghost.backend: ghost.init() must be called first".to_string(),
                    ));
                }
            }

            // Verify background runtime is initialized
            let bg = bg_thread.as_ref().ok_or_else(|| {
                VclError::new("ghost.backend: background runtime not initialized".to_string())
            })?;

            let backend = Backend::new(
                ctx,
                "ghost",
                name,
                GhostBackend {
                    sender: bg.sender.clone(),
                },
                false,
            )?;

            Ok(ghost_backend { backend })
        }

        /// Get the VCL backend for use in `vcl_backend_fetch`.
        ///
        /// When this backend is used, ghost will:
        /// 1. Match the request's Host header against configured virtual hosts
        /// 2. Select a backend using weighted random selection
        /// 3. Forward the request to the selected backend
        /// 4. Return the response (or a synthetic 404/503 on error)
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
            self.backend.vcl_ptr()
        }
    }
}
