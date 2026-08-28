#!/usr/bin/env bash
# Tier 1 — unit / component tests (no cluster, no JVM runtime needed).
# Covers: artifact OCI format, resource->JVM mapping, shim bundle-assembly core,
# admission mutate/capability logic, operator resource builders.
# Prereq: go

tier1_unit() {
  section "Tier 1 — unit / component (go test)"
  if ! have go; then
    skip "tier1: go toolchain" "go not installed"
    return 0
  fi

  ( cd "$BREWLET_CORE_DIR" && go test ./... ) >"$WORK/t1-core.log" 2>&1 \
    && pass "core module: go test ./... (artifact, runtime, shim)" \
    || fail "core module: go test ./..." "see $WORK/t1-core.log"

  ( cd "$BREWLET_KUBERNETES_DIR" && go test ./... ) >"$WORK/t1-kubernetes.log" 2>&1 \
    && pass "kubernetes module: go test ./... (admission, controllers)" \
    || fail "kubernetes module: go test ./..." "see $WORK/t1-kubernetes.log"
}
