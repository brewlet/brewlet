#!/usr/bin/env bash
# Tier 10 — the SHIPPED Helm chart installed into a live cluster (real RBAC).
#
# Tiers 4/6 only cover the control plane partially: tier 4 runs the operator
# OUT-of-cluster and merely `helm template`/`--dry-run`s the chart; tier 6
# hand-rolls the webhook Deployment. Nothing exercises the chart as customers
# actually consume it — a real `helm install` that stands up the operator AND
# the admission webhook IN-CLUSTER behind the chart's own ServiceAccounts,
# ClusterRoles/Bindings, generated serving cert and MutatingWebhookConfiguration.
#
# This tier does exactly that and then proves the shipped RBAC works end to end:
#   1. `helm install` the chart (operator + admission images side-loaded; the
#      provisioner image is deliberately BOGUS so no host-mutating DaemonSet can
#      ever run). CRD + webhook config + both Deployments come up.
#   2. Then we apply a default (catch-all) NodeProfile once the in-cluster
#      webhook is up; this drives the in-cluster operator to create the
#      brewlet RuntimeClass and per-profile provisioner DaemonSet (proves its
#      runtimeclasses/daemonsets RBAC).
#   3. Apply a JavaApplication -> the in-cluster operator reconciles it into a
#      Deployment(+runtimeClassName: brewlet) + Service with owner refs + status
#      (proves its javaapplications/deployments/services/status RBAC).
#   4. The live pod webhook denies a brewlet pod requesting a JDK the (empty)
#      fleet lacks -> NoCompatibleJDK (proves the shipped webhook + node-list
#      RBAC + caBundle wiring reach the API server).
#   5. A custom JDK distribution without its required source is rejected, and a
#      pool conflict is rejected by the live NodeProfile webhook (proves the
#      shipped ValidatingWebhookConfiguration + its caBundle wiring).
#
# No brewlet pod is expected to actually RUN here (no node is provisioned ready,
# so brewlet pods stay Pending/denied) — that path is tiers 8/9. This tier is
# purely about the shipped chart + in-cluster RBAC.
#
# SKIPs gracefully unless: kubectl + reachable cluster, docker, helm, and every
# node is a local containerd docker container we can side-load images into
# (Docker Desktop / kind / CI). Everything is cleaned up.
# Prereqs: kubectl + cluster, docker, helm, go (to build the images).

T10_RELEASE="brewlet-e2e"
T10_RELEASE_NS="default"          # helm release metadata ns; chart targets `brewlet`
T10_NS="brewlet"                  # chart namespace (created by the chart)
T10_APP_NS="brewlet-japp-e2e"     # where we apply the JavaApplication + probe pod
T10_APP="orders-e2e"
T10_OP_IMG="brewlet.local/operator:e2e"
T10_ADM_IMG="brewlet.local/admission:e2e"
T10_PROV_IMG="brewlet.invalid/node-provisioner:e2e"  # bogus on purpose: never runs
T10_NODE=""
T10_RC_PREEXISTING=""
declare -a T10_LOADED_NODES=()

_t10_cleanup() {
  info "tier10: cleaning up"
  kubectl delete javaapplication "$T10_APP" -n "$T10_APP_NS" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T10_APP_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # NodeProfiles (the chart's default + any this tier created) hold a cleanup
  # finalizer that never clears under the bogus provisioner image. Strip it
  # before AND after `helm uninstall`: before so uninstall isn't wedged, after
  # to clear a finalizer the still-running operator may have re-added mid-uninstall.
  force_delete_nodeprofiles
  helm uninstall "$T10_RELEASE" -n "$T10_RELEASE_NS" >/dev/null 2>&1 || true
  force_delete_nodeprofiles
  # helm doesn't manage CRDs in crds/, the operator-created RuntimeClass, or the
  # chart namespace object left behind if uninstall raced — clean them directly.
  [[ -z "$T10_RC_PREEXISTING" ]] && kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete crd javaapplications.apps.brewlet.sh --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete crd nodeprofiles.node.brewlet.sh --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete mutatingwebhookconfiguration brewlet-admission --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete validatingwebhookconfiguration brewlet-nodeprofiles --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T10_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  wait_for bash -c "! kubectl get namespace '$T10_NS' >/dev/null 2>&1" || true
  if [[ -n "$T10_NODE" ]]; then
    label_node "$T10_NODE" brewlet.sh/provision- >/dev/null 2>&1 || true
  fi
  for n in "${T10_LOADED_NODES[@]}"; do
    docker exec "$n" ctr -n k8s.io images rm "$T10_OP_IMG" "$T10_ADM_IMG" >/dev/null 2>&1 || true
  done
  docker rmi "$T10_OP_IMG" "$T10_ADM_IMG" >/dev/null 2>&1 || true
}

# _t10_build_load CMD IMG -> docker build (arch-native) + side-load into all nodes.
_t10_build_load() {
  local cmd="$1" img="$2" nodes="$3" n tarball
  if ! docker build --provenance=false --build-arg CMD="$cmd" -t "$img" \
        -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
        >>"$WORK/t10-build.log" 2>&1; then
    if ! docker build --build-arg CMD="$cmd" -t "$img" \
          -f "$BREWLET_KUBERNETES_DIR/Dockerfile" "$BREWLET_KUBERNETES_DIR" \
          >>"$WORK/t10-build.log" 2>&1; then
      return 1
    fi
  fi
  tarball="$WORK/t10-$cmd.tar"
  docker save "$img" -o "$tarball" 2>>"$WORK/t10-load.log" || return 2
  for n in $nodes; do
    docker exec -i "$n" ctr -n k8s.io images import - <"$tarball" >>"$WORK/t10-load.log" 2>&1 || return 3
  done
  return 0
}

tier10_helm_incluster() {
  section "Tier 10 — shipped Helm chart installed in-cluster (real RBAC)"
  if ! have kubectl || ! k8s_reachable; then skip "tier10: helm install" "no reachable cluster"; return 0; fi
  if ! have docker || ! docker info >/dev/null 2>&1; then skip "tier10: helm install" "docker daemon not available"; return 0; fi
  if ! have helm; then skip "tier10: helm install" "helm not installed"; return 0; fi
  if ! have go; then skip "tier10: helm install" "go not installed (needed to build images)"; return 0; fi

  # --- every node must be a local docker container we can side-load into ----
  local nodes n
  nodes="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##')"
  if [[ -z "$nodes" ]]; then skip "tier10: helm install" "no nodes"; return 0; fi
  for n in $nodes; do
    if ! docker inspect "$n" >/dev/null 2>&1 || ! docker exec "$n" ctr --version >/dev/null 2>&1; then
      skip "tier10: helm install" "node '$n' is not a local containerd docker container (can't side-load images)"
      return 0
    fi
  done
  T10_NODE="$(echo "$nodes" | head -1)"
  # Remember whether a RuntimeClass already existed so cleanup doesn't clobber it.
  kubectl get runtimeclass brewlet >/dev/null 2>&1 && T10_RC_PREEXISTING=1

  trap _t10_cleanup RETURN

  # --- build + side-load the operator and admission images ------------------
  info "tier10: building operator + admission images (docker build, may take a minute)"
  local blrc
  _t10_build_load manager "$T10_OP_IMG" "$nodes"; blrc=$?
  if [[ $blrc -eq 1 ]]; then fail "helm(in-cluster): build operator image" "see $WORK/t10-build.log"; return 0
  elif [[ $blrc -ne 0 ]]; then skip "tier10: helm install" "could not side-load operator image (see $WORK/t10-load.log)"; return 0; fi
  _t10_build_load admission "$T10_ADM_IMG" "$nodes"; blrc=$?
  if [[ $blrc -eq 1 ]]; then fail "helm(in-cluster): build admission image" "see $WORK/t10-build.log"; return 0
  elif [[ $blrc -ne 0 ]]; then skip "tier10: helm install" "could not side-load admission image (see $WORK/t10-load.log)"; return 0; fi
  for n in $nodes; do T10_LOADED_NODES+=("$n"); done
  pass "helm(in-cluster): built + side-loaded operator + admission images"

  # --- helm install the shipped chart ---------------------------------------
  # provisioner image is bogus (IfNotPresent + unresolvable) so the operator's
  # DaemonSet can never run host-mutating code on the node. leader-elect off for
  # a snappy single-replica rollout. Keep defaultProfile disabled during install:
  # a fail-closed NodeProfile validating webhook can come up after resources are
  # applied, so creating NodeProfiles during the same helm transaction is flaky.
  info "tier10: helm install $T10_RELEASE"
  if ! helm install "$T10_RELEASE" "$BREWLET_KUBERNETES_DIR/charts/brewlet" \
        --namespace "$T10_RELEASE_NS" \
        --set images.operator="$T10_OP_IMG" \
        --set images.admission="$T10_ADM_IMG" \
        --set images.provisioner="$T10_PROV_IMG" \
        --set images.pullPolicy=IfNotPresent \
        --set defaultProfile.enabled=false \
        --set provisioner.jdks=temurin-21 \
        --set provisioner.launchers= \
        --set operator.leaderElect=false \
        --wait --timeout 180s >"$WORK/t10-install.log" 2>&1; then
    kubectl get pods -n "$T10_NS" >>"$WORK/t10-install.log" 2>&1 || true
    kubectl logs -n "$T10_NS" -l app=brewlet-operator --tail=50 >>"$WORK/t10-install.log" 2>&1 || true
    kubectl logs -n "$T10_NS" -l app=brewlet-admission --tail=50 >>"$WORK/t10-install.log" 2>&1 || true
    fail "helm(in-cluster): helm install succeeded" "see $WORK/t10-install.log"; return 0
  fi
  pass "helm(in-cluster): chart installed (operator + admission rolled out via --wait)"

  # --- the install created the shipped, cluster-facing objects --------------
  if kubectl get crd javaapplications.apps.brewlet.sh >/dev/null 2>&1; then
    pass "helm(in-cluster): JavaApplication CRD installed"
  else
    fail "helm(in-cluster): JavaApplication CRD installed"; return 0
  fi
  if kubectl get mutatingwebhookconfiguration brewlet-admission >/dev/null 2>&1; then
    pass "helm(in-cluster): MutatingWebhookConfiguration registered"
  else
    fail "helm(in-cluster): MutatingWebhookConfiguration registered"; return 0
  fi
  if kubectl get crd nodeprofiles.node.brewlet.sh >/dev/null 2>&1; then
    pass "helm(in-cluster): NodeProfile CRD installed"
  else
    fail "helm(in-cluster): NodeProfile CRD installed"; return 0
  fi
  if kubectl get validatingwebhookconfiguration brewlet-nodeprofiles >/dev/null 2>&1; then
    pass "helm(in-cluster): NodeProfile ValidatingWebhookConfiguration registered"
  else
    fail "helm(in-cluster): NodeProfile ValidatingWebhookConfiguration registered"
  fi
  local opAvail admAvail
  opAvail="$(kubectl get deploy brewlet-operator -n "$T10_NS" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
  admAvail="$(kubectl get deploy brewlet-admission -n "$T10_NS" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
  assert_eq "helm(in-cluster): operator Deployment available" "${opAvail:-0}" "1"
  assert_eq "helm(in-cluster): admission Deployment available" "${admAvail:-0}" "1"
  if kubectl get clusterrolebinding brewlet-operator brewlet-admission >/dev/null 2>&1; then
    pass "helm(in-cluster): shipped operator + admission ClusterRoleBindings present"
  else
    fail "helm(in-cluster): shipped ClusterRoleBindings present"
  fi
  if kubectl get service brewlet-operator-metrics -n "$T10_NS" >/dev/null 2>&1 \
     || kubectl get service brewlet-node-metrics -n "$T10_NS" >/dev/null 2>&1; then
    fail "helm(in-cluster): optional metrics Services absent by default"
  else
    pass "helm(in-cluster): optional metrics Services absent by default"
  fi

  # --- default NodeProfile converges the cluster singletons ------------------
  # Apply a catch-all default profile now that the webhook endpoint is live.
  info "tier10: apply default NodeProfile and converge RuntimeClass + per-profile DaemonSet"
  local npboot
  for _ in 1 2 3 4 5 6 7 8; do
    npboot="$(kubectl apply -f - 2>&1 <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: default
spec:
  jdks:
    - distribution: temurin
      feature: 21
YAML
)"
    [[ "$npboot" == *"created"* || "$npboot" == *"configured"* || "$npboot" == *"unchanged"* ]] && break
    sleep 2
  done
  assert_contains "helm(in-cluster): default NodeProfile applied" "$npboot" "nodeprofile.node.brewlet.sh/default"

  # The in-cluster operator reconciles the default profile into the brewlet
  # RuntimeClass + the per-profile DaemonSet (brewlet-node-provisioner-default).
  # This no longer depends on a node opt-in label — we still label a node to
  # exercise the node-state path.
  if ! label_node "$T10_NODE" --overwrite brewlet.sh/provision=true >>"$WORK/t10-optin.log" 2>&1; then
    fail "helm(in-cluster): opt node in with brewlet.sh/provision label" "see $WORK/t10-optin.log"; return 0
  fi
  if wait_for kubectl get runtimeclass brewlet; then
    pass "helm(in-cluster): operator created the brewlet RuntimeClass (runtimeclasses RBAC ok)"
  else
    kubectl logs -n "$T10_NS" -l app=brewlet-operator --tail=60 >>"$WORK/t10-optin.log" 2>&1 || true
    fail "helm(in-cluster): operator created the RuntimeClass" "see $WORK/t10-optin.log"
  fi
  if wait_for kubectl get ds brewlet-node-provisioner-default -n "$T10_NS"; then
    pass "helm(in-cluster): operator created the per-profile provisioner DaemonSet (daemonsets RBAC ok)"
  else
    fail "helm(in-cluster): operator created the provisioner DaemonSet"
  fi

  # --- apply a JavaApplication: the in-cluster operator must reconcile it ----
  kubectl create namespace "$T10_APP_NS" >/dev/null 2>&1 || true
  if ! kubectl apply -f - >"$WORK/t10-japp.log" 2>&1 <<YAML
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: $T10_APP
  namespace: $T10_APP_NS
spec:
  artifact:
    image: registry.example.com/team/orders:1.0.0
    pullPolicy: IfNotPresent
  replicas: 1
  resources:
    requests: { cpu: "100m", memory: "128Mi" }
    limits:   { cpu: "500m", memory: "256Mi" }
  jvm:
    version: 21
    distribution: temurin
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
YAML
  then
    fail "helm(in-cluster): apply JavaApplication" "see $WORK/t10-japp.log"; return 0
  fi

  if wait_for kubectl get deploy "$T10_APP" -n "$T10_APP_NS"; then
    pass "helm(in-cluster): operator reconciled JavaApplication -> Deployment (javaapplications/deployments RBAC ok)"
  else
    kubectl logs -n "$T10_NS" -l app=brewlet-operator --tail=60 >>"$WORK/t10-japp.log" 2>&1 || true
    fail "helm(in-cluster): operator reconciled JavaApplication -> Deployment" "see $WORK/t10-japp.log"; return 0
  fi

  local rc owner svc
  rc="$(kubectl get deploy "$T10_APP" -n "$T10_APP_NS" -o jsonpath='{.spec.template.spec.runtimeClassName}' 2>/dev/null)"
  assert_eq "helm(in-cluster): managed Deployment uses runtimeClassName brewlet" "$rc" "brewlet"
  owner="$(kubectl get deploy "$T10_APP" -n "$T10_APP_NS" -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null)"
  assert_eq "helm(in-cluster): Deployment is owned by the JavaApplication" "$owner" "JavaApplication"
  if wait_for kubectl get svc "$T10_APP" -n "$T10_APP_NS"; then
    svc="$(kubectl get svc "$T10_APP" -n "$T10_APP_NS" -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null)"
    assert_eq "helm(in-cluster): operator created the Service (owned by JavaApplication)" "$svc" "JavaApplication"
  else
    fail "helm(in-cluster): operator created the Service"
  fi
  # The operator writes status back through the /status subresource (RBAC).
  if wait_for bash -c "kubectl get javaapplication $T10_APP -n $T10_APP_NS -o jsonpath='{.status.observedGeneration}' 2>/dev/null | grep -q '[0-9]'"; then
    pass "helm(in-cluster): operator wrote JavaApplication .status (status subresource RBAC ok)"
  else
    fail "helm(in-cluster): operator wrote JavaApplication .status"
  fi

  # --- the LIVE webhook must deny a brewlet pod the fleet can't satisfy ------
  # No node is runtime=ready, so every brewlet pod is unschedulable -> the
  # shipped webhook (reached via its generated caBundle + Service) denies with
  # NoCompatibleJDK. Retry: the webhook's node cache can lag a beat.
  local out
  for _ in 1 2 3 4 5 6 7 8; do    out="$(kubectl apply -n "$T10_APP_NS" -f - 2>&1 <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: brewlet-deny-probe
  annotations:
    brewlet.sh/jdk: "temurin-99"
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/orders:1.0.0
YAML
)"
    [[ "$out" == *NoCompatibleJDK* ]] && break
    kubectl delete pod brewlet-deny-probe -n "$T10_APP_NS" --ignore-not-found >/dev/null 2>&1 || true
    sleep 2
  done
  kubectl delete pod brewlet-deny-probe -n "$T10_APP_NS" --ignore-not-found >/dev/null 2>&1 || true
  assert_contains "helm(in-cluster): live webhook denies an unsatisfiable brewlet pod (NoCompatibleJDK)" \
    "$out" "NoCompatibleJDK"

  # --- a custom JDK distribution must declare its source ---------------------
  # Uncurated distributions are supported when source.image and source.javaHome
  # are present. This profile intentionally omits source and must be rejected by
  # the CRD or validating webhook. Retry because the webhook endpoint's
  # caBundle/Service can lag briefly after install.
  local npout npok=""
  for _ in 1 2 3 4 5 6 7 8; do
    if npout="$(kubectl apply -f - 2>&1 <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: e2e-missing-jdk-source
spec:
  jdks:
    - distribution: not-a-real-distro
      feature: 21
YAML
)"; then
      # apply unexpectedly succeeded — the profile was admitted; not a rejection.
      kubectl delete nodeprofile e2e-missing-jdk-source --ignore-not-found >/dev/null 2>&1 || true
      sleep 2
      continue
    fi
    if [[ "$npout" == *"source must be omitted for curated distributions and provided for custom distributions"* ||
          "$npout" == *"source is required for non-curated distribution"* ]]; then
      npok=1
      break
    fi
    sleep 2
  done
  kubectl delete nodeprofile e2e-missing-jdk-source --ignore-not-found >/dev/null 2>&1 || true
  if [[ -n "$npok" ]]; then
    pass "helm(in-cluster): NodeProfile rejects a custom JDK without source"
  else
    fail "helm(in-cluster): NodeProfile rejects a custom JDK without source" \
      "expected the apply to fail because source is required, got: $npout"
  fi

  # (b) a pool conflict: two profiles claiming the same named pool -> PoolConflict.
  # The first (valid) profile is accepted; the second collides.
  if kubectl apply -f - >"$WORK/t10-np-a.log" 2>&1 <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: e2e-pool-a
spec:
  nodePool:
    key: agentpool
    names: [batch]
  jdks:
    - distribution: temurin
      feature: 21
YAML
  then
    pass "helm(in-cluster): NodeProfile webhook admits a valid named-pool profile"
  else
    fail "helm(in-cluster): NodeProfile webhook admits a valid named-pool profile" "see $WORK/t10-np-a.log"
  fi
  local npconf
  npconf="$(kubectl apply -f - 2>&1 <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: e2e-pool-b
spec:
  nodePool:
    key: agentpool
    names: [batch]
  jdks:
    - distribution: temurin
      feature: 21
YAML
)"
  kubectl delete nodeprofile e2e-pool-b --ignore-not-found >/dev/null 2>&1 || true
  # Strip the finalizer from the accepted e2e-pool-a and delete just it (leave
  # the chart's default profile in place; the RETURN trap sweeps everything).
  kubectl patch nodeprofile e2e-pool-a --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  kubectl delete nodeprofile e2e-pool-a --ignore-not-found --wait=false >/dev/null 2>&1 || true
  assert_contains "helm(in-cluster): NodeProfile webhook rejects a pool conflict (PoolConflict)" \
    "$npconf" "PoolConflict"
}
