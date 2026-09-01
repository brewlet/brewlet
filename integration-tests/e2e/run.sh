#!/usr/bin/env bash
# Brewlet end-to-end test suite — orchestrator.
#
# Runs every implemented capability against the local toolchain and your
# Kubernetes cluster (Docker Desktop). Tiers gracefully SKIP when a prerequisite
# is missing, so the same command works on a laptop or in CI.
#
#   ./run.sh                 # run all tiers
#   ./run.sh --tier 2        # run one tier (repeatable: --tier 1 --tier 4)
#   ./run.sh --list          # list tiers
#   ./run.sh --reset         # scrub leftover brewlet state from the cluster, then exit
#   ./run.sh --reset --tier 10   # scrub first, then run tier 10 (recommended for re-runs)
#   JAVA_HOME=... ./run.sh   # pin the JDK used for the local/runc tiers
#
# Exit code is non-zero if any test FAILED (skips do not fail the suite).
#
# Automated agents: read AGENTS.md for a copy-paste runbook, the per-environment
# pass/skip matrix, and troubleshooting for the known Docker-Desktop-vs-kind
# gotchas (control-plane taints, `ctr images unpack`, stale cluster state).
set -uo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$E2E_DIR/.." && pwd)"
MONOREPO_DIR="$(cd "$REPO_DIR/.." && pwd)"
FIXTURES_DIR="$REPO_DIR/fixtures"
WORK="${E2E_WORK:-$(mktemp -d -t brewlet-e2e-XXXX)}"
mkdir -p "$WORK"

for arg in "$@"; do
  case "$arg" in
    --list)
      printf '1 unit  2 cli  3 runc  4 k8s  5 webhook(host)  6 webhook(in-cluster)  7 petclinic  8 appcds(in-cluster)  9 serving(in-cluster)  10 helm(in-cluster)  11 webhook-resilience  12 runnable-image(in-cluster)  13 nodeprofile  14 custom-jdk(in-cluster)  15 metrics(in-cluster)\n'
      exit 0
      ;;
  esac
done

resolve_component_dir() {
  local env_name="$1" default_dir="$2" sibling="$3" marker="$4" value="${!1:-}" candidate
  if [[ -n "$value" ]]; then
    candidate="$value"
  else
    for candidate in "$default_dir" "$REPO_DIR/../$sibling" "$REPO_DIR/../../$sibling"; do
      [[ -e "$candidate/$marker" ]] && break
      candidate=""
    done
  fi
  if [[ -z "$candidate" || ! -e "$candidate/$marker" ]]; then
    printf 'ERROR: %s is not set and no %s checkout was found beside %s.\n' \
      "$env_name" "$sibling" "$REPO_DIR" >&2
    printf 'Set %s to a checkout containing %s.\n' "$env_name" "$marker" >&2
    return 1
  fi
  (cd "$candidate" && pwd)
}

BREWLET_CORE_DIR="$(resolve_component_dir BREWLET_CORE_DIR "$MONOREPO_DIR/core" brewlet go.mod)" || exit 2
BREWLET_KUBERNETES_DIR="$(resolve_component_dir BREWLET_KUBERNETES_DIR "$MONOREPO_DIR/kubernetes" kubernetes charts/brewlet/Chart.yaml)" || exit 2

for required in cmd/brewlet shim/cmd/containerd-shim-brewlet-v2; do
  [[ -d "$BREWLET_CORE_DIR/$required" ]] || {
    printf 'ERROR: BREWLET_CORE_DIR=%s is not a Brewlet core checkout (missing %s).\n' \
      "$BREWLET_CORE_DIR" "$required" >&2
    exit 2
  }
done
for required in cmd/manager cmd/admission deploy charts/brewlet Dockerfile; do
  [[ -e "$BREWLET_KUBERNETES_DIR/$required" ]] || {
    printf 'ERROR: BREWLET_KUBERNETES_DIR=%s is not a Brewlet Kubernetes checkout (missing %s).\n' \
      "$BREWLET_KUBERNETES_DIR" "$required" >&2
    exit 2
  }
done

export REPO_DIR MONOREPO_DIR FIXTURES_DIR BREWLET_CORE_DIR BREWLET_KUBERNETES_DIR WORK

# shellcheck source=lib.sh
source "$E2E_DIR/lib.sh"
source "$E2E_DIR/reset.sh"
source "$E2E_DIR/tier1-unit.sh"
source "$E2E_DIR/tier2-cli.sh"
source "$E2E_DIR/tier3-runc.sh"
source "$E2E_DIR/tier4-k8s.sh"
source "$E2E_DIR/webhook-cases.sh"
source "$E2E_DIR/tier5-webhook.sh"
source "$E2E_DIR/tier6-webhook-incluster.sh"
source "$E2E_DIR/tier7-petclinic.sh"
source "$E2E_DIR/tier8-appcds-incluster.sh"
source "$E2E_DIR/tier9-japp-serving.sh"
source "$E2E_DIR/tier10-helm-incluster.sh"
source "$E2E_DIR/tier11-webhook-resilience.sh"
source "$E2E_DIR/tier12-runnable-image.sh"
source "$E2E_DIR/tier13-nodeprofile.sh"
source "$E2E_DIR/tier14-custom-jdk.sh"
source "$E2E_DIR/tier15-metrics-incluster.sh"

declare -a TIERS=()
DO_RESET=0
usage() { grep -E '^#( |$)' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tier) TIERS+=("$2"); shift 2 ;;
    --reset) DO_RESET=1; shift ;;
    --list) printf '1 unit  2 cli  3 runc  4 k8s  5 webhook(host)  6 webhook(in-cluster)  7 petclinic  8 appcds(in-cluster)  9 serving(in-cluster)  10 helm(in-cluster)  11 webhook-resilience  12 runnable-image(in-cluster)  13 nodeprofile  14 custom-jdk(in-cluster)  15 metrics(in-cluster)\n'; exit 0 ;;
    -h|--help) usage; exit 0 ;;
    *) warn "unknown arg: $1"; usage; exit 2 ;;
  esac
done

# --reset with no tiers = scrub-and-exit; --reset with tiers = scrub-then-run.
if [[ "$DO_RESET" -eq 1 ]]; then
  section "Brewlet E2E — reset"
  e2e_reset
  [[ ${#TIERS[@]} -eq 0 ]] && exit 0
fi

[[ ${#TIERS[@]} -eq 0 ]] && TIERS=(1 2 3 4 5 6 7 8 9 10 11 12 13 14 15)

section "Brewlet E2E — environment"
info "harness   : $REPO_DIR"
info "core      : $BREWLET_CORE_DIR"
info "kubernetes: $BREWLET_KUBERNETES_DIR"
info "fixtures  : $FIXTURES_DIR"
info "work dir  : $WORK (logs & artifacts)"
info "go        : $(have go && go version | awk '{print $3}' || echo 'absent')"
info "java      : $(have java && java -version 2>&1 | head -1 | tr -d '"' || echo 'absent')"
info "docker    : $(have docker && (docker info >/dev/null 2>&1 && echo up || echo 'installed, daemon down') || echo 'absent')"
info "kubectl   : $(have kubectl && (k8s_reachable && kubectl config current-context 2>/dev/null || echo 'no cluster') || echo 'absent')"
info "helm      : $(have helm && helm version --short 2>/dev/null || echo 'absent')"

# Cluster topology profile + preflight. The K8s tiers (>=4) are sensitive to
# leftover state and to node topology, so profile the cluster and auto-scrub
# brewlet.sh/* node labels before running any of them — this is the #1 cause of
# confusing false failures on a re-run (a prior run's advertised JDKs make the
# webhook admit a pod a fresh run expects it to deny, etc.).
_runs_k8s=0
for t in "${TIERS[@]}"; do [[ "$t" =~ ^[0-9]+$ && "$t" -ge 4 ]] && _runs_k8s=1; done
if [[ "$_runs_k8s" -eq 1 ]] && have kubectl && k8s_reachable; then
  profile="$(cluster_profile)"
  nodecount="$(kubectl get nodes -o name 2>/dev/null | wc -l | tr -d ' ')"
  schedulable="$(pick_provisionable_node 2>/dev/null || true)"
  info "cluster   : profile=$profile nodes=$nodecount provisionable-schedulable-node=${schedulable:-none}"
  [[ "$profile" == "docker-desktop" ]] && \
    info "cluster   : Docker Desktop — control-plane is tainted; provision-a-node tiers target a worker (see AGENTS.md)"
  # Auto-scrub node labels every run (safe + idempotent). Warn about heavier
  # cluster-scoped leftovers and point at --reset rather than deleting silently.
  if [[ "$DO_RESET" -ne 1 ]]; then
    scrub_node_labels
    left="$(detect_leftovers)"
    if [[ -n "$left" ]]; then
      warn "preflight: leftover brewlet cluster objects detected — run './run.sh --reset' if K8s tiers fail:"
      printf '  %s\n' $left
    fi
  fi
fi

for t in "${TIERS[@]}"; do
  case "$t" in
    1) tier1_unit ;;
    2) tier2_cli ;;
    3) tier3_runc ;;
    4) tier4_k8s ;;
    5) tier5_webhook ;;
    6) tier6_webhook_incluster ;;
    7) tier7_petclinic ;;
    8) tier8_appcds_incluster ;;
    9) tier9_serving ;;
    10) tier10_helm_incluster ;;
    11) tier11_webhook_resilience ;;
    12) tier12_runnable_image ;;
    13) tier13_nodeprofile ;;
    14) tier14_custom_jdk ;;
    15) tier15_metrics_incluster ;;
    *) warn "no such tier: $t" ;;
  esac
done

print_summary
[[ "$E2E_FAIL" -eq 0 ]]
