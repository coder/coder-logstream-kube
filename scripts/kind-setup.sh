#!/usr/bin/env bash

# This script sets up a KinD cluster for running integration tests locally.
# Usage: ./scripts/kind-setup.sh [create|delete]

set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-logstream-integration-test}"

usage() {
    echo "Usage: $0 [create|delete|status]"
    echo ""
    echo "Commands:"
    echo "  create  - Create a KinD cluster for integration tests"
    echo "  delete  - Delete the KinD cluster"
    echo "  status  - Check if the cluster exists and is running"
    echo ""
    echo "Environment variables:"
    echo "  KIND_CLUSTER_NAME - Name of the cluster (default: logstream-integration-test)"
    exit 1
}

check_kind() {
    if ! command -v kind &> /dev/null; then
        echo "Error: 'kind' is not installed."
        echo "Install it from: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
        exit 1
    fi
}

check_kubectl() {
    if ! command -v kubectl &> /dev/null; then
        echo "Error: 'kubectl' is not installed."
        echo "Install it from: https://kubernetes.io/docs/tasks/tools/"
        exit 1
    fi
}

cluster_exists() {
    kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"
}

create_cluster() {
    check_kind
    check_kubectl

    if cluster_exists; then
        echo "Cluster '${CLUSTER_NAME}' already exists."
        echo "Use '$0 delete' to remove it first, or '$0 status' to check its status."
        exit 0
    fi

    echo "Creating KinD cluster '${CLUSTER_NAME}'..."
    kind create cluster --name "${CLUSTER_NAME}" --wait 60s

    echo ""
    echo "Cluster created successfully!"
    echo ""
    echo "To run integration tests:"
    echo "  go test -tags=integration -v ./..."
    echo ""
    echo "To delete the cluster when done:"
    echo "  $0 delete"
}

delete_cluster() {
    check_kind

    if ! cluster_exists; then
        echo "Cluster '${CLUSTER_NAME}' does not exist."
        exit 0
    fi

    echo "Deleting KinD cluster '${CLUSTER_NAME}'..."
    kind delete cluster --name "${CLUSTER_NAME}"
    echo "Cluster deleted successfully!"
}

status_cluster() {
    check_kind

    if cluster_exists; then
        echo "Cluster '${CLUSTER_NAME}' exists."
        echo ""
        echo "Cluster info:"
        kubectl cluster-info --context "kind-${CLUSTER_NAME}" 2>/dev/null || echo "  (unable to get cluster info)"
        echo ""
        echo "Nodes:"
        kubectl get nodes --context "kind-${CLUSTER_NAME}" 2>/dev/null || echo "  (unable to get nodes)"
    else
        echo "Cluster '${CLUSTER_NAME}' does not exist."
        echo "Use '$0 create' to create it."
    fi
}

case "${1:-}" in
    create)
        create_cluster
        ;;
    delete)
        delete_cluster
        ;;
    status)
        status_cluster
        ;;
    *)
        usage
        ;;
esac
