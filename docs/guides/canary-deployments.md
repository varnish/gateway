# Canary Deployments

## Overview

The Varnish Gateway supports weighted traffic splitting via the standard Gateway API `weight` field in `BackendRef`. Use this for canary deployments and A/B testing.

## Weight Calculation

Weights are not percentages Traffic distribution:

```
backend_probability = backend_weight / sum(all_weights)
```

Example: weights 90/10 → 90% and 10% traffic split

When a service has multiple pods, each pod inherits the same weight. With 2 stable pods (weight 90) and 2 canary pods (weight 10), you get total weights of 180/20 = 90%/10% split. The ratio is preserved.

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

## Complete working example

The manifest below deploys a Gateway, two backend Deployments (`app-stable` running
`hashicorp/http-echo` with text `stable-v1.0`, and `app-canary` with text
`canary-v2.0`), their Services, and an HTTPRoute that splits traffic 90/10.

```yaml
---
# Gateway (if not already deployed)
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: varnish-gateway
  namespace: default
spec:
  gatewayClassName: varnish
  listeners:
    - name: http
      protocol: HTTP
      port: 80

---
# Stable backend deployment (version 1.0)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-stable
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: myapp
      version: stable
  template:
    metadata:
      labels:
        app: myapp
        version: stable
    spec:
      containers:
        - name: app
          image: hashicorp/http-echo
          args:
            - "-text=stable-v1.0"
            - "-listen=:8080"
          ports:
            - containerPort: 8080

---
# Canary backend deployment (version 2.0)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-canary
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: myapp
      version: canary
  template:
    metadata:
      labels:
        app: myapp
        version: canary
    spec:
      containers:
        - name: app
          image: hashicorp/http-echo
          args:
            - "-text=canary-v2.0"
            - "-listen=:8080"
          ports:
            - containerPort: 8080

---
# Stable service
apiVersion: v1
kind: Service
metadata:
  name: app-stable
  namespace: default
spec:
  selector:
    app: myapp
    version: stable
  ports:
    - port: 8080
      targetPort: 8080

---
# Canary service
apiVersion: v1
kind: Service
metadata:
  name: app-canary
  namespace: default
spec:
  selector:
    app: myapp
    version: canary
  ports:
    - port: 8080
      targetPort: 8080

---
# HTTPRoute with weighted traffic split (90% stable, 10% canary)
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: canary-route
  namespace: default
spec:
  parentRefs:
    - name: varnish-gateway
      namespace: default
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

Save as `canary.yaml` and apply with `kubectl apply -f canary.yaml`. Verify the split:

```bash
# Get the gateway service IP
kubectl get svc -n default varnish-gateway

# Make 100 requests and count responses
for i in {1..100}; do curl -H "Host: app.example.com" http://<GATEWAY-IP>/; done | sort | uniq -c
# Expect approximately 90 stable-v1.0 responses and 10 canary-v2.0 responses
```
