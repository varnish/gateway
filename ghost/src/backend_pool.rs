//! Backend management for Ghost VMOD
//!
//! This module provides utilities for creating and managing native Varnish backends.
//! Backends are stored per-director instance (not globally) due to thread safety constraints.

use std::collections::HashMap;
use std::net::SocketAddr;

use varnish::vcl::{Ctx, Endpoint, NativeBackend, NativeBackendConfig, VclError};

/// Backend pool for a single director instance
///
/// Each director owns its own backends. Backends are indexed by "address:port"
/// and reused across config reloads within the same director.
pub struct BackendPool {
    backends: HashMap<String, NativeBackend>,
}

// SAFETY: VCL_BACKEND pointers are thread-safe in Varnish's model.
// They are designed to be used from multiple worker threads concurrently.
// The raw pointer is just an opaque handle managed by Varnish's backend infrastructure.
unsafe impl Send for BackendPool {}
unsafe impl Sync for BackendPool {}

impl BackendPool {
    /// Create a new empty backend pool
    pub fn new() -> Self {
        Self {
            backends: HashMap::new(),
        }
    }

    /// Get or create a backend in the pool
    ///
    /// Returns the backend key ("address:port"). If the backend already exists,
    /// returns the existing one. Otherwise creates a new native backend and adds it to the pool.
    ///
    /// # Arguments
    /// * `ctx` - VCL context (needed for backend creation)
    /// * `address` - IP address as a string
    /// * `port` - Port number
    ///
    /// # Returns
    /// Backend key string for looking up in the pool
    pub fn get_or_create(
        &mut self,
        ctx: &mut Ctx,
        address: &str,
        port: u16,
    ) -> Result<String, VclError> {
        let key = format!("{}:{}", address, port);

        // Check if backend already exists
        if self.backends.contains_key(&key) {
            return Ok(key);
        }

        // Parse IP address
        let ip: std::net::IpAddr = address
            .parse()
            .map_err(|e| VclError::new(format!("Invalid IP address '{}': {}", address, e)))?;

        // Create socket address
        let addr = SocketAddr::new(ip, port);

        // Create endpoint
        let endpoint = Endpoint::ip(addr);

        // Create backend config with a descriptive name
        let backend_name = format!("ghost_{}", key.replace([':', '.'], "_"));
        let config = NativeBackendConfig::new(&backend_name, endpoint);

        // Create native backend
        let backend = NativeBackend::new(ctx, &config, None)?;

        // Insert into pool
        self.backends.insert(key.clone(), backend);

        Ok(key)
    }

    /// Look up a backend in the pool by key
    ///
    /// Returns None if the backend doesn't exist (shouldn't happen in normal use).
    pub fn get(&self, key: &str) -> Option<&NativeBackend> {
        self.backends.get(key)
    }

    /// Get the number of backends in the pool (for diagnostics)
    pub fn len(&self) -> usize {
        self.backends.len()
    }

    /// Check if the pool is empty
    #[allow(dead_code)]
    pub fn is_empty(&self) -> bool {
        self.backends.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_backend_key_format() {
        let key = format!("{}:{}", "10.0.0.1", 8080);
        assert_eq!(key, "10.0.0.1:8080");
    }

    #[test]
    fn test_backend_name_format() {
        let key = "10.0.0.1:8080";
        let name = format!("ghost_{}", key.replace([':', '.'], "_"));
        assert_eq!(name, "ghost_10_0_0_1_8080");
    }

    #[test]
    fn test_ipv6_backend_key() {
        let key = format!("{}:{}", "::1", 8080);
        assert_eq!(key, "::1:8080");
    }

    #[test]
    fn test_backend_pool_creation() {
        let pool = BackendPool::new();
        assert_eq!(pool.len(), 0);
        assert!(pool.is_empty());
    }
}
