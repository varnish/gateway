# Current Deployment Status

## Date: 2026-01-14

## Cluster

- DigitalOcean Kubernetes
- Node: pool-eu1n9e2nq-59rzf (v1.34.1)
- Gateway API CRDs: Installed
- External IP: 24.144.77.118

## Images

Built and pushed to `registry.digitalocean.com/varnish-gateway/`:
- `gateway-operator:v0.1.2` / `:latest`
- `gateway-chaperone:v0.1.2` / `:latest`
- `varnish-ghost:v0.1.2` / `:latest`

## What's Working

1. Operator starts and reconciles Gateway/HTTPRoute resources
2. Operator creates: Deployment, Service, ConfigMap, Secret, ServiceAccount
3. Chaperone starts and loads routing.json
4. Varnish starts and connects to varnishadm (authenticated)
5. ghost.json is generated with correct backend IPs from EndpointSlices
6. LoadBalancer Service gets external IP

## Recent Fix: Initial VCL Loading

Fixed the startup sequence so VCL is loaded before the child starts:

1. Start varnishd (manager only, `-f ""`)
2. Wait for varnishadm connection
3. Load VCL via `vcl.load` + `vcl.use`
4. Start child via `start` command
5. Wait for child ready signal

**Files changed:**
- `internal/varnishadm/varnishadm.go` - Added `Connected()` channel
- `internal/varnishadm/interface.go` - Added `Connected()` to interface
- `internal/varnishadm/mock.go` - Added `Connected()` + start/stop responses
- `cmd/chaperone/main.go` - Rewrote startup sequence
- `internal/vrun/process_test.go` - Updated integration test

## Next Steps

1. [x] Build and push new Docker images (v0.1.2)
2. [ ] Deploy to cluster and verify end-to-end routing works
3. [ ] Test: `curl http://24.144.77.118 -H "Host: alpha.example.com"`
4. [ ] Commit all changes

## Test Apps

```
alpha-route: alpha.example.com -> app-alpha:8080
beta-route: beta.example.com -> app-beta:8080
```
