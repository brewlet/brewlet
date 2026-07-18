#!/usr/bin/env bash
# Tier 9 — a brewlet workload behind a Service, proven to SERVE REAL TRAFFIC under
# REAL cgroup limits, through kubelet/CRI on a provisioned node.
#
# This is the coverage tiers 4–8 leave open: nothing else runs a brewlet workload
# behind a Deployment + Service and (a) curls it for a 200, (b) proves Brewlet's
# central promise — the cgroup-aware JVM whose availableProcessors / maxMemory are
# driven by spec.resources.limits (not the node's host capacity) — end to end
# through kubelet, or (c) exercises scaling + rolling updates of brewlet pods.
#
# Like tier8, it provisions a real brewlet node by hand (shim + full-userland JDK
# root + containerd `brewlet` runtime), imports the demo artifact into the node
# content store, and delivers the artifact purely via the brewlet.sh/artifact-*
# annotations the admission webhook stamps — the shim's setupOverlayRootfs then
# replaces the rootfs entirely (the KWasm property).
#
# WHY BUILD THE DEPLOYMENT DIRECTLY (not via the operator): brewlet artifacts use
# custom OCI layer media types kubelet's ImageStatus cannot unpack, so a pod whose
# `image` is the artifact ref would ImagePullBackOff. The pod therefore carries a
# normal placeholder image (busybox) and the artifact rides on annotations — the
# same pattern tier8 uses. The operator/webhook RECONCILE paths (JavaApplication →
# Deployment/Service/HPA + artifact stamping + affinity) are covered by tiers 4–7;
# this tier closes the "does the workload actually run and serve?" gap.
#
# Unlike tier8, it uses a Deployment + Service (not a raw pod), curls the Service
# from an in-cluster client, and asserts the JVM's cgroup-aware view + scaling +
# rolling update behaviour.
#
# Prereqs: kubectl + reachable cluster, docker (nodes are local containers), go,
# a JDK 21+ ($JAVA_HOME) to build the demo JAR, network access to pull
# eclipse-temurin:21 for the JDK userland root. SKIPs otherwise.

T9_REF="demo/hello:serving-e2e"
T9_JDK="temurin-21"
T9_TEMURIN_IMG="eclipse-temurin:21"
T9_NS="brewlet-serving"
T9_APP="orders"
T9_PORT=8080
T9_CACHE="/opt/brewlet/cds"
T9_JDK_ROOT="/opt/brewlet/jdks/$T9_JDK"
T9_SHIM_DST="/usr/local/bin/containerd-shim-brewlet-v2"
T9_RC_CREATED=""
T9_NODE=""
declare -a T9_PROVISIONED_NODES=()

_t9_cleanup() {
  info "tier9: cleaning up"
  kubectl delete ns "$T9_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [[ -n "$T9_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in ${T9_PROVISIONED_NODES[@]+"${T9_PROVISIONED_NODES[@]}"}; do
    label_node "$n" brewlet.sh/runtime- "brewlet.sh/jdk.$T9_JDK-" "brewlet.sh/jdk-feature.${T9_JDK##*-}-" brewlet.sh/launcher.java- >/dev/null 2>&1 || true
    annotate_node "$n" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  done
  # The node's shim binary / JDK root / config.toml patch are left in place: they
  # are cheap, idempotent, and reused on a re-run (as in tier8).
}

# _t9_node_arch NODE -> prints go-style arch (amd64|arm64) for the node.
_t9_node_arch() {
  case "$(docker exec "$1" uname -m 2>/dev/null)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *)             echo "" ;;
  esac
}

# _t9_stage_jdk NODE ARCH: export a self-contained temurin userland into the node
# at $T9_JDK_ROOT (the shim needs the ELF interpreter + libc at the root, so a
# bare JDK home is not enough). Idempotent: skips if already staged.
_t9_stage_jdk() {
  local node="$1" arch="$2"
  if docker exec "$node" chroot "$T9_JDK_ROOT" /bin/java -version >/dev/null 2>&1; then
    printf '%s\n' "$T9_JDK" | docker exec -i "$node" sh -c \
      'cat > /opt/brewlet/jdks/.brewlet-active'
    return 0
  fi
  # A provisioner-created bare JDK home can execute from the host while still
  # lacking the ELF loader and libc required when it becomes the sandbox root.
  docker exec "$node" rm -rf "$T9_JDK_ROOT" >/dev/null 2>&1 || return 1
  local cid tarball="$WORK/t9-jdk-$arch.tar"
  if [[ ! -f "$tarball" ]]; then
    docker pull --platform "linux/$arch" "$T9_TEMURIN_IMG" >>"$WORK/t9-jdk.log" 2>&1 || return 1
    cid="$(docker create --platform "linux/$arch" "$T9_TEMURIN_IMG" 2>>"$WORK/t9-jdk.log")" || return 1
    docker export "$cid" -o "$tarball" 2>>"$WORK/t9-jdk.log" || { docker rm -f "$cid" >/dev/null 2>&1; return 1; }
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
  docker exec "$node" mkdir -p "$T9_JDK_ROOT" >/dev/null 2>&1
  docker cp "$tarball" "$node":/opt/brewlet/jdk-root.tar >>"$WORK/t9-jdk.log" 2>&1 || return 1
  docker exec "$node" tar -xf /opt/brewlet/jdk-root.tar -C "$T9_JDK_ROOT" >>"$WORK/t9-jdk.log" 2>&1 || return 1
  docker exec "$node" rm -f /opt/brewlet/jdk-root.tar >/dev/null 2>&1 || true
  # temurin puts the JDK at /opt/java/openjdk; the shim's selectJDK does
  # os.Stat(<root>/bin/java). Debian usrmerge means /bin -> usr/bin, so create a
  # RELATIVE symlink at usr/bin/java (absolute ones dangle on the host).
  docker exec "$node" sh -c \
    "test -e '$T9_JDK_ROOT/bin/java' || ln -sf ../../opt/java/openjdk/bin/java '$T9_JDK_ROOT/usr/bin/java'" \
    >>"$WORK/t9-jdk.log" 2>&1 || return 1
  docker exec "$node" test -x "$T9_JDK_ROOT/bin/java" || return 1
  printf '%s\n' "$T9_JDK" | docker exec -i "$node" sh -c \
    'cat > /opt/brewlet/jdks/.brewlet-active'
}

# _t9_patch_containerd NODE: register the brewlet runtime with the same annotation
# passthrough + cgroup-driver handling the core provisioner installs. Idempotent.
_t9_patch_containerd() {
  local node="$1"
  if docker exec "$node" grep -q 'containerd.runtimes.brewlet\]' /etc/containerd/config.toml 2>/dev/null; then
    return 0
  fi
  local systemd=true
  docker exec "$node" grep -qiE '^[[:space:]]*SystemdCgroup[[:space:]]*=[[:space:]]*false' /etc/containerd/config.toml 2>/dev/null && systemd=false
  docker exec -i "$node" sh -c "cat >>/etc/containerd/config.toml" <<EOF

# --- added by e2e tier9 (mirrors brewlet/brewlet provisioner/entrypoint.sh) ---
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet]
  runtime_type = "io.containerd.brewlet.v2"
  pod_annotations = ["brewlet.sh/*"]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.brewlet.options]
    SystemdCgroup = ${systemd}
# --- end brewlet ---
EOF
  docker exec "$node" grep -q 'containerd.runtimes.brewlet\]' /etc/containerd/config.toml 2>/dev/null || return 1
  docker exec "$node" systemctl restart containerd >>"$WORK/t9-containerd.log" 2>&1 || return 1
  local tries=20
  while (( tries-- > 0 )); do
    docker exec "$node" ctr version >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# _t9_advertise NODE: label + annotate the node exactly as the core provisioner does
# so the demo workload's nodeSelector/affinity land on it (§5.2).
_t9_advertise() {
  local node="$1"
  label_node "$node" --overwrite \
    brewlet.sh/runtime=ready \
    "brewlet.sh/jdk.$T9_JDK=true" \
    "brewlet.sh/jdk-feature.${T9_JDK##*-}=true" \
    brewlet.sh/launcher.java=true || return 1
  annotate_node "$node" --overwrite \
    "brewlet.sh/jdks=$T9_JDK" "brewlet.sh/launchers="
}

# _t9_apply_deploy: render the demo JavaApplication-equivalent as a Deployment +
# Service. $1 = artifact digest. $2 = a rollout nonce carried as an env var —
# changing it changes the pod-template hash (used for the rolling-update
# assertion). Resources.limits pin the cgroup so we can prove the JVM reads them.
_t9_apply_deploy() {
  local digest="$1" nonce="${2:-0}"
  kubectl apply -n "$T9_NS" -f - >>"$WORK/t9-deploy.log" 2>&1 <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $T9_APP
  labels: { app: $T9_APP }
spec:
  replicas: 1
  selector: { matchLabels: { app: $T9_APP } }
  template:
    metadata:
      labels: { app: $T9_APP }
      annotations:
        brewlet.sh/artifact-ref: "$T9_REF"
        brewlet.sh/artifact-digest: "$digest"
        brewlet.sh/jdk: "$T9_JDK"
    spec:
      runtimeClassName: brewlet
      nodeSelector: { brewlet.sh/runtime: ready }
      terminationGracePeriodSeconds: 10
      containers:
        - name: app
          image: busybox:1.36
          command: ["sleep", "3600"]
          env:
            - { name: JDK_JAVA_OPTIONS, value: "-XX:MaxRAMPercentage=50.0" }
            - { name: BREWLET_ROLLOUT, value: "$nonce" }
          ports:
            - { name: http, containerPort: $T9_PORT }
          resources:
            requests: { cpu: "250m", memory: "128Mi" }
            limits:   { cpu: "1",    memory: "256Mi" }
          readinessProbe:
            httpGet: { path: /healthz, port: $T9_PORT }
            initialDelaySeconds: 2
            periodSeconds: 3
            failureThreshold: 20
---
apiVersion: v1
kind: Service
metadata:
  name: $T9_APP
spec:
  type: ClusterIP
  selector: { app: $T9_APP }
  ports:
    - { name: http, port: $T9_PORT, targetPort: $T9_PORT }
YAML
}

# _t9_curl PATH: fetch http://<svc>:<port><PATH> from the in-cluster client pod.
# Echoes the body; non-zero on failure. (busybox wget: -T is the timeout flag.)
_t9_curl() {
  kubectl exec -n "$T9_NS" t9-client -- \
    wget -q -O- -T 5 "http://$T9_APP.$T9_NS.svc.cluster.local:$T9_PORT$1" 2>/dev/null
}

# _t9_no_pods: true once no app pods remain (used to assert scale-to-zero drains).
_t9_no_pods() {
  [[ "$(kubectl get pods -n "$T9_NS" -l app="$T9_APP" --no-headers 2>/dev/null | wc -l | tr -d ' ')" == 0 ]]
}

# _t9_curl_retry PATH [tries]: retry _t9_curl until it succeeds.
_t9_curl_retry() {
  local path="$1" tries="${2:-40}" body
  while (( tries-- > 0 )); do
    if body="$(_t9_curl "$path")" && [[ -n "$body" ]]; then printf '%s' "$body"; return 0; fi
    sleep 1
  done
  return 1
}

tier9_serving() {
  section "Tier 9 — brewlet Deployment + Service: real traffic under real cgroup limits"
  if ! have kubectl || ! k8s_reachable; then skip "tier9: serving on a real node" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier9: serving on a real node" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier9: serving on a real node" "go not installed"; return 0; fi

  # Pick a node that is a local containerd docker container we can provision,
  # preferring one that is schedulable (kind's single node, or a Docker Desktop
  # worker — not the tainted control-plane, where the pod would sit Pending).
  T9_NODE="$(pick_provisionable_node)"
  if [[ -z "$T9_NODE" ]]; then
    skip "tier9: serving on a real node" "no node is a local containerd docker container (need kind/CI)"; return 0
  fi
  if ! node_schedulable "$T9_NODE"; then
    skip "tier9: serving on a real node" "only provisionable node ($T9_NODE) is unschedulable (NoSchedule taint); need an untainted worker node"; return 0
  fi
  local arch; arch="$(_t9_node_arch "$T9_NODE")"
  if [[ -z "$arch" ]]; then skip "tier9: serving on a real node" "unknown node arch"; return 0; fi
  info "tier9: node=$T9_NODE arch=$arch"

  trap _t9_cleanup RETURN

  # --- build shim (node arch), brewlet CLI, and the demo JAR -----------------
  info "tier9: building shim ($arch), brewlet CLI, demo JAR"
  local shimbin="$WORK/t9-shim-$arch"
  if ! ( cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -o "$shimbin" ./shim/cmd/containerd-shim-brewlet-v2 ) >"$WORK/t9-build.log" 2>&1; then
    fail "tier9: build shim" "see $WORK/t9-build.log"; return 0
  fi
  if ! ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t9-brewlet" ./cmd/brewlet ) >>"$WORK/t9-build.log" 2>&1; then
    fail "tier9: build brewlet CLI" "see $WORK/t9-build.log"; return 0
  fi
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"
  if [[ ! -f "$jar" ]]; then
    if ! have java; then
      skip "tier9: serving on a real node" "demo JAR absent and no JDK to build it (set JAVA_HOME, JDK 21+)"; return 0
    fi
    local jh; jh="$(resolve_java_home)"
    if ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" >>"$WORK/t9-build.log" 2>&1; then
      fail "tier9: build demo JAR" "see $WORK/t9-build.log"; return 0
    fi
  fi
  pass "tier9: built shim + CLI + demo JAR"

  # --- push the artifact + resolve its manifest digest -----------------------
  local store="$WORK/t9-oci"; rm -rf "$store"
  if ! "$WORK/t9-brewlet" push "$jar" "$T9_REF" --store "$store" --format=artifact >>"$WORK/t9-build.log" 2>&1; then
    fail "tier9: push artifact" "see $WORK/t9-build.log"; return 0
  fi
  local digest
  digest="$(python3 - "$store" "$T9_REF" <<'PY'
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
  if [[ -z "$digest" ]]; then fail "tier9: resolve artifact digest" "index.json had no manifest"; return 0; fi
  info "tier9: artifact digest $digest"

  # Import the OCI layout into the node's k8s.io content store (by digest).
  if ! ( cd "$store" && tar -cf - . ) | docker exec -i "$T9_NODE" ctr -n k8s.io images import --digests - >>"$WORK/t9-import.log" 2>&1; then
    if ! ( cd "$store" && tar -cf - . ) | docker exec -i "$T9_NODE" ctr -n k8s.io images import - >>"$WORK/t9-import.log" 2>&1; then
      fail "tier9: import artifact into node content store" "see $WORK/t9-import.log"; return 0
    fi
  fi
  pass "tier9: pushed + imported artifact ($T9_REF)"

  # --- provision the node: shim binary, JDK userland, containerd runtime -----
  docker cp "$shimbin" "$T9_NODE":"$T9_SHIM_DST" >>"$WORK/t9-prov.log" 2>&1
  docker exec "$T9_NODE" chmod +x "$T9_SHIM_DST" >>"$WORK/t9-prov.log" 2>&1
  docker exec "$T9_NODE" mkdir -p "$T9_CACHE" >>"$WORK/t9-prov.log" 2>&1
  if ! _t9_stage_jdk "$T9_NODE" "$arch"; then
    skip "tier9: serving on a real node" "could not stage temurin JDK userland (see $WORK/t9-jdk.log)"; return 0
  fi
  if ! _t9_patch_containerd "$T9_NODE"; then
    fail "tier9: register brewlet containerd runtime" "see $WORK/t9-containerd.log"; return 0
  fi
  if ! _t9_advertise "$T9_NODE" >>"$WORK/t9-prov.log" 2>&1; then
    fail "tier9: advertise node capabilities" "see $WORK/t9-prov.log"; return 0
  fi
  T9_PROVISIONED_NODES+=("$T9_NODE")
  pass "tier9: provisioned + advertised node ($T9_NODE)"

  # --- RuntimeClass + namespace + in-cluster curl client ---------------------
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T9_RC_CREATED=1
  fi
  kubectl create namespace "$T9_NS" >/dev/null 2>&1 || true

  # A plain (non-brewlet) client pod we exec `wget` from, to hit the Service over
  # the real in-cluster network — proving Service routing, not just a local port.
  kubectl run t9-client -n "$T9_NS" --image=busybox:1.36 --restart=Never \
    --command -- sleep 3600 >>"$WORK/t9-deploy.log" 2>&1 || true

  # --- deploy the brewlet workload -------------------------------------------
  info "tier9: deploying brewlet Deployment + Service ($T9_APP)"
  _t9_apply_deploy "$digest"
  if ! kubectl rollout status -n "$T9_NS" deploy/"$T9_APP" --timeout=150s >>"$WORK/t9-deploy.log" 2>&1; then
    fail "tier9: brewlet Deployment rolled out (pod Ready)" "see $WORK/t9-deploy.log; diag: $(save_pod_diag "$T9_APP" "$T9_NS" "app=$T9_APP")"
    return 0
  fi
  pass "tier9: brewlet Deployment rolled out — java -jar serving as PID 1 via the shim"

  # The pod actually LANDED on the capability-labelled node we provisioned.
  local landed
  landed="$(kubectl get pod -n "$T9_NS" -l app="$T9_APP" -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)"
  assert_eq "tier9: pod scheduled onto the provisioned brewlet node" "$landed" "$T9_NODE"

  # --- (A) real Service traffic ----------------------------------------------
  if ! kubectl wait -n "$T9_NS" --for=condition=Ready pod/t9-client --timeout=60s >>"$WORK/t9-deploy.log" 2>&1; then
    skip "tier9: serve real traffic" "in-cluster curl client not Ready"; return 0
  fi
  local hello
  if hello="$(_t9_curl_retry /hello)"; then
    assert_contains "tier9: GET /hello over the Service returns the app body (200)" "$hello" "Hello from a JAR"
  else
    fail "tier9: GET /hello over the Service" "no 200 from http://$T9_APP.$T9_NS.svc:$T9_PORT/hello"
    return 0
  fi

  # --- (B) the cgroup-aware JVM promise (§ the whole point of Brewlet) --------
  local info_body procs mem
  if info_body="$(_t9_curl_retry /info)"; then
    procs="$(printf '%s' "$info_body" | grep -i availableProcessors | grep -oE '[0-9]+' | head -1)"
    mem="$(printf '%s' "$info_body" | grep -i 'maxMemory' | grep -oE '[0-9]+' | head -1)"
    # limits.cpu = "1" -> the container-aware JDK must see exactly 1 processor,
    # NOT the node's real core count. That is the cgroup-aware guarantee.
    assert_eq "tier9: JVM availableProcessors reflects the CPU limit (cgroup-aware, ==1)" "${procs:-0}" "1"
    # limits.memory = 256Mi -> Runtime.maxMemory (heap) must be bounded well below
    # the limit (and far below host RAM). MaxRAMPercentage=50 -> ~128 MB.
    if [[ -n "$mem" ]] && (( mem > 32 && mem < 256 )); then
      pass "tier9: JVM maxMemory reflects the memory limit (cgroup-aware, ${mem} MB < 256Mi)"
    else
      fail "tier9: JVM maxMemory reflects the memory limit" "got '${mem:-?}' MB (want 32 < mem < 256)"
    fi
  else
    fail "tier9: GET /info for the cgroup-aware JVM view" "no 200 from /info"
  fi

  # --- (C) scale out then to zero --------------------------------------------
  kubectl scale -n "$T9_NS" deploy/"$T9_APP" --replicas=2 >>"$WORK/t9-deploy.log" 2>&1
  if kubectl rollout status -n "$T9_NS" deploy/"$T9_APP" --timeout=120s >>"$WORK/t9-deploy.log" 2>&1 \
     && [[ "$(kubectl get deploy "$T9_APP" -n "$T9_NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" == "2" ]]; then
    pass "tier9: scaled to 2 replicas — multiple brewlet JVM pods co-scheduled + Ready"
  else
    fail "tier9: scale to 2 replicas" "readyReplicas=$(kubectl get deploy "$T9_APP" -n "$T9_NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
  fi
  # Still serves with >1 endpoint behind the Service.
  if _t9_curl_retry /hello >/dev/null; then
    pass "tier9: Service still serves 200 with multiple endpoints"
  else
    fail "tier9: Service serves with multiple endpoints"
  fi
  kubectl scale -n "$T9_NS" deploy/"$T9_APP" --replicas=0 >>"$WORK/t9-deploy.log" 2>&1
  if wait_for _t9_no_pods; then
    pass "tier9: scaled to zero — all brewlet pods drained"
  else
    fail "tier9: scale to zero drains all pods"
  fi

  # --- (D) rolling update -----------------------------------------------------
  kubectl scale -n "$T9_NS" deploy/"$T9_APP" --replicas=1 >>"$WORK/t9-deploy.log" 2>&1
  kubectl rollout status -n "$T9_NS" deploy/"$T9_APP" --timeout=120s >>"$WORK/t9-deploy.log" 2>&1 || true
  # Change the pod template (new rollout nonce) -> a new ReplicaSet -> rolling update.
  _t9_apply_deploy "$digest" "1"
  if kubectl rollout status -n "$T9_NS" deploy/"$T9_APP" --timeout=150s >>"$WORK/t9-deploy.log" 2>&1; then
    local rscount
    rscount="$(kubectl get rs -n "$T9_NS" -l app="$T9_APP" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "${rscount:-0}" -ge 2 ]]; then
      pass "tier9: rolling update rolled to a new ReplicaSet ($rscount total)"
    else
      fail "tier9: rolling update created a new ReplicaSet" "only $rscount ReplicaSet(s)"
    fi
    if _t9_curl_retry /hello >/dev/null; then
      pass "tier9: Service still serves 200 after the rolling update"
    else
      fail "tier9: Service serves after rolling update"
    fi
  else
    fail "tier9: rolling update completed" "see $WORK/t9-deploy.log"
  fi

  kubectl delete -n "$T9_NS" deploy/"$T9_APP" svc/"$T9_APP" --wait=false >/dev/null 2>&1 || true
}
