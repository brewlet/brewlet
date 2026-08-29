#!/usr/bin/env bash
# Tier 12 — the SpinKube workload-delivery model, for real: a brewlet workload whose pod
# `image:` IS the OCI artifact, PULLED and UNPACKED by containerd/kubelet like any
# ordinary image — no placeholder, no out-of-band blob delivery — then RUN by the
# shim on the node-resident JDK.
#
# THE GAP THIS CLOSES. Tiers 8 and 9 prove the shim runs a brewlet workload, but
# they must smuggle the payload in: a native Brewlet artifact uses custom OCI
# layer media types (application/vnd.brewlet.jar.layer.v1+jar, …) that
# containerd's differ cannot untar, so `crictl/kubelet` fail to UNPACK it
# (ImagePullBackOff) and the pod can never name the artifact as its `image`. Those
# tiers therefore give the pod a busybox placeholder image and hand the artifact
# to the shim via `ctr images import` + brewlet.sh/artifact-* annotations. That
# proves the runtime, but NOT the developer-facing promise: `kubectl run
# --image=<my-app>` the way SpinKube delivers a Spin-compatible Wasm application
# from OCI to containerd-shim-spin.
#
# `brewlet push --format=image` fixes this by publishing the SAME jar as a
# STANDARD, kubelet-pullable OCI image (real image config, tar+gzip layers, the
# launch contract carried as the brewlet.sh/jvm-config manifest annotation, and a
# multi-arch index for a portable jar). containerd unpacks it with no special
# config; the shim reads the launch config from the annotation and runs it. See
# docs/runnable-image.md and SPECIFICATION.md §4.
#
# WHAT THIS TIER ASSERTS (beyond tier 9's serving/cgroup checks):
#   (0) containerd unpacks the runnable image successfully — explicitly through
#       `ctr images unpack` on containerd 1.x or during import on containerd 2.x.
#   (1) the pod's container `image:` is the brewlet artifact ref ITSELF (not a
#       placeholder), and the pod reaches Ready — i.e. kubelet pulled/unpacked it.
#   (2) both the signed default and an unsigned bundle explicitly authorized by
#       the Ops-authored bundle policy run on Kubernetes.
#   (3) both running JVMs load the managed dependency; the workload serves real
#       Service traffic and is cgroup-aware.
#
# It reuses tier 9's generic node-provisioning helpers (shim binary, temurin JDK
# userland root, containerd `brewlet` runtime registration, capability labels).
#
# Prereqs: kubectl + reachable cluster, docker (nodes are local kind/CI
# containers), go, a JDK 21+ ($JAVA_HOME) to build the demo JAR, network access
# to pull eclipse-temurin:21 for the JDK userland root. SKIPs otherwise.

T12_REF="demo/hello:runnable-e2e"
T12_UNSIGNED_REF="demo/hello:unsigned-e2e"
T12_NS="brewlet-runnable"
T12_APP="orders-img"
T12_PORT=8080
T12_RC_CREATED=""
T12_NODE=""
declare -a T12_PROVISIONED_NODES=()

_t12_cleanup() {
  info "tier12: cleaning up"
  kubectl delete ns "$T12_NS" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  [[ -n "$T12_RC_CREATED" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  for n in ${T12_PROVISIONED_NODES[@]+"${T12_PROVISIONED_NODES[@]}"}; do
    label_node "$n" brewlet.sh/runtime- "brewlet.sh/jdk.$T9_JDK-" "brewlet.sh/jdk-feature.${T9_JDK##*-}-" brewlet.sh/launcher.java- >/dev/null 2>&1 || true
    annotate_node "$n" brewlet.sh/jdks- brewlet.sh/launchers- >/dev/null 2>&1 || true
  done
  # The node's shim / JDK root / config.toml patch are left in place (cheap,
  # idempotent, reused on a re-run), matching tier 8/9.
}

# _t12_apply_deploy DIGEST [NONCE]: render the workload as a Deployment + Service.
# The decisive difference from tier 9: image is the brewlet artifact REF, pulled
# from the node's content store (imagePullPolicy: Never — pre-loaded by ctr), NOT
# a busybox placeholder. The brewlet.sh/artifact-digest annotation still carries
# the (index) digest so the shim resolves the launch config from the content
# store; the shim follows the index to the node's platform manifest.
_t12_apply_deploy() {
  local digest="$1" nonce="${2:-0}"
  kubectl apply -n "$T12_NS" -f - >>"$WORK/t12-deploy.log" 2>&1 <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $T12_APP
  labels: { app: $T12_APP }
spec:
  replicas: 1
  selector: { matchLabels: { app: $T12_APP } }
  template:
    metadata:
      labels: { app: $T12_APP }
      annotations:
        brewlet.sh/artifact-ref: "$T12_REF"
        brewlet.sh/artifact-digest: "$digest"
        brewlet.sh/jdk: "$T9_JDK"
    spec:
      runtimeClassName: brewlet
      nodeSelector: { brewlet.sh/runtime: ready }
      terminationGracePeriodSeconds: 10
      containers:
        - name: app
          image: $T12_REF
          imagePullPolicy: Never
          env:
            - { name: JDK_JAVA_OPTIONS, value: "-XX:MaxRAMPercentage=50.0" }
            - { name: BREWLET_ROLLOUT, value: "$nonce" }
          ports:
            - { name: http, containerPort: $T12_PORT }
          resources:
            requests: { cpu: "250m", memory: "128Mi" }
            limits:   { cpu: "1",    memory: "256Mi" }
          readinessProbe:
            httpGet: { path: /healthz, port: $T12_PORT }
            initialDelaySeconds: 2
            periodSeconds: 3
            failureThreshold: 20
---
apiVersion: v1
kind: Service
metadata:
  name: $T12_APP
spec:
  type: ClusterIP
  selector: { app: $T12_APP }
  ports:
    - { name: http, port: $T12_PORT, targetPort: $T12_PORT }
YAML
}

# _t12_curl PATH: fetch the Service from the in-cluster client pod (busybox wget).
_t12_curl() {
  kubectl exec -n "$T12_NS" t12-client -- \
    wget -q -O- -T 5 "http://$T12_APP.$T12_NS.svc.cluster.local:$T12_PORT$1" 2>/dev/null
}

_t12_curl_retry() {
  local path="$1" tries="${2:-40}" body
  while (( tries-- > 0 )); do
    if body="$(_t12_curl "$path")" && [[ -n "$body" ]]; then printf '%s' "$body"; return 0; fi
    sleep 1
  done
  return 1
}

tier12_runnable_image() {
  section "Tier 12 — kubelet pulls + unpacks the artifact as its image (SpinKube model)"
  if ! have kubectl || ! k8s_reachable; then skip "tier12: runnable image pulled by kubelet" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier12: runnable image pulled by kubelet" "docker daemon not available"; return 0; fi
  if ! have go; then skip "tier12: runnable image pulled by kubelet" "go not installed"; return 0; fi
  if ! have python3; then skip "tier12: runnable image pulled by kubelet" "python3 not installed"; return 0; fi

  # Pick a node that is a local containerd docker container we can provision,
  # preferring one that is schedulable (kind's single node, or a Docker Desktop
  # worker — not the tainted control-plane, where the pod would sit Pending).
  T12_NODE="$(pick_provisionable_node)"
  if [[ -z "$T12_NODE" ]]; then
    skip "tier12: runnable image pulled by kubelet" "no node is a local containerd docker container (need kind/CI)"; return 0
  fi
  if ! node_schedulable "$T12_NODE"; then
    skip "tier12: runnable image pulled by kubelet" "only provisionable node ($T12_NODE) is unschedulable (NoSchedule taint); need an untainted worker node"; return 0
  fi
  local arch; arch="$(_t9_node_arch "$T12_NODE")"
  if [[ -z "$arch" ]]; then skip "tier12: runnable image pulled by kubelet" "unknown node arch"; return 0; fi
  info "tier12: node=$T12_NODE arch=$arch"

  trap _t12_cleanup RETURN

  # --- build shim (node arch), brewlet CLI, demo JAR ------------------------
  info "tier12: building shim ($arch), brewlet CLI, demo JAR"
  local shimbin="$WORK/t12-shim-$arch"
  if ! ( cd "$BREWLET_CORE_DIR" && GOOS=linux GOARCH="$arch" go build -o "$shimbin" ./shim/cmd/containerd-shim-brewlet-v2 ) >"$WORK/t12-build.log" 2>&1; then
    fail "tier12: build shim" "see $WORK/t12-build.log"; return 0
  fi
  if ! ( cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t12-brewlet" ./cmd/brewlet ) >>"$WORK/t12-build.log" 2>&1; then
    fail "tier12: build brewlet CLI" "see $WORK/t12-build.log"; return 0
  fi
  if ! have java; then
    skip "tier12: runnable image pulled by kubelet" "no JDK to build fixtures (set JAVA_HOME, JDK 21+)"; return 0
  fi
  local jh; jh="$(resolve_java_home)"
  if ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" \
      "$FIXTURES_DIR/demo-app/build.sh" >>"$WORK/t12-build.log" 2>&1 \
      || ! env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" \
      "$FIXTURES_DIR/managed-dependency-app/build.sh" >>"$WORK/t12-build.log" 2>&1; then
    fail "tier12: build demo and managed dependency JARs" "see $WORK/t12-build.log"; return 0
  fi
  local jar="$FIXTURES_DIR/demo-app/target/app.jar"

  # --- publish signed and explicitly Ops-authorized unsigned bundles --------
  local store="$WORK/t12-oci"; rm -rf "$store"
  local managed_dir="$WORK/t12-managed" managed_tar="$WORK/t12-managed/dependencies.tar"
  local managed_lock="$WORK/t12-managed/dependency-lock.json"
  local managed_ref="platform/t12-approved:1"
  local private_key="$WORK/t12-managed/signing-key.pem"
  local public_key="$WORK/t12-managed/signing-key.pub.pem"
  mkdir -p "$managed_dir/lib"
  cp "$FIXTURES_DIR/managed-dependency-app/target/approved.jar" \
    "$managed_dir/lib/approved.jar"
  COPYFILE_DISABLE=1 tar -cf "$managed_tar" -C "$managed_dir/lib" approved.jar
  local dependency_sha
  if have sha256sum; then
    dependency_sha="$(sha256sum "$managed_dir/lib/approved.jar" | awk '{print $1}')"
  else
    dependency_sha="$(shasum -a 256 "$managed_dir/lib/approved.jar" | awk '{print $1}')"
  fi
  printf '{"schemaVersion":1,"artifacts":[{"groupId":"com.example.platform","artifactId":"approved","version":"1","type":"jar","scope":"runtime","fileName":"approved.jar","sha256":"%s"}]}\n' \
    "$dependency_sha" >"$managed_lock"
  if ! "$WORK/t12-brewlet" keygen --private "$private_key" --public "$public_key" \
      >>"$WORK/t12-build.log" 2>&1 \
      || ! "$WORK/t12-brewlet" dependency-bundle "$managed_tar" "$managed_ref" \
        --store "$store" --name approved --version 1 \
        --source-bom com.example.platform:approved-bom:1 --lock "$managed_lock" \
        --signing-key "$private_key" --signer-identity platform-builder \
        >>"$WORK/t12-build.log" 2>&1; then
    fail "tier12: publish signed managed dependency bundle" "see $WORK/t12-build.log"; return 0
  fi
  pass "tier12: published signed managed dependency bundle"
  if "$WORK/t12-brewlet" push "$jar" demo/t12-policy-bypass:1 \
      --store "$store" --dependency-bundle "$managed_ref" \
      --dependency-lock "$managed_lock" --signing-key "$private_key" \
      --builder-identity application-builder --main-class com.example.Hello \
      >>"$WORK/t12-build.log" 2>&1; then
    fail "tier12: application cannot bypass signed bundle policy" \
      "managed image publication unexpectedly succeeded without bundle trust"
    return 0
  else
    pass "tier12: application cannot opt a signed-policy bundle into unsigned mode"
  fi

  local unsigned_bundle_ref="platform/t12-unsigned:1"
  if ! "$WORK/t12-brewlet" dependency-bundle "$managed_tar" "$unsigned_bundle_ref" \
      --store "$store" --name approved-unsigned --version 1 \
      --source-bom com.example.platform:approved-bom:1 --lock "$managed_lock" \
      --allow-unsigned >>"$WORK/t12-build.log" 2>&1 \
      || ! "$WORK/t12-brewlet" push "$jar" "$T12_UNSIGNED_REF" \
      --store "$store" --dependency-bundle "$unsigned_bundle_ref" \
      --dependency-lock "$managed_lock" --signing-key "$private_key" \
      --builder-identity application-builder --main-class com.example.Hello \
      >>"$WORK/t12-build.log" 2>&1; then
    fail "tier12: compose Ops-authorized unsigned bundle" \
      "see $WORK/t12-build.log"
    return 0
  fi
  pass "tier12: Ops-authorized unsigned bundle produces a runnable image"

  # --- push signed bundle composition as a RUNNABLE OCI IMAGE ----------------
  if ! "$WORK/t12-brewlet" push "$jar" "$T12_REF" --store "$store" \
      --dependency-bundle "$managed_ref" --dependency-lock "$managed_lock" \
      --trusted-public-key "$public_key" --trusted-signer-identity platform-builder \
      --signing-key "$private_key" --builder-identity application-builder \
      --main-class com.example.Hello >>"$WORK/t12-build.log" 2>&1; then
    fail "tier12: push signed managed runnable image" "see $WORK/t12-build.log"; return 0
  fi
  local digest unsigned_digest
  digest="$(python3 - "$store" "$T12_REF" <<'PY'
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
  if [[ -z "$digest" ]]; then fail "tier12: resolve image index digest" "index.json had no manifest"; return 0; fi
  unsigned_digest="$(python3 - "$store" "$T12_UNSIGNED_REF" <<'PY'
import json, sys
root, ref = sys.argv[1], sys.argv[2]
idx = json.load(open(f"{root}/index.json"))
for manifest in idx["manifests"]:
    if manifest.get("annotations", {}).get("org.opencontainers.image.ref.name") == ref:
        print(manifest["digest"])
        break
PY
)"
  if [[ -z "$unsigned_digest" ]]; then fail "tier12: resolve unsigned image index digest" "index.json had no unsigned image"; return 0; fi
  info "tier12: runnable image index digest $digest"
  pass "tier12: composed signed bundle into runnable OCI image ($T12_REF)"

  # --- import the STANDARD image into the node content store ----------------
  if ! ( cd "$store" && tar -cf - . ) | docker exec -i "$T12_NODE" ctr -n k8s.io images import --digests - >>"$WORK/t12-import.log" 2>&1; then
    if ! ( cd "$store" && tar -cf - . ) | docker exec -i "$T12_NODE" ctr -n k8s.io images import - >>"$WORK/t12-import.log" 2>&1; then
      fail "tier12: import runnable image into node content store" "see $WORK/t12-import.log"; return 0
    fi
  fi
  local cri_ref="$T12_REF"
  if [[ "${T12_REF%%/*}" != *.* && "${T12_REF%%/*}" != *:* && "${T12_REF%%/*}" != "localhost" ]]; then
    cri_ref="docker.io/$T12_REF"
    docker exec "$T12_NODE" ctr -n k8s.io images tag "$T12_REF" "$cri_ref" >>"$WORK/t12-import.log" 2>&1 || true
    docker exec "$T12_NODE" ctr -n k8s.io images tag "$T12_UNSIGNED_REF" \
      "docker.io/$T12_UNSIGNED_REF" >>"$WORK/t12-import.log" 2>&1 || true
  fi

  # (0) THE decisive assertion: containerd can UNPACK the brewlet image — the
  # operation that fails for a native artifact's custom media types. containerd
  # 1.x exposes a separate unpack command; containerd 2.x unpacks during import
  # by default and exposes --no-unpack only to opt out.
  if ctr_supports_unpack "$T12_NODE"; then
    if docker exec "$T12_NODE" ctr -n k8s.io images unpack --platform "linux/$arch" "$T12_REF" >>"$WORK/t12-import.log" 2>&1; then
      pass "tier12: containerd UNPACKED the brewlet image (standard tar+gzip layers) — the step that ImagePullBackOffs for a native artifact"
    else
      fail "tier12: containerd unpack of the runnable image" "see $WORK/t12-import.log"; return 0
    fi
  elif docker exec "$T12_NODE" ctr -n k8s.io images import --help 2>/dev/null | grep -- "--no-unpack" >/dev/null; then
    pass "tier12: containerd import UNPACKED the brewlet image by default (containerd 2.x)"
  else
    fail "tier12: containerd unpack capability" "node ctr supports neither explicit unpack nor import-time unpack"; return 0
  fi
  if docker exec "$T12_NODE" crictl inspecti "$cri_ref" >/dev/null 2>&1; then
    pass "tier12: runnable image registered with CRI ($cri_ref)"
  else
    fail "tier12: runnable image registered with CRI" "image $cri_ref not available through crictl"; return 0
  fi

  # --- provision the node (reuse tier 9's generic helpers) ------------------
  docker cp "$shimbin" "$T12_NODE":"$T9_SHIM_DST" >>"$WORK/t12-prov.log" 2>&1
  docker exec "$T12_NODE" chmod +x "$T9_SHIM_DST" >>"$WORK/t12-prov.log" 2>&1
  docker exec "$T12_NODE" mkdir -p "$T9_CACHE" >>"$WORK/t12-prov.log" 2>&1
  if ! _t9_stage_jdk "$T12_NODE" "$arch"; then
    skip "tier12: runnable image pulled by kubelet" "could not stage temurin JDK userland (see $WORK/t9-jdk.log)"; return 0
  fi
  if ! _t9_patch_containerd "$T12_NODE"; then
    fail "tier12: register brewlet containerd runtime" "see $WORK/t9-containerd.log"; return 0
  fi
  if ! _t9_advertise "$T12_NODE" >>"$WORK/t12-prov.log" 2>&1; then
    fail "tier12: advertise node capabilities" "see $WORK/t12-prov.log"; return 0
  fi
  T12_PROVISIONED_NODES+=("$T12_NODE")
  pass "tier12: provisioned + advertised node ($T12_NODE)"

  # --- RuntimeClass + namespace + in-cluster curl client --------------------
  if ! kubectl get runtimeclass brewlet >/dev/null 2>&1; then
    kubectl create -f - >/dev/null 2>&1 <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: { name: brewlet }
handler: brewlet
YAML
    T12_RC_CREATED=1
  fi
  kubectl create namespace "$T12_NS" >/dev/null 2>&1 || true
  kubectl run t12-client -n "$T12_NS" --image=busybox:1.36 --restart=Never \
    --command -- sleep 3600 >>"$WORK/t12-deploy.log" 2>&1 || true

  # --- deploy: pod image IS the brewlet artifact ----------------------------
  info "tier12: deploying workload whose image: is the brewlet artifact ($T12_APP)"
  _t12_apply_deploy "$digest"
  if ! kubectl rollout status -n "$T12_NS" deploy/"$T12_APP" --timeout=150s >>"$WORK/t12-deploy.log" 2>&1; then
    kubectl describe -n "$T12_NS" deploy/"$T12_APP" >>"$WORK/t12-deploy.log" 2>&1 || true
    kubectl get pods -n "$T12_NS" -o wide >>"$WORK/t12-deploy.log" 2>&1 || true
    fail "tier12: Deployment whose image is the brewlet artifact rolled out" "see $WORK/t12-deploy.log; $(kubectl get pods -n "$T12_NS" -l app="$T12_APP" 2>&1 | tail -2 | tr '\n' ' ')"
    return 0
  fi

  # (1) The running pod's container image is the brewlet artifact REF itself —
  # kubelet resolved + unpacked it and the shim ran it. No placeholder.
  local podimg
  podimg="$(kubectl get pod -n "$T12_NS" -l app="$T12_APP" -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null)"
  assert_contains "tier12: the running pod's image is the brewlet artifact (SpinKube model, no placeholder)" "$podimg" "$T12_REF"
  pass "tier12: kubelet pulled/unpacked the brewlet image and the shim ran it (java -jar as PID 1)"

  # (2) It loads the managed dependency. (3) It serves and is cgroup-aware.
  if ! kubectl wait -n "$T12_NS" --for=condition=Ready pod/t12-client --timeout=60s >>"$WORK/t12-deploy.log" 2>&1; then
    skip "tier12: serve real traffic" "in-cluster curl client not Ready"; return 0
  fi
  local hello
  if hello="$(_t12_curl_retry /hello)"; then
    assert_contains "tier12: GET /hello over the Service returns the app body (200)" "$hello" "Hello from a JAR"
    assert_contains "tier12: Kubernetes JVM loads the signed managed dependency layer" \
      "$hello" "MANAGED DEPENDENCY OK"
  else
    kubectl get pods,svc,endpoints -n "$T12_NS" -o wide >>"$WORK/t12-deploy.log" 2>&1 || true
    kubectl logs -n "$T12_NS" -l app="$T12_APP" --tail=100 >>"$WORK/t12-deploy.log" 2>&1 || true
    kubectl exec -n "$T12_NS" t12-client -- \
      wget -S -O- -T 5 "http://$T12_APP.$T12_NS.svc.cluster.local:$T12_PORT/hello" \
      >>"$WORK/t12-deploy.log" 2>&1 || true
    fail "tier12: GET /hello over the Service" "no 200 from http://$T12_APP.$T12_NS.svc:$T12_PORT/hello"
    return 0
  fi

  # Roll the same Kubernetes workload to the Ops-authorized unsigned bundle.
  T12_REF="$T12_UNSIGNED_REF"
  _t12_apply_deploy "$unsigned_digest" 1
  if kubectl rollout status -n "$T12_NS" deploy/"$T12_APP" \
      --timeout=150s >>"$WORK/t12-deploy.log" 2>&1 \
      && hello="$(_t12_curl_retry /hello)"; then
    podimg="$(kubectl get deployment -n "$T12_NS" "$T12_APP" \
      -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
    assert_contains "tier12: unsigned-policy workload uses its managed image" \
      "$podimg" "$T12_UNSIGNED_REF"
    assert_contains "tier12: Kubernetes JVM loads the Ops-authorized unsigned bundle" \
      "$hello" "MANAGED DEPENDENCY OK"
  else
    fail "tier12: deploy Ops-authorized unsigned managed bundle" \
      "see $WORK/t12-deploy.log"
    return 0
  fi
  local info_body procs mem
  if info_body="$(_t12_curl_retry /info)"; then
    procs="$(printf '%s' "$info_body" | grep -i availableProcessors | grep -oE '[0-9]+' | head -1)"
    mem="$(printf '%s' "$info_body" | grep -i 'maxMemory' | grep -oE '[0-9]+' | head -1)"
    assert_eq "tier12: JVM availableProcessors reflects the CPU limit (cgroup-aware, ==1)" "${procs:-0}" "1"
    if [[ -n "$mem" ]] && (( mem > 32 && mem < 256 )); then
      pass "tier12: JVM maxMemory reflects the memory limit (cgroup-aware, ${mem} MB < 256Mi)"
    else
      fail "tier12: JVM maxMemory reflects the memory limit" "got '${mem:-?}' MB (want 32 < mem < 256)"
    fi
  else
    fail "tier12: GET /info for the cgroup-aware JVM view" "no 200 from /info"
  fi

  kubectl delete -n "$T12_NS" deploy/"$T12_APP" svc/"$T12_APP" --wait=false >/dev/null 2>&1 || true
}
