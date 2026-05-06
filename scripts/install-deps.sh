#!/usr/bin/env bash
set -euo pipefail

OS="$(uname -s)"

if [[ "${OS}" != "Darwin" ]]; then
  echo "Unsupported OS: ${OS}. Only macOS is supported at this time."
  exit 1
fi

if ! command -v brew &>/dev/null; then
  echo "Homebrew is required but not installed."
  echo "Install it from https://brew.sh and re-run this script."
  exit 1
fi

install_if_missing() {
  local cmd="$1"
  local formula="$2"
  if command -v "${cmd}" &>/dev/null; then
    echo "  [ok] ${cmd} ($(${cmd} --version 2>&1 | head -1))"
  else
    echo "  [installing] ${formula}"
    brew install "${formula}"
  fi
}

echo "==> Checking dependencies"

install_if_missing go        go
install_if_missing docker    docker
install_if_missing kind      kind
install_if_missing kubectl   kubernetes-cli
install_if_missing clusterctl clusterctl
install_if_missing helm      helm
install_if_missing golangci-lint golangci-lint
install_if_missing task      go-task

echo ""
echo "All dependencies satisfied."
echo ""
echo "Note: Docker Desktop must be running before using kind, docker-build, or e2e targets."
