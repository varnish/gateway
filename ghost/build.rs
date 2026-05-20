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
    // libvarnishd (which is loaded into the varnishd process), not in
    // libvarnishapi (what we link against at compile time). For the cdylib
    // target (the actual VMOD .dylib/.so) we want those symbols resolved at
    // dlopen time by the host varnishd. If we *also* compile and link the
    // stubs from c_code/test_stubs.c into the cdylib, those local definitions
    // shadow the real ones at load time (on macOS the dylib's own symbols
    // take precedence in two-level namespace, and even on ELF a local strong
    // definition wins). Every BackendRef::drop / Backend::drop then becomes
    // a silent no-op and refcounts only ever go up — which crashes
    // vcl_KillBackends() on the first VCL discard with
    //     Condition(VTAILQ_EMPTY(&vdire->directors)) not true
    //
    // We only need the stubs for the `lib`-style unit tests (`cargo test
    // --lib`), where no varnishd is in the picture to provide the symbols.
    // VTC integration tests use the real cdylib loaded into varnishd and
    // don't need the stubs.
    //
    // PROFILE is set by cargo and is `debug` for `cargo build` / `cargo
    // test`, and `release` for `cargo build --release` / VTC test runs.
    // For cdylib builds we tell the linker to allow undefined symbols so
    // the link succeeds without the stubs; they're resolved by varnishd
    // at runtime.
    let linking_test_bin = std::env::var("CARGO_CFG_TEST").is_ok();
    if linking_test_bin {
        cc::Build::new()
            .file("c_code/test_stubs.c")
            .compile("test_stubs");
    } else {
        // Allow undefined VRT_* symbols in the cdylib; libvarnishd resolves
        // them at dlopen time inside varnishd.
        if cfg!(target_os = "macos") {
            // macOS dyld refuses undefined symbols by default; opt in to
            // runtime resolution.
            println!("cargo::rustc-link-arg-cdylib=-undefined");
            println!("cargo::rustc-link-arg-cdylib=dynamic_lookup");
        } else {
            // Rust's cdylib target on Linux passes `-Wl,-z,defs`, which
            // forces every symbol resolved at link time. Override so the
            // VRT_* references stay undefined in the .so and dlopen
            // resolves them against the host varnishd. This flag is
            // accepted by both GNU ld and lld (rust-lld).
            println!(
                "cargo::rustc-link-arg-cdylib=-Wl,--unresolved-symbols=ignore-all"
            );
        }
    }
}
