VERSION := $(shell cat .version)
REGISTRY ?= ghcr.io/varnish
OPERATOR_IMAGE := $(REGISTRY)/gateway-operator
CHAPERONE_IMAGE := $(REGISTRY)/gateway-chaperone
VARNISH_IMAGE := $(REGISTRY)/varnish-ghost

.PHONY: help test build build-linux docker clean vendor
.PHONY: build-go test-go build-ghost test-ghost

help:
	@echo "Varnish Gateway Operator - Makefile targets"
	@echo ""
	@echo "Go targets:"
	@echo "  make build-go         Build Go binaries for current platform"
	@echo "  make test-go          Run Go tests"
	@echo "  make build-linux      Build Linux Go binaries for amd64 and arm64"
	@echo ""
	@echo "Rust targets:"
	@echo "  make build-ghost      Build Ghost VMOD (requires Rust toolchain)"
	@echo "  make test-ghost       Run Ghost tests"
	@echo ""
	@echo "Combined targets:"
	@echo "  make build            Build all (Go + Ghost)"
	@echo "  make test             Run all tests"
	@echo ""
	@echo "Docker targets:"
	@echo "  make docker           Build all Docker images for current arch"
	@echo "  make docker-operator  Build operator image"
	@echo "  make docker-chaperone Build chaperone image"
	@echo "  make docker-varnish   Build Varnish+Ghost image"
	@echo "  make docker-push      Build and push single-arch images"
	@echo "  make docker-buildx    Build and push images via buildx (amd64)"
	@echo "  make docker-buildx-setup  Create buildx builder for multi-arch (run once)"
	@echo ""
	@echo "Deploy:"
	@echo "  make deploy-update    Update deploy/ manifests with current version"
	@echo "  make deploy           Update manifests and apply to cluster"
	@echo ""
	@echo "Other:"
	@echo "  make vendor           Update Go vendor directory"
	@echo "  make clean            Remove build artifacts"
	@echo ""
	@echo "Configuration:"
	@echo "  VERSION=$(VERSION)"
	@echo "  REGISTRY=$(REGISTRY)"

# ============================================================================
# Combined targets
# ============================================================================

build: build-go build-ghost

test: test-go test-ghost

# ============================================================================
# Go targets
# ============================================================================

build-go: dist/operator dist/chaperone

test-go:
	go test ./...

dist/operator:
	@mkdir -p dist
	go build -mod=vendor -o dist/operator ./cmd/operator

dist/chaperone:
	@mkdir -p dist
	go build -mod=vendor -o dist/chaperone ./cmd/chaperone

# Build Linux binaries for both architectures
build-linux: build-linux-amd64 build-linux-arm64

build-linux-amd64: dist/operator-linux-amd64 dist/chaperone-linux-amd64

dist/operator-linux-amd64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o dist/operator-linux-amd64 ./cmd/operator

dist/chaperone-linux-amd64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o dist/chaperone-linux-amd64 ./cmd/chaperone

build-linux-arm64: dist/operator-linux-arm64 dist/chaperone-linux-arm64

dist/operator-linux-arm64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -o dist/operator-linux-arm64 ./cmd/operator

dist/chaperone-linux-arm64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -o dist/chaperone-linux-arm64 ./cmd/chaperone

# ============================================================================
# Rust/Ghost targets
# ============================================================================

build-ghost:
	cd ghost && cargo build --release

test-ghost:
	cd ghost && cargo test

# ============================================================================
# Docker images
# ============================================================================

docker: docker-operator docker-chaperone docker-varnish

docker-operator:
	docker build -t $(OPERATOR_IMAGE):$(VERSION) -f docker/operator.Dockerfile .
	docker tag $(OPERATOR_IMAGE):$(VERSION) $(OPERATOR_IMAGE):latest

docker-chaperone:
	docker build -t $(CHAPERONE_IMAGE):$(VERSION) -f docker/chaperone.Dockerfile .
	docker tag $(CHAPERONE_IMAGE):$(VERSION) $(CHAPERONE_IMAGE):latest

docker-varnish:
	docker build -t $(VARNISH_IMAGE):$(VERSION) -f docker/varnish.Dockerfile .
	docker tag $(VARNISH_IMAGE):$(VERSION) $(VARNISH_IMAGE):latest

# Push images (single arch)
docker-push: docker
	docker push $(OPERATOR_IMAGE):$(VERSION)
	docker push $(OPERATOR_IMAGE):latest
	docker push $(CHAPERONE_IMAGE):$(VERSION)
	docker push $(CHAPERONE_IMAGE):latest
	docker push $(VARNISH_IMAGE):$(VERSION)
	docker push $(VARNISH_IMAGE):latest

# Multi-arch build and push (amd64 only for now)
PLATFORMS := linux/amd64
BUILDX_BUILDER := varnish-gateway

docker-buildx-setup:
	docker buildx create --name $(BUILDX_BUILDER) --use || docker buildx use $(BUILDX_BUILDER)
	docker buildx inspect --bootstrap

docker-buildx: docker-buildx-operator docker-buildx-chaperone docker-buildx-varnish

docker-buildx-operator:
	docker buildx build --platform $(PLATFORMS) \
		-t $(OPERATOR_IMAGE):$(VERSION) \
		-t $(OPERATOR_IMAGE):latest \
		-f docker/operator.Dockerfile --push .

docker-buildx-chaperone:
	docker buildx build --platform $(PLATFORMS) \
		-t $(CHAPERONE_IMAGE):$(VERSION) \
		-t $(CHAPERONE_IMAGE):latest \
		-f docker/chaperone.Dockerfile --push .

docker-buildx-varnish:
	docker buildx build --platform $(PLATFORMS) \
		-t $(VARNISH_IMAGE):$(VERSION) \
		-t $(VARNISH_IMAGE):latest \
		-f docker/varnish.Dockerfile --push .

# ============================================================================
# Deploy
# ============================================================================

deploy-update:
	@echo "Updating deploy/01-operator.yaml to version $(VERSION)"
	@# Strip 'v' prefix for image tags (Docker registry uses 0.3.3, not v0.3.3)
	$(eval IMAGE_VERSION := $(shell echo $(VERSION) | sed 's/^v//'))
	@sed -i 's|gateway-operator:[v0-9.]*|gateway-operator:$(IMAGE_VERSION)|' deploy/01-operator.yaml
	@sed -i 's|gateway-chaperone:[v0-9.]*"|gateway-chaperone:$(IMAGE_VERSION)"|' deploy/01-operator.yaml

deploy: deploy-update
	kubectl apply -f deploy/

# ============================================================================
# Maintenance
# ============================================================================

vendor:
	go mod vendor

clean:
	rm -rf dist/
	cd ghost && cargo clean
