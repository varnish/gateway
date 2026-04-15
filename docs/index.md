---
title: Varnish Gateway
template: home.html
hide:
  - navigation
  - toc
---

# Varnish Gateway

## Why Varnish Gateway?

All the routing capabilities of the Gateway API, with the speed and flexibility of [Varnish](https://varnish.org/).

<div class="grid cards" markdown>

-   :material-check-decagram:{ .lg .middle } __Conformance tested__

    ---

    Passes the Gateway API conformance test suite. Standard HTTPRoute matching,
    header and query filters, path types, and weighted traffic splitting.

-   :material-lightning-bolt:{ .lg .middle } __Zero VCL generation for routing__

    ---

    HTTP routing is driven by live, in-memory routing tables derived from
    HTTPRoutes and EndpointSlices — not by VCL. Route and backend changes
    apply in milliseconds, no VCL compile.

-   :material-view-dashboard:{ .lg .middle } __Built-in dashboard__

    ---

    Real-time monitoring with live event stream, backend health, virtual host
    overview, and heartbeat visualization.

-   :material-code-braces:{ .lg .middle } __Custom VCL when you need it__

    ---

    HTTPRoutes cover the spec. Drop into VCL for custom caching, rewriting,
    or request logic that goes beyond the Gateway API.

-   :material-earth:{ .lg .middle } __Multi-listener__

    ---

    Multiple Gateway listeners on different ports. Listener-aware routing
    with per-listener branching via request headers.

-   :material-shield-lock:{ .lg .middle } __TLS termination__

    ---

    High-performance TLS on client and backend sides. Certificates
    hot-reload automatically — no pod restart.

</div>

!!! success "Gateway API Conformance"
    Varnish Gateway passes the official Kubernetes Gateway API conformance test
    suite, covering HTTPRoute matching, request/response header filters, URL
    rewrites, redirects, and weighted traffic splitting.

## How it works

Three components translate Gateway API resources into a running Varnish configuration.

<div class="vg-arch" markdown>

<div class="vg-arch-step" markdown>
<span class="vg-arch-num">1</span>
#### Operator
Watches Gateway and HTTPRoute resources. Generates VCL and routing
configuration. Manages the Varnish Deployment, Service, and RBAC.
</div>

<div class="vg-arch-step" markdown>
<span class="vg-arch-num">2</span>
#### Chaperone
Manages the `varnishd` process. Discovers backend endpoints via EndpointSlices,
merges them with the operator's routing config, and pushes the result to Ghost.
</div>

<div class="vg-arch-step" markdown>
<span class="vg-arch-num">3</span>
#### Ghost VMOD
A Rust-based Varnish module that handles all routing inside Varnish. Matches
requests to backends by host, path, headers, and query parameters.
</div>

</div>

[Read the architecture overview →](concepts/architecture.md){ .md-button }

## Install

=== "Helm"

    Install the Gateway API CRDs:

    ```bash
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
    ```

    Install Varnish Gateway:

    ```bash
    helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
      --namespace varnish-gateway-system \
      --create-namespace
    ```

    Create a Gateway:

    ```yaml
    apiVersion: gateway.networking.k8s.io/v1
    kind: Gateway
    metadata:
      name: varnish
    spec:
      gatewayClassName: varnish
      listeners:
        - name: http
          port: 80
          protocol: HTTP
    ```

=== "kubectl"

    Install the Gateway API CRDs:

    ```bash
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
    ```

    Deploy the operator and resources:

    ```bash
    kubectl apply -f deploy/
    ```

Full instructions are in the [installation guide](getting-started/installation.md).

## Community

<div class="grid cards" markdown>

-   :fontawesome-brands-github:{ .lg .middle } __GitHub__

    ---

    Source code, issue tracker, and releases.

    [varnish/gateway :octicons-arrow-right-24:](https://github.com/varnish/gateway)

-   :material-bug:{ .lg .middle } __Issues__

    ---

    Report bugs, request features, or browse known issues.

    [Open an issue :octicons-arrow-right-24:](https://github.com/varnish/gateway/issues)

-   :material-book-open-variant:{ .lg .middle } __Documentation__

    ---

    Installation, configuration, operations, and reference.

    [Get started :octicons-arrow-right-24:](getting-started/installation.md)

</div>
