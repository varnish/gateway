VERSION := $(shell cat .version)
CHART_VERSION := $(shell cat .version | sed 's/^v//')
REGISTRY ?= ghcr.io/varnish
OPERATOR_IMAGE := $(REGISTRY)/gateway-operator
CHAPERONE_IMAGE := $(REGISTRY)/gateway-chaperone
VARNISH_IMAGE := $(REGISTRY)/varnish-ghost

.PHONY: help test build build-linux docker clean vendor act
.PHONY: build-go test-go test-envtest envtest install-envtest build-ghost test-ghost
.PHONY: helm-lint helm-template helm-package helm-push helm-install helm-upgrade helm-uninstall

help:
	@echo "Varnish Gateway Operator - Makefile targets"
	@echo ""
	@echo "Go targets:"
	@echo "  make build-go         Build Go binaries for current platform"
	@echo "  make test-go          Run Go tests (includes envtest)"
	@echo "  make test-envtest     Run only envtest integration tests"
	@echo "  make envtest          Setup envtest binaries (kube-apiserver, etcd)"
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
	@echo ""
	@echo "CI/Testing:"
	@echo "  make act              Run CI workflow locally with act (requires act tool)"
	@echo ""
	@echo "Deploy:"
	@echo "  make deploy-update    Update deploy/ manifests with current version"
	@echo "  make deploy           Update manifests and apply to cluster"
	@echo ""
	@echo "Helm:"
	@echo "  make helm-lint        Lint Helm chart"
	@echo "  make helm-template    Template Helm chart (dry-run)"
	@echo "  make helm-package     Package Helm chart (.tgz)"
	@echo "  make helm-push        Package and push to OCI registry (requires auth)"
	@echo "  make helm-install     Install Helm chart to cluster"
	@echo "  make helm-upgrade     Upgrade Helm chart on cluster"
	@echo "  make helm-uninstall   Uninstall Helm chart from cluster"
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

# EnvTest Configuration
ENVTEST_K8S_VERSION = 1.31.0
ENVTEST := $(shell pwd)/bin/setup-envtest
ENVTEST_ASSETS_DIR := $(shell pwd)/testbin

# Install setup-envtest tool
install-envtest:
	@mkdir -p bin
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

# Download and setup envtest binaries (kube-apiserver, etcd)
envtest: install-envtest
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path

# Run only envtest-based integration tests
test-envtest: envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" \
		go test -v -tags=integration ./internal/controller/...

test-go: envtest
	go vet ./...
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path)" \
		go test -tags=integration ./...

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
	cd ghost && cargo clippy --release -- -D warnings
	cd ghost && cargo build --release
	cd ghost && cargo test --release --lib
	cd ghost && cargo test --release run_vtc_tests

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

# ============================================================================
# CI/Testing
# ============================================================================

# Run CI workflow locally using act (https://github.com/nektos/act)
# Requires: act tool installed (go install github.com/nektos/act@latest)
act:
	act -W .github/workflows/ci.yml push -P ubuntu-latest=catthehacker/ubuntu:act-latest

# ============================================================================
# Deploy
# ============================================================================

deploy-update:
	@echo "Updating deploy/01-operator.yaml to version $(VERSION)"
	@sed -i 's|gateway-operator:[v0-9.]*|gateway-operator:$(VERSION)|' deploy/01-operator.yaml
	@sed -i 's|gateway-chaperone:[v0-9.]*"|gateway-chaperone:$(VERSION)"|' deploy/01-operator.yaml

deploy: deploy-update
	kubectl apply -f deploy/

# ============================================================================
# Helm
# ============================================================================

CHART_PATH := charts/varnish-gateway
RELEASE_NAME ?= varnish-gateway
NAMESPACE ?= varnish-gateway-system

helm-lint:
	helm lint $(CHART_PATH)

helm-template:
	helm template $(RELEASE_NAME) $(CHART_PATH) --debug

helm-package:
	@mkdir -p dist/charts
	helm package $(CHART_PATH) -d dist/charts

helm-push: helm-package
	@echo "Pushing Helm chart to $(REGISTRY)/varnish/charts"
	helm push dist/charts/varnish-gateway-$(CHART_VERSION).tgz oci://$(REGISTRY)/varnish/charts

helm-install:
	helm install $(RELEASE_NAME) $(CHART_PATH) \
		--namespace $(NAMESPACE) \
		--create-namespace

helm-upgrade:
	helm upgrade $(RELEASE_NAME) $(CHART_PATH) \
		--namespace $(NAMESPACE)

helm-uninstall:
	helm uninstall $(RELEASE_NAME) --namespace $(NAMESPACE)

# ============================================================================
# Maintenance
# ============================================================================

vendor:
	go mod vendor

clean:
	rm -rf dist/
	rm -f operator chaperone
	cd ghost && cargo clean
