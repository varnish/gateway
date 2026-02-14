//! Synthetic 500 backend for matched routes with no backends
//!
//! This backend generates 500 responses when a request matches a route but
//! no backends are available (e.g., invalid backendRef in the HTTPRoute).

use varnish::vcl::{Ctx, VclBackend, VclError, VclResponse};

/// Backend that generates synthetic 500 responses
pub struct InternalErrorBackend;

impl VclBackend<InternalErrorBody> for InternalErrorBackend {
    fn get_response(&self, ctx: &mut Ctx) -> Result<Option<InternalErrorBody>, VclError> {
        let beresp = ctx.http_beresp.as_mut().unwrap();
        beresp.set_status(500);
        beresp.set_header("Content-Type", "application/json")?;

        Ok(Some(InternalErrorBody::new()))
    }
}

/// Response body for 500 error
pub struct InternalErrorBody {
    data: &'static [u8],
    cursor: usize,
}

impl InternalErrorBody {
    /// Create a new 500 response body
    pub fn new() -> Self {
        Self {
            data: b"{\"error\": \"no backends available\"}",
            cursor: 0,
        }
    }
}

impl VclResponse for InternalErrorBody {
    fn read(&mut self, buf: &mut [u8]) -> Result<usize, VclError> {
        let remaining = &self.data[self.cursor..];
        let to_copy = remaining.len().min(buf.len());

        buf[..to_copy].copy_from_slice(&remaining[..to_copy]);
        self.cursor += to_copy;

        Ok(to_copy)
    }

    fn len(&self) -> Option<usize> {
        Some(self.data.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_internal_error_body_new() {
        let body = InternalErrorBody::new();
        assert_eq!(body.cursor, 0);
    }

    #[test]
    fn test_internal_error_body_len() {
        let body = InternalErrorBody::new();
        assert_eq!(body.len(), Some(34));
    }

    #[test]
    fn test_internal_error_body_read() {
        let mut body = InternalErrorBody::new();
        let mut buf = vec![0u8; 100];

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 34);
        assert_eq!(&buf[..n], b"{\"error\": \"no backends available\"}");

        // Second read should return 0 (EOF)
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 0);
    }
}
