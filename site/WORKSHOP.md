# Brewlet workshops

Brewlet has two distinct user journeys. Choose the workshop for your role, or
complete both to exercise the platform handoff end to end.

| Workshop | Audience | Outcome |
|---|---|---|
| [Part 1: Enable a cluster](WORKSHOP-OPS.md) | Kubernetes platform and operations teams | A cluster with ready Brewlet nodes, the `brewlet` RuntimeClass, an approved JDK inventory, and a documented developer handoff |
| [Part 2: Build and deploy a workload](WORKSHOP-DEV.md) | Java application developers | A JAR published as a runnable OCI image and running on the Brewlet-enabled cluster |

## Recommended format

For a shared session, pair one Ops participant with one Dev participant:

1. Ops completes Part 1 and gives the Dev participant the handoff values.
2. Dev completes Part 2 without needing cluster-admin access.
3. Both participants inspect the running workload and its selected JDK.

Use a disposable cluster for the workshop. Brewlet node provisioning is
privileged and changes the node's containerd configuration.

## Repository

Both parts use the Brewlet monorepo:

```bash
git clone https://github.com/brewlet/brewlet.git
cd brewlet
```

The repository contains the runtime, Kubernetes components, Maven plugin,
examples, and integration tests at one revision.

## Maintainer verification

The numbered E2E tiers are the project qualification suite, not the primary
student path. Maintainers can run the coordinated host-side checks with:

```bash
make check-all
```

Cluster-backed capabilities remain available through
`integration-tests/e2e/run.sh`; see the
[integration-test runbook](https://github.com/brewlet/brewlet/blob/main/integration-tests/AGENTS.md).
