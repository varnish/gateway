# Stage 1: Build chaperone (Go)
FROM golang:1-alpine AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -mod=vendor -o /chaperone ./cmd/chaperone

# Stage 2: Build ghost VMOD (Rust)
# Base on varnish image so headers match exactly at compile time
FROM ghcr.io/varnish/varnish-base:8.0 AS rust-builder
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

# Build ghost vmod
RUN cargo build --release

# Stage 3: Runtime image based on Varnish
FROM ghcr.io/varnish/varnish-base:8.0

USER root

# Install libcap2-bin for setcap, set IPC_LOCK capability on varnishd for mlock()
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
    && setcap cap_ipc_lock+ep /usr/sbin/varnishd \
    && apt-get remove -y libcap2-bin && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

USER varnish

# Copy chaperone binary
COPY --from=go-builder /chaperone /usr/local/bin/chaperone

# Copy ghost vmod
COPY --from=rust-builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

# Chaperone manages varnishd, so it's the entrypoint
ENTRYPOINT ["/usr/local/bin/chaperone"]
