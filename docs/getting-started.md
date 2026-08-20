# Getting started (local proof of concept)

Brewlet is developed in one monorepo. Runtime code lives at the root, the
Kubernetes control plane lives in `kubernetes/`, and `integration-tests/` owns
fixture applications and cross-component orchestration.

This guide uses the integration harness so every command matches the same flow CI
validates.

## Prerequisites

| Tool | Why | Required for |
|---|---|---|
| **JDK 21+** | Builds and runs the fixture applications | Local CLI and runc tiers |
| **Go 1.26+** | Builds the CLI and shim | Local CLI and runc tiers |
| **Docker** | Provides Linux, runc, and cgroups on any host | runc tier only |

Point `JAVA_HOME` at a JDK 21 or newer:

```bash
export JAVA_HOME="$HOME/.sdkman/candidates/java/current"   # or any JDK 21+
```

## Clone the repository

Clone the monorepo:

```bash
git clone https://github.com/brewlet/brewlet.git
cd brewlet
```

The harness uses the core at the repository root and `kubernetes/` by default.
To test external checkouts, set explicit paths:

```bash
export BREWLET_CORE_DIR=/path/to/brewlet
export BREWLET_KUBERNETES_DIR=/path/to/kubernetes
```

The harness never changes branches. When using external checkouts, select the
desired revisions before running it.

## Layer 1: ship and run only a JAR

From the repository root:

```bash
./integration-tests/e2e/run.sh --tier 2
```

Tier 2:

1. Builds `brewlet` and `containerd-shim-brewlet-v2` from the monorepo root.
2. Builds the dependency-free Java fixture from `integration-tests/fixtures/`.
3. Pushes only its JAR into a local OCI layout.
4. Inspects and runs the artifact with the JDK from `JAVA_HOME`.
5. Emits an OCI runtime bundle and validates its CPU and memory settings.
6. Repeats the flow for layered classpath and JPMS applications.

The printed work directory contains logs, OCI layouts, binaries, and generated
bundles. Set `E2E_WORK` to retain them at a known path:

```bash
E2E_WORK=/tmp/brewlet-e2e ./integration-tests/e2e/run.sh --tier 2
```

## Layer 2: run through shim, runc, and cgroups

```bash
./integration-tests/e2e/run.sh --tier 3
```

Tier 3 cross-compiles the shim for Linux and runs it in a privileged container.
The shim assembles the OCI bundle and `runc` launches Java as PID 1 under a
1-CPU, 384 MiB cgroup. The fixture reports the limits observed from inside the
JVM. The same tier verifies both `java -jar` and JPMS module-path launches.

## Validate the monorepo

Run all host-side checks from the repository root:

```bash
make check-all
```

Cluster-backed integration tiers are separate because they require Docker,
kubectl, and a reachable cluster.

## Next steps

- [Installation](installation.md) — install the Kubernetes components.
- [Building and publishing](building-and-publishing.md) — publish your own app.
- [Concepts and architecture](concepts.md) — understand component boundaries.
- [Integration-test runbook](https://github.com/brewlet/brewlet/tree/main/integration-tests#readme) —
  run individual Kubernetes and application tiers.
