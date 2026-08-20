#!/usr/bin/env bash
# Splits the Spring PetClinic Spring Boot fat JAR into a *multi-layer classpath*
# deployment, so Brewlet ships (and a registry/node dedups) the slow-moving
# dependency layers separately from the fast-moving business code.
#
# Why: a fat JAR is a single ~63MB blob — change one line of application code and
# the whole 63MB re-pushes and re-pulls. Brewlet's layered-classpath mode
# (entry.mode=classpath + classpath.layer.v1+tar layers unpacked to /app/lib)
# lets us launch `java -cp app.jar:lib/* <MainClass>` instead. If we put the
# third-party dependencies in their own layer(s) and only the compiled classes in
# a thin app JAR, then rebuilding business code changes ONLY the small app-JAR
# layer digest; the dependency layers keep the same digest and are skipped on
# push/pull. This yields the same dedup win as Spring Boot's "layertools" model,
# but via GENERIC, framework-agnostic steps only.
#
# NOTE: this script does NOT read or parse the fat JAR's BOOT-INF/layers.idx.
# Brewlet deliberately does not understand any framework-specific layering manifest
# (see docs/layered-classpath-deployment.md). Instead we map Spring's repackaged
# output onto Brewlet's generic classpath layers using nothing but the JAR's
# structure: BOOT-INF/classes -> thin app JAR, BOOT-INF/lib/*.jar -> dependency tar
# layer(s) grouped by a plain filename convention (see step 4). The upstream build
# is not modified. Any framework that emits an exploded classes dir plus a lib dir
# of JARs maps the same way.
#
# Layers produced (a generic classes/deps split, similar in spirit to layers.idx):
#   lib/  <- dependencies         : release third-party JARs        (rarely change)
#   lib/  <- snapshot-dependencies: *-SNAPSHOT.jar deps, if any     (change sometimes)
#   spring-petclinic-app.jar      : BOOT-INF/classes (your code)    (change often)
#
# Outputs (under target/layered/):
#   spring-petclinic-app.jar        thin application JAR (compiled classes + resources)
#   deps-dependencies.tar           release dependency layer (JARs at tar root)
#   deps-snapshot-dependencies.tar  snapshot dependency layer (only if non-empty)
#   jvm-config.json                 entry.mode=classpath launch config for `brewlet push --config`
#
# The dependency tars are built deterministically (sorted entries, fixed mtime)
# so an unchanged dependency set yields a byte-identical tar — hence an identical
# layer digest — and is deduped rather than re-pushed.
#
# Env overrides:
#   PETCLINIC_JAR   pre-built fat JAR path (skip building; default: target/spring-petclinic.jar,
#                   built via build.sh if missing)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"

fatjar="${PETCLINIC_JAR:-$here/target/spring-petclinic.jar}"
out="$here/target/layered"
work="$out/exploded"
appjar="$out/spring-petclinic-app.jar"

# Reproducible-tar timestamp: fixed so identical inputs -> identical tar bytes.
export TZ=UTC
FIXED_MTIME="200001010000.00" # touch -t CCYYMMDDhhmm.ss

need() { command -v "$1" >/dev/null 2>&1 || { echo "[layered] ERROR: '$1' not found" >&2; exit 1; }; }
need jar
need unzip

# 1) Ensure we have the fat JAR (build it from upstream if absent).
if [[ ! -f "$fatjar" ]]; then
  echo "[layered] fat JAR not found at $fatjar — building via build.sh"
  "$here/build.sh"
  fatjar="$here/target/spring-petclinic.jar"
fi
echo "[layered] fat JAR: $fatjar ($(du -h "$fatjar" | cut -f1))"

# 2) Explode the fat JAR.
rm -rf "$out"
mkdir -p "$work"
( cd "$work" && jar xf "$fatjar" )

# The Start-Class in the manifest is the *application* main class (Spring Boot's
# JarLauncher would normally invoke it); in classpath mode we launch it directly.
start_class="$(unzip -p "$fatjar" META-INF/MANIFEST.MF | tr -d '\r' \
  | awk -F': ' '/^Start-Class:/{print $2}')"
if [[ -z "$start_class" ]]; then
  echo "[layered] ERROR: no Start-Class in manifest (is this a repackaged Spring Boot JAR?)" >&2
  exit 1
fi
echo "[layered] Start-Class (application main): $start_class"

# 3) Thin application JAR = BOOT-INF/classes (compiled classes + resources).
if [[ ! -d "$work/BOOT-INF/classes" ]]; then
  echo "[layered] ERROR: BOOT-INF/classes missing in exploded JAR" >&2
  exit 1
fi
jar --create --file "$appjar" -C "$work/BOOT-INF/classes" .
echo "[layered] thin app JAR: $appjar ($(du -h "$appjar" | cut -f1))"

# 4) Dependency layers, using a generic filename convention (NOT layers.idx): a JAR
#    whose filename carries -SNAPSHOT belongs to snapshot-dependencies, everything
#    else to dependencies. Each group becomes one tar with the JARs at the tar root,
#    so Brewlet unpacks them straight into /app/lib for `-cp lib/*`. This split is
#    framework-agnostic — no BOOT-INF/layers.idx is read.
libdir="$work/BOOT-INF/lib"
if [[ ! -d "$libdir" ]]; then
  echo "[layered] ERROR: BOOT-INF/lib missing in exploded JAR" >&2
  exit 1
fi

# Deterministic tar: fixed mtime + sorted member list, staged from a flat dir.
make_layer_tar() { # <tarpath> <staging-dir>
  local tarpath="$1" stage="$2" list
  list="$(mktemp)"
  ( cd "$stage" && ls -1 . | LC_ALL=C sort >"$list" )
  ( cd "$stage" && touch -t "${FIXED_MTIME%.*}" ./*.jar )
  ( cd "$stage" && tar -cf "$tarpath" -T "$list" )
  rm -f "$list"
}

stage_dep="$out/.stage-dependencies"
stage_snap="$out/.stage-snapshot"
mkdir -p "$stage_dep" "$stage_snap"

ndep=0 nsnap=0
shopt -s nullglob
for j in "$libdir"/*.jar; do
  base="$(basename "$j")"
  if [[ "$base" == *SNAPSHOT*.jar ]]; then
    cp "$j" "$stage_snap/"; nsnap=$((nsnap+1))
  else
    cp "$j" "$stage_dep/"; ndep=$((ndep+1))
  fi
done
shopt -u nullglob

layer_tars=()
if (( ndep > 0 )); then
  make_layer_tar "$out/deps-dependencies.tar" "$stage_dep"
  layer_tars+=("deps-dependencies.tar")
  echo "[layered] dependencies layer      : deps-dependencies.tar ($ndep JARs, $(du -h "$out/deps-dependencies.tar" | cut -f1))"
fi
if (( nsnap > 0 )); then
  make_layer_tar "$out/deps-snapshot-dependencies.tar" "$stage_snap"
  layer_tars+=("deps-snapshot-dependencies.tar")
  echo "[layered] snapshot-dependencies    : deps-snapshot-dependencies.tar ($nsnap JARs, $(du -h "$out/deps-snapshot-dependencies.tar" | cut -f1))"
else
  echo "[layered] snapshot-dependencies    : (empty — release build has no SNAPSHOT deps)"
fi
rm -rf "$stage_dep" "$stage_snap"

# 5) Launch config: classpath mode, app JAR first then the unpacked lib dir.
cat >"$out/jvm-config.json" <<JSON
{
  "schemaVersion": 1,
  "mainJar": "spring-petclinic-app.jar",
  "entry": {
    "mode": "classpath",
    "mainClass": "$start_class",
    "classPath": ["spring-petclinic-app.jar", "lib/*"]
  }
}
JSON
echo "[layered] launch config           : jvm-config.json (entry.mode=classpath)"

# 6) Point callers at the repository-owned orchestration.
echo "[layered] done. Tier 7 pushes and exercises this fixture."
