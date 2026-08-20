# Brewlet workshop

This workshop exercises the Brewlet monorepo. Production components,
specifications, and test fixtures share one revision, while the website remains
separately published from `brewlet/site`.

## Project map

| Location | Responsibility |
|---|---|
| [`brewlet/brewlet`](https://github.com/brewlet/brewlet) | CLI, OCI artifacts, containerd shim, node provisioner |
| [`kubernetes/`](https://github.com/brewlet/brewlet/tree/main/kubernetes) | Operator, admission, APIs, manifests, Helm chart |
| [`maven-plugin/`](https://github.com/brewlet/brewlet/tree/main/maven-plugin) | Maven publishing integration |
| [`specs/`](https://github.com/brewlet/brewlet/tree/main/specs) | Architecture contracts and proposals |
| [`integration-tests/`](https://github.com/brewlet/brewlet/tree/main/integration-tests) | End-to-end orchestration and fixture applications |
| [`brewlet/site`](https://github.com/brewlet/site) | Website, user documentation, and this workshop |

## Prerequisites

| Tool | Minimum use |
|---|---|
| JDK 21+ | Fixture builds and local JVM runs |
| Go 1.26+ | Core and Kubernetes binaries |
| Docker | Linux/runc and in-cluster tiers |
| kubectl and a reachable cluster | Kubernetes tiers |
| Helm | Helm installation tier |

On macOS with SDKMAN:

```bash
export JAVA_HOME="$HOME/.sdkman/candidates/java/current"
export PATH="$JAVA_HOME/bin:$PATH"
```

## 1. Clone the monorepo

```bash
git clone https://github.com/brewlet/brewlet.git
cd brewlet
```

The integration harness uses the core at the repository root and the Kubernetes
component in `kubernetes/`. To test external component checkouts instead:

```bash
export BREWLET_CORE_DIR=/path/to/brewlet
export BREWLET_KUBERNETES_DIR=/path/to/kubernetes
```

The harness does not switch branches. When using external component checkouts,
select the desired revisions before continuing.

## 2. Prove the local developer flow

```bash
./integration-tests/e2e/run.sh --tier 2
```

Tier 2 builds the CLI and shim from the monorepo root, builds fixture applications
from `integration-tests/`, and proves:

- JAR-only OCI push and inspection;
- launch with the node JDK from `JAVA_HOME`;
- OCI bundle generation with CPU and memory settings;
- layered classpath applications; and
- JPMS module-path applications.

Set a stable work directory when you want to inspect the generated OCI layouts,
binaries, bundles, and logs:

```bash
E2E_WORK=/tmp/brewlet-workshop ./integration-tests/e2e/run.sh --tier 2
find /tmp/brewlet-workshop -maxdepth 2 -type f
```

**What to observe:** the application artifact contains application payload and
launch metadata, not an OS or JDK. The local run uses the core CLI with a fixture
owned by the integration harness.

## 3. Exercise the real node mechanism

```bash
./integration-tests/e2e/run.sh --tier 3
```

Tier 3 cross-compiles the core shim for Linux, starts a privileged Linux container,
assembles an OCI runtime bundle, and delegates execution to `runc`. The JVM runs as
PID 1 under a 1-CPU, 384 MiB cgroup and reports those limits from inside the
sandbox. It verifies both fat-JAR and JPMS launches.

**What to observe:** the JDK is mounted from the simulated node runtime root, while
the application comes from the OCI layout produced by the harness.

## 4. Exercise the Kubernetes control plane

Use a disposable cluster or a cluster where you are allowed to create CRDs and
cluster-scoped resources:

```bash
./integration-tests/e2e/run.sh --reset --tier 4
```

Tier 4 builds the operator from `kubernetes/`, installs the
`JavaApplication` CRD, and verifies that a descriptor reconciles into a Deployment,
Service, and HPA. It intentionally tests the control plane without requiring the
node provisioner to mutate the host.

Inspect the Kubernetes component directly to see the owned APIs and manifests:

```bash
ls kubernetes/api kubernetes/cmd kubernetes/deploy kubernetes/charts/brewlet
```

## 5. Run a real Spring Boot application

```bash
./integration-tests/e2e/run.sh --reset --tier 7
```

Tier 7 owns the complete Spring PetClinic proof:

- the pinned upstream build and layering scripts are in
  `integration-tests/fixtures/spring-petclinic/`;
- the CLI and shim come from the monorepo root;
- the `JavaApplication` API and operator come from `kubernetes/`;
- the deployment descriptor is
  `kubernetes/deploy/petclinic-javaapplication.yaml`.

The tier pushes and inspects the real fat JAR, runs it through shim and runc when
Docker is available, reconciles it on Kubernetes when a cluster is reachable, and
verifies deterministic dependency-layer reuse.

## 6. Verify advanced capabilities

Run only the capability you want to inspect:

```bash
./integration-tests/e2e/run.sh --tier 8    # AppCDS in-cluster
./integration-tests/e2e/run.sh --tier 10   # Helm stack
./integration-tests/e2e/run.sh --tier 12   # kubelet-pullable runnable image
./integration-tests/e2e/run.sh --tier 13   # NodeProfile lifecycle
```

For a registry-free Maven smoke test, install the plugin from the monorepo and
use the included demo application to build a local OCI image layout:

```bash
mvn -f maven-plugin/pom.xml install
mvn -f integration-tests/fixtures/demo-app/pom.xml package \
  sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:config \
  sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:build \
  -Dbrewlet.image=demo/hello:maven-workshop

test -f integration-tests/fixtures/demo-app/target/brewlet/jvm-config.json
test -f integration-tests/fixtures/demo-app/target/brewlet/oci/index.json
```

The second command builds `target/app.jar`, infers its `Main-Class`, writes the
Brewlet launch config, and assembles a runnable multi-architecture OCI image
without contacting a registry. Use the plugin's `push` goal instead of `build`
when you have a real registry reference and credentials.

## 7. Validate changes

Run the coordinated host-side checks:

```bash
make check-all
```

Cluster-backed E2E tiers remain available individually when their prerequisites
are installed.

## Cleanup

```bash
./integration-tests/e2e/run.sh --reset
```

The reset removes Brewlet-owned test resources from the active cluster. Generated
local outputs live in the harness work directory; remove the specific directory
you selected with `E2E_WORK` when finished.

## Troubleshooting

| Symptom | Resolution |
|---|---|
| Core directory not found | Use a complete monorepo checkout, or set `BREWLET_CORE_DIR` to an external checkout containing `go.mod` and `cmd/brewlet`. |
| Kubernetes directory not found | Use the monorepo's `kubernetes/`, or set `BREWLET_KUBERNETES_DIR` to an external checkout containing `charts/brewlet/Chart.yaml`. |
| Java tier skips | Point `JAVA_HOME` at a full JDK 21+ and add its `bin` directory to `PATH`. |
| runc tier skips | Start Docker; the tier requires a privileged Linux container. |
| Kubernetes tier fails after a prior run | Re-run with `--reset` before the selected tier. |
| Details are hidden in summary output | Read the logs in the printed work directory or set `E2E_WORK` explicitly. |

See the [integration-test runbook](https://github.com/brewlet/brewlet/blob/main/integration-tests/AGENTS.md)
for the complete prerequisite and environment matrix.
