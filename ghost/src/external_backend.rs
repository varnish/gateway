//! Synthetic backend for Kubernetes ExternalName Services.
//!
//! Modelled on `vmod-reqwest`: one synthetic Varnish backend (with stable
//! VBE stats) fronts a `reqwest::Client` whose connection pool and DNS
//! resolver hide rotating cloud IPs from Varnish.

use std::io::Write;
use std::sync::OnceLock;
use std::time::Duration;

use bytes::Bytes;
use reqwest::header::HeaderName;
use reqwest::Client;
use tokio::runtime::Runtime;
use tokio::sync::mpsc::{Receiver, Sender};
use varnish::vcl::{Ctx, StrOrBytes, VclBackend, VclError, VclResponse};

use crate::config::ExternalProxy;

const DEFAULT_REQUEST_TIMEOUT: Duration = Duration::from_secs(60);
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// Per-stream chunk channel size. Roughly bounds in-flight buffered bytes
/// per response to `CHUNK_CHANNEL_SIZE * reqwest_chunk_size` (~512KB at
/// reqwest's 16KB default), giving backpressure without starving the stream.
const CHUNK_CHANNEL_SIZE: usize = 32;

/// Background tokio runtime shared by all external-proxy backends.
struct BgThread {
    /// Held only for its destructor — dropping it stops the runtime.
    #[allow(dead_code)]
    rt: Runtime,
}

enum RespMsg {
    Headers(HeadersFrame),
    Chunk(Bytes),
    Err(String),
}

struct HeadersFrame {
    status: u16,
    headers: reqwest::header::HeaderMap,
    content_length: Option<u64>,
}

static BG_THREAD: OnceLock<BgThread> = OnceLock::new();

fn bgt() -> &'static BgThread {
    BG_THREAD.get_or_init(|| {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .thread_name("ghost-external-proxy")
            .build()
            .expect("ghost: failed to start tokio runtime for external proxy");
        BgThread { rt }
    })
}

/// Warm the shared tokio runtime so the first request doesn't pay startup cost.
pub fn warm_runtime() {
    let _ = bgt();
}

async fn process_request(client: Client, request: reqwest::Request, resp_tx: Sender<RespMsg>) {
    let mut resp = match client.execute(request).await {
        Ok(r) => r,
        Err(e) => {
            let _ = resp_tx.send(RespMsg::Err(format!("external proxy: {}", e))).await;
            return;
        }
    };

    let frame = HeadersFrame {
        status: resp.status().as_u16(),
        headers: resp.headers().clone(),
        content_length: resp.content_length(),
    };
    if resp_tx.send(RespMsg::Headers(frame)).await.is_err() {
        return;
    }

    loop {
        match resp.chunk().await {
            Ok(None) => return,
            Ok(Some(bytes)) => {
                if resp_tx.send(RespMsg::Chunk(bytes)).await.is_err() {
                    return;
                }
            }
            Err(e) => {
                let _ = resp_tx
                    .send(RespMsg::Err(format!("external proxy chunk: {}", e)))
                    .await;
                return;
            }
        }
    }
}

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
            return Err(VclError::new("external_proxy: hostname is empty".to_string()));
        }
        if proxy.port == 0 {
            return Err(VclError::new("external_proxy: port is zero".to_string()));
        }

        let scheme = if proxy.tls { "https" } else { "http" };
        let base_url = format!("{}://{}:{}", scheme, proxy.hostname, proxy.port);

        // Auto-decompression is intentionally not enabled (feature not
        // compiled in) so proxied bytes pass through unmodified and Varnish
        // can cache the wire representation.
        let client = reqwest::ClientBuilder::new()
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
        // Extract method/path up front and drop the bereq borrow before we
        // need ctx mutably for logging or beresp.
        let (method_str, path, headers_owned) = {
            let bereq = ctx
                .http_bereq
                .as_ref()
                .ok_or_else(|| VclError::new("external_proxy: missing bereq".to_string()))?;
            let m = sob_to_str(bereq.method())?.to_string();
            let p = sob_to_str(bereq.url())?.to_string();
            let headers: Vec<(String, Vec<u8>)> = bereq
                .into_iter()
                .filter(|(k, _)| !is_hop_by_hop(k))
                .map(|(k, v)| (k.to_string(), v.as_ref().to_vec()))
                .collect();
            (m, p, headers)
        };

        let method = reqwest::Method::from_bytes(method_str.as_bytes())
            .map_err(|e| VclError::new(format!("external_proxy: invalid method: {}", e)))?;

        // Request bodies on bereq aren't forwarded yet (GET/HEAD/OPTIONS are
        // the documented ExternalName use case — object-store reads). Surface
        // the limitation rather than silently dropping bytes.
        if matches!(
            method,
            reqwest::Method::POST
                | reqwest::Method::PUT
                | reqwest::Method::PATCH
                | reqwest::Method::DELETE
        ) {
            ctx.log(
                varnish::vcl::LogTag::Error,
                format!(
                    "external_proxy: request body for {} not forwarded; upstream will see empty body",
                    method
                ),
            );
        }

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

        let request = req_builder
            .build()
            .map_err(|e| VclError::new(format!("external_proxy: build request: {}", e)))?;

        let (tx, mut rx) = tokio::sync::mpsc::channel::<RespMsg>(CHUNK_CHANNEL_SIZE);
        bgt().rt.spawn(process_request(self.client.clone(), request, tx));

        let headers_frame = match rx.blocking_recv() {
            Some(RespMsg::Headers(f)) => f,
            Some(RespMsg::Err(e)) => return Err(VclError::new(e)),
            // process_request always emits Headers exactly once before any
            // Chunk and never returns None before sending something.
            Some(RespMsg::Chunk(_)) | None => {
                return Err(VclError::new(
                    "external_proxy: response stream invariant violated".to_string(),
                ))
            }
        };

        let beresp = ctx
            .http_beresp
            .as_mut()
            .ok_or_else(|| VclError::new("external_proxy: missing beresp".to_string()))?;
        beresp.set_status(headers_frame.status);
        beresp.set_proto("HTTP/1.1")?;
        for (k, v) in headers_frame.headers.iter() {
            if is_hop_by_hop(k.as_str()) {
                continue;
            }
            if let Ok(s) = v.to_str() {
                beresp.set_header(k.as_str(), s)?;
            }
        }

        Ok(Some(ExternalBody {
            chan: rx,
            current: None,
            cursor: 0,
            content_length: headers_frame.content_length.map(|c| c as usize),
        }))
    }
}

/// Streaming response body that pulls chunks from the tokio runtime.
pub struct ExternalBody {
    chan: Receiver<RespMsg>,
    current: Option<Bytes>,
    cursor: usize,
    content_length: Option<usize>,
}

impl VclResponse for ExternalBody {
    fn read(&mut self, mut buf: &mut [u8]) -> Result<usize, VclError> {
        let mut total = 0;
        loop {
            if self.current.is_none() {
                match self.chan.blocking_recv() {
                    Some(RespMsg::Chunk(bytes)) => {
                        self.current = Some(bytes);
                        self.cursor = 0;
                    }
                    Some(RespMsg::Err(e)) => return Err(VclError::new(e)),
                    None => return Ok(total),
                    // process_request only emits Headers once, before chunks.
                    Some(RespMsg::Headers(_)) => {
                        return Err(VclError::new(
                            "external_proxy: response stream invariant violated".to_string(),
                        ));
                    }
                }
            }
            let chunk = self.current.as_ref().unwrap();
            let remaining = &chunk[self.cursor..];
            let n = buf
                .write(remaining)
                .map_err(|e| VclError::new(format!("external_proxy: body write: {}", e)))?;
            self.cursor += n;
            total += n;
            if self.cursor >= chunk.len() {
                self.current = None;
            }
            if buf.is_empty() {
                return Ok(total);
            }
        }
    }

    fn len(&self) -> Option<usize> {
        self.content_length
    }
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
    "trailers",
    "transfer-encoding",
    "upgrade",
];

fn is_hop_by_hop(name: &str) -> bool {
    HOP_BY_HOP.iter().any(|h| name.eq_ignore_ascii_case(h))
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
