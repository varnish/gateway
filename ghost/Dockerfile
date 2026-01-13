# Build stage
FROM rust:1.83-bookworm AS builder

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

# Copy source
COPY Cargo.toml Cargo.lock* ./
COPY src ./src
COPY tests ./tests

# Build
RUN cargo build --release

# The .so file will be at /build/target/release/libvmod_ghost.so

# Runtime stage (optional - for testing)
FROM varnish:7.6

COPY --from=builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

# Default command runs varnishd
CMD ["varnishd", "-F", "-f", "/etc/varnish/default.vcl"]
