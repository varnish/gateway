# Current Deployment Status

## Date: 2026-01-14

## Cluster

- DigitalOcean Kubernetes
- Node: pool-eu1n9e2nq-59rzf (v1.34.1)
- Gateway API CRDs: Installed
- External IP: 24.144.77.118

## Images

Built and pushed to `registry.digitalocean.com/varnish-gateway/`:
- `gateway-operator:v0.1.4` / `:latest`
- `gateway-chaperone:v0.1.2` / `:latest`
- `varnish-ghost:v0.1.2` / `:latest`

## What's Working

1. Operator starts and reconciles Gateway/HTTPRoute resources
2. Operator creates: Deployment, Service, ConfigMap, Secret, ServiceAccount
3. Chaperone starts and loads routing.json
4. Varnish starts and connects to varnishadm (authenticated)
5. VCL loads successfully (with dummy backend)
6. ghost.json is generated with correct backend IPs from EndpointSlices
7. LoadBalancer Service gets external IP
8. **End-to-end routing works!**

## Test Results

```
$ curl -s http://24.144.77.118 -H "Host: alpha.example.com"
{
  "app": "alpha",
  "hostname": "app-alpha-66d95d8c96-2rz7s",
  "path": "/"
}

$ curl -s http://24.144.77.118 -H "Host: beta.example.com"
{
  "app": "beta",
  "hostname": "app-beta-8469fcf8fb-nmrs4",
  "path": "/"
}
```

## Fixes Applied Today

1. **Dummy backend in VCL** - Varnish requires at least one backend declaration
2. **ConfigMap overwrite bug** - Gateway controller was overwriting HTTPRoute's routing.json
3. **ghost.recv() empty string check** - Rust Option<String> returns "" not NULL in VCL

## Test Apps

```
alpha-route: alpha.example.com -> app-alpha:8080
beta-route: beta.example.com -> app-beta:8080
```
