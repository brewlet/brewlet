#!/usr/bin/env bash
# Tier 13 — NodeProfile controller against the live cluster (proposal 0001).
# The brewlet-operator runs OUT-OF-CLUSTER (built binary, using your kubeconfig)
# so no operator image build/load is needed. This tier proves the profile-driven
# provisioning model end to end against a real API server:
#   - a default (catch-all) NodeProfile -> brewlet RuntimeClass + one per-profile
#     provisioner DaemonSet (brewlet-node-provisioner-default) targeting every node
#   - a named-pool NodeProfile -> its own DaemonSet (brewlet-node-provisioner-<pool>)
#     with an `In [pool]` nodeAffinity on the resolved pool key; the default DS
#     flips to `NotIn [pool]` to stay a non-overlapping catch-all (§5.6)
#   - status.assignedNodes reflects pool membership on both profiles
#   - finalizer-driven reversal (§5.6): deleting a named profile holds it in
#     Terminating behind node.brewlet.sh/cleanup while a brewlet-cleanup-<pool>
#     DaemonSet is created; the object is GC'd only once the finalizer clears
# Safety: the provisioner image is a NON-EXISTENT ref so DaemonSet pods can never
# run host-mutating code. The cleanup finalizer never clears under that bogus
# image, so this tier force-removes it during teardown. Everything is cleaned up.
# Prereqs: kubectl + reachable cluster, go

T13_NS="brewlet"
T13_POOL="batch"
T13_POOL_KEY="agentpool"           # a real provider pool key (AKS legacy) so the
                                   # default profile auto-detects it for exclusion
T13_MGR_PID=""
T13_NODE=""

_t13_cleanup() {
  info "tier13: cleaning up"
  [[ -n "$T13_MGR_PID" ]] && kill "$T13_MGR_PID" 2>/dev/null || true
  force_delete_nodeprofiles
  kubectl delete daemonset -n "$T13_NS" \
    brewlet-node-provisioner-default "brewlet-node-provisioner-$T13_POOL" "brewlet-cleanup-$T13_POOL" \
    --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete ns "$T13_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  wait_for bash -c "! kubectl get namespace '$T13_NS' >/dev/null 2>&1" || true
  kubectl delete crd nodeprofiles.node.brewlet.sh javaapplications.apps.brewlet.sh \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [[ -n "$T13_NODE" ]]; then
    kubectl label "$T13_NODE" "$T13_POOL_KEY-" brewlet.sh/provision- brewlet.sh/runtime- >/dev/null 2>&1 || true
  fi
}

# --- predicate helpers (for wait_for) ------------------------------------
_t13_rc_exists()      { kubectl get runtimeclass brewlet >/dev/null 2>&1; }
_t13_default_ds()     { kubectl get ds brewlet-node-provisioner-default -n "$T13_NS" >/dev/null 2>&1; }
_t13_pool_ds()        { kubectl get ds "brewlet-node-provisioner-$T13_POOL" -n "$T13_NS" >/dev/null 2>&1; }
_t13_cleanup_ds()     { kubectl get ds "brewlet-cleanup-$T13_POOL" -n "$T13_NS" >/dev/null 2>&1; }
_t13_pool_np_gone()   { ! kubectl get nodeprofile "$T13_POOL" >/dev/null 2>&1; }
_t13_pool_terminating() { [[ -n "$(kubectl get nodeprofile "$T13_POOL" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)" ]]; }

_t13_default_affinity() {
  # jsonpath of the default DS's first nodeAffinity matchExpression operator; "" if none.
  kubectl get ds brewlet-node-provisioner-default -n "$T13_NS" \
    -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator}' 2>/dev/null
}
_t13_default_excludes_pool() { [[ "$(_t13_default_affinity)" == "NotIn" ]]; }

tier13_nodeprofile() {
  section "Tier 13 — NodeProfile controller (operator out-of-cluster)"
  if ! have kubectl; then skip "tier13: node profiles" "kubectl not installed"; return 0; fi
  if ! k8s_reachable; then skip "tier13: node profiles" "no reachable cluster"; return 0; fi
  if ! have go; then skip "tier13: node profiles" "go not installed"; return 0; fi

  info "cluster context: $(kubectl config current-context 2>/dev/null)"
  trap _t13_cleanup RETURN

  # --- build the operator manager binary -----------------------------------
  if ( cd "$BREWLET_KUBERNETES_DIR" && go build -o "$WORK/t13-manager" ./cmd/manager ) \
       >"$WORK/t13-build.log" 2>&1; then
    pass "build brewlet-operator manager"
  else
    fail "build brewlet-operator manager" "see $WORK/t13-build.log"; return 0
  fi

  # --- install the CRDs (both, so the manager's caches can start) ----------
  kubectl create namespace "$T13_NS" >/dev/null 2>&1 || true
  if kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/nodeprofile-crd.yaml" >"$WORK/t13-crd.log" 2>&1 \
     && kubectl apply -f "$BREWLET_KUBERNETES_DIR/deploy/javaapplication-crd.yaml" >>"$WORK/t13-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/nodeprofiles.node.brewlet.sh >>"$WORK/t13-crd.log" 2>&1 \
     && kubectl wait --for=condition=Established --timeout=30s \
          crd/javaapplications.apps.brewlet.sh >>"$WORK/t13-crd.log" 2>&1; then
    pass "CRD: NodeProfile + JavaApplication installed and Established"
  else
    fail "CRD: NodeProfile + JavaApplication Established" "see $WORK/t13-crd.log"; return 0
  fi

  # --- start the operator out-of-cluster -----------------------------------
  local probe; probe="$(free_port)"
  "$WORK/t13-manager" \
      --namespace "$T13_NS" \
      --provisioner-image "brewlet-e2e/nonexistent-provisioner:donotpull" \
      --jdks "temurin-21" --launchers "" \
      --leader-elect=false \
      --metrics-bind-address 0 \
      --health-probe-bind-address ":$probe" \
      >"$WORK/t13-manager.log" 2>&1 &
  T13_MGR_PID=$!
  if retry_curl "http://localhost:$probe/readyz" 40 0.5 >/dev/null; then
    pass "operator: manager started and healthy (readyz)"
  else
    fail "operator: manager readyz" "see $WORK/t13-manager.log"; return 0
  fi

  # --- (1) default catch-all profile ---------------------------------------
  cat >"$WORK/t13-default.yaml" <<'YAML'
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: default
spec:
  jdks:
    - distribution: temurin
      feature: 21
YAML
  if kubectl apply -f "$WORK/t13-default.yaml" >"$WORK/t13-np.log" 2>&1; then
    pass "default profile: accepted by the API server"
  else
    fail "default profile: apply" "see $WORK/t13-np.log"; return 0
  fi

  if wait_for _t13_rc_exists; then
    assert_eq "default profile: reconciler ensured the brewlet RuntimeClass handler" \
      "$(kubectl get runtimeclass brewlet -o jsonpath='{.handler}')" "brewlet"
  else
    fail "default profile: reconciler ensured the brewlet RuntimeClass"
  fi

  if wait_for _t13_default_ds; then
    pass "default profile: reconciler created brewlet-node-provisioner-default"
    assert_eq "default profile: DaemonSet has no restricting nodeAffinity while it is the only profile" \
      "$(kubectl get ds brewlet-node-provisioner-default -n "$T13_NS" -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution}' 2>/dev/null)" ""
  else
    fail "default profile: reconciler created brewlet-node-provisioner-default"
  fi

  local total_nodes
  total_nodes="$(kubectl get nodes -o name 2>/dev/null | wc -l | tr -d ' ')"
  if wait_for bash -c "[[ \"\$(kubectl get nodeprofile default -o jsonpath='{.status.assignedNodes}' 2>/dev/null)\" == \"$total_nodes\" ]]"; then
    pass "default profile: status.assignedNodes == every node ($total_nodes)"
  else
    fail "default profile: status.assignedNodes == $total_nodes" \
      "got '$(kubectl get nodeprofile default -o jsonpath='{.status.assignedNodes}' 2>/dev/null)'"
  fi

  # --- (2) named-pool profile ----------------------------------------------
  T13_NODE="$(kubectl get nodes -o name 2>/dev/null | head -1)"
  if [[ -z "$T13_NODE" ]]; then
    skip "named-pool profile" "no nodes found"
  else
    cat >"$WORK/t13-pool.yaml" <<YAML
apiVersion: node.brewlet.sh/v1alpha1
kind: NodeProfile
metadata:
  name: $T13_POOL
spec:
  nodePool:
    key: $T13_POOL_KEY
    names: [$T13_POOL]
  jdks:
    - distribution: microsoft
      feature: 21
YAML
    if kubectl apply -f "$WORK/t13-pool.yaml" >>"$WORK/t13-np.log" 2>&1; then
      pass "named-pool profile: accepted by the API server"
    else
      fail "named-pool profile: apply" "see $WORK/t13-np.log"
    fi

    if wait_for _t13_pool_ds; then
      pass "named-pool profile: reconciler created brewlet-node-provisioner-$T13_POOL"
      local aff_op aff_key aff_val
      aff_op="$(kubectl get ds "brewlet-node-provisioner-$T13_POOL" -n "$T13_NS" -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator}' 2>/dev/null)"
      aff_key="$(kubectl get ds "brewlet-node-provisioner-$T13_POOL" -n "$T13_NS" -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key}' 2>/dev/null)"
      aff_val="$(kubectl get ds "brewlet-node-provisioner-$T13_POOL" -n "$T13_NS" -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}' 2>/dev/null)"
      assert_eq "named-pool profile: DaemonSet nodeAffinity operator is In" "$aff_op" "In"
      assert_eq "named-pool profile: DaemonSet nodeAffinity key is the resolved pool key" "$aff_key" "$T13_POOL_KEY"
      assert_eq "named-pool profile: DaemonSet nodeAffinity value is the pool name" "$aff_val" "$T13_POOL"
    else
      fail "named-pool profile: reconciler created brewlet-node-provisioner-$T13_POOL"
    fi

    # Label a node into the pool. This is a Node event, so the reconciler
    # re-reconciles EVERY profile: the default recomputes its catch-all
    # exclusion (NotIn [pool]) and both profiles' assigned counts converge.
    kubectl label --overwrite "$T13_NODE" "$T13_POOL_KEY=$T13_POOL" >/dev/null 2>&1

    if wait_for _t13_default_excludes_pool; then
      local exval
      exval="$(kubectl get ds brewlet-node-provisioner-default -n "$T13_NS" -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}' 2>/dev/null)"
      assert_eq "default profile: catch-all DaemonSet now excludes the named pool (NotIn value)" "$exval" "$T13_POOL"
    else
      fail "default profile: catch-all DaemonSet flips to NotIn [$T13_POOL]" \
        "got operator '$(_t13_default_affinity)'"
    fi

    if wait_for bash -c "[[ \"\$(kubectl get nodeprofile $T13_POOL -o jsonpath='{.status.assignedNodes}' 2>/dev/null)\" == \"1\" ]]"; then
      pass "named-pool profile: status.assignedNodes == 1 (the labelled node)"
    else
      fail "named-pool profile: status.assignedNodes == 1" \
        "got '$(kubectl get nodeprofile "$T13_POOL" -o jsonpath='{.status.assignedNodes}' 2>/dev/null)'"
    fi

    # --- (3) finalizer-driven reversal (§5.6) ------------------------------
    kubectl delete nodeprofile "$T13_POOL" --wait=false >/dev/null 2>&1
    if wait_for _t13_pool_terminating; then
      pass "reversal: deleting the named profile holds it in Terminating (cleanup finalizer)"
    else
      fail "reversal: named profile enters Terminating behind its finalizer"
    fi
    # It must NOT be gone yet — the finalizer blocks garbage collection.
    if _t13_pool_np_gone; then
      fail "reversal: finalizer must block GC until cleanup is verified" "profile was deleted immediately"
    else
      pass "reversal: finalizer blocks GC while cleanup is unverified"
    fi
    if wait_for _t13_cleanup_ds; then
      pass "reversal: reconciler launched the brewlet-cleanup-$T13_POOL DaemonSet"
    else
      fail "reversal: reconciler launched the cleanup DaemonSet"
    fi
    # Force-remove the finalizer (the bogus image never lets cleanup verify) and
    # confirm the object is then garbage-collected.
    kubectl patch nodeprofile "$T13_POOL" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
    if wait_for _t13_pool_np_gone; then
      pass "reversal: profile is garbage-collected once the finalizer clears"
    else
      fail "reversal: profile GC'd after finalizer removal"
    fi
  fi
}
