#!/usr/bin/env bash
set -euo pipefail

acr_login() {
  local acr_name="$1"
  echo "Logging into ACR: $acr_name"
  az acr login -n "${acr_name}"
}

cosign_sign() {
  local artifact="$1"
  local version="$2"
  local commit_sha="$3"
  local build_date="$4"
  echo "Signing artifact: $artifact"
  cosign sign --yes \
    -a version="${version}" \
    -a commitSha="${commit_sha}" \
    -a buildDate="${build_date}" \
    "${artifact}"
}

build_date() {
  local epoch_ts="$1"
  if [[ "$OSTYPE" == "darwin"* ]]; then
    date -u -r "${epoch_ts}" "+%Y-%m-%dT%H:%M:%SZ"
  else
    date -u --date="@${epoch_ts}" "+%Y-%m-%dT%H:%M:%SZ"
  fi
}

git_epoch() {
  git log -1 --format='%ct'
}

lock_image() {
  local acr_name="$1"
  local repo="$2"
  local tag="$3"
  echo "Locking image tag $repo:$tag in $acr_name"
  az acr repository update -n "${acr_name}" --image "${repo}:${tag}" --write-enabled false --delete-enabled false
}

trivy_scan() {
  local image="$1"

  echo "Scanning image with Trivy: $image"
  if ! command -v trivy &>/dev/null; then
    echo "Installing Trivy..."
    curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sh -s -- -b /usr/local/bin
  fi

  trivy image --ignore-unfixed --exit-code 1 --no-progress "$image"
}

inject_mcr_image() {
  local chart_path="$1"
  local tag="$2"
  echo "Injecting MCR image into values.yaml: ${tag}"
  yq e -i '.image.repository = "mcr.microsoft.com/aks/eviction-autoscaler"' "${chart_path}/values.yaml"
  yq e -i ".image.tag = \"${tag}\"" "${chart_path}/values.yaml"
}
