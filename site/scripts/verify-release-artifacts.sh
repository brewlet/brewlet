#!/usr/bin/env bash
set -euo pipefail

version="${1:-0.1.0}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

base="https://github.com/brewlet/brewlet/releases/download/v${version}"

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *)
    echo "unsupported operating system: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64 | aarch64) arch="arm64" ;;
  x86_64 | amd64) arch="amd64" ;;
  *)
    echo "unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

curl -fsSL -o "$work/brewlet.tar.gz" \
  "$base/brewlet_${version}_${os}_${arch}.tar.gz"
tar -xzf "$work/brewlet.tar.gz" -C "$work"
test "$("$work/brewlet" version)" = "$version"

curl -fsSL -o "$work/brewlet-maven-plugin.jar" \
  "$base/brewlet-maven-plugin-${version}.jar"
curl -fsSL -o "$work/brewlet-maven-plugin.pom" \
  "$base/brewlet-maven-plugin-${version}.pom"
test -s "$work/brewlet-maven-plugin.jar"
test -s "$work/brewlet-maven-plugin.pom"

helm pull oci://ghcr.io/brewlet/charts/brewlet \
  --version "$version" \
  --destination "$work"
helm template brewlet "$work/brewlet-${version}.tgz" \
  --set-string provisioner.jdks=temurin-21 \
  > "$work/rendered.yaml"

grep -q "ghcr.io/brewlet/operator:${version}" "$work/rendered.yaml"
grep -q "ghcr.io/brewlet/admission:${version}" "$work/rendered.yaml"
grep -q "ghcr.io/brewlet/node-provisioner:${version}" "$work/rendered.yaml"

docker manifest inspect "ghcr.io/brewlet/operator:${version}" >/dev/null
docker manifest inspect "ghcr.io/brewlet/admission:${version}" >/dev/null
docker manifest inspect "ghcr.io/brewlet/node-provisioner:${version}" >/dev/null
