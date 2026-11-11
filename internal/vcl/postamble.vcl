# --- Gateway postamble (runs after user VCL) ---
sub vcl_recv {
    # Deferred pass: ghost sets X-Ghost-Pass instead of calling ctx.set_pass()
    # so that user VCL subroutines get a chance to run first.
    if (req.http.X-Ghost-Pass) {
        unset req.http.X-Ghost-Pass;
        return (pass);
    }
}

sub vcl_backend_error {
    # Report 504 rather than Varnish's default 503 on routes carrying a timeout.
    # Only status and reason are touched — deliberately no return(deliver) — so
    # builtin.vcl still renders the error body, and a user vcl_backend_error that
    # returns first keeps full control (its return means this never runs).
    #
    # Cannot distinguish a timeout from any other fetch failure on such a route:
    # a refused connection also reports 504. Varnish exposes no failure reason
    # in vcl_backend_error.
    if (bereq.http.X-Ghost-Timeout) {
        set beresp.status = 504;
        set beresp.reason = "Gateway Timeout";
        set beresp.ttl = 0s;
    }
}
