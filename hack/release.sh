#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
source "${SCRIPT_DIR}/common.sh"

commit_sha="$(git rev-parse HEAD)"
version="$(git describe --tags --abbrev=0 || echo "snapshot-${commit_sha:0:7}")"

if [[ -n "$(git status --porcelain)" ]]; then
  echo "Error: working directory has uncommitted changes."
  exit 1
fi

RELEASE_ACR="${RELEASE_ACR:-AKSMCRImagesCommon}"
RELEASE_ACR_FQDN="${RELEASE_ACR}.azurecr.io"
IMAGE_REPO="${RELEASE_ACR_FQDN}/public/aks/eviction-autoscaler"

echo "Releasing eviction-autoscaler"
echo "Version: $version"
echo "Commit: $commit_sha"
echo "ACR: $RELEASE_ACR"

acr_login "$RELEASE_ACR"

epoch_ts="$(git_epoch)"
build_dt="$(build_date "$epoch_ts")"

echo "Building and publishing controller image with ko..."
IMG=$(KO_DOCKER_REPO="$IMAGE_REPO" ko publish -B --sbom none -t "$version" ./cmd/manager)
echo "Image pushed: $IMG"

trivy_scan "$IMG"

img_repo="$(echo "$IMG" | cut -d '@' -f 1)"
img_digest="$(echo "$IMG" | cut -d '@' -f 2)"

echo "Updating Helm chart values..."
inject_mcr_image "helm/eviction-autoscaler" "$version"

echo "Packaging Helm chart..."
helm dependency update helm/eviction-autoscaler
helm lint helm/eviction-autoscaler
helm package helm/eviction-autoscaler --version "$version" --app-version "$version"

chart_pkg="eviction-autoscaler-$version.tgz"
helm push "$chart_pkg" "oci://$IMAGE_REPO/helm"
# Sign the Helm chart in the OCI registry with cosign
cosign sign oci://$IMAGE_REPO/helm:"$version" --yes

rm -f "$chart_pkg"

cosign_sign "$IMG" "$version" "$commit_sha" "$build_dt"
cosign_sign "${IMAGE_REPO}:$version" "$version" "$commit_sha" "$build_dt"

lock_image "$RELEASE_ACR" "$IMAGE_REPO" "$version"
echo "Release complete: $version"
