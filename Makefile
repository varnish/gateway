VERSION := $(shell cat .version)
REGISTRY ?= registry.digitalocean.com/varnish-gateway
OPERATOR_IMAGE := $(REGISTRY)/gateway-operator
SIDECAR_IMAGE := $(REGISTRY)/gateway-sidecar

.PHONY: help test build build-linux docker docker-operator docker-sidecar docker-buildx clean vendor

help:
	@echo "Varnish Gateway Operator - Makefile targets"
	@echo ""
	@echo "  make test             Run tests"
	@echo "  make build            Build binaries for current platform (dist/)"
	@echo "  make build-linux      Build Linux binaries for amd64 and arm64"
	@echo "  make build-linux-amd64  Build Linux amd64 binaries"
	@echo "  make build-linux-arm64  Build Linux arm64 binaries"
	@echo "  make docker           Build Docker images for current arch"
	@echo "  make docker-push      Build and push single-arch images"
	@echo "  make docker-buildx    Build and push multi-arch images (amd64+arm64)"
	@echo "  make vendor           Update vendor directory"
	@echo "  make clean            Remove build artifacts"
	@echo ""
	@echo "Configuration:"
	@echo "  VERSION=$(VERSION)"
	@echo "  REGISTRY=$(REGISTRY)"

test:
	go test ./...

# Build for current platform
build: dist/operator dist/sidecar

dist/operator:
	@mkdir -p dist
	go build -mod=vendor -o dist/operator ./cmd/operator

dist/sidecar:
	@mkdir -p dist
	go build -mod=vendor -o dist/sidecar ./cmd/sidecar

# Build Linux binaries for both architectures
build-linux: build-linux-amd64 build-linux-arm64

build-linux-amd64: dist/operator-linux-amd64 dist/sidecar-linux-amd64

dist/operator-linux-amd64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o dist/operator-linux-amd64 ./cmd/operator

dist/sidecar-linux-amd64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o dist/sidecar-linux-amd64 ./cmd/sidecar

build-linux-arm64: dist/operator-linux-arm64 dist/sidecar-linux-arm64

dist/operator-linux-arm64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -o dist/operator-linux-arm64 ./cmd/operator

dist/sidecar-linux-arm64:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -o dist/sidecar-linux-arm64 ./cmd/sidecar

# Docker images
docker: docker-operator docker-sidecar

docker-operator:
	docker build -t $(OPERATOR_IMAGE):$(VERSION) -f Dockerfile.operator .
	docker tag $(OPERATOR_IMAGE):$(VERSION) $(OPERATOR_IMAGE):latest

docker-sidecar:
	docker build -t $(SIDECAR_IMAGE):$(VERSION) -f Dockerfile.sidecar .
	docker tag $(SIDECAR_IMAGE):$(VERSION) $(SIDECAR_IMAGE):latest

# Push images (single arch)
docker-push: docker
	docker push $(OPERATOR_IMAGE):$(VERSION)
	docker push $(OPERATOR_IMAGE):latest
	docker push $(SIDECAR_IMAGE):$(VERSION)
	docker push $(SIDECAR_IMAGE):latest

# Multi-arch build and push (amd64 + arm64)
PLATFORMS := linux/amd64,linux/arm64

docker-buildx: docker-buildx-operator docker-buildx-sidecar

docker-buildx-operator:
	docker buildx build --platform $(PLATFORMS) \
		-t $(OPERATOR_IMAGE):$(VERSION) \
		-t $(OPERATOR_IMAGE):latest \
		-f Dockerfile.operator --push .

docker-buildx-sidecar:
	docker buildx build --platform $(PLATFORMS) \
		-t $(SIDECAR_IMAGE):$(VERSION) \
		-t $(SIDECAR_IMAGE):latest \
		-f Dockerfile.sidecar --push .

# Vendor dependencies
vendor:
	go mod vendor

# Clean build artifacts
clean:
	rm -rf dist/
