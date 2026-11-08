//! Guard against running the VTC suite against a stale VMOD.
//!
//! `cargo test` builds the `rlib` and the test harness, but **not** the
//! `cdylib` — that artifact only comes from `cargo build`. `run_vtc_tests!`
//! resolves the VMOD at runtime through the platform's dylib search path, so
//! `cargo test --release vtc_` happily loads whatever `cargo build --release`
//! left behind last time, however old.
//!
//! Both failure modes are bad, and the quiet one is worse: a stale build that
//! still passes tells you the code you just wrote works. This module turns
//! that into a loud, named failure. See issue #121.

use std::env::consts::{DLL_PREFIX, DLL_SUFFIX};
use std::fs;
use std::path::{Path, PathBuf};
use std::time::SystemTime;

/// Files whose change means the VMOD has to be rebuilt. `tests/*.vtc` is
/// deliberately absent: the harness reads those at run time.
fn newest_source_mtime(manifest_dir: &Path) -> Option<(PathBuf, SystemTime)> {
    let mut newest: Option<(PathBuf, SystemTime)> = None;

    let mut consider = |path: PathBuf| {
        let Ok(mtime) = fs::metadata(&path).and_then(|m| m.modified()) else {
            return;
        };
        if newest.as_ref().is_none_or(|(_, best)| mtime > *best) {
            newest = Some((path, mtime));
        }
    };

    for name in ["build.rs", "Cargo.toml", "Cargo.lock"] {
        consider(manifest_dir.join(name));
    }

    let mut dirs = vec![manifest_dir.join("src")];
    while let Some(dir) = dirs.pop() {
        let Ok(entries) = fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                dirs.push(path);
            } else if path.extension().is_some_and(|e| e == "rs") {
                consider(path);
            }
        }
    }

    newest
}

/// The search path cargo exports for test binaries, named per platform.
fn dylib_search_path() -> String {
    let var = if cfg!(target_os = "macos") {
        "DYLD_FALLBACK_LIBRARY_PATH"
    } else {
        "LD_LIBRARY_PATH"
    };
    std::env::var(var).unwrap_or_default()
}

/// Resolve the VMOD exactly as the generated VTC tests do, so the guard can
/// never check a different file than the one varnishtest will load.
fn resolve_vmod() -> Result<PathBuf, String> {
    let lib_name = format!("{DLL_PREFIX}{}{DLL_SUFFIX}", env!("CARGO_PKG_NAME"));
    varnish::varnishtest::find_vmod_lib(&lib_name, &dylib_search_path())
}

#[test]
fn vtc_vmod_is_not_stale() {
    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));

    let vmod = match resolve_vmod() {
        Ok(path) => path,
        Err(err) => panic!(
            "{err}\n\n\
             The VTC suite needs a built VMOD, and `cargo test` does not build one.\n\
             Run `cargo build --release` first, or use `make test-ghost`."
        ),
    };

    let vmod_mtime = fs::metadata(&vmod)
        .and_then(|m| m.modified())
        .unwrap_or_else(|e| panic!("cannot stat {}: {e}", vmod.display()));

    let Some((newest_src, src_mtime)) = newest_source_mtime(&manifest_dir) else {
        // No readable sources is not this test's problem to report.
        return;
    };

    if vmod_mtime >= src_mtime {
        return;
    }

    let behind = src_mtime
        .duration_since(vmod_mtime)
        .map(|d| format_age(d.as_secs()))
        .unwrap_or_else(|_| "an unknown amount of time".to_string());

    panic!(
        "the VMOD the VTC suite will load is {behind} older than the source it is built \
         from, so every other vtc_* result in this run is meaningless.\n\n  \
         VMOD:   {}\n  \
         Source: {}\n\n\
         `cargo test` builds the rlib and the test harness, but not the cdylib — only \
         `cargo build` produces that. Run:\n\n    \
         cargo build --release && cargo test --release vtc_\n\n\
         or `make test-ghost`, which does both. See issue #121.",
        vmod.display(),
        newest_src.display(),
    );
}

/// Coarse, readable age for the failure message — the exact figure never
/// matters, only whether it is "just now" or "last week".
fn format_age(secs: u64) -> String {
    const MINUTE: u64 = 60;
    const HOUR: u64 = 60 * MINUTE;
    const DAY: u64 = 24 * HOUR;

    let (n, unit) = match secs {
        s if s < MINUTE => (s.max(1), "second"),
        s if s < HOUR => (s / MINUTE, "minute"),
        s if s < DAY => (s / HOUR, "hour"),
        s => (s / DAY, "day"),
    };
    format!("{n} {unit}{}", if n == 1 { "" } else { "s" })
}
