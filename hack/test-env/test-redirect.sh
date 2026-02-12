#!/bin/bash
# RequestRedirect E2E Test Script
# Tests redirect responses (status codes, Location headers)

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
    GATEWAY_SERVICE="varnish-gateway"

    GATEWAY_IP=$(kubectl get svc "$GATEWAY_SERVICE" -n varnish-gateway-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || echo "")

    if [ -z "$GATEWAY_IP" ]; then
        # Try cluster IP if LoadBalancer IP not available
        GATEWAY_IP=$(kubectl get svc "$GATEWAY_SERVICE" -n varnish-gateway-system -o jsonpath='{.spec.clusterIP}')
        echo -e "${YELLOW}Using ClusterIP: $GATEWAY_IP${NC}"
        echo -e "${YELLOW}Note: If tests fail, you may need:${NC}"
        echo -e "${YELLOW}  export GATEWAY_IP=<external-ip>${NC}"
        echo -e "${YELLOW}  or port-forward: kubectl port-forward -n varnish-gateway-system svc/$GATEWAY_SERVICE 8080:80${NC}"
        echo ""
    fi
fi

# Handle port if specified in GATEWAY_IP (e.g., "localhost:8080")
if [[ "$GATEWAY_IP" =~ :[0-9]+$ ]]; then
    GATEWAY_URL="http://${GATEWAY_IP}"
else
    GATEWAY_URL="http://${GATEWAY_IP}"
fi

echo "Testing RequestRedirect Filter"
echo "Gateway: $GATEWAY_URL"
echo "======================================"
echo ""

# Test function for redirect responses
test_redirect() {
    local hostname=$1
    local path=$2
    local expected_status=$3
    local expected_location=$4
    local description=$5

    echo -n "Testing: $description... "

    # Get full HTTP response including headers
    response=$(curl -i -s -H "Host: $hostname" "${GATEWAY_URL}${path}" 2>/dev/null || echo "HTTP/1.1 000 Error")

    # Extract status code from first line (e.g., "HTTP/1.1 301 Moved Permanently")
    actual_status=$(echo "$response" | head -n 1 | grep -oP 'HTTP/[0-9.]+ \K[0-9]+' || echo "000")

    # Extract Location header (case-insensitive)
    actual_location=$(echo "$response" | grep -i '^Location:' | sed 's/^[Ll]ocation: *//' | tr -d '\r\n' || echo "")

    # Check both status and location
    status_match=false
    location_match=false

    if [ "$actual_status" = "$expected_status" ]; then
        status_match=true
    fi

    if [ "$actual_location" = "$expected_location" ]; then
        location_match=true
    fi

    if $status_match && $location_match; then
        echo -e "${GREEN}PASS${NC} (status=$actual_status, location=$actual_location)"
        ((TESTS_PASSED++))
    else
        echo -e "${RED}FAIL${NC}"
        if ! $status_match; then
            echo "  Status: expected=$expected_status, got=$actual_status"
        fi
        if ! $location_match; then
            echo "  Location: expected=$expected_location, got=$actual_location"
        fi
        echo "  Full response (first 10 lines):"
        echo "$response" | head -n 10 | sed 's/^/    /'
        ((TESTS_FAILED++))
    fi
}

# Wait for gateway to be ready
echo "Waiting for gateway to be ready..."
kubectl wait --for=condition=Programmed gateway/varnish-gateway -n varnish-gateway-system --timeout=60s 2>/dev/null || echo -e "${YELLOW}Warning: Gateway condition check failed, proceeding anyway${NC}"
sleep 2

echo ""
echo "==================================="
echo "Test Suite: RequestRedirect Filter"
echo "==================================="

# Test 1: Basic scheme redirect
test_redirect \
    "test-scheme.example.com" \
    "/path" \
    "301" \
    "https://test-scheme.example.com/path" \
    "Scheme redirect (http → https)"

# Test 2: Hostname redirect
test_redirect \
    "old.example.com" \
    "/api" \
    "301" \
    "http://new.example.com/api" \
    "Hostname redirect (old → new)"

# Test 3: Port redirect with default omission
test_redirect \
    "test-port.example.com" \
    "/api" \
    "301" \
    "http://test-port.example.com/api" \
    "Port redirect (port 80 omitted)"

# Test 4: HTTPS default port omission
test_redirect \
    "test-https-port.example.com" \
    "/api" \
    "301" \
    "https://test-https-port.example.com/api" \
    "HTTPS port redirect (port 443 omitted)"

# Test 5: Full path replacement
test_redirect \
    "test-fullpath.example.com" \
    "/old/path" \
    "301" \
    "http://test-fullpath.example.com/new/path" \
    "Full path replacement"

# Test 6: Prefix replacement
test_redirect \
    "test-prefix.example.com" \
    "/v1/api/users" \
    "301" \
    "http://test-prefix.example.com/v2/api/users" \
    "Prefix replacement (/v1 → /v2)"

# Test 7: Query string preservation
test_redirect \
    "test-query.example.com" \
    "/api?token=abc123&user=test" \
    "301" \
    "https://test-query.example.com/api?token=abc123&user=test" \
    "Query string preservation"

# Test 8: Percent-encoding preservation
test_redirect \
    "test-encoding.example.com" \
    "/path%20with%20spaces" \
    "301" \
    "https://test-encoding.example.com/path%20with%20spaces" \
    "Percent-encoding preservation"

# Test 9: Status code 302
test_redirect \
    "test-302.example.com" \
    "/api" \
    "302" \
    "https://test-302.example.com/api" \
    "Status code 302 (Found)"

# Test 10: Combined redirect (all fields)
test_redirect \
    "combined-old.example.com" \
    "/v1/api?token=abc" \
    "301" \
    "https://combined-new.example.com:9443/v2/api?token=abc" \
    "Combined redirect (scheme+hostname+port+path)"

# Test 11a: Route precedence (exact match)
test_redirect \
    "precedence.example.com" \
    "/api/v1/special" \
    "302" \
    "https://precedence.example.com/api/v1/special" \
    "Route precedence (exact match → 302)"

# Test 11b: Route precedence (prefix match)
test_redirect \
    "precedence.example.com" \
    "/api/v1/other" \
    "301" \
    "https://precedence.example.com/api/v1/other" \
    "Route precedence (prefix match → 301)"

# Test 12: Custom port (non-default)
test_redirect \
    "custom-port.example.com" \
    "/api" \
    "301" \
    "https://custom-port.example.com:8443/api" \
    "Custom port (8443 included)"

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
