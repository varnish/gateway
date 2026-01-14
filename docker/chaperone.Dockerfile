# Stage 1: Build chaperone (Go)
FROM golang:1-alpine AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -mod=vendor -o /chaperone ./cmd/chaperone

# Stage 2: Build ghost VMOD (Rust)
FROM rust:1.83-bookworm AS rust-builder

# Install Varnish 8.0 development headers and build dependencies
RUN apt-get update && apt-get install -y \
    curl \
    gnupg \
    apt-transport-https \
    clang \
    libclang-dev \
    && curl -fsSL https://packagecloud.io/varnishcache/varnish80/gpgkey | gpg --dearmor -o /usr/share/keyrings/varnish.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/varnish.gpg] https://packagecloud.io/varnishcache/varnish80/debian/ bookworm main" > /etc/apt/sources.list.d/varnish.list \
    && apt-get update \
    && apt-get install -y varnish-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Copy ghost source
COPY ghost/Cargo.toml ghost/Cargo.lock* ./
COPY ghost/src ./src

# Build ghost vmod
RUN cargo build --release

# Stage 3: Runtime image based on Varnish
FROM varnish:8.0

# Copy chaperone binary
COPY --from=go-builder /chaperone /usr/local/bin/chaperone

# Copy ghost vmod
COPY --from=rust-builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

# Chaperone manages varnishd, so it's the entrypoint
ENTRYPOINT ["/usr/local/bin/chaperone"]
