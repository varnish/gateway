//! Response body handling for Ghost VMOD

use varnish::vcl::{VclError, VclResponse};

/// Response body wrapper for streaming bytes to Varnish
pub struct ResponseBody {
    data: Vec<u8>,
    cursor: usize,
}

impl ResponseBody {
    /// Create a new response body from bytes
    pub fn new(data: Vec<u8>) -> Self {
        ResponseBody { data, cursor: 0 }
    }

    /// Create an empty response body
    #[allow(dead_code)]
    pub fn empty() -> Self {
        ResponseBody {
            data: Vec::new(),
            cursor: 0,
        }
    }
}

impl VclResponse for ResponseBody {
    fn read(&mut self, buf: &mut [u8]) -> Result<usize, VclError> {
        let remaining = self.data.len() - self.cursor;
        if remaining == 0 {
            return Ok(0);
        }

        let to_copy = std::cmp::min(remaining, buf.len());
        buf[..to_copy].copy_from_slice(&self.data[self.cursor..self.cursor + to_copy]);
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
    fn test_response_body_read_all() {
        let mut body = ResponseBody::new(b"hello world".to_vec());
        let mut buf = [0u8; 100];

        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 11);
        assert_eq!(&buf[..n], b"hello world");

        // Second read should return 0
        let n = body.read(&mut buf).unwrap();
        assert_eq!(n, 0);
    }

    #[test]
    fn test_response_body_read_chunks() {
        let mut body = ResponseBody::new(b"hello world".to_vec());
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
    fn test_response_body_len() {
        let body = ResponseBody::new(b"hello".to_vec());
        assert_eq!(body.len(), Some(5));

        let empty = ResponseBody::empty();
        assert_eq!(empty.len(), Some(0));
    }
}
