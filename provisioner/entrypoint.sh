#!/usr/bin/env bash
# brewlet-node-provisioner entrypoint.
#
# Runs as a privileged DaemonSet pod on nodes annotated brewlet.sh/provision=true
# and performs the host-side installation described in https://github.com/brewlet/specs §5.2:
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
# (https://github.com/brewlet/specs/blob/main/proposals/0001-node-profiles.md §5.7), then idles so the operator can
# observe completion before removing the profile finalizer.
#
# NB: this is privileged and mutates the host. Only run it on nodes the platform
# team controls (https://github.com/brewlet/specs §11).
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

# Declarative inventory (comma-separated). See §5.3 / §5.4.
#   JDKS      = <distribution>-<feature>, e.g. "temurin-21,microsoft-25"
#              (curated distributions: temurin, microsoft)
#   LAUNCHERS = launcher names, e.g. "jaz"
JDKS="${JDKS:-temurin-21}"
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

# Registry mirrors for air-gapped / pull-through setups (§5.6): a
# comma-separated list of "<registry-host>=<mirror-host>" pairs the operator
# renders from spec.registry.mirrors. Every copy-from-image pull rewrites its
# ref's registry host through this map.
MIRRORS="${MIRRORS:-}"

log()  { printf '[brewlet-provisioner] %s\n' "$*"; }

# On a fatal error, record a machine-readable reason on the Node object
# (brewlet.sh/provision-error) before exiting non-zero, so the operator's
# NodeReconciler can flip the node to a Failed state instead of leaving it stuck
# in Provisioning (https://github.com/brewlet/specs §14). Best-effort: never mask the original
# failure if annotating fails.
die()  {
  printf '[brewlet-provisioner] ERROR: %s\n' "$*" >&2
  if [[ "${BREWLET_MODE}" != "cleanup" ]] && command -v kubectl >/dev/null 2>&1 && [[ -n "${NODE_NAME:-}" ]]; then
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

# Map `uname -m` to the OCI platform token (used only for logging here; the copy
# runs on the node so `ctr` selects the matching image platform automatically).
host_arch_oci() {
  case "$(uname -m)" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)  echo "arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

# ---------------------------------------------------------------------------
# Step 0 — preflight: Brewlet requires cgroup v2 (unified hierarchy) on the node.
# Modern container-aware JDKs read their heap/CPU limits directly from cgroup v2;
# a cgroup v1-only (or hybrid) node cannot enforce §10 resource semantics. On such
# a node the provisioner refuses and exits non-zero, so the node is NOT marked
# ready (https://github.com/brewlet/specs §10, §14).
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
  die "cgroup v2 is required but not active on this node (${mount} is '${fstype:-unknown}', with no ${mount}/cgroup.controllers). Brewlet refuses to provision cgroup v1-only nodes; the node will not be marked ready. See https://github.com/brewlet/specs §10/§14."
}

# ---------------------------------------------------------------------------
# Step 1 — install the shim binary onto the host PATH.
# ---------------------------------------------------------------------------
install_shim() {
  [[ -x "$SHIM_SRC" ]] || die "shim binary not found in image at $SHIM_SRC"
  mkdir -p "$PREFIX/bin" "$HOST_BIN"
  # /opt/brewlet/bin is the canonical location; /usr/local/bin is on containerd's
  # PATH so runtime_type = io.containerd.brewlet.v2 resolves the shim binary.
  install -m 0755 "$SHIM_SRC" "$PREFIX/bin/$SHIM_NAME"
  install -m 0755 "$SHIM_SRC" "$HOST_BIN/$SHIM_NAME"
  log "installed shim -> $PREFIX/bin/$SHIM_NAME and $HOST_BIN/$SHIM_NAME"
}

# ---------------------------------------------------------------------------
# Step 2 — install JDK runtime roots.  A root is complete once
# /opt/brewlet/jdks/<dist>-<feature>/bin/java exists (§5.3).
# ---------------------------------------------------------------------------
install_jdk() {
  local spec="$1" dist feature dest
  dist="${spec%%-*}"; feature="${spec##*-}"
  dest="$PREFIX/jdks/${dist}-${feature}"
  if [[ -x "$dest/bin/java" ]]; then
    log "JDK ${spec} already present at $dest — skipping"
    return 0
  fi
  log "installing JDK ${spec} via copy-from-image -> $dest"
  mkdir -p "$dest"
  jdk_from_image "$dist" "$feature" "$dest"
  [[ -x "$dest/bin/java" ]] || die "JDK ${spec} install did not produce $dest/bin/java"
  log "JDK ${spec} ready: $("$dest/bin/java" -version 2>&1 | head -1)"
}

# Copy-from-image: pull the vendor's official JDK image via the host containerd
# and copy the JDK tree out onto the host, so no package manager touches the host.
jdk_from_image() {
  local dist="$1" feature="$2" dest="$3" image src
  case "$dist" in
    microsoft) image="mcr.microsoft.com/openjdk/jdk:${feature}-ubuntu"; src="/usr/lib/jvm/msopenjdk-${feature}" ;;
    temurin)   image="docker.io/library/eclipse-temurin:${feature}"; src="/opt/java/openjdk" ;;
    *) die "distribution '${dist}' is not curated; supported: temurin, microsoft" ;;
  esac
  command -v ctr >/dev/null || die "ctr not found in image (required for copy-from-image)"
  local ctr="ctr --address ${CONTAINERD_ADDRESS} --namespace ${CONTAINERD_NAMESPACE}"
  image="$(mirror_ref "$image")"
  log "  pulling $image"
  $ctr image pull "$image" >/dev/null
  $ctr run --rm --net-host \
    --mount "type=bind,src=${dest},dst=/out,options=rbind:rw" \
    "$image" "brewlet-jdk-copy-${dist}-${feature}" \
    cp -a "${src}/." /out/
  chmod -R a-w "$dest" 2>/dev/null || true
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
      if command -v ctr >/dev/null; then
        local ctr="ctr --address ${CONTAINERD_ADDRESS} --namespace ${CONTAINERD_NAMESPACE}"
        local image="mcr.microsoft.com/openjdk/jdk:25-ubuntu"
        image="$(mirror_ref "$image")"
        $ctr image pull "$image" >/dev/null
        $ctr run --rm --net-host \
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

# --- added by brewlet-node-provisioner (https://github.com/brewlet/specs §5.2) ---
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
  local spec dist feature javabin
  IFS=',' read -ra _jdks <<<"$JDKS"
  for spec in "${_jdks[@]}"; do
    [[ -n "$spec" ]] || continue
    dist="${spec%%-*}"; feature="${spec##*-}"
    javabin="$PREFIX/jdks/${dist}-${feature}/bin/java"
    [[ -x "$javabin" ]] || die "validation failed: $javabin missing for ${spec}"
    "$javabin" -version >/dev/null 2>&1 || die "validation failed: '${javabin} -version' errored for ${spec}"
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
  local spec="$1" dist feature javabin props ver vendor arch
  dist="${spec%%-*}"; feature="${spec##*-}"
  javabin="$PREFIX/jdks/${dist}-${feature}/bin/java"
  [[ -x "$javabin" ]] || return 1
  props="$("$javabin" -XshowSettings:properties -version 2>&1 || true)"
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
  kubectl label   node "$NODE_NAME" brewlet.sh/runtime=ready --overwrite
  kubectl annotate node "$NODE_NAME" \
    "brewlet.sh/jdks=${jdk_ann}" \
    "brewlet.sh/jdks-info=${jdks_info}" \
    "brewlet.sh/launchers=${launcher_ann}" --overwrite

  # Per-capability labels drive the admission webhook's nodeAffinity so the
  # scheduler skips incompatible nodes (annotations can't drive nodeAffinity).
  # For each JDK <dist>-<feature> we emit both an exact label and a
  # distribution-agnostic feature label; for each launcher (incl. built-in java)
  # a presence label. See https://github.com/brewlet/specs §8/§14.
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
  kubectl label node "$NODE_NAME" "${caps[@]}" --overwrite
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
  rm -f "$PREFIX/bin/$SHIM_NAME" "$HOST_BIN/$SHIM_NAME"
  log "removed shim binaries"
}

unlabel_node() {
  command -v kubectl >/dev/null || { log "WARN: kubectl not present; skipping node unlabelling"; return 0; }
  log "removing brewlet runtime labels/annotations from ${NODE_NAME}"
  # Drop readiness + inventory annotations and the runtime-ready label.
  kubectl annotate node "$NODE_NAME" \
    brewlet.sh/jdks- brewlet.sh/jdks-info- brewlet.sh/launchers- \
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

  if [[ "${BREWLET_MODE}" == "cleanup" ]]; then
    cleanup_node
    return 0
  fi

  log "provisioning node ${NODE_NAME} (arch $(host_arch_oci), copy-from-image)"
  require_cgroup_v2
  install_shim

  IFS=',' read -ra jdk_list <<<"$JDKS"
  for j in "${jdk_list[@]}"; do [[ -n "$j" ]] && install_jdk "$j"; done

  if [[ -n "$LAUNCHERS" ]]; then
    IFS=',' read -ra launcher_list <<<"$LAUNCHERS"
    for l in "${launcher_list[@]}"; do [[ -n "$l" ]] && install_launcher "$l"; done
  fi

  patch_containerd
  maybe_reload_containerd
  verify_shim
  label_node
  # Clear any stale provision-error from a previous failed attempt now we're good.
  command -v kubectl >/dev/null && kubectl annotate node "$NODE_NAME" "${ANNOTATION_PROVISION_ERROR}-" >/dev/null 2>&1 || true
  log "node ${NODE_NAME} provisioned successfully"

  # Keep the DaemonSet pod alive so the node stays advertised as provisioned.
  log "entering idle loop; the pod stays Ready to keep the node provisioned"
  exec sleep infinity
}

main "$@"
