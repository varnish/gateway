# Canary Deployments

## Overview

The Varnish Gateway supports weighted traffic splitting via the standard Gateway API `weight` field in `BackendRef`. Use this for canary deployments and A/B testing.

## Weight Calculation

Weights are **not percentages**. Traffic distribution:

```
backend_probability = backend_weight / sum(all_weights)
```

Example: weights 90/10 → 90% and 10% traffic split

**Important:** When a service has multiple pods, each pod inherits the same weight. With 2 stable pods (weight 90) and 2 canary pods (weight 10), you get total weights of 180/20 = 90%/10% split. The ratio is preserved.

## Example

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: canary-route
spec:
  parentRefs:
    - name: varnish-gateway
  hostnames:
    - "app.example.com"
  rules:
    - backendRefs:
        - name: app-stable
          port: 8080
          weight: 90
        - name: app-canary
          port: 8080
          weight: 10
```

This routes 90% of requests to `app-stable` service and 10% to `app-canary`.

## Progressive Rollout

Gradually increase canary weight:

1. Start: `weight: 1` (1% traffic)
2. If stable: `weight: 10` (10% traffic)
3. If stable: `weight: 25` (25% traffic)
4. If stable: `weight: 50` (50% traffic)
5. Complete: Remove stable backend or flip weights

## Testing

```bash
# Make 100 requests and count responses
for i in {1..100}; do curl -H "Host: app.example.com" http://GATEWAY_IP/; done | sort | uniq -c
```

## Monitoring

```bash
# Check backend health and request counts
kubectl exec -it deploy/varnish-gateway -- varnishadm backend.list -p
```

See `docs/examples/canary-deployment.yaml` for a complete working example.
