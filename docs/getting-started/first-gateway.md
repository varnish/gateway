# First Gateway

## Where should the Gateway live?

The Gateway resource's namespace is where Varnish actually runs — the operator creates the Varnish Deployment in the same namespace as the Gateway. This walkthrough uses `default` and puts the Gateway alongside the application (the simplest pattern). A shared Gateway in a platform namespace, with HTTPRoutes attaching from app namespaces, is also supported. See [Gateway Topology](../concepts/gateway-topology.md) for the tradeoffs.

## Prerequisites

- Varnish Gateway installed in the cluster (see [Installation](installation.md))
- A backend Service to route traffic to (any HTTP service will do)

Verify the GatewayClass is available:

```bash
kubectl get gatewayclass
```

You should see:

```
NAME      CONTROLLER                      ACCEPTED   AGE
varnish   varnish-software.com/gateway    True       1m
```

## Create a Gateway

A Gateway defines listeners; the ports and protocols Varnish accepts traffic on. Note that you can place
the gateway in the namespace `varnish-gateway-system` or alongside your application.

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: my-gateway
  namespace: default
spec:
  gatewayClassName: varnish
  listeners:
    - name: http
      protocol: HTTP
      port: 80
EOF
```

Wait for the Gateway to be programmed:

```bash
kubectl wait --for=condition=Programmed gateway/my-gateway -n default --timeout=60s
```

Check the status:

```bash
kubectl get gateway my-gateway -n default
```

You should see:

```
NAME         CLASS     ADDRESS         PROGRAMMED   AGE
my-gateway   varnish   10.96.xxx.xxx   True         1m
```

## Create an HTTPRoute

An HTTPRoute binds hostnames and paths to a backend Service:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-route
  namespace: default
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - "example.com"
  rules:
    - backendRefs:
        - name: my-service
          port: 8080
EOF
```

Check the status:

```bash
kubectl get httproute my-route -n default
```

## Verify Traffic Flows

Port-forward to the Gateway and send a request:

```bash
kubectl port-forward -n default svc/my-gateway 8080:80 &
curl -H "Host: example.com" http://localhost:8080/
```

The request should reach `my-service` via Varnish.

## Troubleshooting

If the Gateway or HTTPRoute is not accepted, inspect its conditions:

```bash
kubectl describe gateway my-gateway -n default
kubectl describe httproute my-route -n default
```

For operator-level issues, see [Troubleshooting](../operations/troubleshooting.md).

## Next Steps

- [Custom VCL](../guides/custom-vcl.md) — add your own VCL logic
- [TLS](../guides/tls.md) — terminate HTTPS at the Gateway
- [Canary Deployments](../guides/canary-deployments.md) — split traffic between backends
- [Cache Invalidation](../guides/cache-invalidation.md) — purge and ban cached objects
