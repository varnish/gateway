#!/usr/bin/env bash
# Install and configure MetalLB in a kind cluster so that Services of type
# LoadBalancer get an IP from the Docker bridge network.
set -euo pipefail

METALLB_VERSION="${METALLB_VERSION:-v0.14.9}"

echo "Installing MetalLB ${METALLB_VERSION}..."
kubectl apply -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml"

echo "Waiting for MetalLB pods..."
kubectl wait --namespace metallb-system \
  --for=condition=ready pod \
  --selector=app=metallb \
  --timeout=120s

# Derive an address range from the kind Docker network subnet.
# The kind network is typically something like 172.x.0.0/16; we use .255.200-.255.250.
SUBNET=$(docker network inspect -f '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' kind \
  | grep -v ':' | head -1)  # skip IPv6, take first IPv4

if [ -z "$SUBNET" ]; then
  echo "ERROR: could not determine kind Docker network subnet" >&2
  exit 1
fi

# Extract first two octets (works for /16 networks kind typically creates)
PREFIX=$(echo "$SUBNET" | cut -d. -f1-2)
RANGE_START="${PREFIX}.255.200"
RANGE_END="${PREFIX}.255.250"

echo "Configuring MetalLB address pool: ${RANGE_START}-${RANGE_END}"
kubectl apply -f - <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kind-pool
  namespace: metallb-system
spec:
  addresses:
  - ${RANGE_START}-${RANGE_END}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kind-l2
  namespace: metallb-system
EOF

echo "MetalLB ready."
