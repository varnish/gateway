# TODO

## Phase 1: Complete

See CLAUDE.md for current status.

## Phase 2: Path Matching

- Extend ghost.json generator to include path rules
- Parse HTTPRoute path matches (exact, prefix, regex)
- Order routes by specificity

## Phase 3: Advanced Request Matching

- Header matching (`rule.Matches[].Headers`)
- Method matching (`rule.Matches[].Method`)
- Query parameter matching (`rule.Matches[].QueryParams`)

## Phase 4: Traffic Management

- Traffic splitting (weighted backendRefs)
- RequestMirror filter

## Phase 5: Request/Response Modification

- RequestHeaderModifier filter
- ResponseHeaderModifier filter
- URLRewrite filter
- RequestRedirect filter
- Add `ghost.deliver()` call to VCL preamble

## Phase 6: TLS

- Listener TLS termination (watch `certificateRefs` Secrets)
- Certificate hot-reload on Secret changes
- BackendTLSPolicy support (upstream TLS)

Note: In k8s, cert-manager handles ACME. We just consume `kubernetes.io/tls` Secrets.

## Gateway Features

- Listener hostname filtering
- sectionName matching (`parentRef.SectionName`)
- Cross-namespace routes (ReferenceGrant validation)

## Observability

- Add varnishlog-json subprocess to chaperone for access logging to stdout
- Ensure chaperone uses JSON logging (slog.NewJSONHandler) for consistency
- Both log streams intermingled on stdout with distinguishing fields

## Open Questions

- Cross-namespace services: Chaperone needs RBAC to watch EndpointSlices across namespaces
- Config size limits: ghost.json in ConfigMap has 1MB limit
- Reload rate limiting: Add debouncing for rapid HTTPRoute changes?
