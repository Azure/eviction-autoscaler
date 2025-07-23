#!/bin/bash
set -e

# Simple installation script using Helm chart
# This replaces the legacy manual installation with a Helm-based approach

# Default values
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-mcr.microsoft.com/oss/v2/eviction-autoscaler/eviction-autoscaler}"
IMAGE_TAG="${IMAGE_TAG:-v1.0.1}"
NAMESPACE="${NAMESPACE:-kube-system}"

echo "🚀 Installing eviction-autoscaler using Helm..."
echo "   Image: ${IMAGE_REPOSITORY}:${IMAGE_TAG}"
echo "   Namespace: ${NAMESPACE}"
echo

# Install/upgrade using Helm
helm upgrade --install eviction-autoscaler ./helm/eviction-autoscaler \
    --namespace "${NAMESPACE}" --create-namespace \
    --set image.repository="${IMAGE_REPOSITORY}" \
    --set image.tag="${IMAGE_TAG}"

echo
echo "✅ Installation completed!"
echo "   Check status: kubectl get pods -n ${NAMESPACE}"
echo "   View logs: kubectl logs -f deployment/eviction-autoscaler -n ${NAMESPACE}"
