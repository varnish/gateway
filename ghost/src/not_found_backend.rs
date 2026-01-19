//! Synthetic 404 backend for undefined vhosts
//!
//! This backend generates 404 responses when a request's Host header doesn't
//! match any configured vhost. It uses the synthetic backend pattern to avoid
//! conflicts with user VCL error handlers.

use varnish::vcl::{Ctx, VclBackend, VclError, VclResponse};

/// Backend that generates synthetic 404 responses
pub struct NotFoundBackend;

impl VclBackend<NotFoundBody> for NotFoundBackend {
    fn get_response(&self, ctx: &mut Ctx) -> Result<Option<NotFoundBody>, VclError> {
        let beresp = ctx.http_beresp.as_mut().unwrap();
        beresp.set_status(404);
        beresp.set_header("Content-Type", "application/json")?;

        Ok(Some(NotFoundBody::new()))
    }
}

/// Response body for 404 error
pub struct NotFoundBody {
    data: &'static [u8],
    cursor: usize,
}

impl NotFoundBody {
    /// Create a new 404 response body
    pub fn new() -> Self {
        Self {
            data: b"{\"error\": \"vhost not found\"}",
            cursor: 0,
        }
    }
}

impl VclResponse for NotFoundBody {
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
    fn test_not_found_body_new() {
        let body = NotFoundBody::new();
        assert_eq!(body.cursor, 0);
        assert_eq!(body.data, b"{\"error\": \"vhost not found\"}");
    }

    #[test]
    fn test_not_found_body_len() {
        let body = NotFoundBody::new();
        assert_eq!(body.len(), Some(28));
    }

    #[test]
    fn test_not_found_body_read() {
        let mut body = NotFoundBody::new();
        let mut buf = vec![0u8; 100];

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 28);
        assert_eq!(&buf[..n], b"{\"error\": \"vhost not found\"}");

        // Second read should return 0 (EOF)
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 0);
    }

    #[test]
    fn test_not_found_body_read_partial() {
        let mut body = NotFoundBody::new();
        let mut buf = vec![0u8; 10];

        // First read - partial
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 10);
        assert_eq!(&buf[..n], b"{\"error\": ");

        // Second read - continue
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 10);
        assert_eq!(&buf[..n], b"\"vhost not");

        // Third read - finish (only 8 bytes remaining)
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 8);
        assert_eq!(&buf[..n], b" found\"}");

        // Fourth read - EOF
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 0);
    }
}
