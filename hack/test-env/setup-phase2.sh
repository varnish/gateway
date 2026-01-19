#!/bin/bash
# Quick setup script for Phase 2 testing

set -euo pipefail

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}Setting up Phase 2 test environment${NC}"
echo ""

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 1. Deploy test applications
echo -e "${YELLOW}1. Deploying test applications (alpha, beta, gamma, delta, epsilon)...${NC}"
kubectl apply -f "$SCRIPT_DIR/deployments.yaml"
echo ""

# 2. Wait for apps to be ready
echo -e "${YELLOW}2. Waiting for pods to be ready...${NC}"
kubectl wait --for=condition=ready pod -l app=app-alpha --timeout=60s || true
kubectl wait --for=condition=ready pod -l app=app-beta --timeout=60s || true
kubectl wait --for=condition=ready pod -l app=app-gamma --timeout=60s || true
kubectl wait --for=condition=ready pod -l app=app-delta --timeout=60s || true
kubectl wait --for=condition=ready pod -l app=app-epsilon --timeout=60s || true
echo ""

# 3. Check if gateway exists
echo -e "${YELLOW}3. Checking for varnish-gateway...${NC}"
if ! kubectl get gateway varnish-gateway -n default &>/dev/null; then
    echo "Gateway not found. Deploying from deploy/04-sample-gateway.yaml..."
    kubectl apply -f "$SCRIPT_DIR/../../deploy/04-sample-gateway.yaml"
    sleep 5
else
    echo "Gateway already exists"
fi
echo ""

# 4. Deploy Phase 2 routes
echo -e "${YELLOW}4. Deploying Phase 2 routes (path-based routing)...${NC}"
kubectl apply -f "$SCRIPT_DIR/phase2-routes.yaml"
echo ""

# 5. Wait a bit for routes to be processed
echo -e "${YELLOW}5. Waiting for routes to be processed...${NC}"
sleep 5
echo ""

# 6. Show status
echo -e "${GREEN}Deployment complete!${NC}"
echo ""
echo "Resources deployed:"
kubectl get pods -l 'app in (app-alpha,app-beta,app-gamma,app-delta,app-epsilon)'
echo ""
kubectl get svc -l 'app in (app-alpha,app-beta,app-gamma,app-delta,app-epsilon)'
echo ""
kubectl get httproute
echo ""

# 7. Setup instructions
echo -e "${GREEN}Next steps:${NC}"
echo ""
echo "1. Set your gateway IP:"
echo "   export GATEWAY_IP=\$(kubectl get svc varnish-gateway-default -n default -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
echo ""
echo "2. Run Phase 2 tests:"
echo "   $SCRIPT_DIR/test-phase2.sh"
echo ""
echo "3. Manual testing examples (curl):"
echo "   curl -H 'Host: api.example.com' http://\$GATEWAY_IP/api/v1/health | jq"
echo "   curl -H 'Host: blog.example.com' http://\$GATEWAY_IP/posts/123 | jq"
echo ""
echo "   Or with HTTPie:"
echo "   http http://\$GATEWAY_IP/api/v1/health Host:api.example.com"
echo "   http http://\$GATEWAY_IP/posts/123 Host:blog.example.com"
echo "   http http://\$GATEWAY_IP/users/admin Host:mixed.example.com"
echo ""
