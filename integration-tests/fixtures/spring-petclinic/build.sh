#!/usr/bin/env bash
# Builds the *real* upstream Spring PetClinic into a Spring Boot fat JAR, so
# Brewlet can ship it as an OCI artifact and run it with `java -jar` on a
# node-resident JDK — no Dockerfile, no base image, no JVM baked into an image.
#
# Unlike demo-app/ (a dependency-free HTTP server built with the JDK alone),
# this proves the Brewlet model against a genuine, dependency-heavy Spring Boot
# application: a self-executable fat JAR whose Main-Class is Spring Boot's
# JarLauncher. The output is copied to a stable path so the rest of the tooling
# harness does not depend on the upstream version.
#
# Env overrides:
#   PETCLINIC_REPO   git URL             (default: spring-projects/spring-petclinic)
#   PETCLINIC_REF    commit/branch/tag   (default: the pinned SHA below)
#   PETCLINIC_JAR    pre-built JAR path  (skip the clone/build and just stage it)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"

REPO="${PETCLINIC_REPO:-https://github.com/spring-projects/spring-petclinic.git}"
# Pinned for reproducible builds; override with PETCLINIC_REF=main to track HEAD.
REF="${PETCLINIC_REF:-b3ee2c53e76e9267f03551a7cd36b0983c859c56}"

checkout="$here/.checkout"
out="$here/target"
jar="$out/spring-petclinic.jar"

mkdir -p "$out"

# Fast path: caller already has a fat JAR (e.g. a CI cache or a local build).
if [[ -n "${PETCLINIC_JAR:-}" ]]; then
  echo "[petclinic] staging pre-built JAR: $PETCLINIC_JAR"
  cp "$PETCLINIC_JAR" "$jar"
  echo "[petclinic] -> $jar ($(du -h "$jar" | cut -f1))"
  exit 0
fi

echo "[petclinic] repo: $REPO"
echo "[petclinic] ref : $REF"

# Clone (or reuse) a shallow checkout pinned to REF.
if [[ ! -d "$checkout/.git" ]]; then
  rm -rf "$checkout"
  git init -q "$checkout"
  git -C "$checkout" remote add origin "$REPO"
fi
git -C "$checkout" fetch -q --depth 1 origin "$REF"
git -C "$checkout" checkout -q FETCH_HEAD
echo "[petclinic] checked out $(git -C "$checkout" rev-parse HEAD)"

# Build the repackaged Spring Boot fat JAR. Tests and code-quality gates are
# skipped: we only need the artifact, not to re-validate upstream.
echo "[petclinic] building fat JAR (mvnw -DskipTests package)..."
mvn_cmd=("$checkout/mvnw")
[[ -x "$checkout/mvnw" ]] || mvn_cmd=(mvn)
( cd "$checkout" && "${mvn_cmd[@]}" -q -B \
    -DskipTests -Dcheckstyle.skip=true -Dspotless.check.skip=true -Denforcer.skip=true \
    package )

# The repackaged (executable) JAR is the one whose manifest has a Start-Class.
built=""
for f in "$checkout"/target/*.jar; do
  [[ "$f" == *-sources.jar || "$f" == *-javadoc.jar ]] && continue
  if unzip -p "$f" META-INF/MANIFEST.MF 2>/dev/null | grep -q '^Start-Class:'; then
    built="$f"; break
  fi
done
if [[ -z "$built" ]]; then
  echo "[petclinic] ERROR: no repackaged Spring Boot JAR found under $checkout/target" >&2
  exit 1
fi

cp "$built" "$jar"
echo "[petclinic] done -> $jar ($(du -h "$jar" | cut -f1))"
echo "[petclinic] Main-Class: $(unzip -p "$jar" META-INF/MANIFEST.MF | tr -d '\r' | awk -F': ' '/^Main-Class:/{print $2}')"
