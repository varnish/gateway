//! Response body handling for Ghost VMOD

use std::io::Read;
use varnish::vcl::{VclError, VclResponse};

/// Response body wrapper for streaming bytes to Varnish
///
/// This can either wrap a buffered `Vec<u8>` for synthetic responses,
/// or wrap a `reqwest::blocking::Response` for streaming backend responses.
pub enum ResponseBody {
    /// Pre-buffered data (for synthetic responses)
    Buffered {
        data: Vec<u8>,
        cursor: usize,
    },
    /// Streaming response from backend
    Streaming {
        reader: reqwest::blocking::Response,
        content_length: Option<usize>,
    },
}

impl ResponseBody {
    /// Create a new buffered response body from bytes (for synthetic responses)
    pub fn buffered(data: Vec<u8>) -> Self {
        ResponseBody::Buffered { data, cursor: 0 }
    }

    /// Create a streaming response body from a reqwest response
    pub fn streaming(response: reqwest::blocking::Response) -> Self {
        let content_length = response
            .content_length()
            .and_then(|len| usize::try_from(len).ok());
        ResponseBody::Streaming {
            reader: response,
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
            ResponseBody::Streaming { reader, .. } => {
                reader.read(buf).map_err(|e| {
                    VclError::new(format!("ghost: failed to read streaming response: {}", e))
                })
            }
        }
    }

    fn len(&self) -> Option<usize> {
        match self {
            ResponseBody::Buffered { data, .. } => Some(data.len()),
            ResponseBody::Streaming { content_length, .. } => *content_length,
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
