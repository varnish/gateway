fn main() {
    println!("cargo::rustc-check-cfg=cfg(varnishsys_90_sslflags)");

    // Detect Varnish version and require 9.0+ for BackendTLS support.
    // varnish-sys sets this flag in its own build.rs, but cargo::rustc-cfg
    // only applies to the crate being built, so we must set it here too.
    let version = pkg_config::Config::new()
        .probe("varnishapi")
        .expect("pkg-config failed to find varnishapi")
        .version;
    let ver = semver::Version::parse(&version)
        .unwrap_or_else(|_| panic!("varnishapi invalid version: {version}"));
    if ver < semver::Version::new(9, 0, 0) {
        panic!("Varnish {version} is not supported. Varnish 9.0+ is required for BackendTLS support.");
    }
    println!("cargo::rustc-cfg=varnishsys_90_sslflags");

    // Compile stub implementations of libvarnishd symbols that are referenced
    // by varnish-rs Drop impls but not available in libvarnishapi. These stubs
    // are needed when linking test binaries. For the cdylib target (the actual
    // VMOD .so), these symbols are resolved at runtime by libvarnishd.
    // See c_code/test_stubs.c for details.
    cc::Build::new()
        .file("c_code/test_stubs.c")
        .compile("test_stubs");
}
