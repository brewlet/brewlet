#!/usr/bin/env bash
# Shared admission-webhook assertions, run against whatever webhook is currently
# intercepting pods in a labelled test namespace. Tier 5 wires the webhook up on
# the host (reached via host.docker.internal); Tier 6 runs the very same webhook
# binary *in-cluster* as a Deployment. Both call webhook_cases so the behaviour
# under test is identical — only the transport differs.
#
# The caller owns webhook setup/registration and cleanup. webhook_cases records
# the node it advertised JDKs on in WH_NODE so the caller can restore it.

WH_NODE=""

# _wh_apply NS  (YAML on stdin) -> echoes kubectl apply output (stderr merged).
_wh_apply() { kubectl apply -n "$1" -f - 2>&1; }

# _wh_unreachable OUT -> true when OUT is the API server failing to reach the
# webhook (as opposed to a real admission decision). Callers SKIP in that case.
_wh_unreachable() {
  local o="$1"
  [[ "$o" == *"failed calling webhook"* || "$o" == *"connection refused"* \
     || "$o" == *"context deadline"* || "$o" == *"no route to host"* \
     || "$o" == *"no such host"* || "$o" == *"lookup "* \
     || "$o" == *"name resolution"* || "$o" == *"EOF"* \
     || "$o" == *"tls:"* || "$o" == *"x509"* ]]
}

# _wh_affinity_keys NS NAME -> space-separated nodeAffinity matchExpression keys.
_wh_affinity_keys() {
  kubectl get pod "$2" -n "$1" \
    -o jsonpath='{.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[*].matchExpressions[*].key}' 2>/dev/null
}

# _wh_jdk_pod NS NAME JDK [retry] -> a brewlet pod requesting a specific JDK.
# With retry set, a transient NoCompatibleJDK (webhook node cache not yet synced
# after we annotated the node) is retried a few times.
_wh_jdk_pod() {
  local ns="$1" name="$2" jdk="$3" retry="${4:-}" out
  for _ in 1 2 3 4 5; do
    out="$(_wh_apply "$ns" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $name
  annotations:
    brewlet.sh/jdk: "$jdk"
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/orders:1.0.0
YAML
)"
    if [[ -n "$retry" && "$out" == *NoCompatibleJDK* ]]; then sleep 1; continue; fi
    break
  done
  printf '%s' "$out"
}

# _wh_arch_pod NS NAME ARCH [retry] -> a brewlet pod requesting a specific arch
# constraint (as a non-portable/JNI JAR would). With retry set, a transient
# NoCompatibleArch (node cache not yet synced) is retried a few times.
_wh_arch_pod() {
  local ns="$1" name="$2" arch="$3" retry="${4:-}" out
  for _ in 1 2 3 4 5; do
    out="$(_wh_apply "$ns" <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $name
  annotations:
    brewlet.sh/arch: "$arch"
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/orders:1.0.0
YAML
)"
    if [[ -n "$retry" && "$out" == *NoCompatibleArch* ]]; then sleep 1; continue; fi
    break
  done
  printf '%s' "$out"
}

# webhook_cases NS LABEL -> run every admission assertion against the webhook
# intercepting namespace NS. LABEL prefixes each check name so Tier 5 (host) and
# Tier 6 (in-cluster) results are distinguishable. Returns 2 (and asserts
# nothing) when the webhook is unreachable, so the caller can SKIP the tier.
webhook_cases() {
  local ns="$1" label="$2" out ref dig aff

  # --- positive: artifact-ref/digest stamped (also the reachability probe) ---
  out="$(_wh_apply "$ns" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: wh-stamp-probe
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/orders@sha256:0000000000000000000000000000000000000000000000000000000000000000
YAML
)"
  if _wh_unreachable "$out"; then return 2; fi
  if [[ "$out" == *created* || "$out" == *configured* ]]; then
    ref="$(kubectl get pod wh-stamp-probe -n "$ns" -o jsonpath='{.metadata.annotations.brewlet\.sh/artifact-ref}' 2>/dev/null)"
    dig="$(kubectl get pod wh-stamp-probe -n "$ns" -o jsonpath='{.metadata.annotations.brewlet\.sh/artifact-digest}' 2>/dev/null)"
    assert_contains "$label: stamped brewlet.sh/artifact-ref on the pod" "$ref" "registry.example.com/team/orders@sha256:"
    assert_contains "$label: stamped brewlet.sh/artifact-digest (digest-pinned ref)" "$dig" "sha256:"
    kubectl delete pod wh-stamp-probe -n "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  else
    fail "$label: stamps artifact annotations on a brewlet pod" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- negative: unsatisfiable launcher is denied --------------------------
  out="$(_wh_apply "$ns" <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: wh-deny-probe
  annotations:
    brewlet.sh/launcher: "nonexistent-launcher"
spec:
  runtimeClassName: brewlet
  containers:
    - name: app
      image: registry.example.com/team/orders:1.0.0
YAML
)"
  if [[ "$out" == *NoCompatibleLauncher* ]]; then
    pass "$label: denies a pod requesting a launcher no node provides (NoCompatibleLauncher)"
  else
    kubectl delete pod wh-deny-probe -n "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fail "$label: NoCompatibleLauncher denial" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- two JDKs installed on a SINGLE node ---------------------------------
  # Mark one node ready and advertise two JDK roots on it (as the provisioner
  # would with JDKS=temurin-21,microsoft-25). That one node must then satisfy
  # pods requesting either JDK, and reject a JDK it does not have.
  WH_NODE="$(kubectl get nodes -o name 2>/dev/null | head -1)"
  if [[ -z "$WH_NODE" ]]; then
    skip "$label: multi-JDK on a single node" "no nodes found"
    return 0
  fi
  info "$label: advertising temurin-21 + microsoft-25 on $WH_NODE"
  kubectl label   --overwrite "$WH_NODE" brewlet.sh/runtime=ready >/dev/null 2>&1
  kubectl annotate --overwrite "$WH_NODE" brewlet.sh/jdks=temurin-21,microsoft-25 >/dev/null 2>&1
  sleep 2  # let the webhook's node cache observe the annotation

  out="$(_wh_jdk_pod "$ns" wh-jdk-temurin temurin-21 retry)"
  if [[ "$out" == *created* ]]; then
    aff="$(_wh_affinity_keys "$ns" wh-jdk-temurin)"
    assert_contains "$label: node satisfies a temurin-21 request (steered by jdk label)" \
      "$aff" "brewlet.sh/jdk.temurin-21"
  else
    fail "$label: temurin-21 request admitted" "$(printf '%s' "$out" | tail -1)"
  fi

  out="$(_wh_jdk_pod "$ns" wh-jdk-microsoft microsoft-25 retry)"
  if [[ "$out" == *created* ]]; then
    aff="$(_wh_affinity_keys "$ns" wh-jdk-microsoft)"
    assert_contains "$label: SAME node also satisfies a microsoft-25 request" \
      "$aff" "brewlet.sh/jdk.microsoft-25"
  else
    fail "$label: microsoft-25 request admitted" "$(printf '%s' "$out" | tail -1)"
  fi

  out="$(_wh_jdk_pod "$ns" wh-jdk-feature 21 retry)"
  if [[ "$out" == *created* ]]; then
    aff="$(_wh_affinity_keys "$ns" wh-jdk-feature)"
    assert_contains "$label: bare feature '21' matches either distribution (feature label)" \
      "$aff" "brewlet.sh/jdk-feature.21"
  else
    fail "$label: bare feature '21' request admitted" "$(printf '%s' "$out" | tail -1)"
  fi

  out="$(_wh_jdk_pod "$ns" wh-jdk-absent temurin-17)"
  if [[ "$out" == *NoCompatibleJDK* ]]; then
    pass "$label: a JDK the node lacks (temurin-17) is denied (NoCompatibleJDK)"
  else
    kubectl delete pod wh-jdk-absent -n "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fail "$label: NoCompatibleJDK for an absent JDK" "$(printf '%s' "$out" | tail -1)"
  fi

  # --- arch constraint for non-portable (JNI) JARs -------------------------
  # arch is the standard kubelet-provided kubernetes.io/arch label (no
  # provisioner setup needed). A pod requesting the ready node's own arch is
  # steered onto it; a pod requesting the OTHER (absent) arch is denied.
  local node_arch other_arch
  node_arch="$(kubectl get "$WH_NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/arch}' 2>/dev/null)"
  if [[ -z "$node_arch" ]]; then
    skip "$label: arch constraint" "node has no kubernetes.io/arch label"
  else
    if [[ "$node_arch" == "amd64" ]]; then other_arch="arm64"; else other_arch="amd64"; fi

    out="$(_wh_arch_pod "$ns" wh-arch-match "$node_arch" retry)"
    if [[ "$out" == *created* ]]; then
      aff="$(_wh_affinity_keys "$ns" wh-arch-match)"
      assert_contains "$label: non-portable JAR requesting the node's arch ($node_arch) is steered (kubernetes.io/arch affinity)" \
        "$aff" "kubernetes.io/arch"
    else
      fail "$label: arch=$node_arch request admitted" "$(printf '%s' "$out" | tail -1)"
    fi

    out="$(_wh_arch_pod "$ns" wh-arch-absent "$other_arch")"
    if [[ "$out" == *NoCompatibleArch* ]]; then
      pass "$label: an arch no ready node provides ($other_arch) is denied (NoCompatibleArch)"
    else
      kubectl delete pod wh-arch-absent -n "$ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
      fail "$label: NoCompatibleArch for an absent arch" "$(printf '%s' "$out" | tail -1)"
    fi
  fi
  return 0
}
