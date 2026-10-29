//! Synthetic backend for Kubernetes ExternalName Services.
//!
//! Modelled on `vmod-reqwest`: one synthetic Varnish backend (with stable
//! VBE stats) fronts a `reqwest::blocking::Client` whose connection pool and
//! DNS resolver hide rotating cloud IPs from Varnish. The blocking client
//! runs its own internal runtime thread, so the Varnish worker simply blocks
//! on `send()` and on body reads.

use std::io::{Read, Write};
use std::time::Duration;

use reqwest::blocking::{Client, Response};
use reqwest::header::HeaderName;
use varnish::vcl::{Ctx, StrOrBytes, VclBackend, VclError, VclResponse};

use crate::config::ExternalProxy;

const DEFAULT_REQUEST_TIMEOUT: Duration = Duration::from_secs(60);
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// Methods the synthetic backend will forward. Anything else is rejected
/// with 405 because we cannot stream a request body through the current
/// VclBackend bridge.
const ALLOWED_METHODS: &str = "GET, HEAD, OPTIONS";

/// Body returned with the synthetic 405 so curl/log readers see why.
const METHOD_NOT_ALLOWED_BODY: &[u8] =
    b"external proxy backend does not forward request bodies; allowed methods: GET, HEAD, OPTIONS\n";

/// Synthetic backend that proxies requests via `reqwest` to a single upstream
/// hostname. One `ExternalBackend` per unique (hostname, port, tls) tuple —
/// reqwest keeps the connection pool and DNS cache underneath, so rotating
/// IPs don't pollute Varnish's VBE stats.
pub struct ExternalBackend {
    base_url: String,
    upstream_host: String,
    client: Client,
}

impl ExternalBackend {
    pub fn new(proxy: &ExternalProxy) -> Result<Self, VclError> {
        if proxy.hostname.is_empty() {
            return Err(VclError::new(
                "external_proxy: hostname is empty".to_string(),
            ));
        }
        if proxy.port == 0 {
            return Err(VclError::new("external_proxy: port is zero".to_string()));
        }

        let scheme = if proxy.tls { "https" } else { "http" };
        let base_url = format!("{}://{}:{}", scheme, proxy.hostname, proxy.port);

        // Auto-decompression is intentionally not enabled (feature not
        // compiled in) so proxied bytes pass through unmodified and Varnish
        // can cache the wire representation.
        let client = reqwest::blocking::ClientBuilder::new()
            .timeout(DEFAULT_REQUEST_TIMEOUT)
            .connect_timeout(DEFAULT_CONNECT_TIMEOUT)
            // Surface 30x to the cache layer instead of following.
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .map_err(|e| {
                VclError::new(format!(
                    "external_proxy: failed to build reqwest client: {}",
                    e
                ))
            })?;

        Ok(Self {
            base_url,
            upstream_host: proxy.hostname.clone(),
            client,
        })
    }
}

impl VclBackend<ExternalBody> for ExternalBackend {
    fn get_response(&self, ctx: &mut Ctx<'_>) -> Result<Option<ExternalBody>, VclError> {
        // Pull the method first so the 405 fast-path skips the header copy.
        // Each block drops its bereq borrow before we touch ctx mutably.
        let method_str = {
            let bereq = ctx
                .http_bereq
                .as_ref()
                .ok_or_else(|| VclError::new("external_proxy: missing bereq".to_string()))?;
            sob_to_str(bereq.method())?.to_string()
        };

        let method = reqwest::Method::from_bytes(method_str.as_bytes())
            .map_err(|e| VclError::new(format!("external_proxy: invalid method: {}", e)))?;

        // Bodies on bereq aren't streamed into reqwest yet. Rather than
        // forwarding a body-bearing method with the body silently dropped —
        // which can succeed at a forgiving upstream and look like a normal
        // 2xx to the client — reject it locally with 405 + Allow.
        if method_implies_body(&method) {
            ctx.log(
                varnish::vcl::LogTag::Error,
                format!(
                    "external_proxy: rejecting {} with 405; request bodies are not forwarded",
                    method
                ),
            );
            let beresp = ctx
                .http_beresp
                .as_mut()
                .ok_or_else(|| VclError::new("external_proxy: missing beresp".to_string()))?;
            beresp.set_status(405);
            beresp.set_proto("HTTP/1.1")?;
            beresp.set_header("Allow", ALLOWED_METHODS)?;
            beresp.set_header("Content-Type", "text/plain; charset=utf-8")?;
            beresp.set_header("Cache-Control", "no-store")?;
            return Ok(Some(ExternalBody::from_static(METHOD_NOT_ALLOWED_BODY)));
        }

        let (path, headers_owned) = {
            let bereq = ctx
                .http_bereq
                .as_ref()
                .ok_or_else(|| VclError::new("external_proxy: missing bereq".to_string()))?;
            let p = sob_to_str(bereq.url())?.to_string();
            let headers: Vec<(String, Vec<u8>)> = bereq
                .into_iter()
                .filter(|(k, _)| forward_client_header(k))
                .map(|(k, v)| (k.to_string(), v.as_ref().to_vec()))
                .collect();
            (p, headers)
        };

        let url = format!("{}{}", self.base_url, path);
        let mut req_builder = self.client.request(method, &url);
        // Host is set explicitly to the externalName so object stores route to
        // the right bucket.
        for (k, v) in headers_owned {
            if let Ok(name) = HeaderName::try_from(k.as_str()) {
                req_builder = req_builder.header(name, v);
            }
        }
        req_builder = req_builder.header("host", &self.upstream_host);

        let resp = req_builder
            .send()
            .map_err(|e| VclError::new(format!("external proxy: {}", e)))?;

        let content_length = resp
            .content_length()
            .map(|c| {
                usize::try_from(c).map_err(|e| {
                    VclError::new(format!(
                        "external_proxy: invalid content-length ({}): {}",
                        c, e
                    ))
                })
            })
            .transpose()?;

        let beresp = ctx
            .http_beresp
            .as_mut()
            .ok_or_else(|| VclError::new("external_proxy: missing beresp".to_string()))?;
        beresp.set_status(resp.status().as_u16());
        beresp.set_proto("HTTP/1.1")?;
        for (k, v) in resp.headers() {
            if is_hop_by_hop(k.as_str()) {
                continue;
            }
            if let Ok(s) = v.to_str() {
                beresp.set_header(k.as_str(), s)?;
            }
        }

        Ok(Some(ExternalBody::streamed(resp, content_length)))
    }
}

/// Response body for a synthetic backend. Either streams the upstream
/// `reqwest` response (success path) or serves a fixed static buffer
/// (locally generated responses like the 405 rejection).
pub struct ExternalBody {
    state: BodyState,
}

enum BodyState {
    Streamed {
        resp: Response,
        content_length: Option<usize>,
    },
    Static {
        data: &'static [u8],
        cursor: usize,
    },
}

impl ExternalBody {
    fn streamed(resp: Response, content_length: Option<usize>) -> Self {
        Self {
            state: BodyState::Streamed {
                resp,
                content_length,
            },
        }
    }

    fn from_static(data: &'static [u8]) -> Self {
        Self {
            state: BodyState::Static { data, cursor: 0 },
        }
    }
}

impl VclResponse for ExternalBody {
    fn read(&mut self, mut buf: &mut [u8]) -> Result<usize, VclError> {
        match &mut self.state {
            BodyState::Streamed { resp, .. } => resp
                .read(buf)
                .map_err(|e| VclError::new(format!("external proxy chunk: {}", e))),
            BodyState::Static { data, cursor } => {
                let remaining = &data[*cursor..];
                let n = buf
                    .write(remaining)
                    .map_err(|e| VclError::new(format!("external_proxy: body write: {}", e)))?;
                *cursor += n;
                Ok(n)
            }
        }
    }

    fn len(&self) -> Option<usize> {
        match &self.state {
            BodyState::Streamed { content_length, .. } => *content_length,
            BodyState::Static { data, .. } => Some(data.len()),
        }
    }
}

fn method_implies_body(method: &reqwest::Method) -> bool {
    matches!(
        *method,
        reqwest::Method::POST
            | reqwest::Method::PUT
            | reqwest::Method::PATCH
            | reqwest::Method::DELETE
    )
}

fn sob_to_str<'a>(value: Option<StrOrBytes<'a>>) -> Result<&'a str, VclError> {
    match value {
        Some(StrOrBytes::Utf8(s)) => Ok(s),
        Some(StrOrBytes::Bytes(b)) => std::str::from_utf8(b)
            .map_err(|e| VclError::new(format!("external_proxy: non-utf8 value: {}", e))),
        None => Err(VclError::new(
            "external_proxy: missing bereq field".to_string(),
        )),
    }
}

/// RFC 7230 §6.1 hop-by-hop headers — must not be forwarded.
const HOP_BY_HOP: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
];

fn is_hop_by_hop(name: &str) -> bool {
    HOP_BY_HOP.iter().any(|h| name.eq_ignore_ascii_case(h))
}

/// Whether a client (bereq) header should be copied verbatim onto the
/// upstream reqwest request.
///
/// Hop-by-hop headers are dropped per RFC 7230. `Host` is dropped too: it is
/// NOT hop-by-hop, so without this guard it would be copied here and then a
/// SECOND `Host` appended by `req_builder.header("host", ...)` — reqwest's
/// `.header()` appends rather than replaces, so two conflicting `Host:` lines
/// would go on the wire (client value first). Strict upstreams reject that
/// (nginx 400; S3/GCS SigV4 signature mismatch). The upstream `Host` is set
/// exactly once, from `self.upstream_host`.
fn forward_client_header(name: &str) -> bool {
    !is_hop_by_hop(name) && !name.eq_ignore_ascii_case("host")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn is_hop_by_hop_basic() {
        assert!(is_hop_by_hop("Connection"));
        assert!(is_hop_by_hop("keep-alive"));
        assert!(is_hop_by_hop("Transfer-Encoding"));
        assert!(!is_hop_by_hop("Content-Type"));
        assert!(!is_hop_by_hop("Host"));
    }

    #[test]
    fn forward_client_header_drops_host_and_hop_by_hop() {
        // Host must be dropped so it isn't duplicated with the explicit
        // upstream Host set later (reqwest `.header()` appends).
        assert!(!forward_client_header("Host"));
        assert!(!forward_client_header("host"));
        assert!(!forward_client_header("HOST"));

        // Hop-by-hop headers are still dropped.
        assert!(!forward_client_header("Connection"));
        assert!(!forward_client_header("transfer-encoding"));

        // Ordinary end-to-end headers are forwarded.
        assert!(forward_client_header("Accept"));
        assert!(forward_client_header("User-Agent"));
        assert!(forward_client_header("Authorization"));
        assert!(forward_client_header("X-Amz-Date"));
    }

    #[test]
    fn method_implies_body_matches_body_carrying_verbs() {
        assert!(method_implies_body(&reqwest::Method::POST));
        assert!(method_implies_body(&reqwest::Method::PUT));
        assert!(method_implies_body(&reqwest::Method::PATCH));
        assert!(method_implies_body(&reqwest::Method::DELETE));
        assert!(!method_implies_body(&reqwest::Method::GET));
        assert!(!method_implies_body(&reqwest::Method::HEAD));
        assert!(!method_implies_body(&reqwest::Method::OPTIONS));
    }

    #[test]
    fn static_body_drains_in_chunks() {
        let mut body = ExternalBody::from_static(b"hello world");
        assert_eq!(body.len(), Some(11));

        let mut buf = [0u8; 5];
        let n = <ExternalBody as VclResponse>::read(&mut body, &mut buf).unwrap();
        assert_eq!(n, 5);
        assert_eq!(&buf[..n], b"hello");

        let mut buf = [0u8; 32];
        let n = <ExternalBody as VclResponse>::read(&mut body, &mut buf).unwrap();
        assert_eq!(n, 6);
        assert_eq!(&buf[..n], b" world");

        let n = <ExternalBody as VclResponse>::read(&mut body, &mut buf).unwrap();
        assert_eq!(n, 0);
    }

    #[test]
    fn external_backend_new_validates_inputs() {
        let bad = ExternalProxy {
            hostname: String::new(),
            port: 443,
            tls: true,
        };
        assert!(ExternalBackend::new(&bad).is_err());

        let bad_port = ExternalProxy {
            hostname: "example.com".to_string(),
            port: 0,
            tls: false,
        };
        assert!(ExternalBackend::new(&bad_port).is_err());

        let good = ExternalProxy {
            hostname: "example.com".to_string(),
            port: 443,
            tls: true,
        };
        let be = ExternalBackend::new(&good).unwrap();
        assert_eq!(be.base_url, "https://example.com:443");
        assert_eq!(be.upstream_host, "example.com");
    }
}
