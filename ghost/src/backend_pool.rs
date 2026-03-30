//! Backend management for Ghost VMOD
//!
//! This module provides utilities for creating and managing native Varnish backends.
//! Backends are stored per-director instance (not globally) due to thread safety constraints.

use std::collections::HashMap;
use std::ffi::CString;
use std::net::SocketAddr;
use std::sync::Arc;

use crate::config::BackendTLS;
use varnish::vcl::{Ctx, NativeBackend, NativeBackendBuilder, VclError};

/// Backend pool for a single director instance
///
/// Each director owns its own backends. Backends are indexed by "address:port"
/// and reused across config reloads within the same director.
/// Backends are wrapped in Arc for efficient cloning during hot-reload.
#[derive(Clone, Debug)]
pub struct BackendPool {
    backends: HashMap<String, Arc<NativeBackend>>,
}

// SAFETY: NativeBackend wraps VCL_BACKEND pointers which are thread-safe in Varnish.
// Multiple worker threads can concurrently access backends through shared VCL state.
// The raw pointer is an opaque handle managed by Varnish's backend infrastructure
// with its own synchronization guarantees.
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
    /// Returns the backend key. If the backend already exists,
    /// returns the existing one. Otherwise creates a new native backend and adds it to the pool.
    /// TLS and non-TLS backends for the same address:port are stored separately.
    ///
    /// # Arguments
    /// * `ctx` - VCL context (needed for backend creation)
    /// * `address` - IP address as a string
    /// * `port` - Port number
    /// * `tls` - Optional TLS configuration from BackendTLSPolicy
    ///
    /// # Returns
    /// Backend key string for looking up in the pool
    pub fn get_or_create(
        &mut self,
        ctx: &mut Ctx,
        address: &str,
        port: u16,
        tls: Option<&BackendTLS>,
    ) -> Result<String, VclError> {
        let key = match tls {
            Some(t) => format!("{}:{}:tls:{}", address, port, t.hostname),
            None => format!("{}:{}", address, port),
        };

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

        // Create backend with builder
        let backend_name = format!("ghost_{}", key.replace([':', '.', '/'], "_"));
        let c_name = CString::new(backend_name)
            .map_err(|e| VclError::new(format!("Invalid backend name: {}", e)))?;
        let builder = NativeBackendBuilder::new_ip(&c_name, addr);

        // Configure TLS if BackendTLSPolicy applies to this backend.
        // Uses a Cargo patch for varnish-sys to fix .tls() return type.
        // TODO: Remove patch once varnish-sys > 0.6.0 is released with the fix.
        // hostname_cstr must outlive builder since hosthdr() borrows it.
        #[cfg(varnishsys_90_sslflags)]
        let hostname_cstr = tls
            .as_ref()
            .map(|t| CString::new(t.hostname.as_str()))
            .transpose()
            .map_err(|e| VclError::new(format!("Invalid TLS hostname: {}", e)))?;
        #[cfg(varnishsys_90_sslflags)]
        let builder = if tls.is_some() {
            let builder = builder.hosthdr(hostname_cstr.as_ref().unwrap());
            builder.tls(true, true)
        } else {
            builder
        };
        #[cfg(not(varnishsys_90_sslflags))]
        if tls.is_some() {
            return Err(VclError::new(
                "BackendTLS requires Varnish 9.0+ (varnishsys_90_sslflags)".to_string(),
            ));
        }
        let backend = builder.build(ctx)?;

        // Insert into pool (wrapped in Arc)
        // SAFETY: NativeBackend contains VCL_BACKEND pointers which are thread-safe
        // in Varnish's model. See BackendPool's Send+Sync impl for details.
        #[allow(clippy::arc_with_non_send_sync)]
        self.backends.insert(key.clone(), Arc::new(backend));

        Ok(key)
    }

    /// Look up a backend in the pool by key
    ///
    /// Returns None if the backend doesn't exist (shouldn't happen in normal use).
    pub fn get(&self, key: &str) -> Option<Arc<NativeBackend>> {
        self.backends.get(key).cloned()
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

    /// Remove all backends except those in the provided set of keys
    ///
    /// This is used during config reload to clean up backends that are
    /// no longer referenced in the routing state.
    pub fn retain_only(&mut self, keys_to_keep: &std::collections::HashSet<String>) {
        self.backends
            .retain(|key, _backend| keys_to_keep.contains(key));
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
