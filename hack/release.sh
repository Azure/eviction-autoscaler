#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
source "${SCRIPT_DIR}/common.sh"

commit_sha="$(git rev-parse HEAD)"

RELEASE_ACR="${RELEASE_ACR:-aksmcrimagescommon}"
RELEASE_ACR_FQDN="${RELEASE_ACR}.azurecr.io"
IMAGE_REPO="${RELEASE_ACR_FQDN}/public/aks/eviction-autoscaler"
RELEASE_PLATFORMS="${RELEASE_PLATFORMS:-linux/amd64,linux/arm64}"
BUILDX_BUILDER="${BUILDX_BUILDER:-eviction-autoscaler-release-builder}"
repo_path="public/aks/eviction-autoscaler"  # adjust if your ko publish path changes

# Accept tag as optional parameter, otherwise get from git
if [[ -n "${1:-}" ]]; then
  latest_git_tag="$1"
  # Validate that the provided tag matches X.Y.Z format (numeric components)
  if [[ ! "$latest_git_tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Invalid tag format '$latest_git_tag'. Expected format: X.Y.Z (e.g., 1.2.3)"
    exit 1
  fi
  echo "Using provided tag: $latest_git_tag"
else
  # Get the latest tag from the Git repository
  latest_git_tag=$(git tag -l '[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -n 1 || true)
fi

if [[ -z "$latest_git_tag" ]]; then
  echo "No new tags found - skipping release"
  exit 0
fi

# Use the tag as version (no 'v' prefix to remove)
version="${latest_git_tag}"
echo "Latest Git tag: $latest_git_tag"
echo "Version: $version"

# Check if this version already exists in ACR
set +e
existing_tag=$(az acr repository show-tags -n "$RELEASE_ACR" --repository "$repo_path" -o tsv 2>/dev/null | grep -E "^${version}$" || true)
set -e

if [[ -n "$existing_tag" ]]; then
  echo "Version $version already exists in ACR - skipping release"
  echo "To release a new version, create a new tag:"
  echo "  git tag <new-version>  (e.g., 1.0.1)"
  echo "  git push origin <new-version>"
  exit 1
fi

echo "Releasing eviction-autoscaler"
echo "New version to release: $version"
echo "Commit: $commit_sha"
echo "ACR: $RELEASE_ACR"

epoch_ts="$(git_epoch)"
build_dt="$(build_date "$epoch_ts")"

echo "Building and publishing multi-arch controller image with Docker buildx..."
echo "Platforms: ${RELEASE_PLATFORMS}"
BUILDX_BUILDER_CREATED=false
if ! docker buildx inspect "${BUILDX_BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDX_BUILDER}" --use
  BUILDX_BUILDER_CREATED=true
fi
docker buildx use "${BUILDX_BUILDER}"
docker buildx build --platform "${RELEASE_PLATFORMS}" -t "${IMAGE_REPO}:${version}" --push .
IMG="${IMAGE_REPO}:${version}"
img_digest="$(crane digest "$IMG")"
IMG_REF="${IMAGE_REPO}@${img_digest}"
echo "Image pushed: ${IMG_REF}"

echo "Verifying manifest contains amd64 and arm64..."
docker buildx imagetools inspect "$IMG" | grep -E "linux/(amd64|arm64)"

trivy_scan "$IMG_REF"
cosign_sign "$IMG_REF" "$version" "$commit_sha" "$build_dt"

img_repo="$(echo "$IMG" | cut -d '@' -f 1)"
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

lock_image "$RELEASE_ACR" "$img_path"

if [[ "$BUILDX_BUILDER_CREATED" == "true" ]]; then
  echo "Cleaning up buildx builder: ${BUILDX_BUILDER}"
  docker buildx rm "${BUILDX_BUILDER}" >/dev/null 2>&1 || echo "Warning: failed to remove builder ${BUILDX_BUILDER}"
fi

echo "Release complete: $version"
