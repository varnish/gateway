//! Background async runtime for connection pooling
//!
//! This module provides a tokio-based background runtime that handles HTTP requests
//! asynchronously while allowing Varnish worker threads to block-wait for responses.
//! The key benefit is that the async reqwest::Client maintains proper connection pools
//! that survive across requests and config reloads.

use std::time::Duration;
use tokio::runtime::Runtime;
use tokio::sync::mpsc::{unbounded_channel, UnboundedReceiver, UnboundedSender};
use tokio::sync::oneshot;

/// Request to be processed by the background runtime
pub struct HttpRequest {
    pub method: reqwest::Method,
    pub url: String,
    pub headers: Vec<(String, String)>,
    pub response_tx: oneshot::Sender<HttpResult>,
}

/// Result of an HTTP request
pub type HttpResult = Result<HttpResponse, String>;

/// Response from the background runtime
pub struct HttpResponse {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body_rx: tokio::sync::mpsc::Receiver<BodyChunk>,
}

/// A chunk of body data or end-of-stream
pub type BodyChunk = Result<Vec<u8>, String>;

/// Background thread holding the tokio runtime and request channel
///
/// This struct is shared across all ghost backends in a VCL via `#[shared_per_vcl]`.
/// It contains the tokio runtime and the channel sender for submitting HTTP requests.
pub struct BgThread {
    /// The tokio runtime (kept alive for the lifetime of the VCL)
    #[allow(dead_code)]
    rt: Runtime,
    /// Channel sender for submitting HTTP requests to the background runtime
    pub sender: UnboundedSender<HttpRequest>,
}

impl BgThread {
    /// Create a new background thread with tokio runtime
    pub fn new() -> Result<Self, String> {
        let rt = Runtime::new()
            .map_err(|e| format!("failed to create tokio runtime: {}", e))?;

        let (sender, receiver) = unbounded_channel::<HttpRequest>();

        let client = reqwest::Client::builder()
            .pool_max_idle_per_host(32)
            .pool_idle_timeout(Duration::from_secs(90))
            .tcp_keepalive(Duration::from_secs(60))
            .connect_timeout(Duration::from_secs(5))
            .timeout(Duration::from_secs(30))
            .build()
            .map_err(|e| format!("failed to create HTTP client: {}", e))?;

        // Spawn the request processing loop on the runtime
        rt.spawn(request_loop(receiver, client));

        Ok(BgThread { rt, sender })
    }
}

/// Main loop that processes incoming requests
async fn request_loop(mut receiver: UnboundedReceiver<HttpRequest>, client: reqwest::Client) {
    while let Some(req) = receiver.recv().await {
        let client = client.clone();
        tokio::spawn(async move {
            process_request(client, req).await;
        });
    }
}

/// Process a single HTTP request
async fn process_request(client: reqwest::Client, req: HttpRequest) {
    let mut builder = client.request(req.method, &req.url);

    for (name, value) in req.headers {
        builder = builder.header(name, value);
    }

    let result = builder.send().await;

    match result {
        Ok(response) => {
            let status = response.status().as_u16();
            let headers: Vec<_> = response
                .headers()
                .iter()
                .filter_map(|(k, v)| v.to_str().ok().map(|v| (k.to_string(), v.to_string())))
                .collect();

            // Create channel for streaming body
            let (body_tx, body_rx) = tokio::sync::mpsc::channel(16);

            let http_response = HttpResponse {
                status,
                headers,
                body_rx,
            };

            // Send response metadata first
            if req.response_tx.send(Ok(http_response)).is_err() {
                return; // Receiver dropped, abort
            }

            // Stream body chunks
            use futures_util::StreamExt;
            let mut stream = response.bytes_stream();
            while let Some(chunk_result) = stream.next().await {
                match chunk_result {
                    Ok(chunk) => {
                        if body_tx.send(Ok(chunk.to_vec())).await.is_err() {
                            break; // Receiver dropped
                        }
                    }
                    Err(e) => {
                        let _ = body_tx.send(Err(e.to_string())).await;
                        break;
                    }
                }
            }
            // Channel closes when body_tx is dropped
        }
        Err(e) => {
            let _ = req.response_tx.send(Err(e.to_string()));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_bgthread_creation() {
        let bg = BgThread::new();
        assert!(bg.is_ok(), "BgThread should be created successfully");
    }
}
