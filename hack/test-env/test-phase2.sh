#!/bin/bash
# Phase 2 Path-Based Routing Test Script
# Tests exact, prefix, and regex path matching

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Counters
TESTS_PASSED=0
TESTS_FAILED=0

# Get gateway endpoint
# Check environment variable first, then try to discover from kubectl
if [ -n "${GATEWAY_IP:-}" ]; then
    echo -e "${YELLOW}Using GATEWAY_IP from environment: $GATEWAY_IP${NC}"
else
    GATEWAY_SERVICE="varnish-gateway-default"
    GATEWAY_IP=$(kubectl get svc "$GATEWAY_SERVICE" -n default -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")

    if [ -z "$GATEWAY_IP" ]; then
        # Try cluster IP if LoadBalancer IP not available
        GATEWAY_IP=$(kubectl get svc "$GATEWAY_SERVICE" -n default -o jsonpath='{.spec.clusterIP}')
        echo -e "${YELLOW}Using ClusterIP: $GATEWAY_IP${NC}"
        echo -e "${YELLOW}Note: If tests fail, you may need:${NC}"
        echo -e "${YELLOW}  export GATEWAY_IP=<external-ip>${NC}"
        echo -e "${YELLOW}  or port-forward: kubectl port-forward -n default svc/$GATEWAY_SERVICE 8080:80${NC}"
        echo ""
    fi
fi

# Handle port if specified in GATEWAY_IP (e.g., "localhost:8080")
if [[ "$GATEWAY_IP" =~ :[0-9]+$ ]]; then
    GATEWAY_URL="http://${GATEWAY_IP}"
else
    GATEWAY_URL="http://${GATEWAY_IP}"
fi

echo "Testing Phase 2 Path-Based Routing"
echo "Gateway: $GATEWAY_URL"
echo "======================================"
echo ""

# Test function
test_route() {
    local hostname=$1
    local path=$2
    local expected_app=$3
    local description=$4

    echo -n "Testing: $description... "

    response=$(curl -s -H "Host: $hostname" "${GATEWAY_URL}${path}" 2>/dev/null || echo '{"app":"error"}')
    actual_app=$(echo "$response" | jq -r '.app' 2>/dev/null || echo "error")

    if [ "$actual_app" = "$expected_app" ]; then
        echo -e "${GREEN}PASS${NC} (app=$actual_app)"
        ((TESTS_PASSED++))
    else
        echo -e "${RED}FAIL${NC} (expected=$expected_app, got=$actual_app)"
        echo "  Response: $response"
        ((TESTS_FAILED++))
    fi
}

# Wait for gateway to be ready
echo "Waiting for gateway to be ready..."
sleep 2

echo ""
echo "==================================="
echo "Test Suite 1: api.example.com"
echo "Path prefix matching with specificity"
echo "==================================="
test_route "api.example.com" "/api/v1/health" "alpha" "Exact match: /api/v1/health → alpha"
test_route "api.example.com" "/api/v2/users" "beta" "Prefix /api/v2/ → beta"
test_route "api.example.com" "/api/v2/products" "beta" "Prefix /api/v2/ → beta"
test_route "api.example.com" "/api/v1/users" "gamma" "Prefix /api/v1/ → gamma"
test_route "api.example.com" "/api/v1/products" "gamma" "Prefix /api/v1/ → gamma"
test_route "api.example.com" "/api/legacy" "delta" "Prefix /api/ → delta"
test_route "api.example.com" "/api/other" "delta" "Prefix /api/ → delta"
test_route "api.example.com" "/docs" "epsilon" "No match, default → epsilon"
test_route "api.example.com" "/" "epsilon" "Root path, default → epsilon"

echo ""
echo "==================================="
echo "Test Suite 2: static.example.com"
echo "Exact path matching"
echo "==================================="
test_route "static.example.com" "/index.html" "alpha" "Exact: /index.html → alpha"
test_route "static.example.com" "/about.html" "beta" "Exact: /about.html → beta"
test_route "static.example.com" "/contact.html" "gamma" "Exact: /contact.html → gamma"
test_route "static.example.com" "/assets/style.css" "delta" "Prefix: /assets/ → delta"
test_route "static.example.com" "/assets/js/app.js" "delta" "Prefix: /assets/ → delta"
test_route "static.example.com" "/other.html" "epsilon" "No match, default → epsilon"

echo ""
echo "==================================="
echo "Test Suite 3: blog.example.com"
echo "Regex path matching"
echo "==================================="
test_route "blog.example.com" "/admin" "alpha" "Exact: /admin → alpha"
test_route "blog.example.com" "/posts/123" "beta" "Regex numeric ID: /posts/123 → beta"
test_route "blog.example.com" "/posts/456789" "beta" "Regex numeric ID: /posts/456789 → beta"
test_route "blog.example.com" "/posts/550e8400-e29b-41d4-a716-446655440000" "gamma" "Regex UUID: /posts/{uuid} → gamma"
test_route "blog.example.com" "/posts/draft" "delta" "Prefix fallback: /posts/draft → delta"
test_route "blog.example.com" "/posts/" "delta" "Prefix: /posts/ → delta"
test_route "blog.example.com" "/categories/tech" "epsilon" "Prefix: /categories/ → epsilon"
test_route "blog.example.com" "/" "alpha" "Root default → alpha"

echo ""
echo "==================================="
echo "Test Suite 4: mixed.example.com"
echo "Route ordering with overlapping paths"
echo "==================================="
test_route "mixed.example.com" "/users/admin" "alpha" "Exact most specific: /users/admin → alpha"
test_route "mixed.example.com" "/users/123" "beta" "Regex: /users/123 → beta"
test_route "mixed.example.com" "/users/456" "beta" "Regex: /users/456 → beta"
test_route "mixed.example.com" "/users/john" "gamma" "Prefix fallback: /users/john → gamma"
test_route "mixed.example.com" "/users/" "gamma" "Prefix: /users/ → gamma"
test_route "mixed.example.com" "/products" "delta" "Root prefix: /products → delta"
test_route "mixed.example.com" "/" "delta" "Root: / → delta"

echo ""
echo "==================================="
echo "Test Results"
echo "==================================="
echo -e "Total tests: $((TESTS_PASSED + TESTS_FAILED))"
echo -e "${GREEN}Passed: $TESTS_PASSED${NC}"
echo -e "${RED}Failed: $TESTS_FAILED${NC}"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}All tests passed!${NC}"
    exit 0
else
    echo -e "${RED}Some tests failed.${NC}"
    exit 1
fi
