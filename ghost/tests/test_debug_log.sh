#!/bin/bash
set -e

# Start varnishtest in background and capture its output
TMPDIR=$(mktemp -d)
VMOD="$(pwd)/target/release/libvmod_ghost.so"

# Create ghost config
cat > $TMPDIR/ghost.json <<EOF
{
    "version": 2,
    "vhosts": {
        "test.example.com": {
            "routes": [
                {
                    "path_match": {
                        "type": "PathPrefix",
                        "value": "/api/v1"
                    },
                    "backends": [
                        {"address": "127.0.0.1", "port": 9999, "weight": 100}
                    ],
                    "filters": {
                        "url_rewrite": {
                            "path_type": "ReplacePrefixMatch",
                            "replace_prefix_match": "/api/v2"
                        }
                    },
                    "priority": 100
                }
            ]
        }
    }
}
EOF

echo "Config created at $TMPDIR/ghost.json"
cat $TMPDIR/ghost.json

# Start a simple backend server with nc
echo "Starting backend on port 9999..."
nc -l 9999 &
NC_PID=$!

# Start varnish with the config
echo "Starting varnish..."
varnishd -n $TMPDIR/varnish -a 127.0.0.1:8080 -F -f - <<VCLEOF &
vcl 4.1;
import ghost from "$VMOD";
import std;

backend dummy none;

sub vcl_init {
    ghost.init("$TMPDIR/ghost.json");
    new router = ghost.ghost_backend();
}

sub vcl_recv {
    std.log("vcl_recv: " + req.url + " host=" + req.http.host);
    if (req.url == "/.varnish-ghost/reload") {
        if (router.reload()) {
            return (synth(200, "OK"));
        } else {
            return (synth(500, "Reload failed"));
        }
    }
}

sub vcl_backend_fetch {
    std.log("BEFORE: bereq.url=" + bereq.url);
    set bereq.backend = router.backend();
    std.log("AFTER: bereq.url=" + bereq.url);
}
VCLEOF
VARNISH_PID=$!

sleep 2

# Test the reload
echo "Testing reload..."
curl -v http://127.0.0.1:8080/.varnish-ghost/reload

# Send test request
echo "Sending test request..."
curl -v -H "Host: test.example.com" http://127.0.0.1:8080/api/v1/users

# Cleanup
kill $VARNISH_PID $NC_PID 2>/dev/null || true
rm -rf $TMPDIR
