#!/usr/bin/env bash
# Shared helpers for the Brewlet end-to-end test suite.
# Sourced by run.sh and every tier script.

# --- output --------------------------------------------------------------
if [[ -t 1 ]]; then
  C_RESET=$'\033[0m'; C_RED=$'\033[31m'; C_GRN=$'\033[32m'
  C_YEL=$'\033[33m'; C_BLU=$'\033[34m'; C_BOLD=$'\033[1m'
else
  C_RESET=""; C_RED=""; C_GRN=""; C_YEL=""; C_BLU=""; C_BOLD=""
fi

section() { printf '\n%s== %s ==%s\n' "$C_BOLD$C_BLU" "$*" "$C_RESET"; }
info()    { printf '%s--%s %s\n' "$C_BLU" "$C_RESET" "$*"; }
warn()    { printf '%s!!%s %s\n' "$C_YEL" "$C_RESET" "$*"; }

# --- result tracking -----------------------------------------------------
# Each result is "STATUS<TAB>NAME<TAB>DETAIL".
declare -a E2E_RESULTS=()
E2E_PASS=0; E2E_FAIL=0; E2E_SKIP=0

pass() { E2E_RESULTS+=("PASS"$'\t'"$1"$'\t'"${2:-}"); E2E_PASS=$((E2E_PASS+1));
         printf '  %sPASS%s %s\n' "$C_GRN" "$C_RESET" "$1"; }
fail() { E2E_RESULTS+=("FAIL"$'\t'"$1"$'\t'"${2:-}"); E2E_FAIL=$((E2E_FAIL+1));
         printf '  %sFAIL%s %s%s\n' "$C_RED" "$C_RESET" "$1" "${2:+ — $2}"; }
skip() { E2E_RESULTS+=("SKIP"$'\t'"$1"$'\t'"${2:-}"); E2E_SKIP=$((E2E_SKIP+1));
         printf '  %sSKIP%s %s%s\n' "$C_YEL" "$C_RESET" "$1" "${2:+ — $2}"; }

# check NAME: run a command; PASS on exit 0, FAIL otherwise (last stderr line kept).
# usage: check "name" command args...
check() {
  local name="$1"; shift
  local out
  if out="$("$@" 2>&1)"; then
    pass "$name"
    return 0
  else
    fail "$name" "$(printf '%s' "$out" | tail -1)"
    return 1
  fi
}

# assert_contains NAME HAYSTACK NEEDLE
assert_contains() {
  local name="$1" hay="$2" needle="$3"
  if [[ "$hay" == *"$needle"* ]]; then pass "$name"; return 0
  else fail "$name" "expected to find: $needle"; return 1; fi
}

# assert_not_contains NAME HAYSTACK NEEDLE
assert_not_contains() {
  local name="$1" hay="$2" needle="$3"
  if [[ "$hay" != *"$needle"* ]]; then pass "$name"; return 0
  else fail "$name" "expected NOT to find: $needle"; return 1; fi
}

# assert_file NAME PATH
assert_file() {
  local name="$1" path="$2"
  if [[ -f "$path" ]]; then pass "$name"; return 0
  else fail "$name" "missing file: $path"; return 1; fi
}

# assert_eq NAME GOT WANT
assert_eq() {
  local name="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then pass "$name"; return 0
  else fail "$name" "got '$got' want '$want'"; return 1; fi
}

# --- prereq detection ----------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

resolve_java_home() {
  if [[ -n "${JAVA_HOME:-}" && -x "$JAVA_HOME/bin/java" ]]; then
    printf '%s' "$JAVA_HOME"
  elif [[ "$(uname -s)" == "Darwin" && -x /usr/libexec/java_home ]]; then
    /usr/libexec/java_home
  else
    local java_path
    java_path="$(command -v java)"
    if have readlink && readlink -f "$java_path" >/dev/null 2>&1; then
      java_path="$(readlink -f "$java_path")"
    fi
    (cd "$(dirname "$java_path")/.." && pwd)
  fi
}

# k8s_reachable: true if kubectl can talk to a cluster.
k8s_reachable() { kubectl version >/dev/null 2>&1 || kubectl cluster-info >/dev/null 2>&1; }

# retry_curl URL [tries] [sleep]: fetch URL, retrying; echoes body on success.
retry_curl() {
  local url="$1" tries="${2:-40}" nap="${3:-0.5}" body
  for _ in $(seq 1 "$tries"); do
    if body="$(curl -sf "$url" 2>/dev/null)"; then printf '%s' "$body"; return 0; fi
    sleep "$nap"
  done
  return 1
}

# wait_for CMD... : retry a predicate command up to ~30s.
wait_for() {
  local tries=60
  while (( tries-- > 0 )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  return 1
}

# free_port: print an unused localhost TCP port.
free_port() {
  python3 - <<'PY' 2>/dev/null || echo 0
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
}

# --- cluster / node topology helpers -------------------------------------
# These make the K8s tiers robust across environments (single-node kind/CI vs a
# multi-node, tainted-control-plane Docker Desktop cluster) so environment
# differences produce clear SKIPs instead of confusing FAILs. See AGENTS.md.

# cluster_profile: print a one-word guess of the cluster flavour based on node
# names — "kind", "docker-desktop", "minikube", or "other".
cluster_profile() {
  local names
  names="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##' | tr '\n' ' ')"
  case "$names" in
    *desktop-*)                 echo "docker-desktop" ;;
    *kind-*|*-control-plane*)   echo "kind" ;;
    *minikube*)                 echo "minikube" ;;
    "")                         echo "none" ;;
    *)                          echo "other" ;;
  esac
}

# node_provisionable NODE: true if the node is a local containerd docker
# container we can `docker exec` into (kind nodes, Docker Desktop nodes) — the
# prerequisite for tiers that install the shim / JDK / brewlet runtime on it.
node_provisionable() {
  local n="$1"
  docker inspect "$n" >/dev/null 2>&1 && docker exec "$n" ctr --version >/dev/null 2>&1
}

# node_schedulable NODE: true if the node carries no NoSchedule/NoExecute taint.
# A brewlet pod ships no tolerations, so a tainted node (e.g. a Docker Desktop
# control-plane with node-role.kubernetes.io/control-plane:NoSchedule) can never
# host it — the pod would sit Pending / FailedScheduling.
node_schedulable() {
  local n="$1" effects
  effects="$(kubectl get node "$n" \
    -o jsonpath='{range .spec.taints[*]}{.effect}{"\n"}{end}' 2>/dev/null)"
  ! grep -qE 'NoSchedule|NoExecute' <<<"$effects"
}

# label_node NODE ARGS... / annotate_node NODE ARGS...: use kubectl's explicit
# TYPE NAME form, not a bare node name or resource/name shorthand. This is
# accepted across kubectl versions and avoids silently failing to advertise a
# provisioned node.
label_node() {
  local n="${1#node/}"; shift
  kubectl label node "$n" "$@"
}

annotate_node() {
  local n="${1#node/}"; shift
  kubectl annotate node "$n" "$@"
}

# pick_provisionable_node: print the best node to provision + run a brewlet pod
# on. Prefers a node that is BOTH provisionable AND schedulable; if none is
# schedulable, falls back to the first provisionable node (the caller can then
# check node_schedulable and SKIP with a clear reason). Empty output + non-zero
# exit means no provisionable node exists at all.
pick_provisionable_node() {
  local nodes n first_prov=""
  nodes="$(kubectl get nodes -o name 2>/dev/null | sed 's#node/##')"
  for n in $nodes; do
    if node_provisionable "$n"; then
      [[ -z "$first_prov" ]] && first_prov="$n"
      if node_schedulable "$n"; then printf '%s' "$n"; return 0; fi
    fi
  done
  [[ -n "$first_prov" ]] && { printf '%s' "$first_prov"; return 0; }
  return 1
}

# ctr_supports_unpack NODE: true if the node's `ctr` exposes `images unpack`.
# Some trimmed containerd CLIs (e.g. the one shipped in Docker Desktop nodes)
# omit the subcommand tier 12 relies on.
ctr_supports_unpack() {
  local n="$1"
  docker exec "$n" ctr images unpack --help >/dev/null 2>&1
}

# save_pod_diag NAME NS [SELECTOR]: dump pods, recent events, and pod logs for a
# namespace (optionally narrowed by label selector) into $WORK/diag-NAME.log so
# failures are debuggable AFTER a tier's cleanup trap has torn the objects down.
# Prints the path so it can be threaded into a fail() detail message.
save_pod_diag() {
  local name="$1" ns="$2" selector="${3:-}" out="$WORK/diag-$1.log"
  {
    echo "=== diag: $name (ns=$ns${selector:+ selector=$selector}) @ $(date -u +%FT%TZ) ==="
    echo "--- pods ---"
    kubectl get pods -n "$ns" ${selector:+-l "$selector"} -o wide 2>&1
    echo "--- events (last 20) ---"
    kubectl get events -n "$ns" --sort-by=.lastTimestamp 2>&1 | tail -20
    echo "--- describe pods ---"
    kubectl describe pods -n "$ns" ${selector:+-l "$selector"} 2>&1 | tail -60
    echo "--- logs (previous + current) ---"
    kubectl logs -n "$ns" ${selector:+-l "$selector"} --tail=80 --all-containers 2>&1
  } >"$out" 2>&1
  tail -120 "$out" >&2 || true
  printf '%s' "$out"
}

# force_delete_nodeprofiles: delete every NodeProfile, force-removing the
# node.brewlet.sh/cleanup finalizer first. A NodeProfile holds that finalizer
# until its cleanup DaemonSet reports Ready on every assigned node (§5.6); with
# the e2e suite's deliberately-bogus provisioner image the cleanup pods can
# never become Ready, so a plain `kubectl delete` would hang the object in
# Terminating forever and wedge `helm uninstall` / CRD + namespace deletion.
# Stripping the finalizer is a TEST-TEARDOWN concern only — the product behaviour
# (hold until cleanup is verified) is intentional. No-op if the CRD is absent.
force_delete_nodeprofiles() {
  kubectl get crd nodeprofiles.node.brewlet.sh >/dev/null 2>&1 || return 0
  local np
  for np in $(kubectl get nodeprofiles.node.brewlet.sh -o name 2>/dev/null); do
    kubectl patch "$np" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
  done
  kubectl delete nodeprofiles.node.brewlet.sh --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# --- summary -------------------------------------------------------------
print_summary() {
  section "Summary"
  local status name detail
  if (( E2E_PASS + E2E_FAIL + E2E_SKIP > 0 )); then
    for row in "${E2E_RESULTS[@]}"; do
      IFS=$'\t' read -r status name detail <<<"$row"
      case "$status" in
        PASS) printf '  %sPASS%s  %s\n' "$C_GRN" "$C_RESET" "$name" ;;
        FAIL) printf '  %sFAIL%s  %s%s\n' "$C_RED" "$C_RESET" "$name" "${detail:+ — $detail}" ;;
        SKIP) printf '  %sSKIP%s  %s%s\n' "$C_YEL" "$C_RESET" "$name" "${detail:+ — $detail}" ;;
      esac
    done
  fi
  printf '\n  %stotal: %d passed, %d failed, %d skipped%s\n' \
    "$C_BOLD" "$E2E_PASS" "$E2E_FAIL" "$E2E_SKIP" "$C_RESET"
}
