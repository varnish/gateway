#!/bin/bash
# Randomly scales deployments between 1-3 replicas every 15-45 seconds

set -e

DEPLOYMENTS="app-alpha app-beta"

echo "Starting chaos... Ctrl+C to stop"
echo "Watching: $DEPLOYMENTS"
echo ""

while true; do
    for deploy in $DEPLOYMENTS; do
        REPLICAS=$((1 + RANDOM % 3))
        echo "$(date '+%H:%M:%S') Scaling $deploy to $REPLICAS replicas"
        kubectl scale deployment "$deploy" --replicas="$REPLICAS"
    done

    # Wait 15-45 seconds before next change
    WAIT=$((15 + RANDOM % 30))
    echo "$(date '+%H:%M:%S') Waiting ${WAIT}s..."
    echo ""
    sleep $WAIT
done
