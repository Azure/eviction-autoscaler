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
