#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
source "${SCRIPT_DIR}/common.sh"

commit_sha="$(git rev-parse HEAD)"

RELEASE_ACR="${RELEASE_ACR:-aksmcrimagescommon}"
RELEASE_ACR_FQDN="${RELEASE_ACR}.azurecr.io"
IMAGE_REPO="${RELEASE_ACR_FQDN}/public/aks/eviction-autoscaler"
repo_path="public/aks/eviction-autoscaler/cmd"  # adjust if your ko publish path changes

# List all tags, filter for semver, sort, and get the latest
latest_tag=$(az acr repository show-tags -n "$RELEASE_ACR" --repository "$repo_path" -o tsv | \
  grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1)

if [[ -z "$latest_tag" ]]; then
  base_version="0.1.0"
else
  base_version="$latest_tag"
fi

# Increment patch version
IFS='.' read -r major minor patch <<< "$base_version"
patch=$((patch + 1))
version="${major}.${minor}.${patch}"

echo "Releasing eviction-autoscaler"
echo "Latest tag: $latest_tag"
echo "New version: $version"
echo "Commit: $commit_sha"
echo "ACR: $RELEASE_ACR"

epoch_ts="$(git_epoch)"
build_dt="$(build_date "$epoch_ts")"

echo "Building and publishing controller image with dalec..."
dalec publish --destination "${IMAGE_REPO}:${version}"
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

lock_image "$RELEASE_ACR" "$img_path"
echo "Release complete: $version"
