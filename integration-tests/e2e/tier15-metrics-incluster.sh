#!/usr/bin/env bash
# Tier 15 — live, metrics-enabled Helm installation through the complete
# operator -> provisioner -> shim -> exporter path on a local containerd node.
#
# This tier proves the opt-in observability feature against kind/CI:
#   - the shipped chart exposes operator, admission, and node scrape surfaces;
#   - the real provisioner image contains and runs the metrics exporter sidecar;
#   - a real Brewlet workload sends shim datagrams to the node exporter;
#   - JDK/launcher inventory, launch, artifact, and AppCDS metrics are exported;
#   - admission outcomes and NodeProfile/provisioning transitions are exported.
#
# The provisioner performs the same targeted host mutation as tiers 8, 9, and 14.
# Cleanup deletes the profile first so its real cleanup DaemonSet reverses the
# runtime registration before Helm is uninstalled.

T15_RELEASE="brewlet-metrics-e2e"
T15_RELEASE_NS="default"
T15_NS="brewlet"
T15_APP_NS="brewlet-metrics-e2e"
T15_PROFILE="metrics"
T15_POOL_KEY="brewlet.sh/e2e-pool"
T15_POOL="metrics"
T15_JDK="temurin-21"
T15_REF="demo/hello:metrics-e2e-$$"
T15_APP="metrics-orders"
T15_OP_IMG="brewlet.local/operator:metrics-e2e"
T15_ADM_IMG="brewlet.local/admission:metrics-e2e"
T15_PROV_IMG="brewlet.local/node-provisioner:metrics-e2e"
T15_NODE=""
T15_ARCH=""
T15_HELM_INSTALLED=""
T15_PROFILE_CREATED=""
T15_APP_NS_CREATED=""
T15_NODE_TOUCHED=""
T15_JDK_PREEXISTING=""
T15_JDK_ACTIVE_PREEXISTING=""
T15_JDK_ACTIVE_SNAPSHOT=""
T15_CONTAINERD_CONFIG_SNAPSHOT=""
T15_ARTIFACT_DIGEST=""
T15_CDS_SNAPSHOT=""
declare -a T15_LOADED_NODES=()
declare -a T15_BUILT_IMAGES=()

_t15_profile_gone() {
  ! kubectl get nodeprofile "$T15_PROFILE" >/dev/null 2>&1
}

_t15_wait_profile_cleanup() {
  local tries=120
  while (( tries-- > 0 )); do
    _t15_profile_gone && return 0
    sleep 1
  done
  return 1
}

_t15_wait_namespace_gone() {
  local namespace="$1" tries=120
  while (( tries-- > 0 )); do
    ! kubectl get namespace "$namespace" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

_t15_force_host_cleanup() {
  docker exec "$T15_NODE" sh -c '
    config=/etc/containerd/config.toml
    if [ -f "${config}.brewlet.bak" ]; then
      cp -a "${config}.brewlet.bak" "$config"
      rm -f "${config}.brewlet.bak"
    elif grep -q "containerd.runtimes.brewlet" "$config" 2>/dev/null; then
      sed -i.brewlet-cleanup \
        "/# --- added by brewlet-node-provisioner/,/# --- end brewlet ---/d" "$config"
      rm -f "${config}.brewlet-cleanup"
    fi
    rm -f /opt/brewlet/bin/containerd-shim-brewlet-v2 \
      /usr/local/bin/containerd-shim-brewlet-v2 /usr/local/bin/brewlet-ctr
    systemctl restart containerd
  ' >/dev/null 2>&1 || return 1
  local tries=30
  while (( tries-- > 0 )); do
    docker exec "$T15_NODE" ctr version >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

_t15_restore_containerd_config() {
  [[ -n "$T15_CONTAINERD_CONFIG_SNAPSHOT" &&
     -f "$T15_CONTAINERD_CONFIG_SNAPSHOT" ]] || return 0
  local current="$WORK/t15-containerd-after.toml" tries=30
  docker exec "$T15_NODE" cat /etc/containerd/config.toml >"$current" 2>/dev/null ||
    return 1
  if cmp -s "$T15_CONTAINERD_CONFIG_SNAPSHOT" "$current"; then
    docker exec "$T15_NODE" rm -f /etc/containerd/config.toml.brewlet.bak \
      >/dev/null 2>&1 || true
    return 0
  fi
  docker exec -i "$T15_NODE" sh -c \
    'cat > /etc/containerd/config.toml; rm -f /etc/containerd/config.toml.brewlet.bak' \
    <"$T15_CONTAINERD_CONFIG_SNAPSHOT" >/dev/null 2>&1 || return 1
  docker exec "$T15_NODE" systemctl restart containerd >/dev/null 2>&1 || return 1
  while (( tries-- > 0 )); do
    docker exec "$T15_NODE" ctr version >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

_t15_remove_created_cds_files() {
  [[ -n "$T15_CDS_SNAPSHOT" && -f "$T15_CDS_SNAPSHOT" ]] || return 0
  local after="$WORK/t15-cds-after.txt" path
  docker exec "$T15_NODE" find /opt/brewlet/cds -maxdepth 1 -type f -print \
    2>/dev/null | sort >"$after" || true
  while IFS= read -r path; do
    case "$path" in
      /opt/brewlet/cds/*)
        docker exec "$T15_NODE" sh -c 'rm -f "$1"' sh "$path" >/dev/null 2>&1 || true
        ;;
    esac
  done < <(comm -13 "$T15_CDS_SNAPSHOT" "$after")
}

_t15_cleanup() {
  info "tier15: cleaning up"
  if [[ -n "$T15_APP_NS_CREATED" ]]; then
    kubectl delete deploy "$T15_APP" -n "$T15_APP_NS" \
      --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
    kubectl delete ns "$T15_APP_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi

  if [[ -n "$T15_PROFILE_CREATED" ]] &&
     kubectl get nodeprofile "$T15_PROFILE" >/dev/null 2>&1; then
    kubectl delete nodeprofile "$T15_PROFILE" --wait=false >/dev/null 2>&1 || true
    if ! _t15_wait_profile_cleanup; then
      warn "tier15: profile cleanup did not finish; removing the test finalizer"
      if _t15_force_host_cleanup; then
        pass "tier15: fallback reversed host runtime provisioning"
      else
        fail "tier15: fallback reversed host runtime provisioning"
      fi
      kubectl patch nodeprofile "$T15_PROFILE" --type=merge \
        -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    fi
  fi

  if [[ -n "$T15_HELM_INSTALLED" ]]; then
    helm uninstall "$T15_RELEASE" -n "$T15_RELEASE_NS" >/dev/null 2>&1 || true
    kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete crd javaapplications.apps.brewlet.sh nodeprofiles.node.brewlet.sh \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl delete mutatingwebhookconfiguration brewlet-admission \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete validatingwebhookconfiguration brewlet-nodeprofiles \
      --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete ns "$T15_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  [[ -n "$T15_APP_NS_CREATED" ]] &&
    wait_for bash -c "! kubectl get namespace '$T15_APP_NS' >/dev/null 2>&1" || true
  [[ -n "$T15_HELM_INSTALLED" ]] &&
    wait_for bash -c "! kubectl get namespace '$T15_NS' >/dev/null 2>&1" || true

  if [[ -n "$T15_NODE_TOUCHED" ]]; then
    if ! _t15_restore_containerd_config; then
      fail "tier15: restore the node's original containerd configuration"
    fi
    _t15_remove_created_cds_files
    label_node "$T15_NODE" "$T15_POOL_KEY-" brewlet.sh/provision- \
      brewlet.sh/runtime- "brewlet.sh/jdk.$T15_JDK-" \
      "brewlet.sh/jdk-feature.${T15_JDK##*-}-" brewlet.sh/launcher.java- \
      >/dev/null 2>&1 || true
    annotate_node "$T15_NODE" brewlet.sh/jdks- brewlet.sh/jdks-info- \
      brewlet.sh/launchers- brewlet.sh/profile- brewlet.sh/profile-generation- \
      brewlet.sh/provision-state- brewlet.sh/provision-error- >/dev/null 2>&1 || true
    if [[ -z "$T15_JDK_PREEXISTING" ]]; then
      docker exec "$T15_NODE" sh -c \
        'chmod -R u+w "$1" 2>/dev/null || true; rm -rf "$1"' \
        sh "/opt/brewlet/jdks/$T15_JDK" >/dev/null 2>&1 || true
    fi
    if [[ -n "$T15_JDK_ACTIVE_PREEXISTING" ]]; then
      docker exec -i "$T15_NODE" sh -c \
        'mkdir -p /opt/brewlet/jdks; cat > /opt/brewlet/jdks/.brewlet-active' \
        <"$T15_JDK_ACTIVE_SNAPSHOT" >/dev/null 2>&1 || true
    else
      docker exec "$T15_NODE" rm -f /opt/brewlet/jdks/.brewlet-active \
        >/dev/null 2>&1 || true
    fi
    docker exec "$T15_NODE" rm -f /opt/brewlet/metrics/telemetry.sock \
      >/dev/null 2>&1 || true
  fi

  local n
  for n in ${T15_LOADED_NODES[@]+"${T15_LOADED_NODES[@]}"}; do
    docker exec "$n" ctr -n k8s.io images rm \
      "$T15_OP_IMG" "$T15_ADM_IMG" "$T15_PROV_IMG" >/dev/null 2>&1 || true
    if [[ -n "$T15_ARTIFACT_DIGEST" ]]; then
      docker exec "$n" ctr -n k8s.io images rm "$T15_REF" "$T15_ARTIFACT_DIGEST" \
        >/dev/null 2>&1 || true
    fi
  done
  if [[ -n "${T15_BUILT_IMAGES[*]-}" ]]; then
    docker rmi ${T15_BUILT_IMAGES[@]+"${T15_BUILT_IMAGES[@]}"} >/dev/null 2>&1 || true
  fi
}

_t15_node_arch() {
  case "$(docker exec "$1" uname -m 2>/dev/null)" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64) echo amd64 ;;
    *) echo "" ;;
  esac
}

_t15_build_load_kubernetes_image() {
  local cmd="$1" image="$2" nodes="$3" n
  local tarball="$WORK/t15-$cmd.tar"
  if ! docker build --provenance=false --build-arg CMD="$cmd" -t "$image" \
      -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
      >>"$WORK/t15-build.log" 2>&1; then
    docker build --build-arg CMD="$cmd" -t "$image" \
      -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
      >>"$WORK/t15-build.log" 2>&1 || return 1
  fi
  T15_BUILT_IMAGES+=("$image")
  docker save "$image" -o "$tarball" >>"$WORK/t15-load.log" 2>&1 || return 1
  for n in $nodes; do
    docker exec -i "$n" ctr -n k8s.io images import - <"$tarball" \
      >>"$WORK/t15-load.log" 2>&1 || return 1
  done
}

_t15_build_load_provisioner() {
  local nodes="$1" tarball="$WORK/t15-provisioner.tar" n tries=30 image_id
  if ! docker build --provenance=false --platform "linux/$T15_ARCH" \
      -t "$T15_PROV_IMG" -f "$MONOREPO_DIR/provisioner/Dockerfile" "$MONOREPO_DIR" \
      >>"$WORK/t15-build.log" 2>&1; then
    docker build --platform "linux/$T15_ARCH" \
      -t "$T15_PROV_IMG" -f "$MONOREPO_DIR/provisioner/Dockerfile" "$MONOREPO_DIR" \
      >>"$WORK/t15-build.log" 2>&1 || return 1
  fi
  T15_BUILT_IMAGES+=("$T15_PROV_IMG")
  while (( tries-- > 0 )); do
    docker image inspect "$T15_PROV_IMG" >/dev/null 2>&1 && break
    sleep 1
  done
  docker image inspect "$T15_PROV_IMG" >/dev/null 2>&1 || return 1
  image_id="$(docker image inspect -f '{{.Id}}' "$T15_PROV_IMG")"
  docker run --rm --platform "linux/$T15_ARCH" --entrypoint /bin/test "$image_id" \
    -x /opt/brewlet-dist/brewlet-metrics-exporter || return 2
  docker save "$T15_PROV_IMG" -o "$tarball" >>"$WORK/t15-load.log" 2>&1 || return 1
  for n in $nodes; do
    docker exec -i "$n" ctr -n k8s.io images import - <"$tarball" \
      >>"$WORK/t15-load.log" 2>&1 || return 1
  done
}

_t15_scrape() {
  kubectl exec -n "$T15_APP_NS" t15-client -- \
    wget -q -O- -T 5 "$1" 2>/dev/null
}

_t15_scrape_until() {
  local url="$1" pattern="$2" tries="${3:-60}" body
  while (( tries-- > 0 )); do
    if body="$(_t15_scrape "$url")" && grep -Eq "$pattern" <<<"$body"; then
      printf '%s' "$body"
      return 0
    fi
    sleep 1
  done
  return 1
}

_t15_assert_metric() {
  local name="$1" body="$2" pattern="$3"
  if grep -Eq "$pattern" <<<"$body"; then
    pass "$name"
  else
    fail "$name" "missing metric matching: $pattern"
  fi
}

_t15_build_artifact() {
  local jar="$FIXTURES_DIR/demo-app/target/app.jar" store="$WORK/t15-oci" jh
  if [[ ! -f "$jar" ]]; then
    jh="$(resolve_java_home)"
    env JAVA_HOME="$jh" PATH="$jh/bin:$PATH" "$FIXTURES_DIR/demo-app/build.sh" \
      >>"$WORK/t15-app.log" 2>&1 || return 1
  fi
  (cd "$BREWLET_CORE_DIR" && go build -o "$WORK/t15-brewlet" ./cmd/brewlet) \
    >>"$WORK/t15-app.log" 2>&1 || return 1
  rm -rf "$store"
  "$WORK/t15-brewlet" push "$jar" "$T15_REF" --store "$store" --format=artifact \
    >>"$WORK/t15-app.log" 2>&1 || return 1
}

_t15_artifact_digest() {
  python3 - "$WORK/t15-oci" "$T15_REF" <<'PY'
import json, sys
root, ref = sys.argv[1], sys.argv[2]
tag = ref.split(":")[-1]
index = json.load(open(f"{root}/index.json"))
for manifest in index["manifests"]:
    annotation = manifest.get("annotations", {}).get("org.opencontainers.image.ref.name")
    if annotation in (ref, tag):
        print(manifest["digest"])
        break
else:
    print(index["manifests"][0]["digest"])
PY
}

tier15_metrics_incluster() {
  section "Tier 15 — live runtime Prometheus metrics in-cluster"
  if ! have kubectl || ! k8s_reachable; then
    skip "tier15: live metrics" "no reachable cluster"; return 0
  fi
  if ! have docker || ! docker info >/dev/null 2>&1; then
    skip "tier15: live metrics" "docker daemon not available"; return 0
  fi
  if ! have helm; then skip "tier15: live metrics" "helm not installed"; return 0; fi
  if ! have go; then skip "tier15: live metrics" "go not installed"; return 0; fi
  if ! have java; then skip "tier15: live metrics" "JDK not installed"; return 0; fi
  if ! have python3; then skip "tier15: live metrics" "python3 not installed"; return 0; fi

  local nodes n
  nodes="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##')"
  if [[ -z "$nodes" ]]; then skip "tier15: live metrics" "no nodes"; return 0; fi
  for n in $nodes; do
    if ! node_provisionable "$n"; then
      skip "tier15: live metrics" "node '$n' is not a local containerd docker container"
      return 0
    fi
  done
  T15_NODE="$(pick_provisionable_node)"
  if [[ -z "$T15_NODE" ]] || ! node_schedulable "$T15_NODE"; then
    skip "tier15: live metrics" "no schedulable local containerd node"; return 0
  fi
  T15_ARCH="$(_t15_node_arch "$T15_NODE")"
  if [[ -z "$T15_ARCH" ]]; then skip "tier15: live metrics" "unknown node architecture"; return 0; fi
  local namespace deleting
  for namespace in "$T15_NS" "$T15_APP_NS"; do
    deleting="$(kubectl get namespace "$namespace" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
    if [[ -n "$deleting" ]] && ! _t15_wait_namespace_gone "$namespace"; then
      fail "tier15: wait for prior namespace cleanup" "$namespace remained terminating"
      return 0
    fi
  done
  local leftovers
  leftovers="$(detect_leftovers)"
  if [[ -n "$leftovers" ]] ||
     helm status "$T15_RELEASE" -n "$T15_RELEASE_NS" >/dev/null 2>&1 ||
     kubectl get ns "$T15_NS" >/dev/null 2>&1 ||
     kubectl get ns "$T15_APP_NS" >/dev/null 2>&1; then
    fail "tier15: clean Brewlet cluster state" "run ./run.sh --reset before tier 15"
    return 0
  fi
  docker exec "$T15_NODE" test -e "/opt/brewlet/jdks/$T15_JDK" >/dev/null 2>&1 &&
    T15_JDK_PREEXISTING=1
  T15_JDK_ACTIVE_SNAPSHOT="$WORK/t15-jdk-active-before.txt"
  if docker exec "$T15_NODE" test -f /opt/brewlet/jdks/.brewlet-active \
      >/dev/null 2>&1; then
    docker exec "$T15_NODE" cat /opt/brewlet/jdks/.brewlet-active \
      >"$T15_JDK_ACTIVE_SNAPSHOT"
    T15_JDK_ACTIVE_PREEXISTING=1
  else
    : >"$T15_JDK_ACTIVE_SNAPSHOT"
  fi
  T15_CONTAINERD_CONFIG_SNAPSHOT="$WORK/t15-containerd-before.toml"
  docker exec "$T15_NODE" cat /etc/containerd/config.toml \
    >"$T15_CONTAINERD_CONFIG_SNAPSHOT" || {
      fail "tier15: snapshot the node's containerd configuration"
      return 0
    }
  for n in $nodes; do T15_LOADED_NODES+=("$n"); done
  trap _t15_cleanup RETURN

  : >"$WORK/t15-build.log"
  : >"$WORK/t15-load.log"
  info "tier15: building and side-loading operator, admission, and provisioner images"
  if ! _t15_build_load_kubernetes_image manager "$T15_OP_IMG" "$nodes" ||
     ! _t15_build_load_kubernetes_image admission "$T15_ADM_IMG" "$nodes"; then
    fail "tier15: build and load control-plane images" "see t15-build.log and t15-load.log"
    return 0
  fi
  local provisioner_rc=0
  _t15_build_load_provisioner "$nodes" || provisioner_rc=$?
  if [[ "$provisioner_rc" -eq 2 ]]; then
    fail "tier15: provisioner image contains executable metrics exporter"
    return 0
  elif [[ "$provisioner_rc" -ne 0 ]]; then
    fail "tier15: build and load provisioner image" "see t15-build.log and t15-load.log"
    return 0
  fi
  pass "tier15: provisioner image contains executable /opt/brewlet-dist/brewlet-metrics-exporter"

  if ! _t15_build_artifact; then
    fail "tier15: build Brewlet workload artifact" "see $WORK/t15-app.log"; return 0
  fi
  local digest
  digest="$(_t15_artifact_digest)"
  if [[ -z "$digest" ]]; then
    fail "tier15: resolve workload artifact digest"; return 0
  fi
  if ! (cd "$WORK/t15-oci" && tar -cf - .) |
      docker exec -i "$T15_NODE" ctr -n k8s.io images import --digests - \
        >>"$WORK/t15-app.log" 2>&1; then
    if ! (cd "$WORK/t15-oci" && tar -cf - .) |
        docker exec -i "$T15_NODE" ctr -n k8s.io images import - \
          >>"$WORK/t15-app.log" 2>&1; then
      fail "tier15: import workload artifact into node content store" "see $WORK/t15-app.log"
      return 0
    fi
  fi
  T15_ARTIFACT_DIGEST="$digest"
  pass "tier15: built and imported a real Brewlet workload artifact"

  info "tier15: installing the shipped chart with metrics.enabled=true"
  T15_HELM_INSTALLED=1
  if ! helm install "$T15_RELEASE" "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
      --namespace "$T15_RELEASE_NS" \
      --set images.operator="$T15_OP_IMG" \
      --set images.admission="$T15_ADM_IMG" \
      --set images.provisioner="$T15_PROV_IMG" \
      --set images.pullPolicy=IfNotPresent \
      --set defaultProfile.enabled=false \
      --set operator.leaderElect=false \
      --set metrics.enabled=true \
      --wait --timeout 180s >"$WORK/t15-install.log" 2>&1; then
    save_pod_diag t15-install "$T15_NS" >>"$WORK/t15-install.log" 2>&1 || true
    fail "tier15: install metrics-enabled chart" "see $WORK/t15-install.log"
    return 0
  fi
  pass "tier15: metrics-enabled Helm install rolled out operator and admission"

  if kubectl get svc brewlet-operator-metrics brewlet-node-metrics -n "$T15_NS" \
      >/dev/null 2>&1 &&
     [[ "$(kubectl get svc brewlet-admission -n "$T15_NS" \
       -o jsonpath='{.spec.ports[?(@.name=="metrics")].port}')" == "8080" ]]; then
    pass "tier15: operator, admission, and node metrics Services are exposed"
  else
    fail "tier15: operator, admission, and node metrics Services are exposed"
  fi
  assert_eq "tier15: operator metrics listener is enabled" \
    "$(kubectl get deploy brewlet-operator -n "$T15_NS" \
      -o jsonpath='{.spec.template.spec.containers[0].args[?(@=="--metrics-bind-address=:8080")]}')" \
    "--metrics-bind-address=:8080"
  assert_eq "tier15: admission metrics listener is enabled" \
    "$(kubectl get deploy brewlet-admission -n "$T15_NS" \
      -o jsonpath='{.spec.template.spec.containers[0].args[?(@=="--metrics-bind-address=:8080")]}')" \
    "--metrics-bind-address=:8080"

  T15_APP_NS_CREATED=1
  kubectl create namespace "$T15_APP_NS" >/dev/null 2>&1 || true
  kubectl run t15-client -n "$T15_APP_NS" --image=busybox:1.36 --restart=Never \
    --command -- sleep 3600 >>"$WORK/t15-app.log" 2>&1 || true
  if ! kubectl wait -n "$T15_APP_NS" --for=condition=Ready pod/t15-client \
      --timeout=60s >>"$WORK/t15-app.log" 2>&1; then
    fail "tier15: in-cluster metrics client became Ready" "see $WORK/t15-app.log"
    return 0
  fi

  T15_NODE_TOUCHED=1
  label_node "$T15_NODE" --overwrite \
    "$T15_POOL_KEY=$T15_POOL" brewlet.sh/provision=true \
    >>"$WORK/t15-profile.log" 2>&1
  T15_PROFILE_CREATED=1
  if ! kubectl apply -f - >>"$WORK/t15-profile.log" 2>&1 <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: $T15_PROFILE
spec:
  nodePool:
    key: $T15_POOL_KEY
    names: [$T15_POOL]
  jdks:
    - distribution: temurin
      feature: 21
  rollout:
    validate: true
    containerdRestart: validated
YAML
  then
    fail "tier15: apply metrics NodeProfile" "see $WORK/t15-profile.log"; return 0
  fi

  local ds="brewlet-node-provisioner-$T15_PROFILE"
  if ! wait_for kubectl get ds "$ds" -n "$T15_NS"; then
    fail "tier15: operator created profile-managed provisioner DaemonSet"
    return 0
  fi
  assert_eq "tier15: exporter sidecar is wired into the provisioner DaemonSet" \
    "$(kubectl get ds "$ds" -n "$T15_NS" \
      -o jsonpath='{.spec.template.spec.containers[1].name}')" "metrics-exporter"
  assert_eq "tier15: exporter sidecar runs the baked exporter binary" \
    "$(kubectl get ds "$ds" -n "$T15_NS" \
      -o jsonpath='{.spec.template.spec.containers[1].command[0]}')" \
    "/opt/brewlet-dist/brewlet-metrics-exporter"
  assert_eq "tier15: exporter sidecar exposes the configured node metrics port" \
    "$(kubectl get ds "$ds" -n "$T15_NS" \
      -o jsonpath='{.spec.template.spec.containers[1].ports[0].containerPort}')" "9090"

  if ! kubectl rollout status ds/"$ds" -n "$T15_NS" --timeout=300s \
      >>"$WORK/t15-profile.log" 2>&1; then
    fail "tier15: real provisioner and exporter sidecar became Ready" \
      "diag: $(save_pod_diag t15-provisioner "$T15_NS" "brewlet.sh/nodeprofile=$T15_PROFILE")"
    return 0
  fi
  pass "tier15: real provisioner and exporter sidecar became Ready"
  if ! wait_for bash -c \
    "[[ \"\$(kubectl get node '$T15_NODE' -o jsonpath='{.metadata.labels.brewlet\\.sh/runtime}' 2>/dev/null)\" == ready ]]"; then
    fail "tier15: provisioner advertised the node as runtime-ready"
    return 0
  fi
  if ! wait_for docker exec "$T15_NODE" test -S /opt/brewlet/metrics/telemetry.sock; then
    fail "tier15: exporter created the host telemetry socket"
    return 0
  fi
  pass "tier15: exporter is listening on /opt/brewlet/metrics/telemetry.sock"

  local node_metrics
  node_metrics="$(_t15_scrape_until \
    "http://brewlet-node-metrics.$T15_NS.svc:9090/metrics" \
    'brewlet_jdk_info\{[^}]*distribution="temurin"[^}]*feature="21"')"
  if [[ -z "$node_metrics" ]]; then
    fail "tier15: scrape node exporter inventory" "node metrics never exposed temurin-21"
    return 0
  fi
  _t15_assert_metric "tier15: JDK inventory metric describes provisioned temurin-21" \
    "$node_metrics" 'brewlet_jdk_info\{[^}]*distribution="temurin"[^}]*feature="21"[^}]*\} 1'
  _t15_assert_metric "tier15: launcher inventory metric includes vanilla java" \
    "$node_metrics" 'brewlet_launcher_info\{launcher="java"\} 1'
  _t15_assert_metric "tier15: JDK installation timestamp metric is exported" \
    "$node_metrics" 'brewlet_jdk_installed_timestamp_seconds\{[^}]*distribution="temurin"[^}]*feature="21"'

  T15_CDS_SNAPSHOT="$WORK/t15-cds-before.txt"
  docker exec "$T15_NODE" find /opt/brewlet/cds -maxdepth 1 -type f -print \
    2>/dev/null | sort >"$T15_CDS_SNAPSHOT" || true
  if ! kubectl apply -n "$T15_APP_NS" -f - >>"$WORK/t15-app.log" 2>&1 <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $T15_APP
spec:
  replicas: 1
  selector:
    matchLabels: { app: $T15_APP }
  template:
    metadata:
      labels: { app: $T15_APP }
      annotations:
        brewlet.sh/artifact-ref: "$T15_REF"
        brewlet.sh/artifact-digest: "$digest"
        brewlet.sh/jdk: "$T15_JDK"
        brewlet.sh/cds-regenerate: "true"
    spec:
      runtimeClassName: brewlet
      nodeSelector:
        $T15_POOL_KEY: "$T15_POOL"
        brewlet.sh/runtime: ready
      containers:
        - name: app
          image: busybox:1.36
          command: ["sleep", "3600"]
          ports: [{ name: http, containerPort: 8080 }]
          resources:
            requests: { cpu: "100m", memory: "128Mi" }
            limits: { cpu: "1", memory: "256Mi" }
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
            periodSeconds: 3
            failureThreshold: 40
YAML
  then
    fail "tier15: create admitted Brewlet workload" "see $WORK/t15-app.log"; return 0
  fi
  if ! kubectl rollout status deploy/"$T15_APP" -n "$T15_APP_NS" --timeout=180s \
      >>"$WORK/t15-app.log" 2>&1; then
    fail "tier15: real Brewlet workload launched" \
      "diag: $(save_pod_diag t15-workload "$T15_APP_NS" "app=$T15_APP")"
    return 0
  fi
  pass "tier15: real Brewlet workload launched through the provisioned shim"

  node_metrics="$(_t15_scrape_until \
    "http://brewlet-node-metrics.$T15_NS.svc:9090/metrics" \
    'brewlet_sandbox_launches_total\{[^}]*outcome="success"' 90)"
  if [[ -z "$node_metrics" ]]; then
    fail "tier15: shim telemetry reached the node exporter" "launch counter was not observed"
    return 0
  fi
  _t15_assert_metric "tier15: successful sandbox launch outcome is exported" \
    "$node_metrics" \
    'brewlet_sandbox_launches_total\{[^}]*artifact_format="native"[^}]*entry_mode="jar"[^}]*outcome="success"[^}]*reason="none"[^}]*\} 1'
  _t15_assert_metric "tier15: successful process-start phase latency is exported" \
    "$node_metrics" \
    'brewlet_sandbox_launch_duration_seconds_count\{outcome="success",phase="process_start"\} 1'
  _t15_assert_metric "tier15: successful containerd artifact resolution is exported" \
    "$node_metrics" \
    'brewlet_artifact_resolution_duration_seconds_count\{artifact_format="native",backend="containerd",outcome="success"\} 1'
  _t15_assert_metric "tier15: AppCDS launch decision is exported" \
    "$node_metrics" \
    'brewlet_cds_regeneration_decisions_total\{role="(consume|write|defer|skip)"\} [1-9][0-9]*'

  local deny
  deny="$(kubectl apply -n "$T15_APP_NS" -f - 2>&1 <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: t15-denied
  annotations:
    brewlet.sh/jdk: temurin-99
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: busybox:1.36
YAML
)"
  kubectl delete pod t15-denied -n "$T15_APP_NS" --ignore-not-found >/dev/null 2>&1 || true
  assert_contains "tier15: live admission webhook rejects an unavailable JDK" \
    "$deny" "NoCompatibleJDK"

  local admission_metrics operator_metrics
  admission_metrics="$(_t15_scrape_until \
    "http://brewlet-admission.$T15_NS.svc:8080/metrics" \
    'brewlet_admission_requests_total\{outcome="denied",reason="NoCompatibleJDK"\}' 60)"
  if [[ -z "$admission_metrics" ]]; then
    fail "tier15: scrape admission outcome metrics"
  else
    _t15_assert_metric "tier15: admitted workload outcome is exported" \
      "$admission_metrics" \
      'brewlet_admission_requests_total\{outcome="admitted",reason="none"\} [1-9][0-9]*'
    _t15_assert_metric "tier15: denied workload outcome is exported" \
      "$admission_metrics" \
      'brewlet_admission_requests_total\{outcome="denied",reason="NoCompatibleJDK"\} 1'
  fi

  operator_metrics="$(_t15_scrape_until \
    "http://brewlet-operator-metrics.$T15_NS.svc:8080/metrics" \
    'brewlet_nodeprofile_nodes\{profile="metrics",state="ready"\} 1' 60)"
  if [[ -z "$operator_metrics" ]]; then
    fail "tier15: scrape NodeProfile and provisioning metrics"
  else
    _t15_assert_metric "tier15: NodeProfile assigned-node gauge is exported" \
      "$operator_metrics" \
      'brewlet_nodeprofile_nodes\{profile="metrics",state="assigned"\} 1'
    _t15_assert_metric "tier15: NodeProfile ready-node gauge is exported" \
      "$operator_metrics" \
      'brewlet_nodeprofile_nodes\{profile="metrics",state="ready"\} 1'
    _t15_assert_metric "tier15: provisioning transition is exported" \
      "$operator_metrics" \
      'brewlet_node_provision_transitions_total\{state="Provisioning"\} [1-9][0-9]*'
    _t15_assert_metric "tier15: ready transition is exported" \
      "$operator_metrics" \
      'brewlet_node_provision_transitions_total\{state="Ready"\} [1-9][0-9]*'
  fi
}
