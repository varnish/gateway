fn main() {
    // Compile stub implementations of libvarnishd symbols that are referenced
    // by varnish-rs Drop impls but not available in libvarnishapi. These stubs
    // are needed when linking test binaries. For the cdylib target (the actual
    // VMOD .so), these symbols are resolved at runtime by libvarnishd.
    // See c_code/test_stubs.c for details.
    cc::Build::new()
        .file("c_code/test_stubs.c")
        .compile("test_stubs");
}
