//! Response body handling for Ghost VMOD

use std::sync::Mutex;
use varnish::vcl::{VclError, VclResponse};

use crate::runtime::BodyChunk;

/// Response body wrapper for streaming bytes to Varnish
///
/// This can either wrap a buffered `Vec<u8>` for synthetic responses,
/// or stream from the async runtime via a channel.
pub enum ResponseBody {
    /// Pre-buffered data (for synthetic responses)
    Buffered {
        data: Vec<u8>,
        cursor: usize,
    },
    /// Async streaming via channel from background runtime
    AsyncStreaming {
        /// Channel receiver for body chunks (wrapped in Mutex for interior mutability)
        receiver: Mutex<Option<tokio::sync::mpsc::Receiver<BodyChunk>>>,
        /// Buffer for partially consumed chunks
        buffer: Mutex<(Vec<u8>, usize)>,
        /// Content length if known
        content_length: Option<usize>,
    },
}

impl ResponseBody {
    /// Create a new buffered response body from bytes (for synthetic responses)
    pub fn buffered(data: Vec<u8>) -> Self {
        ResponseBody::Buffered { data, cursor: 0 }
    }

    /// Create an async streaming response body from a channel receiver
    pub fn async_streaming(
        rx: tokio::sync::mpsc::Receiver<BodyChunk>,
        content_length: Option<usize>,
    ) -> Self {
        ResponseBody::AsyncStreaming {
            receiver: Mutex::new(Some(rx)),
            buffer: Mutex::new((Vec::new(), 0)),
            content_length,
        }
    }

    /// Create an empty response body
    #[allow(dead_code)]
    pub fn empty() -> Self {
        ResponseBody::Buffered {
            data: Vec::new(),
            cursor: 0,
        }
    }
}

impl VclResponse for ResponseBody {
    fn read(&mut self, buf: &mut [u8]) -> Result<usize, VclError> {
        match self {
            ResponseBody::Buffered { data, cursor } => {
                let remaining = data.len() - *cursor;
                if remaining == 0 {
                    return Ok(0);
                }

                let to_copy = std::cmp::min(remaining, buf.len());
                buf[..to_copy].copy_from_slice(&data[*cursor..*cursor + to_copy]);
                *cursor += to_copy;

                Ok(to_copy)
            }
            ResponseBody::AsyncStreaming {
                receiver, buffer, ..
            } => {
                // First, try to serve from the buffer
                {
                    let buf_guard = buffer.get_mut().unwrap();
                    let (ref data, ref mut cursor) = *buf_guard;
                    let remaining = data.len() - *cursor;
                    if remaining > 0 {
                        let to_copy = std::cmp::min(remaining, buf.len());
                        buf[..to_copy].copy_from_slice(&data[*cursor..*cursor + to_copy]);
                        *cursor += to_copy;
                        return Ok(to_copy);
                    }
                }

                // Buffer exhausted, try to receive more data
                let rx_guard = receiver.get_mut().unwrap();
                if let Some(rx) = rx_guard.as_mut() {
                    // Blocking receive from async channel
                    match rx.blocking_recv() {
                        Some(Ok(chunk)) => {
                            if chunk.is_empty() {
                                return Ok(0);
                            }
                            let to_copy = std::cmp::min(chunk.len(), buf.len());
                            buf[..to_copy].copy_from_slice(&chunk[..to_copy]);

                            // Store remainder in buffer if chunk is larger than buf
                            if chunk.len() > buf.len() {
                                let buf_guard = buffer.get_mut().unwrap();
                                *buf_guard = (chunk, to_copy);
                            }

                            Ok(to_copy)
                        }
                        Some(Err(e)) => {
                            Err(VclError::new(format!("ghost: stream error: {}", e)))
                        }
                        None => {
                            // Stream ended
                            *rx_guard = None;
                            Ok(0)
                        }
                    }
                } else {
                    // Already exhausted
                    Ok(0)
                }
            }
        }
    }

    fn len(&self) -> Option<usize> {
        match self {
            ResponseBody::Buffered { data, .. } => Some(data.len()),
            ResponseBody::AsyncStreaming { content_length, .. } => *content_length,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_buffered_response_body_read_all() {
        let mut body = ResponseBody::buffered(b"hello world".to_vec());
        let mut buf = [0u8; 100];

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 11);
        assert_eq!(&buf[..n], b"hello world");

        // Second read should return 0
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 0);
    }

    #[test]
    fn test_buffered_response_body_read_chunks() {
        let mut body = ResponseBody::buffered(b"hello world".to_vec());
        let mut buf = [0u8; 5];

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 5);
        assert_eq!(&buf[..n], b"hello");

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 5);
        assert_eq!(&buf[..n], b" worl");

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 1);
        assert_eq!(&buf[..n], b"d");

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 0);
    }

    #[test]
    fn test_buffered_response_body_len() {
        let body = ResponseBody::buffered(b"hello".to_vec());
        assert_eq!(body.len(), Some(5));

        let empty = ResponseBody::empty();
        assert_eq!(empty.len(), Some(0));
    }
}
