#!/usr/bin/env bash
set -euo pipefail

version="${1:-0.1.0}"
work="$(mktemp -d)"
app_pid=""
script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

cleanup() {
  if [[ -n "$app_pid" ]]; then
    kill "$app_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

base="https://github.com/brewlet/brewlet/releases/download/v${version}"

cat "$script_dir/../install.sh" \
  | BREWLET_VERSION="$version" BREWLET_INSTALL_DIR="$work/bin" sh
test "$("$work/bin/brewlet" version)" = "$version"

curl -fsSL -o "$work/source.tar.gz" \
  "https://github.com/brewlet/brewlet/archive/refs/tags/v${version}.tar.gz"
tar -xzf "$work/source.tar.gz" -C "$work"

example="$work/brewlet-${version}/integration-tests/fixtures/demo-app"
mvn -q -f "$example/pom.xml" clean package
test -f "$example/target/app.jar"

ref="demo/hello:${version}"
"$work/bin/brewlet" push "$example/target/app.jar" "$ref" \
  --store "$work/oci" \
  --format artifact \
  > "$work/push.log"
"$work/bin/brewlet" inspect "$ref" --store "$work/oci" \
  > "$work/inspect.log"
grep -q '"mainJar": "app.jar"' "$work/inspect.log"

port=$((18000 + ($$ % 1000)))
"$work/bin/brewlet" run "$ref" --store "$work/oci" \
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

"$work/bin/brewlet" bundle "$ref" \
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
