vcl 4.1;

import ghost;
import std;

backend dummy { .host = "127.0.0.1"; .port = "80"; }

acl localhost {
    "127.0.0.1";
    "::1";
}

sub vcl_init {
    ghost.init("%s");
    new router = ghost.ghost_backend();
}

sub vcl_recv {
    # Handle reload endpoint (localhost only)
    if (req.url == "/.varnish-ghost/reload" && client.ip ~ localhost) {
        if (router.reload()) {
            return (synth(200, "OK"));
        } else {
            set req.http.X-Ghost-Error = router.last_error();
            return (synth(500, "Reload failed"));
        }
    }
    # Route request using ghost (listener-aware).
    # Ghost sets req.hash_ignore_busy via C API and X-Ghost-Pass header for pass mode.
    # The actual return(pass) is deferred to the postamble vcl_recv so that
    # user VCL concatenated between preamble and postamble gets a chance to run.
    set req.backend_hint = router.recv();
}

sub vcl_hash {
    # Additional cache key data from cache policy
    if (req.http.X-Ghost-Cache-Key-Extra) {
        hash_data(req.http.X-Ghost-Cache-Key-Extra);
        unset req.http.X-Ghost-Cache-Key-Extra;
    }
}

sub vcl_synth {
    # Surface ghost reload errors to chaperone via header
    if (req.url == "/.varnish-ghost/reload") {
        if (req.http.X-Ghost-Error) {
            set resp.http.x-ghost-error = req.http.X-Ghost-Error;
        }
    }
}

sub vcl_backend_fetch {
    # Clean up internal cache policy headers before sending to backend
    unset bereq.http.X-Ghost-Default-TTL;
    unset bereq.http.X-Ghost-Forced-TTL;
    unset bereq.http.X-Ghost-Grace;
    unset bereq.http.X-Ghost-Keep;
}

sub vcl_backend_response {
    # Copy filter context from req to beresp for vcl_deliver
    if (bereq.http.X-Ghost-Filter-Context) {
        set beresp.http.X-Ghost-Filter-Context = bereq.http.X-Ghost-Filter-Context;
    }

    # Apply cache policy: forced TTL overrides everything.
    # Note: this only affects responses Varnish considers cacheable.
    # beresp.uncacheable is write-once-to-true, so we cannot force-cache
    # responses that Varnish has already marked uncacheable.
    if (bereq.http.X-Ghost-Forced-TTL) {
        set beresp.ttl = std.duration(bereq.http.X-Ghost-Forced-TTL, 0s);
        unset beresp.http.Set-Cookie;
        unset beresp.http.Cache-Control;
        unset beresp.http.Expires;
    }
    # Apply cache policy: default TTL when origin has no Cache-Control
    else if (bereq.http.X-Ghost-Default-TTL) {
        if (!beresp.http.Cache-Control) {
            set beresp.ttl = std.duration(bereq.http.X-Ghost-Default-TTL, 0s);
        }
    }

    # Apply grace and keep from cache policy
    if (bereq.http.X-Ghost-Grace) {
        set beresp.grace = std.duration(bereq.http.X-Ghost-Grace, 0s);
    }
    if (bereq.http.X-Ghost-Keep) {
        set beresp.keep = std.duration(bereq.http.X-Ghost-Keep, 0s);
    }
}

sub vcl_deliver {
    ghost.deliver();
}

# --- User VCL concatenated below ---
