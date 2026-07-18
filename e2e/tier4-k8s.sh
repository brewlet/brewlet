#!/usr/bin/env bash
# Tier 4 — Kubernetes control-plane capabilities against the live cluster.
# The brewlet-operator runs OUT-OF-CLUSTER (built binary, using your kubeconfig)
# so no operator image build/load is needed. Covers:
#   - CRD install (JavaApplication + NodeProfile)
#   - JavaApplication controller: reconcile -> Deployment(+runtimeClassName)+Service+HPA,
#     owner refs, status, and garbage-collection on delete
#   - NodeProfile controller: a default (catch-all) NodeProfile -> RuntimeClass +
#     per-profile provisioner DaemonSet (brewlet-node-provisioner-default) + status
#   - Node lifecycle controller: opt-in node -> Provisioning -> Ready state transitions
#   - Helm chart packaging (lint / template / dry-run install)
# Safety: the provisioner image is set to a NON-EXISTENT ref so DaemonSet pods
# can never actually run host-mutating code on your node. Everything is cleaned up.
# Prereqs: kubectl + reachable cluster, go

T4_NS_OP="brewlet"
T4_NS_APP="brewlet-e2e"
T4_MGR_PID=""
T4_NODE=""

_t4_cleanup() {
  info "tier4: cleaning up"
  [[ -n "$T4_MGR_PID" ]] && kill "$T4_MGR_PID" 2>/dev/null || true
  kubectl delete javaapplication orders -n "$T4_NS_APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete ns "$T4_NS_APP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # NodeProfiles hold a cleanup finalizer that never clears under the bogus
  # provisioner image; strip it before deleting the DaemonSets and CRD.
  force_delete_nodeprofiles
  kubectl delete daemonset brewlet-node-provisioner-default -n "$T4_NS_OP" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T4_NS_OP" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete crd javaapplications.apps.brewlet.sh --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete crd nodeprofiles.node.brewlet.sh --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [[ -n "$T4_NODE" ]]; then
    kubectl label "$T4_NODE" brewlet.sh/provision- brewlet.sh/runtime- >/dev/null 2>&1 || true
    kubectl annotate "$T4_NODE" brewlet.sh/provision-state- >/dev/null 2>&1 || true
  fi
}

# predicate helpers (for wait_for)
_t4_dep_exists()  { kubectl get deploy orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_svc_exists()  { kubectl get svc orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_hpa_exists()  { kubectl get hpa orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_dep_gone()    { ! kubectl get deploy orders -n "$T4_NS_APP" >/dev/null 2>&1; }
_t4_jdk_set()     { [[ "$(kubectl get javaapplication orders -n "$T4_NS_APP" -o jsonpath='{.status.selectedJdk}' 2>/dev/null)" == "21" ]]; }
_t4_rc_exists()   { kubectl get runtimeclass brewlet >/dev/null 2>&1; }
_t4_ds_exists()   { kubectl get daemonset brewlet-node-provisioner-default -n "$T4_NS_OP" >/dev/null 2>&1; }
_t4_np_assigned() { [[ "$(kubectl get nodeprofile default -o jsonpath='{.status.assignedNodes}' 2>/dev/null)" =~ ^[1-9][0-9]*$ ]]; }
_t4_state_is()    { [[ "$(kubectl get "$T4_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}' 2>/dev/null)" == "$1" ]]; }

tier4_k8s() {
  section "Tier 4 — Kubernetes control plane (operator out-of-cluster)"
  if ! have kubectl; then skip "tier4: k8s control plane" "kubectl not installed"; return 0; fi
  if ! k8s_reachable; then skip "tier4: k8s control plane" "no reachable cluster"; return 0; fi
  if ! have go; then skip "tier4: k8s control plane" "go not installed"; return 0; fi

  info "cluster context: $(kubectl config current-context 2>/dev/null)"
  trap _t4_cleanup RETURN

  # --- build the operator manager binary -----------------------------------
  if ( cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t4-manager" ./cmd/manager ) \
       >"$WORK/t4-build.log" 2>&1; then
    pass "build brewlet-operator manager"
  else
    fail "build brewlet-operator manager" "see $WORK/t4-build.log"; return 0
  fi

  # --- install the CRDs -----------------------------------------------------
  # Both must exist before the manager starts: controller-runtime's typed caches
  # open informers for JavaApplication AND NodeProfile at boot and fail if either
  # CRD is missing.
  kubectl create namespace "$T4_NS_OP" >/dev/null 2>&1 || true
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >"$WORK/t4-crd.log" 2>&1 \
     && kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/nodeprofile-crd.yaml" >>"$WORK/t4-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/javaapplications.apps.brewlet.sh >>"$WORK/t4-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/nodeprofiles.node.brewlet.sh >>"$WORK/t4-crd.log" 2>&1; then
    pass "CRD: JavaApplication + NodeProfile installed and Established"
  else
    fail "CRD: JavaApplication + NodeProfile Established" "see $WORK/t4-crd.log"; return 0
  fi

  # --- start the operator out-of-cluster ------------------------------------
  local probe; probe="$(free_port)"
  "$WORK/t4-manager" \
      --namespace "$T4_NS_OP" \
      --provisioner-image "brewlet-e2e/nonexistent-provisioner:donotpull" \
      --jdks "temurin-21" --launchers "" \
      --leader-elect=false \
      --metrics-bind-address 0 \
      --health-probe-bind-address ":$probe" \
      >"$WORK/t4-manager.log" 2>&1 &
  T4_MGR_PID=$!
  if retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    pass "operator: manager started and healthy (readyz)"
  else
    fail "operator: manager readyz" "see $WORK/t4-manager.log"; return 0
  fi

  # --- JavaApplication controller ------------------------------------------
  kubectl create namespace "$T4_NS_APP" >/dev/null 2>&1 || true
  cat >"$WORK/t4-japp.yaml" <<'YAML'
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders
  namespace: brewlet-e2e
spec:
  artifact:
    image: registry.example.com/team/orders:1.4.2
    pullPolicy: IfNotPresent
  replicas: 1
  resources:
    requests: { cpu: "250m", memory: "256Mi" }
    limits:   { cpu: "1",    memory: "512Mi" }
  jvm:
    version: 21
    distribution: temurin
    launcher: jaz
    args: ["-XX:MaxRAMPercentage=75.0"]
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  autoscaling:
    enabled: true
    minReplicas: 1
    maxReplicas: 5
    targetCPUUtilizationPercentage: 70
YAML
  if kubectl apply -f "$WORK/t4-japp.yaml" >"$WORK/t4-apply.log" 2>&1; then
    pass "JavaApplication: descriptor accepted by the API server"
  else
    fail "JavaApplication: apply descriptor" "see $WORK/t4-apply.log"
  fi

  if wait_for _t4_dep_exists; then
    pass "controller: reconciled a managed Deployment"
    assert_eq "controller: Deployment uses runtimeClassName brewlet" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.spec.runtimeClassName}')" "brewlet"
    assert_eq "controller: Deployment is owned by the JavaApplication" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.metadata.ownerReferences[0].kind}')" "JavaApplication"
    # spec.jvm.distribution + launcher fold into the pod-template annotations the
    # webhook/shim consume: "<dist>-<feature>" and the launcher name.
    assert_eq "controller: jvm.distribution+version fold into brewlet.sh/jdk annotation" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.metadata.annotations.brewlet\.sh/jdk}')" "temurin-21"
    assert_eq "controller: jvm.launcher folds into brewlet.sh/launcher annotation" \
      "$(kubectl get deploy orders -n "$T4_NS_APP" -o jsonpath='{.spec.template.metadata.annotations.brewlet\.sh/launcher}')" "jaz"
  else
    fail "controller: reconciled a managed Deployment" "see $WORK/t4-manager.log"
  fi

  if wait_for _t4_svc_exists; then
    pass "controller: reconciled a managed Service"
  else
    fail "controller: reconciled a managed Service"
  fi

  if wait_for _t4_hpa_exists; then
    assert_eq "controller: reconciled an HPA (autoscaling on)" \
      "$(kubectl get hpa orders -n "$T4_NS_APP" -o jsonpath='{.spec.maxReplicas}')" "5"
  else
    fail "controller: reconciled an HPA"
  fi

  if wait_for _t4_jdk_set; then
    pass "controller: JavaApplication status reflects selected JDK (21)"
  else
    fail "controller: status.selectedJdk == 21" \
      "got '$(kubectl get javaapplication orders -n "$T4_NS_APP" -o jsonpath='{.status.selectedJdk}' 2>/dev/null)'"
  fi

  # --- garbage collection on delete ----------------------------------------
  kubectl delete javaapplication orders -n "$T4_NS_APP" --wait=false >/dev/null 2>&1
  if wait_for _t4_dep_gone; then
    pass "controller: deleting the JavaApplication GCs its Deployment (owner refs)"
  else
    fail "controller: child Deployment garbage-collected on delete"
  fi

  # --- NodeProfile controller: default (catch-all) profile -----------------
  # Creating a default NodeProfile (empty pool = every node) is what now drives
  # RuntimeClass + provisioner-DaemonSet creation (the node annotation no longer
  # does). The DaemonSet references the bogus provisioner image, so its pods
  # never run host-mutating code.
  cat >"$WORK/t4-nodeprofile.yaml" <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: default
spec:
  jdks:
    - distribution: temurin
      feature: 21
YAML
  if kubectl apply -f "$WORK/t4-nodeprofile.yaml" >"$WORK/t4-np.log" 2>&1; then
    pass "NodeProfile: default catch-all profile accepted by the API server"
  else
    fail "NodeProfile: apply default profile" "see $WORK/t4-np.log"
  fi

  if wait_for _t4_rc_exists; then
    assert_eq "NodeProfile: reconciler ensured the brewlet RuntimeClass handler" \
      "$(kubectl get runtimeclass brewlet -o jsonpath='{.handler}')" "brewlet"
  else
    fail "NodeProfile: reconciler ensured the brewlet RuntimeClass"
  fi

  if wait_for _t4_ds_exists; then
    pass "NodeProfile: reconciler created the per-profile provisioner DaemonSet (brewlet-node-provisioner-default)"
    # The catch-all default with no sibling named pools targets every node, so it
    # carries no restricting nodeAffinity.
    assert_eq "NodeProfile: default DaemonSet has no restricting nodeAffinity (every node)" \
      "$(kubectl get ds brewlet-node-provisioner-default -n "$T4_NS_OP" -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution}' 2>/dev/null)" ""
  else
    fail "NodeProfile: reconciler created the per-profile provisioner DaemonSet"
  fi

  if wait_for _t4_np_assigned; then
    pass "NodeProfile: status.assignedNodes reflects the claimed fleet"
  else
    fail "NodeProfile: status.assignedNodes populated" \
      "got '$(kubectl get nodeprofile default -o jsonpath='{.status.assignedNodes}' 2>/dev/null)'"
  fi

  # --- Node lifecycle controller: state transitions ------------------------
  T4_NODE="$(kubectl get nodes -o name 2>/dev/null | head -1)"
  if [[ -z "$T4_NODE" ]]; then
    skip "node lifecycle: opt-in flow" "no nodes found"
  else
    info "node lifecycle: using $T4_NODE"
    kubectl label --overwrite "$T4_NODE" brewlet.sh/provision=true >/dev/null 2>&1

    if wait_for _t4_state_is "Provisioning"; then
      pass "node lifecycle: node marked Provisioning while awaiting the runtime"
    else
      fail "node lifecycle: node state Provisioning" \
        "got '$(kubectl get "$T4_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}' 2>/dev/null)'"
    fi

    # Simulate the provisioner finishing: it advertises the runtime-ready label.
    kubectl label --overwrite "$T4_NODE" brewlet.sh/runtime=ready >/dev/null 2>&1
    if wait_for _t4_state_is "Ready"; then
      pass "node lifecycle: node transitions to Ready once the runtime is advertised"
    else
      fail "node lifecycle: node state Ready" \
        "got '$(kubectl get "$T4_NODE" -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}' 2>/dev/null)'"
    fi
  fi

  # --- Helm chart packaging -------------------------------------------------
  if have helm; then
    if helm lint "$BREWLET_KUBERNETES_DIR/charts/brewlet" >"$WORK/t4-helm-lint.log" 2>&1; then
      pass "helm: chart lints clean"
    else
      fail "helm: chart lint" "see $WORK/t4-helm-lint.log"
    fi
    local tmpl
    if tmpl="$(helm template brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" 2>>"$WORK/t4-helm-template.log")"; then
      assert_contains "helm: renders the operator Deployment" "$tmpl" "brewlet-operator"
      assert_contains "helm: renders the admission webhook" "$tmpl" "admission"
      assert_contains "helm: renders provisioner RBAC" "$tmpl" "ServiceAccount"
      assert_contains "helm: renders the default NodeProfile CR" "$tmpl" "kind: NodeProfile"
      assert_contains "helm: renders the NodeProfile validating webhook" "$tmpl" "/validate-nodeprofiles"
    else
      fail "helm: template render" "see $WORK/t4-helm-template.log"
    fi
    if helm install brewlet "$BREWLET_KUBERNETES_DIR/charts/brewlet" --dry-run --namespace "$T4_NS_OP" \
         >"$WORK/t4-helm-dryrun.log" 2>&1; then
      pass "helm: install --dry-run succeeds against the cluster"
    else
      fail "helm: install --dry-run" "see $WORK/t4-helm-dryrun.log"
    fi
  else
    skip "helm: chart packaging" "helm not installed"
  fi
}
