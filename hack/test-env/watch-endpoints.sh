#!/bin/bash
# Watch EndpointSlices for our test services

echo "Watching EndpointSlices for app-alpha and app-beta"
echo "Press Ctrl+C to stop"
echo ""

kubectl get endpointslices \
    -l 'kubernetes.io/service-name in (app-alpha, app-beta)' \
    --watch \
    -o custom-columns=\
'NAME:.metadata.name,SERVICE:.metadata.labels.kubernetes\.io/service-name,ENDPOINTS:.endpoints[*].addresses[*],READY:.endpoints[*].conditions.ready'
