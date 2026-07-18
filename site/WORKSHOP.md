# Brewlet workshop

This workshop exercises Brewlet as the multi-repository project it is today.
Production components, specifications, documentation, and test fixtures are
independently versioned; the integration harness selects component checkouts and
tests them together.

## Repository map

| Repository | Responsibility |
|---|---|
| [`brewlet/brewlet`](https://github.com/brewlet/brewlet) | CLI, OCI artifacts, containerd shim, node provisioner |
| [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes) | Operator, admission, APIs, manifests, Helm chart |
| [`brewlet/maven-plugin`](https://github.com/brewlet/maven-plugin) | Maven publishing integration |
| [`brewlet/specs`](https://github.com/brewlet/specs) | Architecture contracts and proposals |
| [`brewlet/integration-tests`](https://github.com/brewlet/integration-tests) | End-to-end orchestration and fixture applications |
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

## 1. Prepare separate checkouts

```bash
mkdir brewlet-workspace
cd brewlet-workspace

git clone https://github.com/brewlet/brewlet.git
git clone https://github.com/brewlet/kubernetes.git
git clone https://github.com/brewlet/integration-tests.git
git clone https://github.com/brewlet/maven-plugin.git
```

The sibling layout is a convenience, not a monorepo requirement. For checkouts
stored elsewhere:

```bash
export BREWLET_CORE_DIR=/path/to/brewlet
export BREWLET_KUBERNETES_DIR=/path/to/kubernetes
```

The harness does not switch branches. Check out the desired revisions in each
component repository before continuing.

## 2. Prove the local developer flow

```bash
./integration-tests/e2e/run.sh --tier 2
```

Tier 2 builds the CLI and shim from `brewlet/brewlet`, builds fixture applications
from `brewlet/integration-tests`, and proves:

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
launch metadata, not an OS or JDK. The local run uses the independently built core
CLI with a fixture owned by the test repository.

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

Tier 4 builds the operator from `brewlet/kubernetes`, installs the
`JavaApplication` CRD, and verifies that a descriptor reconciles into a Deployment,
Service, and HPA. It intentionally tests the control plane without requiring the
node provisioner to mutate the host.

Inspect the Kubernetes repository directly to see the owned APIs and manifests:

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
- the CLI and shim come from the selected core checkout;
- the `JavaApplication` API and operator come from the selected Kubernetes checkout;
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

For Maven publishing, build the independently versioned plugin and use its goals
from an application project:

```bash
(cd maven-plugin && mvn install)
mvn package sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT:push \
  -Dbrewlet.image=registry.example.com/team/app:1.0.0
```

## 7. Validate repository-local changes

Each repository has its own checks:

```bash
(cd brewlet && make check)
(cd kubernetes && make ci)
(cd maven-plugin && mvn verify)
```

Repository-local checks do not replace cross-repository validation. Point the
integration harness at the exact branches under test before opening linked pull
requests.

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
| Core checkout not found | Set `BREWLET_CORE_DIR` to a checkout containing `go.mod` and `cmd/brewlet`. |
| Kubernetes checkout not found | Set `BREWLET_KUBERNETES_DIR` to a checkout containing `charts/brewlet/Chart.yaml`. |
| Java tier skips | Point `JAVA_HOME` at a full JDK 21+ and add its `bin` directory to `PATH`. |
| runc tier skips | Start Docker; the tier requires a privileged Linux container. |
| Kubernetes tier fails after a prior run | Re-run with `--reset` before the selected tier. |
| Details are hidden in summary output | Read the logs in the printed work directory or set `E2E_WORK` explicitly. |

See the [integration-test runbook](https://github.com/brewlet/integration-tests/blob/main/AGENTS.md)
for the complete prerequisite and environment matrix.
