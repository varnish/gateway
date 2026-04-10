# Stage 1: Build chaperone (Go)
FROM golang:1.26-alpine AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -mod=vendor -o /chaperone ./cmd/chaperone

# Stage 2: Build ghost VMOD (Rust)
# Use debian base and install varnish + varnish-dev from the apt repo in one
# transaction so they are always version-matched (the varnish:9.0 Docker image
# can lag behind the apt repo, causing dependency conflicts).
FROM debian:trixie-slim AS rust-builder

RUN export DEBIAN_FRONTEND=noninteractive \
    && apt-get update \
    && apt-get install -y curl gpg dirmngr \
    && mkdir -p /etc/apt/keyrings \
    && gpg --batch --keyserver hkps://keys.openpgp.org \
         --recv-keys 694566269779DFAC975ED9BDD0525EAE838B3344 \
    && gpg --batch --armor --export 694566269779DFAC975ED9BDD0525EAE838B3344 \
         > /etc/apt/keyrings/varnish.gpg \
    && . /etc/os-release \
    && echo "deb [signed-by=/etc/apt/keyrings/varnish.gpg] https://packages.varnish-software.com/varnish/debian $VERSION_CODENAME main" \
         > /etc/apt/sources.list.d/varnish.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
         build-essential pkg-config clang libclang-dev \
         varnish varnish-dev \
    && curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.92.0 \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/root/.cargo/bin:${PATH}"

WORKDIR /build

# Copy ghost source
COPY ghost/Cargo.toml ghost/Cargo.lock* ./
COPY ghost/build.rs ./build.rs
COPY ghost/src ./src
COPY ghost/c_code ./c_code
COPY ghost/patches ./patches

# Build ghost vmod
RUN cargo build --release

# Stage 3: Runtime image based on Debian with Varnish from apt
FROM debian:trixie-slim

RUN export DEBIAN_FRONTEND=noninteractive \
    && apt-get update \
    && apt-get install -y curl gpg dirmngr \
    && mkdir -p /etc/apt/keyrings \
    && gpg --batch --keyserver hkps://keys.openpgp.org \
         --recv-keys 694566269779DFAC975ED9BDD0525EAE838B3344 \
    && gpg --batch --armor --export 694566269779DFAC975ED9BDD0525EAE838B3344 \
         > /etc/apt/keyrings/varnish.gpg \
    && . /etc/os-release \
    && echo "deb [signed-by=/etc/apt/keyrings/varnish.gpg] https://packages.varnish-software.com/varnish/debian $VERSION_CODENAME main" \
         > /etc/apt/sources.list.d/varnish.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends varnish libcap2-bin \
    && setcap cap_ipc_lock,cap_net_bind_service+ep /usr/sbin/varnishd \
    && apt-get remove -y libcap2-bin curl gpg dirmngr && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* ~/.gnupg \
    && adduser --uid 1000 --quiet --system --no-create-home --home /nonexistent --group varnish || true

USER varnish

# Copy chaperone binary
COPY --from=go-builder /chaperone /usr/local/bin/chaperone

# Copy ghost vmod
COPY --from=rust-builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/

# Chaperone manages varnishd, so it's the entrypoint
ENTRYPOINT ["/usr/local/bin/chaperone"]
