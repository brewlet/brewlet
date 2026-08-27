#!/usr/bin/env bash
# Tier 8 — node-side AppCDS regeneration, proven END TO END in a real cluster.
#
# This is the decisive coverage for docs/appcds.md §4.3/§8: it provisions a real
# brewlet node (shim + full-userland JDK root + containerd `brewlet` runtime),
# deploys a genuine `runtimeClassName: brewlet` pod carrying the deployment-
# descriptor annotations the admission webhook stamps (brewlet.sh/artifact-*,
# jdk, cds-regenerate), and then:
#
#   1. WRITE   rollout: the elected writer launches with
#              -XX:+AutoCreateSharedArchive -XX:SharedArchiveFile=<node cache>.
#              A graceful delete lets the JVM dump the archive into the node
#              cache (/opt/brewlet/cds) — we assert a .jsa lands there.
#   2. CONSUME rollout: with a valid archive present the next pod launches with
#              -Xshare:auto -XX:SharedArchiveFile (NO AutoCreate). We assert the
#              args flipped AND that the .jsa is actually mmap'd into the JVM
#              (via /proc/1/maps) — a real CDS hit, not a silent fallback.
#
# Unlike tier6 (which only side-loads a normal image), this tier provisions the
# whole Brewlet runtime by hand — equivalent to the core provisioner — so it
# only runs where the nodes are local containerd docker containers we can reach
# with `docker exec` (kind / CI). It SKIPs everywhere else.
#
# Prereqs: kubectl + reachable cluster, docker (nodes are local containers),
# go, a JDK 21+ ($JAVA_HOME) to build the demo JAR, and network access to pull
# eclipse-temurin:21 for the JDK userland root.
#
# NOTE: brewlet artifacts use custom OCI layer media types that kubelet's
# ImageStatus cannot unpack, so the pod `image` is a normal placeholder
# (busybox) and the artifact is delivered purely via annotations — the shim's
# setupOverlayRootfs replaces the rootfs entirely; as with SpinKube's Wasm
# runtime path, no Linux userland from the workload image is executed.

T8_REF="demo/hello:appcds-e2e"
T8_JDK="temurin-21"
T8_TEMURIN_IMG="eclipse-temurin:21"
T8_NS="brewlet-appcds-ic"
T8_CACHE="/opt/brewlet/cds"
T8_JDK_ROOT="/opt/brewlet/jdks/$T8_JDK"
T8_SHIM_DST="/usr/local/bin/containerd-shim-brewlet-v2"
T8_RC_CREATED=""
T8_NODE=""
declare -a T8_PROVISIONED_NODES=()

_t8_cleanup() {
  info "tier8: cleaning up"
  kubectl delete ns "$T8_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ -n "$T8_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in ${T8_PROVISIONED_NODES[@]+"${T8_PROVISIONED_NODES[@]}"}; do
    label_node "$n" brewlet.sh/runtime- >/dev/null 2>&1 || true
    annotate_node "$n" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  done
  # We intentionally leave the node's shim binary / JDK root / config.toml patch
  # in place: they are cheap, idempotent, and reused on a re-run. The disposable
  # cluster is torn down by the operator anyway.
}

# _t8_node_arch NODE -> prints go-style arch (amd64|arm64) for the node.
_t8_node_arch() {
  case "$(docker exec "$1" uname -m 2>/dev/null)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *)             echo "" ;;
  esac
}

# _t8_stage_jdk NODE ARCH: export a self-contained temurin userland into the
# node at $T8_JDK_ROOT (§5.3 needs the ELF interpreter + libc at the root, so a
# bare JDK home is not enough). Idempotent: skips if already staged.
_t8_stage_jdk() {
  local node="$1" arch="$2"
  if docker exec "$node" chroot "$T8_JDK_ROOT" /bin/java -version >/dev/null 2>&1; then
    printf '%s\n' "$T8_JDK" | docker exec -i "$node" sh -c \
      'cat > /opt/brewlet/jdks/.brewlet-active'
    return 0
  fi
  # A provisioner-created bare JDK home can execute from the host while still
  # lacking the ELF loader and libc required when it becomes the sandbox root.
  docker exec "$node" rm -rf "$T8_JDK_ROOT" >/dev/null 2>&1 || return 1
  local cid tarball="$WORK/t8-jdk-$arch.tar"
  if [[ ! -f "$tarball" ]]; then
    docker pull --platform "linux/$arch" "$T8_TEMURIN_IMG" >>"$WORK/t8-jdk.log" 2>&1 || return 1
    cid="$(docker create --platform "linux/$arch" "$T8_TEMURIN_IMG" 2>>"$WORK/t8-jdk.log")" || return 1
    docker export "$cid" -o "$tarball" 2>>"$WORK/t8-jdk.log" || { docker rm -f "$cid" >/dev/null 2>&1; return 1; }
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  docker exec "$node" mkdir -p "$T8_JDK_ROOT" >/dev/null 2>&1
  docker cp "$tarball" "$node":/opt/brewlet/jdk-root.tar >>"$WORK/t8-jdk.log" 2>&1 || return 1
  docker exec "$node" tar -xf /opt/brewlet/jdk-root.tar -C "$T8_JDK_ROOT" >>"$WORK/t8-jdk.log" 2>&1 || return 1
  docker exec "$node" rm -f /opt/brewlet/jdk-root.tar >/dev/null 2>&1 || true
  # temurin puts the JDK at /opt/java/openjdk; the shim's selectJDK does
  # os.Stat(<root>/bin/java). Debian usrmerge means /bin -> usr/bin, so create a
  # RELATIVE symlink at usr/bin/java (absolute ones dangle on the host).
  docker exec "$node" sh -c \
    "test -e '$T8_JDK_ROOT/bin/java' || ln -sf ../../opt/java/openjdk/bin/java '$T8_JDK_ROOT/usr/bin/java'" \
    >>"$WORK/t8-jdk.log" 2>&1 || return 1
  docker exec "$node" test -x "$T8_JDK_ROOT/bin/java" || return 1
  printf '%s\n' "$T8_JDK" | docker exec -i "$node" sh -c \
    'cat > /opt/brewlet/jdks/.brewlet-active'
}

# _t8_patch_containerd NODE: register the brewlet runtime with the same
# annotation passthrough + cgroup-driver handling the core provisioner installs.
# Idempotent: skips when the brewlet block already exists.
_t8_patch_containerd() {
  local node="$1"
  if docker exec "$node" grep -q 'containerd.runtimes.brewlet\]' /etc/containerd/config.toml 2>/dev/null; then
    return 0
  fi
  local systemd=true
  docker exec "$node" grep -qiE '^[[:space:]]*SystemdCgroup[[:space:]]*=[[:space:]]*false' /etc/containerd/config.toml 2>/dev/null && systemd=false
  docker exec -i "$node" sh -c "cat >>/etc/containerd/config.toml" <<EOF

# --- added by e2e tier8 (mirrors brewlet/brewlet provisioner/entrypoint.sh) ---
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
  pod_annotations = ["brewlet.sh/*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet.options]
    SystemdCgroup = ${systemd}
# --- end brewlet ---
EOF
  docker exec "$node" grep -q 'containerd.runtimes.brewlet\]' /etc/containerd/config.toml 2>/dev/null || return 1
  docker exec "$node" systemctl restart containerd >>"$WORK/t8-containerd.log" 2>&1 || return 1
  # containerd needs a moment to come back and re-register CRI.
  local tries=20
  while (( tries-- > 0 )); do
    docker exec "$node" ctr version >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# _t8_pod_cmdline POD: prints the launched JVM argv (NULs -> spaces).
_t8_pod_cmdline() {
  kubectl exec -n "$T8_NS" "$1" -- cat /proc/1/cmdline 2>/dev/null | tr '\0' ' '
}

tier8_appcds_incluster() {
  section "Tier 8 — node-side AppCDS regeneration in-cluster (write -> consume)"
  if ! have kubectl || ! k8s_reachable; then skip "tier8: appcds in-cluster" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier8: appcds in-cluster" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier8: appcds in-cluster" "go not installed"; return 0; fi

  # Pick a node that is a local containerd docker container we can provision,
  # preferring one that is schedulable (kind's single node, or a Docker Desktop
  # worker — not the tainted control-plane, where the pod would sit Pending).
  T8_NODE="$(pick_provisionable_node)"
  if [[ -z "$T8_NODE" ]]; then
    skip "tier8: appcds in-cluster" "no node is a local containerd docker container (need kind/CI)"; return 0
  fi
  if ! node_schedulable "$T8_NODE"; then
    skip "tier8: appcds in-cluster" "only provisionable node ($T8_NODE) is unschedulable (NoSchedule taint); need an untainted worker node"; return 0
  fi
  local arch; arch="$(_t8_node_arch "$T8_NODE")"
  if [[ -z "$arch" ]]; then skip "tier8: appcds in-cluster" "unknown node arch"; return 0; fi
  info "tier8: node=$T8_NODE arch=$arch"

  trap _t8_cleanup RETURN

  # --- build shim (for the node arch), brewlet CLI, and the demo JAR ---------
  info "tier8: building shim ($arch), brewlet CLI, demo JAR"
  local shimbin="$WORK/t8-shim-$arch"
  : >"$WORK/t8-build.log"
  local build_try build_ok=0
  for build_try in 1 2; do
    if ( cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -p "${GO_BUILD_PARALLELISM:-2}" -o "$shimbin" ./shim/cmd/containerd-shim-brewlet-v2 ) >>"$WORK/t8-build.log" 2>&1; then
      build_ok=1
      break
    fi
    [[ "$build_try" -lt 2 ]] && warn "tier8: build shim failed (attempt $build_try/2); retrying"
  done
  if [[ "$build_ok" -ne 1 ]]; then
    tail -n 80 "$WORK/t8-build.log" >&2 || true
    fail "tier8: build shim" "see $WORK/t8-build.log"; return 0
  fi
  if ! ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t8-brewlet" ./cmd/brewlet ) >>"$WORK/t8-build.log" 2>&1; then
    fail "tier8: build brewlet CLI" "see $WORK/t8-build.log"; return 0
  fi
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"
  if [[ ! -f "$jar" ]]; then
    if ! have java; then
      skip "tier8: appcds in-cluster" "demo JAR absent and no JDK to build it (set JAVA_HOME, JDK 21+)"; return 0
    fi
    local jh; jh="$(resolve_java_home)"
    if ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" >>"$WORK/t8-build.log" 2>&1; then
      fail "tier8: build demo JAR" "see $WORK/t8-build.log"; return 0
    fi
  fi
  pass "tier8: built shim + CLI + demo JAR"

  # --- push the artifact with node-side regeneration opted in ----------------
  # Push the artifact normally. Node-side regeneration is NOT baked into the
  # artifact (PR #76): it is a deployment-time decision the operator carries on
  # the pod as brewlet.sh/cds-regenerate (set on the pods below).
  local store="$WORK/t8-oci"; rm -rf "$store"
  if ! "$WORK/t8-brewlet" push "$jar" "$T8_REF" --store "$store" --format=artifact >>"$WORK/t8-build.log" 2>&1; then
    fail "tier8: push artifact" "see $WORK/t8-build.log"; return 0
  fi
  # Manifest digest the shim resolves from the node content store (annotation).
  local digest
  digest="$(python3 - "$store" "$T8_REF" <<'PY'
import json, sys
root, ref = sys.argv[1], sys.argv[2]
tag = ref.split(":")[-1]
idx = json.load(open(f"{root}/index.json"))
for m in idx["manifests"]:
    ann = m.get("annotations", {})
    if ann.get("org.opencontainers.image.ref.name") in (ref, tag):
        print(m["digest"]); break
else:
    print(idx["manifests"][0]["digest"])
PY
)"
  if [[ -z "$digest" ]]; then fail "tier8: resolve artifact digest" "index.json had no manifest"; return 0; fi
  info "tier8: artifact digest $digest"

  # Import the OCI layout into the node's k8s.io content store (by digest). The
  # resolver reads blobs from the content store; the tag is cosmetic.
  if ! ( cd "$store" && tar -cf - . ) | docker exec -i "$T8_NODE" ctr -n k8s.io images import --digests - >>"$WORK/t8-import.log" 2>&1; then
    # older ctr lacks --digests; retry without it
    if ! ( cd "$store" && tar -cf - . ) | docker exec -i "$T8_NODE" ctr -n k8s.io images import - >>"$WORK/t8-import.log" 2>&1; then
      fail "tier8: import artifact into node content store" "see $WORK/t8-import.log"; return 0
    fi
  fi
  pass "tier8: pushed + imported artifact ($T8_REF)"

  # --- provision the node: shim binary, JDK userland, containerd runtime -----
  docker cp "$shimbin" "$T8_NODE":"$T8_SHIM_DST" >>"$WORK/t8-prov.log" 2>&1
  docker exec "$T8_NODE" chmod +x "$T8_SHIM_DST" >>"$WORK/t8-prov.log" 2>&1
  docker exec "$T8_NODE" mkdir -p "$T8_CACHE" >>"$WORK/t8-prov.log" 2>&1
  if ! _t8_stage_jdk "$T8_NODE" "$arch"; then
    skip "tier8: appcds in-cluster" "could not stage temurin JDK userland (see $WORK/t8-jdk.log)"; return 0
  fi
  if ! _t8_patch_containerd "$T8_NODE"; then
    fail "tier8: register brewlet containerd runtime" "see $WORK/t8-containerd.log"; return 0
  fi
  if ! label_node "$T8_NODE" --overwrite brewlet.sh/runtime=ready >>"$WORK/t8-prov.log" 2>&1; then
    fail "tier8: advertise node runtime label" "see $WORK/t8-prov.log"; return 0
  fi
  if ! annotate_node "$T8_NODE" --overwrite "brewlet.sh/jdks=$T8_JDK" "brewlet.sh/launchers=" >>"$WORK/t8-prov.log" 2>&1; then
    fail "tier8: advertise node JDK annotations" "see $WORK/t8-prov.log"; return 0
  fi
  T8_PROVISIONED_NODES+=("$T8_NODE")
  pass "tier8: provisioned node ($T8_NODE)"

  # --- RuntimeClass + namespace ---------------------------------------------
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T8_RC_CREATED=1
  fi
  kubectl create namespace "$T8_NS" >/dev/null 2>&1 || true

  # A pod carrying exactly the annotations the admission webhook stamps from
  # spec.jvm.cds.regenerate. $1 = pod name.
  _t8_apply_pod() {
    kubectl apply -n "$T8_NS" -f - >>"$WORK/t8-pod.log" 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $1
  annotations:
    brewlet.sh/artifact-ref: "$T8_REF"
    brewlet.sh/artifact-digest: "$digest"
    brewlet.sh/jdk: "$T8_JDK"
    brewlet.sh/cds-regenerate: "true"
spec:
  runtimeClassName: brewlet
  terminationGracePeriodSeconds: 30
  nodeSelector: { brewlet.sh/runtime: ready }
  containers:
    - name: app
      image: busybox:1.36
      command: ["sleep", "3600"]
      readinessProbe:
        httpGet: { path: /healthz, port: 8080 }
        initialDelaySeconds: 1
        periodSeconds: 2
        failureThreshold: 30
YAML
  }

  # --- ROLLOUT 1: writer -----------------------------------------------------
  # Start from a clean cache so rollout 1 deterministically elects a WRITER
  # (a leftover archive from a previous run would make it a consumer instead).
  docker exec "$T8_NODE" sh -c "rm -f $T8_CACHE/*.jsa $T8_CACHE/*.jsa.writer 2>/dev/null" || true
  info "tier8: deploying WRITE rollout (regen-writer)"
  _t8_apply_pod regen-writer
  if ! kubectl wait -n "$T8_NS" --for=condition=Ready pod/regen-writer --timeout=120s >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: writer pod Ready" "see $WORK/t8-pod.log; diag: $(save_pod_diag regen-writer "$T8_NS")"
    return 0
  fi
  local wcmd; wcmd="$(_t8_pod_cmdline regen-writer)"
  assert_contains "tier8: writer launches with AutoCreateSharedArchive" "$wcmd" "AutoCreateSharedArchive"
  assert_contains "tier8: writer targets the node CDS cache" "$wcmd" "$(basename "$T8_CACHE")"

  # Graceful delete lets AutoCreateSharedArchive dump the archive at JVM exit.
  info "tier8: graceful delete of writer (JVM dumps the archive on exit)"
  kubectl delete -n "$T8_NS" pod/regen-writer --grace-period=30 --wait=true >>"$WORK/t8-pod.log" 2>&1 || true

  local jsa
  if wait_for docker exec "$T8_NODE" sh -c "ls $T8_CACHE/*.jsa >/dev/null 2>&1"; then
    jsa="$(docker exec "$T8_NODE" sh -c "ls -1 $T8_CACHE/*.jsa 2>/dev/null | head -1" | tr -d '\r')"
    local sz; sz="$(docker exec "$T8_NODE" sh -c "wc -c < '$jsa' 2>/dev/null" | tr -d '[:space:]')"
    if [[ "${sz:-0}" -gt 0 ]]; then
      pass "tier8: writer dumped AppCDS archive to node cache ($(basename "$jsa"), ${sz} bytes)"
    else
      fail "tier8: writer dumped AppCDS archive" "archive is empty"
    fi
  else
    fail "tier8: writer dumped AppCDS archive" "no .jsa in $T8_CACHE after graceful exit"
    return 0
  fi

  # --- ROLLOUT 2: consumer ---------------------------------------------------
  info "tier8: deploying CONSUME rollout (regen-consumer)"
  _t8_apply_pod regen-consumer
  if ! kubectl wait -n "$T8_NS" --for=condition=Ready pod/regen-consumer --timeout=120s >>"$WORK/t8-pod.log" 2>&1; then
    fail "tier8: consumer pod Ready" "see $WORK/t8-pod.log; diag: $(save_pod_diag regen-consumer "$T8_NS")"
    return 0
  fi
  local ccmd; ccmd="$(_t8_pod_cmdline regen-consumer)"
  assert_contains "tier8: consumer launches with -Xshare:auto + SharedArchiveFile" "$ccmd" "SharedArchiveFile"
  assert_not_contains "tier8: consumer does NOT re-create the archive" "$ccmd" "AutoCreateSharedArchive"

  # Decisive proof: the archive is actually mmap'd into the consuming JVM.
  local maps; maps="$(kubectl exec -n "$T8_NS" regen-consumer -- cat /proc/1/maps 2>/dev/null || true)"
  assert_contains "tier8: consumer mmap'd the .jsa (real CDS hit, not fallback)" "$maps" ".jsa"

  kubectl delete -n "$T8_NS" pod/regen-consumer --wait=false >/dev/null 2>&1 || true
}
