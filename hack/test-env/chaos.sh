#!/bin/bash
# Interactive chaos injection for testing endpoint churn
# Run this to manually trigger failures in the test environment

set -e

DEPLOYMENTS="app-alpha app-beta"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

show_status() {
    echo -e "${BLUE}=== Current Status ===${NC}"
    kubectl get pods -l 'app in (app-alpha, app-beta)' -o wide 2>/dev/null || echo "No pods found"
    echo ""
}

kill_random_pod() {
    local app=$1
    if [ -z "$app" ]; then
        # Pick random app
        if [ $((RANDOM % 2)) -eq 0 ]; then
            app="app-alpha"
        else
            app="app-beta"
        fi
    fi

    local pod=$(kubectl get pods -l "app=$app" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$pod" ]; then
        echo -e "${RED}Killing pod: $pod${NC}"
        kubectl delete pod "$pod" --grace-period=0 --force 2>/dev/null || true
    else
        echo -e "${YELLOW}No pods found for $app${NC}"
    fi
}

kill_specific_pod() {
    echo -e "${BLUE}Available pods:${NC}"
    local pods=$(kubectl get pods -l 'app in (app-alpha, app-beta)' -o jsonpath='{.items[*].metadata.name}')
    local i=1
    local pod_array=()
    for pod in $pods; do
        echo "  $i) $pod"
        pod_array+=("$pod")
        ((i++))
    done

    if [ ${#pod_array[@]} -eq 0 ]; then
        echo -e "${YELLOW}No pods available${NC}"
        return
    fi

    echo -n "Select pod number (or 'c' to cancel): "
    read -r selection

    if [ "$selection" = "c" ]; then
        return
    fi

    if [[ "$selection" =~ ^[0-9]+$ ]] && [ "$selection" -ge 1 ] && [ "$selection" -le ${#pod_array[@]} ]; then
        local pod="${pod_array[$((selection-1))]}"
        echo -e "${RED}Killing pod: $pod${NC}"
        kubectl delete pod "$pod" --grace-period=0 --force 2>/dev/null || true
    else
        echo -e "${YELLOW}Invalid selection${NC}"
    fi
}

scale_deployment() {
    echo -e "${BLUE}Scale which deployment?${NC}"
    echo "  1) app-alpha"
    echo "  2) app-beta"
    echo "  3) Both"
    echo -n "Select (or 'c' to cancel): "
    read -r deploy_choice

    if [ "$deploy_choice" = "c" ]; then
        return
    fi

    echo -n "Number of replicas (0-5): "
    read -r replicas

    if ! [[ "$replicas" =~ ^[0-5]$ ]]; then
        echo -e "${YELLOW}Invalid replica count${NC}"
        return
    fi

    case $deploy_choice in
        1)
            echo -e "${YELLOW}Scaling app-alpha to $replicas replicas${NC}"
            kubectl scale deployment app-alpha --replicas="$replicas"
            ;;
        2)
            echo -e "${YELLOW}Scaling app-beta to $replicas replicas${NC}"
            kubectl scale deployment app-beta --replicas="$replicas"
            ;;
        3)
            echo -e "${YELLOW}Scaling both to $replicas replicas${NC}"
            kubectl scale deployment app-alpha app-beta --replicas="$replicas"
            ;;
        *)
            echo -e "${YELLOW}Invalid selection${NC}"
            ;;
    esac
}

continuous_chaos() {
    echo -e "${YELLOW}Starting gentle continuous chaos...${NC}"
    echo "This will kill one random pod every 30-60 seconds."
    echo "Press Ctrl+C to stop."
    echo ""

    trap 'echo -e "\n${GREEN}Chaos stopped.${NC}"; return' INT

    while true; do
        show_status
        kill_random_pod
        local wait=$((30 + RANDOM % 30))
        echo -e "${BLUE}Next chaos in ${wait}s...${NC}"
        sleep $wait
    done
}

rolling_chaos() {
    echo -e "${YELLOW}Starting rolling chaos...${NC}"
    echo "This alternates between scaling deployments up and down."
    echo "Press Ctrl+C to stop."
    echo ""

    trap 'echo -e "\n${GREEN}Chaos stopped. Resetting to 2 replicas each.${NC}"; kubectl scale deployment app-alpha app-beta --replicas=2; return' INT

    local phase=0
    while true; do
        show_status
        case $((phase % 4)) in
            0)
                echo -e "${YELLOW}Phase: Scale app-alpha down${NC}"
                kubectl scale deployment app-alpha --replicas=1
                ;;
            1)
                echo -e "${YELLOW}Phase: Scale app-beta up${NC}"
                kubectl scale deployment app-beta --replicas=3
                ;;
            2)
                echo -e "${YELLOW}Phase: Scale app-alpha up, app-beta down${NC}"
                kubectl scale deployment app-alpha --replicas=3
                kubectl scale deployment app-beta --replicas=1
                ;;
            3)
                echo -e "${YELLOW}Phase: Reset both${NC}"
                kubectl scale deployment app-alpha app-beta --replicas=2
                ;;
        esac
        ((phase++))
        echo -e "${BLUE}Next phase in 20s...${NC}"
        sleep 20
    done
}

reset_environment() {
    echo -e "${GREEN}Resetting environment to stable state...${NC}"
    kubectl scale deployment app-alpha app-beta --replicas=2
    echo "Done. Both deployments scaled to 2 replicas."
}

show_menu() {
    echo ""
    echo -e "${BLUE}=== Chaos Menu ===${NC}"
    echo "  1) Kill a random pod"
    echo "  2) Kill a specific pod"
    echo "  3) Scale a deployment"
    echo "  4) Start gentle continuous chaos"
    echo "  5) Start rolling chaos (scale up/down)"
    echo "  6) Reset to stable state (2 replicas each)"
    echo "  s) Show current status"
    echo "  q) Quit"
    echo ""
    echo -n "Select option: "
}

main() {
    echo -e "${BLUE}Chaos Injection Tool${NC}"
    echo "Use this to test endpoint churn handling."
    echo ""

    show_status

    while true; do
        show_menu
        read -r choice

        case $choice in
            1)
                kill_random_pod
                ;;
            2)
                kill_specific_pod
                ;;
            3)
                scale_deployment
                ;;
            4)
                continuous_chaos
                ;;
            5)
                rolling_chaos
                ;;
            6)
                reset_environment
                ;;
            s|S)
                show_status
                ;;
            q|Q)
                echo -e "${GREEN}Goodbye!${NC}"
                exit 0
                ;;
            *)
                echo -e "${YELLOW}Invalid option${NC}"
                ;;
        esac
    done
}

# Allow running specific commands directly
case "${1:-}" in
    --kill)
        kill_random_pod "$2"
        ;;
    --scale)
        kubectl scale deployment "${2:-app-alpha}" --replicas="${3:-2}"
        ;;
    --reset)
        reset_environment
        ;;
    --status)
        show_status
        ;;
    --continuous)
        continuous_chaos
        ;;
    *)
        main
        ;;
esac
