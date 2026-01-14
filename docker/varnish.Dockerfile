# Multi-stage build for Varnish 8 with Ghost VMOD
#
# Stage 1: Build Ghost VMOD from Rust
# Stage 2: Varnish runtime with Ghost installed

# Build stage
FROM rust:1.75-bookworm AS builder

# Install Varnish dev dependencies for varnish-rs
RUN apt-get update && apt-get install -y \
    varnish-dev \
    libvarnishapi-dev \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY ghost/ .

RUN cargo build --release

# Runtime stage
FROM varnish:7.5-bookworm

# Copy the built vmod
COPY --from=builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

# Default configuration directory
RUN mkdir -p /var/run/varnish

EXPOSE 80 6081

CMD ["varnishd", "-F", "-a", ":80", "-f", "/etc/varnish/default.vcl"]
