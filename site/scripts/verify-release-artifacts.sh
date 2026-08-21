#!/usr/bin/env bash
set -euo pipefail

version="${1:-0.1.0}"
work="$(mktemp -d)"
app_pid=""

cleanup() {
  if [[ -n "$app_pid" ]]; then
    kill "$app_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

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

curl -fsSL -o "$work/source.tar.gz" \
  "https://github.com/brewlet/brewlet/archive/refs/tags/v${version}.tar.gz"
tar -xzf "$work/source.tar.gz" -C "$work"

example="$work/brewlet-${version}/integration-tests/fixtures/demo-app"
mvn -q -f "$example/pom.xml" clean package
test -f "$example/target/app.jar"

ref="demo/hello:${version}"
"$work/brewlet" push "$example/target/app.jar" "$ref" \
  --store "$work/oci" \
  --format artifact \
  > "$work/push.log"
"$work/brewlet" inspect "$ref" --store "$work/oci" \
  > "$work/inspect.log"
grep -q '"mainJar": "app.jar"' "$work/inspect.log"

port=$((18000 + ($$ % 1000)))
"$work/brewlet" run "$ref" --store "$work/oci" \
  -- "-Dserver.port=${port}" \
  > "$work/run.log" 2>&1 &
app_pid=$!

ready=false
for _ in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:${port}/healthz" > /dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
"$ready"
curl -fsS "http://127.0.0.1:${port}/hello" \
  | grep -q "Hello from a JAR"
kill "$app_pid" 2>/dev/null || true
wait "$app_pid" 2>/dev/null || true
app_pid=""

"$work/brewlet" bundle "$ref" \
  --store "$work/oci" \
  --cpu 1 \
  --memory 256Mi \
  --out "$work/bundle" \
  > "$work/bundle.log"
test -f "$work/bundle/config.json"

curl -fsSL -o "$work/brewlet-maven-plugin.jar" \
  "$base/brewlet-maven-plugin-${version}.jar"
curl -fsSL -o "$work/brewlet-maven-plugin.pom" \
  "$base/brewlet-maven-plugin-${version}.pom"
test -s "$work/brewlet-maven-plugin.jar"
test -s "$work/brewlet-maven-plugin.pom"

mvn -q org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file \
  -Dfile="$work/brewlet-maven-plugin.jar" \
  -DpomFile="$work/brewlet-maven-plugin.pom"
mvn -q -f "$example/pom.xml" package \
  "sh.brewlet:brewlet-maven-plugin:${version}:config" \
  "sh.brewlet:brewlet-maven-plugin:${version}:build" \
  -Dbrewlet.image="demo/hello:${version}"
test -f "$example/target/brewlet/jvm-config.json"
test -f "$example/target/brewlet/oci/index.json"

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
