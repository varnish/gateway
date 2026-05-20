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

    // VRT_DelDirector, VRT_delete_backend, and VRT_Assign_Backend live in
    // libvarnishd (loaded into the varnishd process), not in libvarnishapi
    // (what we link against). For the cdylib (the actual VMOD .dylib/.so)
    // we want those symbols resolved at dlopen time by the host varnishd.
    // If we *also* provide local strong definitions, those would shadow the
    // real ones at load time (on macOS via two-level namespace, on Linux via
    // ELF link-time binding) — every backend release silently becomes a
    // no-op and the first vcl.discard crashes vcl_KillBackends() with
    //     Condition(VTAILQ_EMPTY(&vdire->directors)) not true
    //
    // So the cdylib link allows undefined symbols and libvarnishd resolves
    // them at dlopen. The unit-test binary (which has no varnishd around it)
    // gets no-op stubs from src/lib.rs's `#[cfg(test)] mod test_stubs`.
    if cfg!(target_os = "macos") {
        // macOS dyld refuses undefined symbols by default; opt in to runtime
        // resolution.
        println!("cargo::rustc-link-arg-cdylib=-undefined");
        println!("cargo::rustc-link-arg-cdylib=dynamic_lookup");
    } else {
        // Rust's cdylib target on Linux passes `-Wl,-z,defs`, which forces
        // every symbol resolved at link time. Override so the VRT_*
        // references stay undefined in the .so; accepted by both GNU ld and
        // lld (rust-lld).
        println!("cargo::rustc-link-arg-cdylib=-Wl,--unresolved-symbols=ignore-all");
    }
}
