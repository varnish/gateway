//! Send + Sync wrapper for Varnish BackendRef.

use varnish::vcl::BackendRef;

/// Wrapper for BackendRef that implements Send + Sync.
///
/// SAFETY: BackendRef wraps a VCL_BACKEND opaque Varnish handle designed for
/// multi-threaded use. While individual Varnish workers are single-threaded,
/// we use Arc and atomic operations because multiple workers may access the
/// same director concurrently (via shared VCL state). The raw pointer is
/// managed by Varnish's backend infrastructure which provides its own
/// synchronization guarantees.
#[derive(Debug)]
pub(crate) struct SendSyncBackendRef(pub(crate) BackendRef);

unsafe impl Send for SendSyncBackendRef {}
unsafe impl Sync for SendSyncBackendRef {}
