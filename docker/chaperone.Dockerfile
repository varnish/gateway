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
# transaction so they are always version-matched. VARNISH_VERSION is the
# upstream major.minor.patch (e.g. 9.0.1); the apt-get glob pattern matches
# whatever distro suffix the repo has stamped on it (e.g. 9.0.1-1~trixie).
FROM debian:trixie-slim AS rust-builder

ARG VARNISH_VERSION
RUN test -n "${VARNISH_VERSION}" || (echo "ERROR: VARNISH_VERSION build-arg is required (read it from chaperone.varnishVersion in charts/varnish-gateway/values.yaml — \`make docker-chaperone\` does this for you)" >&2 && exit 1)

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
         "varnish=${VARNISH_VERSION}*" "varnish-dev=${VARNISH_VERSION}*" \
    && curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.92.0 \
    && rm -rf /var/lib/apt/lists/*

ENV PATH="/root/.cargo/bin:${PATH}"

WORKDIR /build

# Copy ghost source
COPY ghost/Cargo.toml ghost/Cargo.lock* ./
COPY ghost/build.rs ./build.rs
COPY ghost/src ./src

# Build ghost vmod
RUN cargo build --release

# Stage 3: Runtime image based on Debian with Varnish from apt
FROM debian:trixie-slim

ARG VARNISH_VERSION
RUN test -n "${VARNISH_VERSION}" || (echo "ERROR: VARNISH_VERSION build-arg is required (read it from chaperone.varnishVersion in charts/varnish-gateway/values.yaml — \`make docker-chaperone\` does this for you)" >&2 && exit 1)

LABEL org.opencontainers.image.title="varnish-gateway-chaperone"
LABEL org.opencontainers.image.source="https://github.com/varnish/gateway"
LABEL org.opencontainers.image.varnish-version="${VARNISH_VERSION}"

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
    && apt-get install -y --no-install-recommends "varnish=${VARNISH_VERSION}*" libcap2-bin \
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
