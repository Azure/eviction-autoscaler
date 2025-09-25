#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
source "${SCRIPT_DIR}/common.sh"


commit_sha="$(git rev-parse HEAD)"

base_version="0.1"
commit_count=$(git rev-list --count HEAD)
version="${base_version}.${commit_count}"

# Create a tag for the latest commit if it doesn't already have one
if ! git describe --tags --exact-match "$version" >/dev/null 2>&1; then
  git tag "$version"
  git push origin "$version"
  echo "Tagged latest commit as $version"
fi

RELEASE_ACR="${RELEASE_ACR:-aksmcrimagescommon}"
RELEASE_ACR_FQDN="${RELEASE_ACR}.azurecr.io"
IMAGE_REPO="${RELEASE_ACR_FQDN}/public/aks/eviction-autoscaler"

echo "Releasing eviction-autoscaler"
echo "Version: $version"
echo "Commit: $commit_sha"
echo "ACR: $RELEASE_ACR"

epoch_ts="$(git_epoch)"
build_dt="$(build_date "$epoch_ts")"

echo "Building and publishing controller image with ko..."
IMG=$(KO_DOCKER_REPO="$IMAGE_REPO" ko publish -B --sbom none -t "$version" ./cmd)
echo "Image pushed: $IMG"

trivy_scan "$IMG"

img_repo="$(echo "$IMG" | cut -d '@' -f 1)"
img_digest="$(echo "$IMG" | cut -d '@' -f 2)"
img_path="$(echo "$img_repo" | cut -d "/" -f 2-)"

echo "Updating Helm chart values..."
inject_mcr_image "helm/eviction-autoscaler" "$version"

echo "Packaging Helm chart..."
helm dependency update helm/eviction-autoscaler
helm lint helm/eviction-autoscaler
helm package helm/eviction-autoscaler --version "$version" --app-version "$version"

chart_pkg="eviction-autoscaler-$version.tgz"
helm push "$chart_pkg" "oci://$IMAGE_REPO/helm"

# Optional: wait a few seconds for ACR to register the new manifest
sleep 5

# Get digest and sign the chart
chart_ref="${IMAGE_REPO}/helm/eviction-autoscaler:${version}"
chart_digest=$(crane digest "$chart_ref")
echo "Chart pushed: ${chart_ref}@${chart_digest}"
echo "Signing chart..."
cosign sign "${IMAGE_REPO}/helm/eviction-autoscaler@${chart_digest}" --yes

rm -f "$chart_pkg"

lock_image "$RELEASE_ACR" "$img_path" "$version"
echo "Release complete: $version"
