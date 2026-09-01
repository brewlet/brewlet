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

validation_root="$(mktemp -d)"
chmod -R u+w "$validation_root" 2>/dev/null || true
trap 'rm -f "$calls"; chmod -R u+w "$dest" "$validation_root" 2>/dev/null || true; rm -rf "$dest" "$validation_root"' EXIT

mkdir -p "$validation_root/launchers/jaz/bin"
cat >"$validation_root/launchers/jaz/bin/jaz" <<'EOF'
#!/usr/bin/env bash
[[ "${JAZ_PRINT_VERSION:-}" == "1" ]]
[[ "${JAZ_EXIT_WITHOUT_FLUSH:-}" == "1" ]]
printf 'jaz-probed\n' >>"$LAUNCHER_PROBE_CALLS"
EOF
chmod 0755 "$validation_root/launchers/jaz/bin/jaz"

(
  PREFIX="$validation_root"
  JDKS=temurin-21
  LAUNCHERS=java,jaz
  BREWLET_VALIDATE=true
  LAUNCHER_PROBE_CALLS="$calls"
  export LAUNCHER_PROBE_CALLS
  jdk_root_complete() { return 0; }
  validate_runtime
)
grep -Fxq "jaz-probed" "$calls"

assert_launcher_validation_fails() {
  local expected="$1"
  local output
  if output="$(
    (
      PREFIX="$validation_root"
      JDKS=temurin-21
      LAUNCHERS=jaz
      BREWLET_VALIDATE=true
      BREWLET_MODE=cleanup
      jdk_root_complete() { return 0; }
      validate_runtime
    ) 2>&1
  )"; then
    echo "expected launcher validation to fail with ${expected}" >&2
    exit 1
  fi
  grep -Fq "ERROR: ${expected}" <<<"$output"
}

rm -f "$validation_root/launchers/jaz/bin/jaz"
assert_launcher_validation_fails "launcher-jaz-missing"

printf '#!/usr/bin/env bash\nexit 0\n' >"$validation_root/launchers/jaz/bin/jaz"
chmod 0644 "$validation_root/launchers/jaz/bin/jaz"
assert_launcher_validation_fails "launcher-jaz-not-executable"

printf '#!/usr/bin/env bash\nexit 7\n' >"$validation_root/launchers/jaz/bin/jaz"
chmod 0755 "$validation_root/launchers/jaz/bin/jaz"
assert_launcher_validation_fails "launcher-jaz-probe-failed"

(
  PREFIX="$validation_root"
  JDKS=temurin-21
  LAUNCHERS=jaz
  BREWLET_VALIDATE=false
  jdk_root_complete() { return 1; }
  validate_runtime
)

long_launcher="launcher-name-that-is-deliberately-longer-than-forty-eight-characters"
bounded_launcher="${long_launcher:0:48}"
if output="$(
  (
    PREFIX="$validation_root"
    JDKS=temurin-21
    LAUNCHERS="$long_launcher"
    BREWLET_VALIDATE=true
    BREWLET_MODE=cleanup
    jdk_root_complete() { return 0; }
    validate_runtime
  ) 2>&1
)"; then
  echo "expected a missing long-named launcher to fail validation" >&2
  exit 1
fi
grep -Fq "ERROR: launcher-${bounded_launcher}-missing" <<<"$output"
if grep -Fq "$long_launcher" <<<"$output"; then
  echo "launcher validation reason was not bounded" >&2
  exit 1
fi

assert_restart_validation_order() {
  local mode="$1" expected="$2" order
  order="$(
    (
      BREWLET_CONTAINERD_RESTART="$mode"
      validate_runtime() { printf 'validate\n'; }
      reload_containerd() { printf 'reload\n'; }
      maybe_reload_containerd
      validate_readiness_after_reload
    )
  )"
  [[ "$order" == "$expected" ]] || {
    echo "unexpected ${mode} restart/validation order: ${order}" >&2
    exit 1
  }
}

assert_restart_validation_order validated $'validate\nreload'
assert_restart_validation_order sighup $'reload\nvalidate'
assert_restart_validation_order none $'[brewlet-provisioner] BREWLET_CONTAINERD_RESTART=none; leaving containerd untouched (restart it out of band to activate the brewlet runtime)\nvalidate'
