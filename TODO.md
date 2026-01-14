# TODO

## HTTPRoute Features

- Header matching (`rule.Matches[].Headers`)
- Method matching (`rule.Matches[].Method`)
- Query parameter matching (`rule.Matches[].QueryParams`)
- Traffic splitting (weighted backends)
- URL rewriting filters
- Request/response header modification filters

## Gateway Features

- Listener hostname filtering (routes should filter by listener)
- sectionName matching (`parentRef.SectionName`)
- Cross-namespace routes (ReferenceGrant validation)

## TLS

- Listener TLS termination (watch `certificateRefs` Secrets)
- Certificate hot-reload on Secret changes
- BackendTLSPolicy support (upstream TLS to backends)

Note: In k8s, cert-manager handles ACME. We just consume `kubernetes.io/tls` Secrets.
