//! Ghost VMOD - Gateway API routing for Varnish
//!
//! A purpose-built Varnish vmod for Kubernetes Gateway API implementation.
//! Handles backend management, request routing, and configuration hot-reloading.

use parking_lot::RwLock;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use varnish::vcl::{Backend, Ctx, HttpHeaders, StrOrBytes, VclBackend, VclError};

mod config;
mod response;
mod routing;

use config::Config;
use response::ResponseBody;
use routing::MatchResult;

// Run VTC tests
varnish::run_vtc_tests!("tests/*.vtc");

/// Headers to filter out when forwarding requests to backends
const FILTERED_REQUEST_HEADERS: &[&str] = &[
    "host",
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

/// Global state for the ghost VMOD
struct GhostState {
    config_path: PathBuf,
    config: Config,
    http_client: reqwest::blocking::Client,
}

/// Global state storage
static STATE: RwLock<Option<Arc<GhostState>>> = RwLock::new(None);

/// The ghost backend - wraps our routing logic
#[allow(non_camel_case_types)]
pub struct ghost_backend {
    backend: Backend<GhostBackend, ResponseBody>,
}

/// Our VclBackend implementation that does the actual routing
struct GhostBackend;

impl VclBackend<ResponseBody> for GhostBackend {
    fn get_response(&self, ctx: &mut Ctx) -> Result<Option<ResponseBody>, VclError> {
        // Get state
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

        // Build request with copied headers
        let method = get_method(bereq).unwrap_or_else(|| "GET".to_string());
        let mut request = match method.as_str() {
            "GET" => state.http_client.get(&target_url),
            "POST" => state.http_client.post(&target_url),
            "PUT" => state.http_client.put(&target_url),
            "DELETE" => state.http_client.delete(&target_url),
            "HEAD" => state.http_client.head(&target_url),
            "PATCH" => state.http_client.patch(&target_url),
            _ => state.http_client.request(
                reqwest::Method::from_bytes(method.as_bytes())
                    .map_err(|e| VclError::new(format!("ghost: invalid method: {}", e)))?,
                &target_url,
            ),
        };

        // Copy headers (filtering out hop-by-hop headers)
        request = copy_request_headers(bereq, request);

        // Set X-Forwarded-Host
        request = request.header("X-Forwarded-Host", &host);

        // Send request
        let response = request
            .send()
            .map_err(|e| VclError::new(format!("ghost: backend request failed: {}", e)))?;

        // Set response headers on beresp
        let beresp = ctx
            .http_beresp
            .as_mut()
            .ok_or_else(|| VclError::new("ghost: no beresp available".to_string()))?;

        beresp.set_status(response.status().as_u16());

        // Copy response headers (filtering hop-by-hop)
        for (name, value) in response.headers() {
            let name_str = name.as_str();
            if !FILTERED_RESPONSE_HEADERS
                .iter()
                .any(|h| h.eq_ignore_ascii_case(name_str))
            {
                if let Ok(v) = value.to_str() {
                    let _ = beresp.set_header(name_str, v);
                }
            }
        }

        // Return streaming response - body is read on demand by Varnish,
        // avoiding buffering the entire response in memory
        Ok(Some(ResponseBody::streaming(response)))
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

/// Copy request headers to reqwest request builder
fn copy_request_headers(
    http: &HttpHeaders,
    mut request: reqwest::blocking::RequestBuilder,
) -> reqwest::blocking::RequestBuilder {
    for (name, value) in http {
        let name_lower = name.to_lowercase();
        if !FILTERED_REQUEST_HEADERS.iter().any(|h| *h == name_lower) {
            if let Some(v) = str_or_bytes_to_string(&value) {
                request = request.header(name, v);
            }
        }
    }
    request
}

/// Reload configuration from disk
fn reload_config() -> Result<(), String> {
    let state_guard = STATE.read();
    let current_state = state_guard.as_ref().ok_or("ghost not initialized")?;

    let config_path = current_state.config_path.clone();
    drop(state_guard);

    let config = config::load(&config_path)?;

    let http_client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .connect_timeout(Duration::from_secs(5))
        .build()
        .map_err(|e| format!("failed to create HTTP client: {}", e))?;

    let new_state = GhostState {
        config_path,
        config,
        http_client,
    };

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

        let http_client = reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(30))
            .connect_timeout(Duration::from_secs(5))
            .build()
            .map_err(|e| {
                VclError::new(format!("ghost.init: failed to create HTTP client: {}", e))
            })?;

        let state = GhostState {
            config_path,
            config,
            http_client,
        };

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
        /// Must be called after `ghost.init()` has been called.
        ///
        /// # Errors
        ///
        /// Returns an error if `ghost.init()` has not been called first.
        pub fn new(ctx: &mut Ctx, #[vcl_name] name: &str) -> Result<Self, VclError> {
            // Verify state is initialized
            {
                let state_guard = STATE.read();
                if state_guard.is_none() {
                    return Err(VclError::new(
                        "ghost.backend: ghost.init() must be called first".to_string(),
                    ));
                }
            }

            let backend = Backend::new(ctx, "ghost", name, GhostBackend, false)?;

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
