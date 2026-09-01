#!/usr/bin/env bash
# Reset any Brewlet state a previous e2e run (or an aborted one) left behind in
# your cluster and on its nodes, so the suite starts clean and re-runs are
# deterministic. This is the single biggest reliability win for re-running the
# suite: stale node labels or a leftover cluster-scoped object from an earlier
# run make otherwise-healthy tiers FAIL for reasons that look like product bugs
# (e.g. a webhook "admitting" a JDK a scrubbed run thought was absent, or
# `helm install` refusing to adopt a ClusterRole it didn't create).
#
# Safety: this only ever touches Brewlet's own objects — `brewlet.sh/*` node
# labels/annotations and cluster/namespaced objects named/labelled `brewlet`.
# It never deletes your application workloads.
#
#   ./reset.sh              # scrub node labels + brewlet cluster objects
#   ./run.sh --reset        # same, via the orchestrator
#   ./run.sh --reset --tier 10   # reset first, then run tier 10
#
# Sourced by run.sh; also runnable standalone.
set -uo pipefail

# When run standalone, pull in the shared helpers (info/warn/have/...).
if ! declare -F info >/dev/null 2>&1; then
  _RESET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  # shellcheck source=lib.sh
  source "$_RESET_DIR/lib.sh"
fi

# Fixed namespaces the tiers use for their fixtures. Throwaway per-run
# namespaces (tiers 5/6/10/11) are random and self-cleaned by their RETURN
# traps; we only need to sweep the deterministic ones here.
E2E_FIXED_NS=(brewlet brewlet-e2e brewlet-e2e-app brewlet-custom-jdk brewlet-metrics-e2e)

# scrub_node_labels: remove every brewlet.sh/* label and annotation from every
# node. These are advertised by the provisioner (or simulated by tiers) and are
# always safe to reset for an e2e cluster — a genuinely provisioned node simply
# gets re-labelled by its provisioner DaemonSet.
scrub_node_labels() {
  local n keys
  for n in $(kubectl get nodes -o name 2>/dev/null); do
    n="${n#node/}"
    keys="$(kubectl get node "$n" -o json 2>/dev/null | python3 -c '
import json,sys
m=json.load(sys.stdin)["metadata"]
ks=[k for k in list(m.get("labels",{}))+list(m.get("annotations",{})) if k.startswith("brewlet.sh/")]
print("\n".join(ks))
' 2>/dev/null)"
    [[ -z "$keys" ]] && continue
    local labels=() annos=() k
    while IFS= read -r k; do
      [[ -z "$k" ]] && continue
      # A label and an annotation can share a key; strip from both maps.
      labels+=("${k}-"); annos+=("${k}-")
    done <<<"$keys"
    label_node "$n" "${labels[@]}" >/dev/null 2>&1 || true
    annotate_node "$n" "${annos[@]}"  >/dev/null 2>&1 || true
  done
}

# scrub_cluster_objects: delete brewlet's cluster-scoped + fixed-namespace
# objects, whether created by Helm, by the raw deploy/ manifests, or by the
# operator at runtime.
scrub_cluster_objects() {
  # NodeProfiles carry a cleanup finalizer that wedges deletion under the e2e
  # suite's bogus provisioner image — strip finalizers before anything else so
  # the CRD (and any namespace hosting cleanup DaemonSets) can be removed.
  force_delete_nodeprofiles
  # Helm-labelled objects (chart install).
  kubectl delete clusterrole,clusterrolebinding,mutatingwebhookconfiguration,validatingwebhookconfiguration \
    -l 'app.kubernetes.io/name=brewlet' --ignore-not-found >/dev/null 2>&1 || true
  # Well-known names (raw manifests / operator-created / older runs).
  kubectl delete clusterrole        brewlet-operator brewlet-admission brewlet-node-provisioner --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding brewlet-operator brewlet-admission brewlet-node-provisioner --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete runtimeclass brewlet --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete crd javaapplications.apps.brewlet.sh --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete crd nodeprofiles.node.brewlet.sh --ignore-not-found --wait=false >/dev/null 2>&1 || true
  local ns
  for ns in "${E2E_FIXED_NS[@]}"; do
    kubectl delete ns "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  for ns in "${E2E_FIXED_NS[@]}"; do
    wait_for bash -c "! kubectl get namespace '$ns' >/dev/null 2>&1" ||
      warn "reset: namespace '$ns' is still terminating"
  done
}

# detect_leftovers: print a short list of brewlet cluster-scoped leftovers, if
# any. Used by the preflight to nudge the user toward `--reset` without deleting
# anything itself.
detect_leftovers() {
  kubectl get clusterrole,clusterrolebinding,runtimeclass,crd,mutatingwebhookconfiguration,validatingwebhookconfiguration \
    -o name 2>/dev/null | grep -iE 'brewlet' || true
}

# e2e_reset: full scrub. Safe to run repeatedly.
e2e_reset() {
  if ! have kubectl || ! k8s_reachable; then
    warn "reset: no reachable cluster — nothing to do"
    return 0
  fi
  info "reset: scrubbing brewlet state on cluster '$(kubectl config current-context 2>/dev/null)'"
  scrub_node_labels
  scrub_cluster_objects
  local left; left="$(detect_leftovers)"
  if [[ -n "$left" ]]; then
    warn "reset: some brewlet objects are still terminating:"; printf '  %s\n' $left
  else
    info "reset: clean — no brewlet node labels or cluster objects remain"
  fi
}

# Run directly when invoked as a script (not sourced).
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  e2e_reset
fi
