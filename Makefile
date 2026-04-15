VERSION := $(shell cat .version)
CHART_VERSION := $(shell cat .version | sed 's/^v//')
REGISTRY ?= ghcr.io/varnish
OPERATOR_IMAGE := $(REGISTRY)/gateway-operator
CHAPERONE_IMAGE := $(REGISTRY)/gateway-chaperone


.PHONY: help test build build-linux docker clean vendor act
.PHONY: build-go test-go test-envtest envtest install-envtest build-ghost test-ghost
.PHONY: helm-lint helm-template
.PHONY: test-conformance test-conformance-report test-conformance-single
.PHONY: kind-create kind-delete kind-load kind-deploy test-conformance-kind
.PHONY: manifests generate verify-manifests
.PHONY: docs-venv docs-serve docs-build docs-clean

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

	@echo ""
	@echo "CI/Testing:"
	@echo "  make act              Run CI workflow locally with act (requires act tool)"
	@echo ""
	@echo "Conformance:"
	@echo "  make test-conformance                             Run all conformance tests (requires live cluster)"
	@echo "  make test-conformance-report                      Run conformance tests and generate report"
	@echo "  make test-conformance-single TEST=TestShortName   Run a single conformance test"
	@echo "  make test-conformance-kind                        Full cycle: kind cluster, build, deploy, test, teardown"
	@echo ""
	@echo "Kind cluster:"
	@echo "  make kind-create      Create kind cluster and install Gateway API CRDs"
	@echo "  make kind-delete      Delete kind cluster"
	@echo "  make kind-load        Load Docker images into kind cluster"
	@echo "  make kind-deploy      Deploy operator to kind cluster"
	@echo ""
	@echo "Deploy:"
	@echo "  make deploy-update    Update deploy/ manifests with current version"
	@echo "  make deploy           Update manifests and apply to cluster"
	@echo ""
	@echo "Helm:"
	@echo "  make helm-lint        Lint Helm chart"
	@echo "  make helm-template    Template Helm chart (dry-run)"
	@echo ""
	@echo "Code generation:"
	@echo "  make manifests        Generate CRD manifests from Go types (controller-gen)"
	@echo "  make generate         Generate deepcopy functions (controller-gen)"
	@echo "  make verify-manifests Verify generated files are up-to-date"
	@echo ""
	@echo "Docs site (MkDocs Material):"
	@echo "  make docs-serve       Live-reload dev server at http://127.0.0.1:8000"
	@echo "  make docs-build       Build static site into _site/ (strict mode)"
	@echo "  make docs-clean       Remove _site/ and the docs virtualenv"
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
	cd ghost && LD_LIBRARY_PATH=$$(pwd)/target/release cargo test --release --lib
	cd ghost && LD_LIBRARY_PATH=$$(pwd)/target/release cargo test --release run_vtc_tests

# ============================================================================
# Docker images
# ============================================================================

docker: docker-operator docker-chaperone

docker-operator:
	docker build -t $(OPERATOR_IMAGE):$(VERSION) -f docker/operator.Dockerfile .
	docker tag $(OPERATOR_IMAGE):$(VERSION) $(OPERATOR_IMAGE):latest

docker-chaperone:
	docker build -t $(CHAPERONE_IMAGE):$(VERSION) -f docker/chaperone.Dockerfile .
	docker tag $(CHAPERONE_IMAGE):$(VERSION) $(CHAPERONE_IMAGE):latest

# ============================================================================
# CI/Testing
# ============================================================================

# Run CI workflow locally using act (https://github.com/nektos/act)
# Requires: act tool installed (go install github.com/nektos/act@latest)
act:
	act -W .github/workflows/ci.yml push -P ubuntu-latest=catthehacker/ubuntu:act-latest

# ============================================================================
# Gateway API Conformance Tests (requires live cluster with operator deployed)
# ============================================================================

test-conformance:
	go test -tags=conformance -v -timeout 350s -count=1 ./conformance/ \
		-args --gateway-class=varnish

test-conformance-report:
	@mkdir -p dist
	CONFORMANCE_REPORT_PATH=dist/conformance-report.yaml \
	GATEWAY_VERSION=$(VERSION) \
	go test -tags=conformance -v -timeout 350s -count=1 ./conformance/ \
		-args --gateway-class=varnish

test-conformance-single:
ifndef TEST
	$(error TEST is required. Usage: make test-conformance-single TEST=HTTPRouteMethodMatching)
endif
	go test -tags=conformance -v -timeout 350s -count=1 ./conformance/ \
		-args --gateway-class=varnish --run-test=$(TEST)

# ============================================================================
# Kind cluster for conformance testing
# ============================================================================

GATEWAY_API_VERSION ?= v1.4.0
KIND_CLUSTER_NAME ?= varnish-gw
KIND := go tool kind

kind-create:
	$(KIND) create cluster --name $(KIND_CLUSTER_NAME) --wait 60s
	kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_VERSION)/standard-install.yaml
	./hack/kind-metallb.sh

kind-delete:
	$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)

kind-load:
	$(KIND) load docker-image $(OPERATOR_IMAGE):$(VERSION) --name $(KIND_CLUSTER_NAME)
	$(KIND) load docker-image $(CHAPERONE_IMAGE):$(VERSION) --name $(KIND_CLUSTER_NAME)

kind-deploy: deploy-update
	kubectl apply -f deploy/00-prereqs.yaml
	kubectl wait --for=condition=Established crd/gatewayclassparameters.gateway.varnish-software.com --timeout=30s
	kubectl apply -f deploy/01-operator.yaml -f deploy/02-chaperone-rbac.yaml -f deploy/03-gatewayclass.yaml
	kubectl rollout status deployment/varnish-gateway-operator -n varnish-gateway-system --timeout=120s

test-conformance-kind: KIND_VERSION = kind
test-conformance-kind:
	$(MAKE) kind-create
	$(MAKE) docker VERSION=$(KIND_VERSION)
	$(MAKE) kind-load VERSION=$(KIND_VERSION)
	$(MAKE) kind-deploy VERSION=$(KIND_VERSION)
	$(MAKE) test-conformance; rc=$$?; $(MAKE) kind-delete; exit $$rc

# ============================================================================
# Deploy
# ============================================================================

deploy-update:
	@echo "Updating deploy/01-operator.yaml to version $(VERSION)"
	@perl -pi -e 's|gateway-operator:[a-zA-Z0-9._-]+|gateway-operator:$(VERSION)|g; s|gateway-chaperone:[a-zA-Z0-9._-]+|gateway-chaperone:$(VERSION)|g' deploy/01-operator.yaml

deploy: deploy-update
	kubectl apply -f deploy/00-prereqs.yaml -f deploy/01-operator.yaml -f deploy/02-chaperone-rbac.yaml -f deploy/03-gatewayclass.yaml

dev-deploy:
	$(MAKE) docker VERSION=dev
	$(MAKE) deploy VERSION=dev
	kubectl rollout restart deployment/varnish-gateway-operator -n varnish-gateway-system
	@if kubectl get deployment/varnish-gateway -n varnish-gateway-system >/dev/null 2>&1; then \
		kubectl rollout restart deployment/varnish-gateway -n varnish-gateway-system; \
	fi

# ============================================================================
# Helm
# ============================================================================

CHART_PATH := charts/varnish-gateway
RELEASE_NAME ?= varnish-gateway

helm-lint:
	helm lint $(CHART_PATH)

helm-template:
	helm template $(RELEASE_NAME) $(CHART_PATH) --debug

# ============================================================================
# Code generation (controller-gen)
# ============================================================================

CONTROLLER_GEN = go tool controller-gen

# Generate CRD manifests from Go types into Helm chart, then assemble deploy/00-prereqs.yaml
manifests:
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=charts/varnish-gateway/crds
	@echo "Assembling deploy/00-prereqs.yaml..."
	@cat deploy/namespace.yaml > deploy/00-prereqs.yaml
	@echo "---" >> deploy/00-prereqs.yaml
	@cat charts/varnish-gateway/crds/gateway.varnish-software.com_gatewayclassparameters.yaml >> deploy/00-prereqs.yaml
	@echo "---" >> deploy/00-prereqs.yaml
	@cat charts/varnish-gateway/crds/gateway.varnish-software.com_varnishcachepolicies.yaml >> deploy/00-prereqs.yaml
	@echo "---" >> deploy/00-prereqs.yaml
	@cat charts/varnish-gateway/crds/gateway.varnish-software.com_varnishcacheinvalidations.yaml >> deploy/00-prereqs.yaml

# Generate deepcopy functions from Go types
generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

# Verify generated files are up-to-date (for CI)
verify-manifests: manifests generate
	@if [ -n "$$(git diff --name-only)" ]; then \
		echo "ERROR: Generated files are out of date. Run 'make manifests generate' and commit the changes."; \
		git diff --name-only; \
		exit 1; \
	fi

# ============================================================================
# Docs site (MkDocs Material)
# ============================================================================

DOCS_VENV := .venv-docs
DOCS_PY   := $(DOCS_VENV)/bin/python
DOCS_PIP  := $(DOCS_VENV)/bin/pip
MKDOCS    := $(DOCS_VENV)/bin/mkdocs

# Create the virtualenv and install pinned deps. Re-runs when
# requirements-docs.txt changes.
$(DOCS_VENV)/.stamp: requirements-docs.txt
	@python3 -m venv $(DOCS_VENV)
	@$(DOCS_PIP) install --quiet --upgrade pip
	@$(DOCS_PIP) install --quiet -r requirements-docs.txt
	@touch $@

docs-venv: $(DOCS_VENV)/.stamp

# Sync the version badge in the hero with .version before building/serving.
# Uses a temp mkdocs config so the source file stays stable in git.
docs-serve: docs-venv
	@VG_VERSION=$(VERSION) $(MKDOCS) serve --dev-addr 127.0.0.1:8000 \
		--config-file mkdocs.yml

docs-build: docs-venv
	@echo "Building docs site for $(VERSION)..."
	@perl -pi -e 's|^(\s*version:\s*)v[0-9][^\s]*|$${1}$(VERSION)|' mkdocs.yml
	@$(MKDOCS) build --strict --config-file mkdocs.yml
	@echo "Built site in _site/"

docs-clean:
	rm -rf _site $(DOCS_VENV)

# ============================================================================
# Maintenance
# ============================================================================

vendor:
	go mod vendor

clean:
	rm -rf dist/ _site/
	rm -f operator chaperone
	cd ghost && cargo clean
