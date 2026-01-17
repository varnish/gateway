# Multi-stage build for Varnish 7.6 with Ghost VMOD
#
# Stage 1: Build Ghost VMOD from Rust
# Stage 2: Varnish runtime with Ghost installed

# Build stage
FROM rust:1.92-bookworm AS builder

# Install Varnish 7.6 development headers and build dependencies
RUN apt-get update && apt-get install -y \
    curl \
    gnupg \
    apt-transport-https \
    clang \
    libclang-dev \
    && curl -fsSL https://packagecloud.io/varnishcache/varnish76/gpgkey | gpg --dearmor -o /usr/share/keyrings/varnish.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/varnish.gpg] https://packagecloud.io/varnishcache/varnish76/debian/ bookworm main" > /etc/apt/sources.list.d/varnish.list \
    && apt-get update \
    && apt-get install -y varnish-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Copy ghost source
COPY ghost/Cargo.toml ghost/Cargo.lock* ./
COPY ghost/src ./src

RUN cargo build --release

# Runtime stage
FROM varnish:7.6

# Copy the built vmod
COPY --from=builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

EXPOSE 80 6081

CMD ["varnishd", "-F", "-a", ":80", "-f", "/etc/varnish/default.vcl"]
