# Quick Phase 2 Test Reference

## Setup
```bash
export GATEWAY_IP=24.144.77.118  # your gateway IP
```

## Test Commands (HTTPie)

### Exact Path Match
```bash
http http://$GATEWAY_IP/api/v1/health Host:api.example.com
# Expected: {"app": "alpha", ...}
```

### Prefix Matching (Specificity)
```bash
http http://$GATEWAY_IP/api/v2/users Host:api.example.com
# Expected: {"app": "beta", ...}

http http://$GATEWAY_IP/api/v1/products Host:api.example.com
# Expected: {"app": "gamma", ...}

http http://$GATEWAY_IP/api/legacy Host:api.example.com
# Expected: {"app": "delta", ...}

http http://$GATEWAY_IP/docs Host:api.example.com
# Expected: {"app": "epsilon", ...}
```

### Regex Matching - Numeric IDs
```bash
http http://$GATEWAY_IP/posts/123 Host:blog.example.com
# Expected: {"app": "beta", ...}

http http://$GATEWAY_IP/posts/999 Host:blog.example.com
# Expected: {"app": "beta", ...}
```

### Regex Matching - UUIDs
```bash
http http://$GATEWAY_IP/posts/550e8400-e29b-41d4-a716-446655440000 Host:blog.example.com
# Expected: {"app": "gamma", ...}
```

### Route Ordering
```bash
# Exact match (highest priority)
http http://$GATEWAY_IP/users/admin Host:mixed.example.com
# Expected: {"app": "alpha", ...}

# Regex match (second priority)
http http://$GATEWAY_IP/users/123 Host:mixed.example.com
# Expected: {"app": "beta", ...}

# Prefix match (lower priority)
http http://$GATEWAY_IP/users/john Host:mixed.example.com
# Expected: {"app": "gamma", ...}
```

### Static Files
```bash
http http://$GATEWAY_IP/index.html Host:static.example.com
# Expected: {"app": "alpha", ...}

http http://$GATEWAY_IP/about.html Host:static.example.com
# Expected: {"app": "beta", ...}

http http://$GATEWAY_IP/assets/style.css Host:static.example.com
# Expected: {"app": "delta", ...}
```

## Full Test Suite
```bash
./hack/test-env/test-phase2.sh
```

## See All Routes
```bash
kubectl get httproute
kubectl describe httproute api-routes
kubectl describe httproute blog-routes
kubectl describe httproute mixed-routes
kubectl describe httproute static-routes
```
