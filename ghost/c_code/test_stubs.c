/*
 * Stub implementations of libvarnishd symbols for test linking.
 *
 * These symbols (VRT_DelDirector, VRT_delete_backend, VRT_Assign_Backend)
 * are defined in libvarnishd but NOT in libvarnishapi. When building test
 * binaries (cargo test --lib), the linker needs them resolved even though
 * they're never called during unit tests (no Varnish runtime context exists).
 *
 * In production, the real VMOD shared library (.so) is loaded into varnishd
 * which provides these symbols at runtime.
 */

#include <stddef.h>

/* VCL_BACKEND is const struct director* */
typedef const struct director *VCL_BACKEND;

void VRT_DelDirector(VCL_BACKEND *bp)
{
	(void)bp;
}

void VRT_delete_backend(const void *ctx, VCL_BACKEND *bp)
{
	(void)ctx;
	(void)bp;
}

void VRT_Assign_Backend(VCL_BACKEND *dst, VCL_BACKEND src)
{
	(void)dst;
	(void)src;
}
