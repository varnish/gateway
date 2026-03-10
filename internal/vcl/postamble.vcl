# --- Gateway postamble (runs after user VCL) ---
sub vcl_recv {
    # Deferred pass: ghost sets X-Ghost-Pass instead of calling ctx.set_pass()
    # so that user VCL subroutines get a chance to run first.
    if (req.http.X-Ghost-Pass) {
        unset req.http.X-Ghost-Pass;
        return (pass);
    }
}
