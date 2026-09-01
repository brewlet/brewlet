#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/entrypoint.sh"

calls="$(mktemp)"
dest="$(mktemp -d)"
trap 'rm -f "$calls"; chmod -R u+w "$dest" 2>/dev/null || true; rm -rf "$dest"' EXIT

host_ctr() {
  printf '%s\n' "$*" >>"$calls"
}

host_exec() {
  printf '%s\n' "$*" >>"$calls"
}

export JDK_CUSTOM_SOURCE_COUNT=1
export JDK_CUSTOM_SOURCE_0_TOKEN=zulu-21
export JDK_CUSTOM_SOURCE_0_IMAGE=docker.io/library/azul-zulu:21
export JDK_CUSTOM_SOURCE_0_JAVA_HOME=/usr/lib/jvm/zulu21

parse_custom_jdk_sources
jdk_from_image zulu 21 "$dest"

grep -Fq "image pull docker.io/library/azul-zulu:21" "$calls"
grep -Fq "images mount docker.io/library/azul-zulu:21 /opt/brewlet/.image-mount-zulu-21" "$calls"
grep -Fq "cp -a /opt/brewlet/.image-mount-zulu-21/. $dest/" "$calls"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-java-home"
grep -Fxq "docker.io/library/azul-zulu:21" "$dest/.brewlet-source"
grep -Fxq "/usr/lib/jvm/zulu21" "$dest/.brewlet-source"

if (
  JDK_CUSTOM_SOURCE_COUNT=0
  CUSTOM_JDK_TOKENS=("")
  CUSTOM_JDK_IMAGES=("")
  CUSTOM_JDK_JAVA_HOMES=("")
  parse_custom_jdk_sources
  jdk_from_image unknown 21 "$dest"
) >/dev/null 2>&1; then
  echo "expected an unknown JDK without a custom source to fail" >&2
  exit 1
fi


if (
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_IMAGE=azul-zulu:21
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected an unqualified custom image reference to fail" >&2
  exit 1
fi

if (
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_JAVA_HOME=/
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected javaHome=/ to fail" >&2
  exit 1
fi

if (
  JDK_CUSTOM_SOURCE_COUNT=1
  JDK_CUSTOM_SOURCE_0_TOKEN=../../../host-21
  parse_custom_jdk_sources
) >/dev/null 2>&1; then
  echo "expected a path-traversing custom JDK token to fail" >&2
  exit 1
fi

new_containerd_test_dir() {
  local dir
  dir="$(mktemp -d "$dest/containerd.XXXXXX")"
  printf 'version = 2\n' >"$dir/config.toml"
  printf '%s' "$dir"
}

mock_containerd_dump() {
  printf '%s\n' "$*" >>"$calls"
  if [[ "$1" == "containerd" && "$2" == "--config" && "$4" == "config" && "$5" == "dump" ]]; then
    cat "$3"
    [[ ! -f "$CONTAINERD_DROPIN_FILE" ]] || cat "$CONTAINERD_DROPIN_FILE"
  fi
}

mock_reload_containerd() {
  printf 'reload\n' >>"$calls"
}

# A host config that imports config.toml.d uses the drop-in and leaves the
# primary config untouched.
dropin_dir="$(new_containerd_test_dir)"
printf 'imports = ["./config.toml.d/*.toml"]\n' >>"$dropin_dir/config.toml"
(
  CONTAINERD_CONFIG="$dropin_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$dropin_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'containerd.runtimes.brewlet' "$dropin_dir/config.toml.d/99-brewlet.toml"
if grep -Fq 'containerd.runtimes.brewlet' "$dropin_dir/config.toml"; then
  echo "expected drop-in support to leave the primary containerd config unchanged" >&2
  exit 1
fi

# Re-running an unchanged validated render still validates it but does not
# reload containerd again.
: >"$calls"
(
  CONTAINERD_CONFIG="$dropin_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$dropin_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'containerd --config '"$dropin_dir/config.toml"' config dump' "$calls"
if grep -Fxq 'reload' "$calls"; then
  echo "expected an unchanged validated config to skip containerd reload" >&2
  exit 1
fi

# Hosts without an enabled import use the backed-up in-place fallback.
fallback_dir="$(new_containerd_test_dir)"
: >"$calls"
(
  CONTAINERD_CONFIG="$fallback_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$fallback_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() { mock_containerd_dump "$@"; }
  reload_containerd() { mock_reload_containerd; }
  configure_containerd
)
grep -Fq 'containerd.runtimes.brewlet' "$fallback_dir/config.toml"
grep -Fxq 'version = 2' "$fallback_dir/config.toml.brewlet.bak"
grep -Fxq 'reload' "$calls"

# A malformed effective config fails validation, restores the primary config,
# and reports a concise reason.
malformed_dir="$(new_containerd_test_dir)"
if (
  CONTAINERD_CONFIG="$malformed_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$malformed_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() {
    printf 'toml: malformed configuration\n' >&2
    return 1
  }
  reload_containerd() { echo "unexpected reload" >&2; return 1; }
  configure_containerd
) >"$malformed_dir/output" 2>&1; then
  echo "expected malformed containerd configuration to fail" >&2
  exit 1
fi
grep -Fq 'config dump rejected' "$malformed_dir/output"
grep -Fxq 'version = 2' "$malformed_dir/config.toml"

# A successful parse that omits the brewlet handler is also rejected and a
# newly rendered drop-in is removed.
missing_dir="$(new_containerd_test_dir)"
printf 'imports = ["%s/*.toml"]\n' "$missing_dir/config.toml.d" >>"$missing_dir/config.toml"
if (
  CONTAINERD_CONFIG="$missing_dir/config.toml"
  CONTAINERD_DROPIN_DIR="$missing_dir/config.toml.d"
  CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
  BREWLET_CONTAINERD_RESTART=validated
  BREWLET_VALIDATE=false
  NODE_NAME=""
  host_exec() {
    printf '%s\n' \
      'version = 2' \
      '[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet-old]' \
      '  runtime_type = "io.containerd.brewlet.v2"'
  }
  reload_containerd() { echo "unexpected reload" >&2; return 1; }
  configure_containerd
) >"$missing_dir/output" 2>&1; then
  echo "expected a parsed config without the brewlet handler to fail" >&2
  exit 1
fi
grep -Fq 'brewlet runtime handler is missing' "$missing_dir/output"
if [[ -e "$missing_dir/config.toml.d/99-brewlet.toml" ]]; then
  echo "expected failed drop-in validation to restore the prior host state" >&2
  exit 1
fi

# The legacy modes retain their original behavior: sighup patches in place and
# reloads, while none patches in place without signalling containerd.
for mode in sighup none; do
  legacy_dir="$(new_containerd_test_dir)"
  : >"$calls"
  (
    CONTAINERD_CONFIG="$legacy_dir/config.toml"
    CONTAINERD_DROPIN_DIR="$legacy_dir/config.toml.d"
    CONTAINERD_DROPIN_FILE="$CONTAINERD_DROPIN_DIR/99-brewlet.toml"
    BREWLET_CONTAINERD_RESTART="$mode"
    NODE_NAME=""
    reload_containerd() { mock_reload_containerd; }
    configure_containerd
  )
  grep -Fq 'containerd.runtimes.brewlet' "$legacy_dir/config.toml"
  if [[ "$mode" == "sighup" ]]; then
    grep -Fxq 'reload' "$calls"
  elif grep -Fxq 'reload' "$calls"; then
    echo "expected none mode not to reload containerd" >&2
    exit 1
  fi
done
