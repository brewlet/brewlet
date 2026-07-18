# Brewlet integration tests

Cross-repository end-to-end harness for the Brewlet project. This repository owns
only test orchestration and fixture applications; production code, Kubernetes
manifests, Helm charts, specifications, and general documentation remain in their
component repositories:

- [brewlet/brewlet](https://github.com/brewlet/brewlet) - CLI, artifact runtime,
  containerd shim, and node provisioner
- [brewlet/kubernetes](https://github.com/brewlet/kubernetes) - operator,
  admission webhooks, manifests, and Helm chart
- [brewlet/specs](https://github.com/brewlet/specs) - specifications and proposals
- [brewlet/site](https://github.com/brewlet/site) - project documentation and site

## Layout

```text
e2e/       runner, reset helper, shared library, and tiers 1-14
fixtures/  repository-owned Java demo applications and PetClinic build fixture
.github/   cross-repository GitHub Actions matrix
```

## Checkouts

The harness never clones or modifies component repositories. Point it at existing
checkouts:

```bash
export BREWLET_CORE_DIR=/path/to/brewlet
export BREWLET_KUBERNETES_DIR=/path/to/kubernetes
./e2e/run.sh --tier 1 --tier 2
```

If the variables are unset, `e2e/run.sh` looks for sibling directories named
`brewlet` and `kubernetes`. Missing or invalid checkouts produce an actionable
error. `CORE_REF` and `KUBERNETES_REF` default to `main`; locally they document
which externally checked-out revisions are under test, while workflow dispatch
uses them to select checkout refs.

## Running

```bash
./e2e/run.sh                 # all 14 tiers
./e2e/run.sh --list          # tier catalog
./e2e/run.sh --tier 4        # one tier
./e2e/run.sh --reset         # remove Brewlet test state from the active cluster
./e2e/run.sh --reset --tier 10
E2E_WORK=/tmp/brewlet-e2e ./e2e/run.sh --tier 9
```

Tiers skip when an optional host prerequisite is unavailable and fail only when
an exercised capability fails. The suite covers:

| Tier | Scope | Primary repositories |
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

See [AGENTS.md](AGENTS.md) for cluster requirements, cleanup, and troubleshooting.
