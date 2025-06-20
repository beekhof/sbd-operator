#!/bin/bash

# Deployment script for SBD Agent Kubernetes resources
# This script deploys the SBD Agent DaemonSet and related resources

set -e

# Configuration
NAMESPACE="sbd-system"
DEPLOYMENT_NAME="sbd-agent"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to check if kubectl is available
check_kubectl() {
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is not installed or not in PATH"
        exit 1
    fi
    
    if ! kubectl cluster-info &> /dev/null; then
        log_error "kubectl cannot connect to Kubernetes cluster"
        exit 1
    fi
    
    log_success "kubectl is configured and cluster is accessible"
}

# Function to deploy resources
deploy_resources() {
    log_info "Deploying SBD Agent resources..."
    
    # Create namespace first
    log_info "Creating namespace: ${NAMESPACE}"
    kubectl apply -f deploy/sbd-system-namespace.yaml
    
    # Wait for namespace to be ready
    log_info "Waiting for namespace to be ready..."
    sleep 2  # Brief pause to ensure namespace is fully created
    
    # Deploy the DaemonSet and related resources
    log_info "Deploying SBD Agent DaemonSet..."
    kubectl apply -f deploy/sbd-agent-daemonset-simple.yaml
    
    log_success "Resources deployed successfully"
}

# Function to check deployment status
check_status() {
    log_info "Checking deployment status..."
    
    # Check namespace
    if kubectl get namespace ${NAMESPACE} &> /dev/null; then
        log_success "Namespace ${NAMESPACE} exists"
    else
        log_error "Namespace ${NAMESPACE} does not exist"
        return 1
    fi
    
    # Check DaemonSet
    if kubectl get daemonset ${DEPLOYMENT_NAME} -n ${NAMESPACE} &> /dev/null; then
        log_success "DaemonSet ${DEPLOYMENT_NAME} exists"
        
        # Show DaemonSet status
        echo ""
        log_info "DaemonSet status:"
        kubectl get daemonset ${DEPLOYMENT_NAME} -n ${NAMESPACE}
        
        # Show pod status
        echo ""
        log_info "Pod status:"
        kubectl get pods -n ${NAMESPACE} -l app=${DEPLOYMENT_NAME}
        
        # Check if all pods are ready
        local desired=$(kubectl get daemonset ${DEPLOYMENT_NAME} -n ${NAMESPACE} -o jsonpath='{.status.desiredNumberScheduled}')
        local ready=$(kubectl get daemonset ${DEPLOYMENT_NAME} -n ${NAMESPACE} -o jsonpath='{.status.numberReady}')
        
        if [ "${desired}" = "${ready}" ]; then
            log_success "All pods are ready (${ready}/${desired})"
        else
            log_warning "Not all pods are ready (${ready}/${desired})"
        fi
    else
        log_error "DaemonSet ${DEPLOYMENT_NAME} does not exist"
        return 1
    fi
}

# Function to show logs
show_logs() {
    log_info "Showing recent logs from SBD Agent pods..."
    
    local pods=$(kubectl get pods -n ${NAMESPACE} -l app=${DEPLOYMENT_NAME} -o jsonpath='{.items[*].metadata.name}')
    
    if [ -z "${pods}" ]; then
        log_warning "No SBD Agent pods found"
        return 1
    fi
    
    for pod in ${pods}; do
        echo ""
        log_info "Logs from pod: ${pod}"
        echo "----------------------------------------"
        kubectl logs ${pod} -n ${NAMESPACE} --tail=20 || log_warning "Could not get logs from ${pod}"
    done
}

# Function to delete resources
delete_resources() {
    log_info "Deleting SBD Agent resources..."
    
    # Delete DaemonSet and related resources
    if kubectl get -f deploy/sbd-agent-daemonset-simple.yaml &> /dev/null; then
        kubectl delete -f deploy/sbd-agent-daemonset-simple.yaml
        log_success "DaemonSet and RBAC resources deleted"
    else
        log_warning "DaemonSet resources not found"
    fi
    
    # Delete namespace (optional, commented out by default)
    # kubectl delete -f deploy/sbd-system-namespace.yaml
    # log_success "Namespace deleted"
    
    log_warning "Namespace ${NAMESPACE} was not deleted. Delete manually if needed."
}

# Function to wait for rollout
wait_for_rollout() {
    log_info "Waiting for DaemonSet rollout to complete..."
    
    if kubectl rollout status daemonset/${DEPLOYMENT_NAME} -n ${NAMESPACE} --timeout=300s; then
        log_success "DaemonSet rollout completed successfully"
    else
        log_error "DaemonSet rollout failed or timed out"
        return 1
    fi
}

# Function to show usage
show_usage() {
    echo "Usage: $0 [COMMAND]"
    echo ""
    echo "Commands:"
    echo "  deploy    Deploy SBD Agent resources (default)"
    echo "  status    Check deployment status"
    echo "  logs      Show logs from SBD Agent pods"
    echo "  delete    Delete SBD Agent resources"
    echo "  wait      Wait for rollout to complete"
    echo "  help      Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 deploy"
    echo "  $0 status"
    echo "  $0 logs"
}

# Parse command line arguments
COMMAND=${1:-"deploy"}

case ${COMMAND} in
    deploy)
        log_info "SBD Agent Deployment Script"
        echo ""
        check_kubectl
        deploy_resources
        wait_for_rollout
        check_status
        ;;
    status)
        check_kubectl
        check_status
        ;;
    logs)
        check_kubectl
        show_logs
        ;;
    delete)
        check_kubectl
        delete_resources
        ;;
    wait)
        check_kubectl
        wait_for_rollout
        ;;
    help|--help|-h)
        show_usage
        ;;
    *)
        log_error "Unknown command: ${COMMAND}"
        show_usage
        exit 1
        ;;
esac

log_success "Script completed successfully!" 