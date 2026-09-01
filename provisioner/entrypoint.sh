#!/usr/bin/env bash
# brewlet-node-provisioner entrypoint.
#
# Runs as a privileged DaemonSet pod on nodes annotated brewlet.sh/provision=true
# and performs the host-side installation described in https://github.com/brewlet/brewlet/tree/main/specs §5.2:
#
#   0. Preflight: require cgroup v2 (unified hierarchy); refuse the node otherwise.
#   1. Install the shim binary into the host PATH (/opt/brewlet/bin + /usr/local/bin).
#   2. Install one or more read-only JDK runtime roots under /opt/brewlet/jdks/
#      via copy-from-image (ctr against the host containerd).
#   3. Optionally install launcher layers (e.g. jaz) under /opt/brewlet/launchers/.
#   4. Register the `brewlet` runtime in /etc/containerd/config.toml and reload.
#   5. Label the node brewlet.sh/runtime=ready and advertise the installed
#      JDKs/launchers via annotations.
#
# The script is idempotent: it is safe to re-run, and only does work that is
# still missing. It then sleeps forever so the DaemonSet pod stays Ready.
#
# It also runs in a reversal mode (BREWLET_MODE=cleanup): a short-lived
# brewlet-cleanup DaemonSet the operator launches for a deleted NodeProfile.
# In that mode it restores the containerd config backup, removes the shim,
# and drops the brewlet runtime + capability labels/annotations from the node
# (https://github.com/brewlet/brewlet/blob/main/specs/proposals/0001-node-profiles.md §5.7), then idles so the operator can
# observe completion before removing the profile finalizer.
#
# NB: this is privileged and mutates the host. Only run it on nodes the platform
# team controls (https://github.com/brewlet/brewlet/tree/main/specs §11).
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (all overridable via the DaemonSet env).
# ---------------------------------------------------------------------------
NODE_NAME="${NODE_NAME:-$(hostname)}"
PREFIX="${BREWLET_PREFIX:-/opt/brewlet}"                 # host-mounted at $PREFIX
HOST_BIN="${HOST_BIN:-/host/usr/local/bin}"              # host /usr/local/bin (on containerd PATH)
CONTAINERD_CONFIG="${CONTAINERD_CONFIG:-/etc/containerd/config.toml}"
SHIM_SRC="${SHIM_SRC:-/opt/brewlet-dist/containerd-shim-brewlet-v2}"  # baked into the image
SHIM_NAME="containerd-shim-brewlet-v2"
CTR_SRC="${CTR_SRC:-/usr/local/bin/ctr}"
HOST_CTR="$HOST_BIN/brewlet-ctr"
HOST_CTR_PATH="/usr/local/bin/brewlet-ctr"
JDK_HOME_METADATA=".brewlet-java-home"
JDK_SOURCE_METADATA=".brewlet-source"
JDK_ACTIVE_INVENTORY=".brewlet-active"

# Declarative inventory (comma-separated). See §5.3 / §5.4.
#   JDKS      = <distribution>-<feature>, e.g. "temurin-21,microsoft-25"
#   JDK_CUSTOM_SOURCE_<n>_{TOKEN,IMAGE,JAVA_HOME} = copy source for each
#              non-curated JDK; JDK_CUSTOM_SOURCE_COUNT declares the entry count
#   LAUNCHERS = launcher names, e.g. "jaz"
JDKS="${JDKS:-temurin-21}"
JDK_CUSTOM_SOURCE_COUNT="${JDK_CUSTOM_SOURCE_COUNT:-0}"
LAUNCHERS="${LAUNCHERS:-}"

# JDKs and launchers are obtained exclusively via copy-from-image: the vendor's
# official image is pulled through the host containerd and the runtime tree is
# copied out onto the host, so no package manager ever touches the host (§5.3).
CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/containerd/containerd.sock}"
CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-k8s.io}"

# Operating mode, set by the DaemonSet the operator renders per NodeProfile:
#   provision (default) = install the shim/JDKs/launchers and mark the node ready
#   cleanup             = reverse all host state for a deleted profile (§5.6)
BREWLET_MODE="${BREWLET_MODE:-provision}"

# When and whether to reload containerd after (re)writing its config (§5.6 /
# proposal 0002). One of:
#   validated (default) = smoke-test the installed JDK roots first, then SIGHUP
#   sighup              = SIGHUP containerd unconditionally after writing config
#   none                = never signal containerd; a human/rollout restarts it
BREWLET_CONTAINERD_RESTART="${BREWLET_CONTAINERD_RESTART:-validated}"

# Whether to run the post-install validation smoke test at all. The profile's
# spec.rollout.validate=false sets this to skip validation (§5.6).
BREWLET_VALIDATE="${BREWLET_VALIDATE:-true}"
BREWLET_PROFILE_NAME="${BREWLET_PROFILE_NAME:-default}"
BREWLET_PROFILE_GENERATION="${BREWLET_PROFILE_GENERATION:-0}"

# Registry mirrors for air-gapped / pull-through setups (§5.6): a
# comma-separated list of "<registry-host>=<mirror-host>" pairs the operator
# renders from spec.registry.mirrors. Every copy-from-image pull rewrites its
# ref's registry host through this map.
MIRRORS="${MIRRORS:-}"

log()  { printf '[brewlet-provisioner] %s\n' "$*"; }

# On a fatal error, record a machine-readable reason on the Node object
# (brewlet.sh/provision-error) before exiting non-zero, so the operator's
# NodeReconciler can flip the node to a Failed state instead of leaving it stuck
# in Provisioning (https://github.com/brewlet/brewlet/tree/main/specs §14). Best-effort: never mask the original
# failure if annotating fails.
die()  {
  printf '[brewlet-provisioner] ERROR: %s\n' "$*" >&2
  if [[ "${BREWLET_MODE}" != "cleanup" ]] && command -v kubectl >/dev/null 2>&1 && [[ -n "${NODE_NAME:-}" ]]; then
    if command -v clear_node_advertisement >/dev/null 2>&1; then
      clear_node_advertisement
    fi
    kubectl annotate node "$NODE_NAME" "${ANNOTATION_PROVISION_ERROR}=$*" --overwrite >/dev/null 2>&1 || true
  fi
  exit 1
}

# Node annotation the operator reads to fail a node whose provisioning errored.
ANNOTATION_PROVISION_ERROR="brewlet.sh/provision-error"

# ---------------------------------------------------------------------------
# Registry mirror rewriting (§5.6). Given an image ref, if its registry host
# has a configured mirror, swap the host for the mirror; otherwise return the
# ref unchanged. The map is parsed once into MIRROR_KEYS/MIRROR_VALS.
# ---------------------------------------------------------------------------
MIRROR_KEYS=()
MIRROR_VALS=()
parse_mirrors() {
  [[ -n "$MIRRORS" ]] || return 0
  local pair host mirror
  IFS=',' read -ra _pairs <<<"$MIRRORS"
  for pair in "${_pairs[@]}"; do
    [[ -n "$pair" ]] || continue
    host="${pair%%=*}"; mirror="${pair#*=}"
    [[ -n "$host" && -n "$mirror" && "$host" != "$mirror" ]] || continue
    MIRROR_KEYS+=("$host")
    MIRROR_VALS+=("$mirror")
    log "registry mirror: ${host} -> ${mirror}"
  done
}

mirror_ref() {
  local ref="$1" host rest i
  host="${ref%%/*}"; rest="${ref#*/}"
  for i in "${!MIRROR_KEYS[@]}"; do
    if [[ "$host" == "${MIRROR_KEYS[$i]}" ]]; then
      printf '%s/%s' "${MIRROR_VALS[$i]}" "$rest"
      return 0
    fi
  done
  printf '%s' "$ref"
}

# Custom JDK sources are transported as indexed environment variables so image
# references and paths do not need delimiter escaping.
CUSTOM_JDK_TOKENS=("")
CUSTOM_JDK_IMAGES=("")
CUSTOM_JDK_JAVA_HOMES=("")
parse_custom_jdk_sources() {
  [[ "$JDK_CUSTOM_SOURCE_COUNT" =~ ^[0-9]+$ ]] \
    || die "JDK_CUSTOM_SOURCE_COUNT must be a non-negative integer"

  local i token_var image_var home_var token image java_home existing
  for ((i = 0; i < JDK_CUSTOM_SOURCE_COUNT; i++)); do
    token_var="JDK_CUSTOM_SOURCE_${i}_TOKEN"
    image_var="JDK_CUSTOM_SOURCE_${i}_IMAGE"
    home_var="JDK_CUSTOM_SOURCE_${i}_JAVA_HOME"
    token="$(printenv "$token_var" 2>/dev/null || true)"
    image="$(printenv "$image_var" 2>/dev/null || true)"
    java_home="$(printenv "$home_var" 2>/dev/null || true)"
    [[ -n "$token" && -n "$image" && -n "$java_home" ]] \
      || die "custom JDK source ${i} requires ${token_var}, ${image_var}, and ${home_var}"
    [[ "$token" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?-[1-9][0-9]*$ ]] \
      || die "custom JDK source ${i} token must be a safe <distribution>-<feature> value"
    [[ "$image" != *"://"* && "$image" != *[[:space:]]* && "$image" == */* ]] \
      || die "custom JDK source ${i} image must be a fully qualified OCI reference"
    local image_host="${image%%/*}"
    [[ "$image_host" == "localhost" || "$image_host" == *.* || "$image_host" == *:* ]] \
      || die "custom JDK source ${i} image must include an explicit registry host"
    [[ "$java_home" == /* && "$java_home" != *[[:space:]]* \
       && "$java_home" != "/" && "$java_home" != */ \
       && "$java_home" != *"//"* && "$java_home" != *"/./"* \
       && "$java_home" != *"/../"* && "$java_home" != *"/." && "$java_home" != *"/.." ]] \
      || die "custom JDK source ${i} javaHome must be a clean absolute path below /"
    for existing in "${CUSTOM_JDK_TOKENS[@]}"; do
      [[ "$existing" != "$token" ]] || die "duplicate custom JDK source for ${token}"
    done
    CUSTOM_JDK_TOKENS+=("$token")
    CUSTOM_JDK_IMAGES+=("$image")
    CUSTOM_JDK_JAVA_HOMES+=("$java_home")
  done
}

# Map `uname -m` to the OCI platform token (used only for logging here; the copy
# runs on the node so `ctr` selects the matching image platform automatically).
host_arch_oci() {
  case "$(uname -m)" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)  echo "arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

# Run the bundled ctr from the host mount namespace. Pull/unpack operations apply
# mounts in the caller's namespace; using the pod namespace with the host socket
# makes containerd return host paths the client cannot see.
host_ctr() {
  [[ -x "$HOST_CTR" ]] || die "host ctr helper not installed at $HOST_CTR"
  command -v nsenter >/dev/null || die "nsenter not found (required for host containerd operations)"
  nsenter --target 1 --mount -- "$HOST_CTR_PATH" \
    --address "$CONTAINERD_ADDRESS" --namespace "$CONTAINERD_NAMESPACE" "$@"
}

host_exec() {
  command -v nsenter >/dev/null || die "nsenter not found (required for host operations)"
  nsenter --target 1 --mount -- "$@"
}

# ---------------------------------------------------------------------------
# Step 0 — preflight: Brewlet requires cgroup v2 (unified hierarchy) on the node.
# Modern container-aware JDKs read their heap/CPU limits directly from cgroup v2;
# a cgroup v1-only (or hybrid) node cannot enforce §10 resource semantics. On such
# a node the provisioner refuses and exits non-zero, so the node is NOT marked
# ready (https://github.com/brewlet/brewlet/tree/main/specs §10, §14).
# ---------------------------------------------------------------------------
require_cgroup_v2() {
  local mount="${CGROUP_ROOT:-/sys/fs/cgroup}"
  # The unified (v2) hierarchy exposes a cgroup.controllers file at its root; a
  # cgroup v1-only or hybrid node has no such file at the cgroup mount root.
  if [[ -r "${mount}/cgroup.controllers" ]]; then
    log "cgroup v2 (unified hierarchy) detected at ${mount}"
    return 0
  fi
  # Fallback: confirm the mount is a cgroup2 filesystem (e.g. if cgroup.controllers
  # is unreadable for permission reasons but the hierarchy is unified).
  local fstype=""
  fstype="$(stat -f -c %T "$mount" 2>/dev/null || true)"
  if [[ "$fstype" == "cgroup2fs" ]]; then
    log "cgroup v2 filesystem detected at ${mount}"
    return 0
  fi
  die "cgroup v2 is required but not active on this node (${mount} is '${fstype:-unknown}', with no ${mount}/cgroup.controllers). Brewlet refuses to provision cgroup v1-only nodes; the node will not be marked ready. See https://github.com/brewlet/brewlet/tree/main/specs §10/§14."
}

# ---------------------------------------------------------------------------
# Step 1 — install the shim binary onto the host PATH.
# ---------------------------------------------------------------------------
install_shim() {
  [[ -x "$SHIM_SRC" ]] || die "shim binary not found in image at $SHIM_SRC"
  [[ -x "$CTR_SRC" ]] || die "ctr binary not found in image at $CTR_SRC"
  mkdir -p "$PREFIX/bin" "$HOST_BIN"
  # /opt/brewlet/bin is the canonical location; /usr/local/bin is on containerd's
  # PATH so runtime_type = io.containerd.brewlet.v2 resolves the shim binary.
  install -m 0755 "$SHIM_SRC" "$PREFIX/bin/$SHIM_NAME"
  install -m 0755 "$SHIM_SRC" "$HOST_BIN/$SHIM_NAME"
  install -m 0755 "$CTR_SRC" "$HOST_CTR"
  log "installed shim and host ctr helper"
}

# ---------------------------------------------------------------------------
# Step 2 — install JDK runtime roots. Each inventory directory is a complete
# image rootfs plus metadata pointing at the JDK or jlink runtime within it.
# ---------------------------------------------------------------------------
jdk_home_in_root() {
  local root="$1"
  if [[ -f "$root/$JDK_HOME_METADATA" ]]; then
    cat "$root/$JDK_HOME_METADATA"
  else
    printf '/'
  fi
}

jdk_java() {
  local root="$1" java_home="$2" mounted=false rc
  shift 2
  mkdir -p "$root/proc" 2>/dev/null || return 1
  if ! mountpoint -q "$root/proc"; then
    mount -t proc proc "$root/proc" || return 1
    mounted=true
  fi
  if chroot "$root" "${java_home%/}/bin/java" "$@"; then
    rc=0
  else
    rc=$?
  fi
  if [[ "$mounted" == true ]]; then
    umount "$root/proc" || return 1
  fi
  return "$rc"
}

jdk_root_complete() {
  local root="$1" java_home
  [[ -d "$root" ]] || return 1
  java_home="$(jdk_home_in_root "$root")"
  [[ -x "$root${java_home%/}/bin/java" ]] || return 1
  jdk_java "$root" "$java_home" -version >/dev/null 2>&1
}

install_jdk() {
  local spec="$1" dist feature dest stage retired
  [[ "$spec" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?-[1-9][0-9]*$ ]] \
    || die "invalid JDK token '${spec}'; expected <distribution>-<positive-feature>"
  dist="${spec%-*}"; feature="${spec##*-}"
  dest="$PREFIX/jdks/${dist}-${feature}"
  resolve_jdk_source "$dist" "$feature"
  if jdk_root_complete "$dest" &&
    [[ -f "$dest/$JDK_SOURCE_METADATA" ]] &&
    cmp -s <(printf '%s\n%s\n' "$JDK_SOURCE_IMAGE" "$JDK_SOURCE_JAVA_HOME") "$dest/$JDK_SOURCE_METADATA"; then
    log "JDK ${spec} already present at $dest — skipping"
    return 0
  fi
  stage="${dest}.staging.$$"
  chmod -R u+w "$stage" 2>/dev/null || true
  rm -rf "$stage"
  log "installing JDK ${spec} via copy-from-image -> $dest"
  mkdir -p "$stage"
  jdk_from_image "$dist" "$feature" "$stage"
  date -u +%Y-%m-%dT%H:%M:%SZ >"$stage/.brewlet-installed-at"
  jdk_root_complete "$stage" || die "JDK ${spec} install did not produce a runnable root"
  local java_home; java_home="$(jdk_home_in_root "$stage")"
  log "JDK ${spec} ready: $(jdk_java "$stage" "$java_home" -version 2>&1 | head -1)"

  if [[ -e "$dest" ]]; then
    retired="${dest}.retired.$(date +%s).$$"
    mv "$dest" "$retired" || die "could not retain the previous JDK ${spec} root"
  fi
  if ! mv "$stage" "$dest"; then
    [[ -n "${retired:-}" && -e "$retired" ]] && mv "$retired" "$dest" || true
    die "could not activate the new JDK ${spec} root"
  fi
}

JDK_SOURCE_IMAGE=""
JDK_SOURCE_JAVA_HOME=""
resolve_jdk_source() {
  local dist="$1" feature="$2" token="${1}-${2}" i
  JDK_SOURCE_IMAGE=""
  JDK_SOURCE_JAVA_HOME=""
  case "$dist" in
    microsoft)
      JDK_SOURCE_IMAGE="mcr.microsoft.com/openjdk/jdk:${feature}-ubuntu"
      JDK_SOURCE_JAVA_HOME="/usr/lib/jvm/msopenjdk-${feature}"
      ;;
    temurin)
      JDK_SOURCE_IMAGE="docker.io/library/eclipse-temurin:${feature}"
      JDK_SOURCE_JAVA_HOME="/opt/java/openjdk"
      ;;
    *)
      for i in "${!CUSTOM_JDK_TOKENS[@]}"; do
        if [[ "${CUSTOM_JDK_TOKENS[$i]}" == "$token" ]]; then
          JDK_SOURCE_IMAGE="${CUSTOM_JDK_IMAGES[$i]}"
          JDK_SOURCE_JAVA_HOME="${CUSTOM_JDK_JAVA_HOMES[$i]}"
          break
        fi
      done
      [[ -n "$JDK_SOURCE_IMAGE" && -n "$JDK_SOURCE_JAVA_HOME" ]] \
        || die "distribution '${dist}' is not curated and ${token} has no custom JDK source"
      ;;
  esac
  JDK_SOURCE_IMAGE="$(mirror_ref "$JDK_SOURCE_IMAGE")"
}

# Copy-from-image: pull and mount the source image through host containerd, then
# copy its complete userland root. The shim uses that root as the sandbox lower
# layer and mounts source.javaHome at /opt/jdk. Mount/copy avoids requiring a
# shell or package tools in custom jlink images.
jdk_from_image() {
  local dist="$1" feature="$2" dest="$3" image src mount_dir
  resolve_jdk_source "$dist" "$feature"
  image="$JDK_SOURCE_IMAGE"
  src="$JDK_SOURCE_JAVA_HOME"
  log "  pulling $image"
  host_ctr image pull "$image" >/dev/null
  mount_dir="$PREFIX/.image-mount-${dist}-${feature}"
  host_exec mkdir -p "$mount_dir"
  if ! host_ctr images mount "$image" "$mount_dir" >/dev/null; then
    host_exec rmdir "$mount_dir" >/dev/null 2>&1 || true
    die "could not mount JDK source image $image"
  fi
  if ! host_exec cp -a "$mount_dir/." "$dest/"; then
    host_ctr images unmount "$mount_dir" >/dev/null 2>&1 || true
    host_exec rmdir "$mount_dir" >/dev/null 2>&1 || true
    die "could not copy JDK source image $image"
  fi
  host_ctr images unmount "$mount_dir" >/dev/null
  host_exec rmdir "$mount_dir" >/dev/null 2>&1 || true
  mkdir -p "$dest/proc"
  printf '%s\n' "$src" >"$dest/$JDK_HOME_METADATA"
  printf '%s\n%s\n' "$image" "$src" >"$dest/$JDK_SOURCE_METADATA"
}

# ---------------------------------------------------------------------------
# Step 2b — install launcher layers (e.g. jaz).  Independent of the JDKs (§5.4).
# ---------------------------------------------------------------------------
install_launcher() {
  local name="$1"
  local dest="$PREFIX/launchers/${name}"
  if [[ "$name" == "java" ]]; then
    log "launcher 'java' is provided by the JDK root; nothing to stage"
    return 0
  fi
  if [[ -x "$dest/bin/${name}" ]]; then
    log "launcher ${name} already present — skipping"
    return 0
  fi
  log "installing launcher ${name} -> $dest/bin/${name}"
  mkdir -p "$dest/bin"
  case "$name" in
    jaz)
      # jaz ships preinstalled in the Microsoft Build of OpenJDK images, so copy
      # it out via the host containerd; otherwise fall back to a binary baked into
      # the provisioner image at /opt/brewlet-dist/launchers/jaz.
      if [[ -x "$HOST_CTR" ]]; then
        local image="mcr.microsoft.com/openjdk/jdk:25-ubuntu"
        image="$(mirror_ref "$image")"
        host_ctr image pull "$image" >/dev/null
        host_ctr run --rm --net-host \
          --mount "type=bind,src=${dest},dst=/out,options=rbind:rw" \
          "$image" "brewlet-launcher-copy-jaz" cp -a /usr/bin/jaz /out/bin/jaz
      elif [[ -x /opt/brewlet-dist/launchers/jaz ]]; then
        install -m 0755 /opt/brewlet-dist/launchers/jaz "$dest/bin/jaz"
      else
        die "cannot install launcher 'jaz': ctr unavailable and no baked binary at /opt/brewlet-dist/launchers/jaz"
      fi ;;
    *)
      die "unknown launcher '${name}'" ;;
  esac
  chmod -R a-w "$dest" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Step 3 — register the brewlet runtime in containerd and reload.
# ---------------------------------------------------------------------------
patch_containerd() {
  [[ -f "$CONTAINERD_CONFIG" ]] || die "containerd config not found at $CONTAINERD_CONFIG"
  if grep -q 'io.containerd.grpc.v1.cri".containerd.runtimes.brewlet' "$CONTAINERD_CONFIG"; then
    log "containerd already has the brewlet runtime — skipping config patch"
    return 0
  fi
  log "registering brewlet runtime in $CONTAINERD_CONFIG"
  cp -a "$CONTAINERD_CONFIG" "${CONTAINERD_CONFIG}.brewlet.bak"

  # Mirror the node's cgroup driver. containerd's CRI plugin only synthesizes the
  # runc-native options for the built-in runc runtime types; for a custom handler
  # like brewlet it passes a generic runtimeoptions.Options carrying this block
  # verbatim, which the shim translates back into runc options (SystemdCgroup
  # must match the kubelet cgroup driver or pod cgroups are created in the wrong
  # place). Default to the container-runtime norm and inherit the value the
  # existing runc runtime already uses when we can detect it.
  local systemd_cgroup="true"
  if grep -qiE '^[[:space:]]*SystemdCgroup[[:space:]]*=[[:space:]]*false' "$CONTAINERD_CONFIG"; then
    systemd_cgroup="false"
  fi

  cat >>"$CONTAINERD_CONFIG" <<EOF

# --- added by brewlet-node-provisioner (https://github.com/brewlet/brewlet/tree/main/specs §5.2) ---
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
  # Propagate the deployment-descriptor annotations the admission webhook stamps
  # (brewlet.sh/artifact-ref, artifact-digest, jdk, launcher, cds-regenerate)
  # onto the OCI spec so the shim can resolve the artifact and apply node-side
  # AppCDS regeneration. Without this allowlist CRI drops them and the shim has
  # no manifest digest to read from the content store.
  pod_annotations = ["brewlet.sh/*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet.options]
    SystemdCgroup = ${systemd_cgroup}
# --- end brewlet ---
EOF
}

# validate_runtime is the "validated" restart gate: before we ask containerd to
# pick up the brewlet runtime, smoke-test every installed JDK root so a broken
# copy-from-image never gets flipped live. Honors BREWLET_VALIDATE=false (skip).
validate_runtime() {
  if [[ "${BREWLET_VALIDATE}" == "false" ]]; then
    log "validation disabled (BREWLET_VALIDATE=false); skipping smoke test"
    return 0
  fi
  local spec dist feature root java_home
  IFS=',' read -ra _jdks <<<"$JDKS"
  for spec in "${_jdks[@]}"; do
    [[ -n "$spec" ]] || continue
    dist="${spec%-*}"; feature="${spec##*-}"
    root="$PREFIX/jdks/${dist}-${feature}"
    java_home="$(jdk_home_in_root "$root")"
    jdk_root_complete "$root" \
      || die "validation failed: '${java_home%/}/bin/java -version' errored inside ${root} for ${spec}"
  done
  log "validation passed: all JDK roots smoke-tested"
}

# maybe_reload_containerd applies BREWLET_CONTAINERD_RESTART (§5.6): reload
# always, only after validation, or never (defer to an out-of-band restart).
maybe_reload_containerd() {
  case "${BREWLET_CONTAINERD_RESTART}" in
    none)
      log "BREWLET_CONTAINERD_RESTART=none; leaving containerd untouched (restart it out of band to activate the brewlet runtime)" ;;
    sighup)
      reload_containerd ;;
    validated|"")
      validate_runtime
      reload_containerd ;;
    *)
      die "invalid BREWLET_CONTAINERD_RESTART='${BREWLET_CONTAINERD_RESTART}' (want: validated|sighup|none)" ;;
  esac
}

# Reload containerd so it picks up the new runtime. We SIGHUP the host containerd
# process (the DaemonSet runs with hostPID: true, so its PID is visible here).
reload_containerd() {
  local pid
  pid="$(pgrep -x containerd | head -1 || true)"
  if [[ -n "$pid" ]]; then
    log "reloading containerd (SIGHUP pid ${pid})"
    kill -HUP "$pid" || log "WARN: could not signal containerd; a manual restart may be needed"
  else
    log "WARN: containerd process not visible (need hostPID: true); skipping reload"
  fi
}

# ---------------------------------------------------------------------------
# Step 4 — verify the shim is resolvable.
# ---------------------------------------------------------------------------
verify_shim() {
  # The Runtime v2 shim has no standalone version flag; existence + exec bit is
  # the practical smoke test here (containerd invokes it via the TTRPC protocol).
  [[ -x "$PREFIX/bin/$SHIM_NAME" ]] && log "shim binary present and executable" \
    || die "shim binary missing after install"
}

# ---------------------------------------------------------------------------
# Step 5 — advertise readiness on the Node object.
# ---------------------------------------------------------------------------

# Minimal JSON string escaper (backslash + double-quote); enough for the vendor
# strings and versions we emit into the brewlet.sh/jdks-info annotation.
json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

# Emit a JSON object describing one installed JDK root, reading vendor / full
# (minor) version / architecture straight from the JDK via -XshowSettings. Prints
# nothing (returns non-zero) when the root's java binary is missing.
jdk_info_obj() {
  local spec="$1" dist feature root java_home props ver vendor arch
  dist="${spec%-*}"; feature="${spec##*-}"
  root="$PREFIX/jdks/${dist}-${feature}"
  java_home="$(jdk_home_in_root "$root")"
  jdk_root_complete "$root" || return 1
  props="$(jdk_java "$root" "$java_home" -XshowSettings:properties -version 2>&1 || true)"
  ver="$(printf '%s\n'    "$props" | sed -n 's/^[[:space:]]*java\.version[[:space:]]*=[[:space:]]*//p' | head -1)"
  vendor="$(printf '%s\n' "$props" | sed -n 's/^[[:space:]]*java\.vendor[[:space:]]*=[[:space:]]*//p'  | head -1)"
  arch="$(printf '%s\n'   "$props" | sed -n 's/^[[:space:]]*os\.arch[[:space:]]*=[[:space:]]*//p'      | head -1)"
  printf '{"distribution":"%s","vendor":"%s","feature":%s,"version":"%s","arch":"%s"}' \
    "$(json_escape "$dist")" "$(json_escape "$vendor")" "${feature:-0}" \
    "$(json_escape "$ver")" "$(json_escape "$arch")"
}

# Build the rich inventory annotation value: a JSON array of jdk_info_obj entries
# for every installed root in $JDKS.
jdks_info_json() {
  local first=1 obj out="["
  for j in "$@"; do
    [[ -n "$j" ]] || continue
    obj="$(jdk_info_obj "$j" || true)"
    [[ -n "$obj" ]] || continue
    if [[ $first -eq 1 ]]; then first=0; else out+=","; fi
    out+="$obj"
  done
  out+="]"
  printf '%s' "$out"
}

label_node() {
  command -v kubectl >/dev/null || { log "WARN: kubectl not present; skipping node labelling"; return 0; }
  # Advertise the inventory as a comma-separated list; a single annotation value
  # carries commas fine (only label *values* can't).
  local jdk_ann launcher_ann jdks_info
  jdk_ann="${JDKS}"
  launcher_ann="java${LAUNCHERS:+,${LAUNCHERS}}"
  IFS=',' read -ra _jdks <<<"$JDKS"
  # Rich, developer-facing inventory (vendor, major, minor version, arch) so
  # devs can inspect prod JDKs via `kubectl get nodes` or `brewlet jdks`.
  jdks_info="$(jdks_info_json "${_jdks[@]}")"
  log "labelling node ${NODE_NAME} ready; jdks=${jdk_ann} launchers=${launcher_ann}"
  log "advertising jdks-info=${jdks_info}"
  kubectl annotate node "$NODE_NAME" \
    "brewlet.sh/jdks=${jdk_ann}" \
    "brewlet.sh/jdks-info=${jdks_info}" \
    "brewlet.sh/launchers=${launcher_ann}" \
    "brewlet.sh/profile=${BREWLET_PROFILE_NAME}" \
    "brewlet.sh/profile-generation=${BREWLET_PROFILE_GENERATION}" --overwrite || return 1

  # Per-capability labels drive the admission webhook's nodeAffinity so the
  # scheduler skips incompatible nodes (annotations can't drive nodeAffinity).
  # For each JDK <dist>-<feature> we emit both an exact label and a
  # distribution-agnostic feature label; for each launcher (incl. built-in java)
  # a presence label. See https://github.com/brewlet/brewlet/tree/main/specs §8/§14.
  local caps=()
  for j in "${_jdks[@]}"; do
    [[ -n "$j" ]] || continue
    caps+=( "brewlet.sh/jdk.${j}=true" "brewlet.sh/jdk-feature.${j##*-}=true" )
  done
  caps+=( "brewlet.sh/launcher.java=true" )
  if [[ -n "$LAUNCHERS" ]]; then
    IFS=',' read -ra _launchers <<<"$LAUNCHERS"
    for l in "${_launchers[@]}"; do
      [[ -n "$l" ]] && caps+=( "brewlet.sh/launcher.${l}=true" )
    done
  fi
  log "advertising scheduling labels: ${caps[*]}"
  kubectl label node "$NODE_NAME" "${caps[@]}" --overwrite || return 1
  kubectl label node "$NODE_NAME" brewlet.sh/runtime=ready --overwrite || return 1
}

clear_node_advertisement() {
  command -v kubectl >/dev/null || return 0
  local old_jdks old_launchers caps=()
  old_jdks="$(kubectl get node "$NODE_NAME" -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}' 2>/dev/null || true)"
  old_launchers="$(kubectl get node "$NODE_NAME" -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}' 2>/dev/null || true)"
  IFS=',' read -ra _old_jdks <<<"$old_jdks"
  for j in "${_old_jdks[@]}"; do
    [[ -n "$j" ]] || continue
    caps+=( "brewlet.sh/jdk.${j}-" "brewlet.sh/jdk-feature.${j##*-}-" )
  done
  IFS=',' read -ra _old_launchers <<<"$old_launchers"
  for l in "${_old_launchers[@]}"; do
    [[ -n "$l" ]] && caps+=( "brewlet.sh/launcher.${l}-" )
  done
  kubectl label node "$NODE_NAME" brewlet.sh/runtime- "${caps[@]}" >/dev/null 2>&1 || return 1
  kubectl annotate node "$NODE_NAME" \
    brewlet.sh/jdks- brewlet.sh/jdks-info- brewlet.sh/launchers- \
    brewlet.sh/profile- brewlet.sh/profile-generation- \
    "${ANNOTATION_PROVISION_ERROR}-" >/dev/null 2>&1 || return 1
}

write_active_jdk_inventory() {
  local tmp="$PREFIX/jdks/${JDK_ACTIVE_INVENTORY}.tmp.$$"
  mkdir -p "$PREFIX/jdks"
  tr ',' '\n' <<<"$JDKS" >"$tmp"
  mv "$tmp" "$PREFIX/jdks/$JDK_ACTIVE_INVENTORY"
}

# ---------------------------------------------------------------------------
# Reversal (BREWLET_MODE=cleanup) — undo everything provision mode installed for
# a deleted NodeProfile (§5.6), following the kata-deploy cleanup pattern.
# ---------------------------------------------------------------------------

# Remove the brewlet runtime block from containerd's config. Prefer restoring the
# pre-brewlet backup patch_containerd wrote; otherwise strip the fenced block in
# place so we never clobber unrelated edits made after provisioning.
unpatch_containerd() {
  [[ -f "$CONTAINERD_CONFIG" ]] || { log "containerd config not found; nothing to unpatch"; return 0; }
  if ! grep -q 'io.containerd.grpc.v1.cri".containerd.runtimes.brewlet' "$CONTAINERD_CONFIG"; then
    log "brewlet runtime not present in $CONTAINERD_CONFIG — nothing to remove"
    return 0
  fi
  if [[ -f "${CONTAINERD_CONFIG}.brewlet.bak" ]]; then
    log "restoring containerd config from ${CONTAINERD_CONFIG}.brewlet.bak"
    cp -a "${CONTAINERD_CONFIG}.brewlet.bak" "$CONTAINERD_CONFIG"
    rm -f "${CONTAINERD_CONFIG}.brewlet.bak"
  else
    log "stripping brewlet config block in place (no backup found)"
    sed -i.brewlet-cleanup '/# --- added by brewlet-node-provisioner/,/# --- end brewlet ---/d' "$CONTAINERD_CONFIG"
    rm -f "${CONTAINERD_CONFIG}.brewlet-cleanup"
  fi
}

remove_shim() {
  rm -f "$PREFIX/bin/$SHIM_NAME" "$HOST_BIN/$SHIM_NAME" "$HOST_CTR"
  log "removed shim binaries and host ctr helper"
}

unlabel_node() {
  command -v kubectl >/dev/null || { log "WARN: kubectl not present; skipping node unlabelling"; return 0; }
  log "removing brewlet runtime labels/annotations from ${NODE_NAME}"
  # Drop readiness + inventory annotations and the runtime-ready label.
  kubectl annotate node "$NODE_NAME" \
    brewlet.sh/jdks- brewlet.sh/jdks-info- brewlet.sh/launchers- \
    brewlet.sh/profile- brewlet.sh/profile-generation- \
    "${ANNOTATION_PROVISION_ERROR}-" >/dev/null 2>&1 || true
  kubectl label node "$NODE_NAME" brewlet.sh/runtime- >/dev/null 2>&1 || true

  # Drop every per-capability scheduling label this profile could have set.
  local caps=()
  IFS=',' read -ra _jdks <<<"$JDKS"
  for j in "${_jdks[@]}"; do
    [[ -n "$j" ]] || continue
    caps+=( "brewlet.sh/jdk.${j}-" "brewlet.sh/jdk-feature.${j##*-}-" )
  done
  caps+=( "brewlet.sh/launcher.java-" )
  if [[ -n "$LAUNCHERS" ]]; then
    IFS=',' read -ra _launchers <<<"$LAUNCHERS"
    for l in "${_launchers[@]}"; do
      [[ -n "$l" ]] && caps+=( "brewlet.sh/launcher.${l}-" )
    done
  fi
  [[ ${#caps[@]} -gt 0 ]] && kubectl label node "$NODE_NAME" "${caps[@]}" >/dev/null 2>&1 || true
}

cleanup_node() {
  log "cleaning up node ${NODE_NAME} for deleted NodeProfile (BREWLET_MODE=cleanup)"
  # Remove the runtime first so no new brewlet pods land while we tear down, then
  # reload containerd (unless disabled), drop the shim, and unlabel the node.
  unpatch_containerd
  case "${BREWLET_CONTAINERD_RESTART}" in
    none) log "BREWLET_CONTAINERD_RESTART=none; leaving containerd reload to an out-of-band restart" ;;
    *)    reload_containerd ;;
  esac
  remove_shim
  unlabel_node
  log "node ${NODE_NAME} cleanup complete"

  # Stay Ready so the operator can observe the cleanup DaemonSet as complete
  # before it removes the profile finalizer and deletes this DaemonSet (§5.6).
  log "entering idle loop; the pod stays Ready so the operator can confirm cleanup"
  exec sleep infinity
}

main() {
  parse_mirrors
  parse_custom_jdk_sources

  if [[ "${BREWLET_MODE}" == "cleanup" ]]; then
    cleanup_node
    return 0
  fi

  log "provisioning node ${NODE_NAME} (arch $(host_arch_oci), copy-from-image)"
  clear_node_advertisement || die "could not remove stale node readiness before provisioning"
  require_cgroup_v2
  install_shim

  IFS=',' read -ra jdk_list <<<"$JDKS"
  for j in "${jdk_list[@]}"; do [[ -n "$j" ]] && install_jdk "$j"; done
  write_active_jdk_inventory

  if [[ -n "$LAUNCHERS" ]]; then
    IFS=',' read -ra launcher_list <<<"$LAUNCHERS"
    for l in "${launcher_list[@]}"; do [[ -n "$l" ]] && install_launcher "$l"; done
  fi

  patch_containerd
  maybe_reload_containerd
  verify_shim
  label_node || die "could not publish node runtime inventory"
  # Clear any stale provision-error from a previous failed attempt now we're good.
  command -v kubectl >/dev/null && kubectl annotate node "$NODE_NAME" "${ANNOTATION_PROVISION_ERROR}-" >/dev/null 2>&1 || true
  log "node ${NODE_NAME} provisioned successfully"

  # Keep provisioning independent from observability. The exporter runs as a
  # sidecar in the same pod, so an exporter failure cannot reprovision the node.
  log "entering idle loop; the pod stays Ready to keep the node provisioned"
  exec sleep infinity
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
