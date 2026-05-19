//! Statistics tracking for vhost directors.
//!
//! Provides per-vhost request counters for observability.
//! Stats are reset on config reload since they're tied to the current routing state.

use parking_lot::RwLock;
use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::SystemTime;

/// Statistics for a single vhost director
#[derive(Debug)]
pub struct VhostStats {
    /// Number of backend selections per backend key
    pub backend_selections: RwLock<HashMap<String, u64>>,
    /// Total requests handled by this vhost
    pub total_requests: AtomicU64,
    /// Timestamp of last request
    pub last_request: RwLock<Option<SystemTime>>,
}

impl VhostStats {
    /// Create new empty stats
    pub fn new() -> Self {
        Self {
            backend_selections: RwLock::new(HashMap::new()),
            total_requests: AtomicU64::new(0),
            last_request: RwLock::new(None),
        }
    }

    /// Record a request and backend selection
    pub fn record_request(&self, backend_key: &str) {
        // Increment total requests
        self.total_requests.fetch_add(1, Ordering::Relaxed);

        // Update last request time
        *self.last_request.write() = Some(SystemTime::now());

        // Increment backend selection counter
        let mut selections = self.backend_selections.write();
        *selections.entry(backend_key.to_string()).or_insert(0) += 1;
    }

    /// Get total requests handled
    pub fn total_requests(&self) -> u64 {
        self.total_requests.load(Ordering::Relaxed)
    }

    /// Get backend selections (cloned snapshot)
    pub fn backend_selections(&self) -> HashMap<String, u64> {
        self.backend_selections.read().clone()
    }

    /// Get last request time
    pub fn last_request(&self) -> Option<SystemTime> {
        *self.last_request.read()
    }
}

impl Default for VhostStats {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_vhost_stats_new() {
        let stats = VhostStats::new();
        assert_eq!(stats.total_requests(), 0);
        assert!(stats.last_request().is_none());
        assert_eq!(stats.backend_selections().len(), 0);
    }

    #[test]
    fn test_vhost_stats_record_request() {
        let stats = VhostStats::new();

        stats.record_request("10.0.0.1:8080");
        assert_eq!(stats.total_requests(), 1);
        assert!(stats.last_request().is_some());

        let selections = stats.backend_selections();
        assert_eq!(selections.get("10.0.0.1:8080"), Some(&1));
    }

    #[test]
    fn test_vhost_stats_multiple_backends() {
        let stats = VhostStats::new();

        stats.record_request("10.0.0.1:8080");
        stats.record_request("10.0.0.2:8080");
        stats.record_request("10.0.0.1:8080");

        assert_eq!(stats.total_requests(), 3);

        let selections = stats.backend_selections();
        assert_eq!(selections.get("10.0.0.1:8080"), Some(&2));
        assert_eq!(selections.get("10.0.0.2:8080"), Some(&1));
    }
}
