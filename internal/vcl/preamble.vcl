vcl 4.1;

import ghost;

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
    # Route request using ghost (listener-aware)
    set req.backend_hint = router.recv();
}

sub vcl_synth {
    # Surface ghost reload errors to chaperone via header
    if (req.url == "/.varnish-ghost/reload") {
        if (req.http.X-Ghost-Error) {
            set resp.http.x-ghost-error = req.http.X-Ghost-Error;
        }
    }
}

sub vcl_backend_response {
    # Copy filter context from req to beresp for vcl_deliver
    if (bereq.http.X-Ghost-Filter-Context) {
        set beresp.http.X-Ghost-Filter-Context = bereq.http.X-Ghost-Filter-Context;
    }
}

sub vcl_deliver {
    ghost.deliver();
}

# --- User VCL concatenated below ---
