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
BUILDX_BUILDER_CREATED=false
repo_path="public/aks/eviction-autoscaler"  # adjust if your ko publish path changes

cleanup_buildx_builder() {
  if [[ "${BUILDX_BUILDER_CREATED:-false}" == "true" ]]; then
    echo "Cleaning up buildx builder: ${BUILDX_BUILDER}"
    docker buildx rm "${BUILDX_BUILDER}" >/dev/null 2>&1 || echo "Warning: failed to remove builder ${BUILDX_BUILDER}"
  fi
}

trap cleanup_buildx_builder EXIT

require_command() {
  local command_name="$1"
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "Error: required command '$command_name' is not installed or not on PATH"
    exit 1
  fi
}

check_required_tools() {
  require_command git
  require_command docker
  require_command az
  require_command helm
  require_command yq
  require_command crane
  require_command cosign
}

check_required_tools

normalize_arch() {
  local arch="$1"
  case "$arch" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    ppc64le) echo "ppc64le" ;;
    s390x) echo "s390x" ;;
    *) echo "$arch" ;;
  esac
}

qemu_binfmt_entry() {
  local arch="$1"
  case "$arch" in
    arm64) echo "qemu-aarch64" ;;
    ppc64le) echo "qemu-ppc64le" ;;
    s390x) echo "qemu-s390x" ;;
    arm) echo "qemu-arm" ;;
    *) echo "qemu-${arch}" ;;
  esac
}

validate_binfmt_for_release_platforms() {
  local host_arch
  host_arch="$(normalize_arch "$(uname -m)")"

  if [[ ! -d /proc/sys/fs/binfmt_misc ]]; then
    for platform in ${RELEASE_PLATFORMS//,/ }; do
      local os arch
      os="${platform%%/*}"
      arch="${platform#*/}"
      arch="${arch%%/*}"
      arch="$(normalize_arch "$arch")"
      if [[ "$os" == "linux" && "$arch" != "$host_arch" ]]; then
        echo "Error: /proc/sys/fs/binfmt_misc not found, but cross-arch build requested for $platform"
        echo "Install binfmt handlers first, for example:"
        echo "  docker run --rm --privileged multiarch/qemu-user-static --reset -p yes"
        exit 1
      fi
    done
    return
  fi

  local missing=0
  for platform in ${RELEASE_PLATFORMS//,/ }; do
    local os arch entry
    os="${platform%%/*}"
    arch="${platform#*/}"
    arch="${arch%%/*}"
    arch="$(normalize_arch "$arch")"

    if [[ "$os" != "linux" || "$arch" == "$host_arch" ]]; then
      continue
    fi

    entry="$(qemu_binfmt_entry "$arch")"
    if [[ ! -f "/proc/sys/fs/binfmt_misc/${entry}" ]]; then
      echo "Error: Missing binfmt handler '${entry}' required for ${platform} on host arch ${host_arch}"
      missing=1
    fi
  done

  if [[ "$missing" -ne 0 ]]; then
    echo "Install binfmt handlers first, for example:"
    echo "  docker run --rm --privileged multiarch/qemu-user-static --reset -p yes"
    exit 1
  fi
}

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
  if [[ "${FORCE_OVERRIDE:-false}" == "true" ]]; then
    echo "WARNING: Version $version already exists in ACR. FORCE_OVERRIDE=true — proceeding with override."
    echo "The existing image and Helm chart will be replaced in ACR and replicated to MCR."
  else
    echo "Version $version already exists in ACR - skipping release"
    echo "To release a new version, create a new tag:"
    echo "  git tag <new-version>  (e.g., 1.0.1)"
    echo "  git push origin <new-version>"
    echo "To override the existing version, re-run the workflow with force_override=true."
    exit 1
  fi
fi

echo "Releasing eviction-autoscaler"
echo "New version to release: $version"
echo "Commit: $commit_sha"
echo "ACR: $RELEASE_ACR"

epoch_ts="$(git_epoch)"
build_dt="$(build_date "$epoch_ts")"

echo "Building and publishing multi-arch controller image with Docker buildx..."
echo "Platforms: ${RELEASE_PLATFORMS}"
validate_binfmt_for_release_platforms
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

echo "Verifying manifest contains platforms: ${RELEASE_PLATFORMS}..."
manifest_output="$(docker buildx imagetools inspect "$IMG")"
for platform in ${RELEASE_PLATFORMS//,/ }; do
  if ! grep -q "$platform" <<< "$manifest_output"; then
    echo "Error: Manifest for $IMG is missing platform: $platform"
    exit 1
  fi
done
echo "All requested platforms are present in the manifest."

trivy_scan "$IMG_REF" "$RELEASE_PLATFORMS"
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

echo "Release complete: $version"
