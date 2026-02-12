# Multi-stage build for Varnish with Ghost VMOD
#
# Stage 1: Build Ghost VMOD from Rust
# Stage 2: Varnish runtime with Ghost installed

# Build stage - base on varnish image so headers match exactly
FROM ghcr.io/varnish/varnish-base:8.0 AS builder
USER root

# Install Rust toolchain and build dependencies
RUN apt-get update && apt-get install -y \
    curl \
    build-essential \
    pkg-config \
    clang \
    libclang-dev \
    && curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.92.0 \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/root/.cargo/bin:${PATH}"

WORKDIR /build

# Copy ghost source
COPY ghost/Cargo.toml ghost/Cargo.lock* ./
COPY ghost/src ./src

RUN cargo build --release

# Runtime stage
FROM ghcr.io/varnish/varnish-base:8.0

# Copy the built vmod
COPY --from=builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

EXPOSE 80 6081

CMD ["varnishd", "-F", "-a", ":80", "-f", "/etc/varnish/default.vcl"]
