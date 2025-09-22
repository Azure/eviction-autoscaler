#!/usr/bin/env bash
set -e

# Update package lists and install basic dependencies (excluding docker.io to avoid containerd conflict)
sudo apt-get update
sudo apt-get install -y curl jq make ca-certificates apt-transport-https gnupg lsb-release

# Remove containerd if present, to avoid conflicts
if dpkg -l | grep -q containerd; then
  echo "Removing conflicting containerd package..."
  sudo apt-get remove -y containerd
fi

# Install Docker using the official script (recommended way)
if ! command -v docker >/dev/null 2>&1; then
  echo "Installing Docker..."
  curl -fsSL https://get.docker.com | sudo sh
fi

# Ensure Azure CLI is available
if ! command -v az >/dev/null 2>&1; then
  echo "Installing Azure CLI..."
  curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash
fi

# Install ko (container image builder for Go applications)
if ! command -v ko >/dev/null 2>&1; then
  curl -LO https://github.com/google/ko/releases/download/v0.13.0/ko-linux-amd64
  chmod +x ko-linux-amd64 && sudo mv ko-linux-amd64 /usr/local/bin/ko
fi

# Install yq (YAML processor)
if ! command -v yq >/dev/null 2>&1; then
  curl -sSfL https://github.com/mikefarah/yq/releases/download/v4.43.1/yq_linux_amd64 -o /tmp/yq
  sudo mv /tmp/yq /usr/local/bin/yq
  sudo chmod +x /usr/local/bin/yq
fi

# Install cosign (container signing tool)
if ! command -v cosign >/dev/null 2>&1; then
  curl -LO https://github.com/sigstore/cosign/releases/download/v2.0.0/cosign-linux-amd64
  chmod +x cosign-linux-amd64 && sudo mv cosign-linux-amd64 /usr/local/bin/cosign
fi

# Install trivy (container/image scanning tool)
if ! command -v trivy >/dev/null 2>&1; then
  curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sudo sh -s -- -b /usr/local/bin
fi

echo "All tools installed successfully."