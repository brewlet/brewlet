# Brewlet integration tests

Cross-component end-to-end harness for the Brewlet monorepo. This directory owns
test orchestration and fixture applications. Production code, Kubernetes
manifests, and specifications remain in their owning monorepo directories.

## Layout

```text
e2e/       runner, reset helper, shared library, and tiers 1-15
fixtures/  harness-owned Java demo applications and PetClinic build fixture
```

## Running from the monorepo

From the repository root, run:

```bash
integration-tests/e2e/run.sh --tier 1 --tier 2
```

The harness defaults to `core/` and `kubernetes/` in the monorepo. The
`BREWLET_CORE_DIR` and `BREWLET_KUBERNETES_DIR` overrides remain available for
testing external checkouts.

## Running

```bash
integration-tests/e2e/run.sh                 # all 15 tiers
integration-tests/e2e/run.sh --list          # tier catalog
integration-tests/e2e/run.sh --tier 4        # one tier
integration-tests/e2e/run.sh --reset         # remove Brewlet test state
integration-tests/e2e/run.sh --reset --tier 10
E2E_WORK=/tmp/brewlet-e2e integration-tests/e2e/run.sh --tier 9
```

Tiers skip when an optional host prerequisite is unavailable and fail only when
an exercised capability fails. The suite covers:

| Tier | Scope | Primary components |
|---:|---|---|
| 1 | Go unit/component suites | core + Kubernetes |
| 2 | CLI push, inspect, run, bundle, classpath, and JPMS | core + fixtures |
| 3 | shim to runc under Linux cgroups | core + fixtures |
| 4 | operator control plane and Helm packaging | Kubernetes |
| 5-6 | host and in-cluster admission webhooks | Kubernetes |
| 7 | Spring PetClinic artifact, layered deployment, runc, reconcile | both + fixtures |
| 8-9 | AppCDS and serving through kubelet/CRI | core + fixtures |
| 10-11 | installed Helm stack and webhook resilience | Kubernetes |
| 12 | runnable image pulled and unpacked by kubelet | core + fixtures |
| 13 | NodeProfile lifecycle | Kubernetes |
| 14 | custom JDK NodeProfile through a live Azul Zulu workload | both + fixtures |
| 15 | live opt-in Prometheus metrics through Helm, provisioner, shim, and exporter | both + fixtures |

See [AGENTS.md](AGENTS.md) for cluster requirements, cleanup, and troubleshooting.
