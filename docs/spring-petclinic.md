# Example: Spring PetClinic

Spring PetClinic proves the Brewlet model with a real, dependency-heavy Spring
Boot application rather than the dependency-free demo fixture.

The example spans repositories:

| Piece | Owner |
|---|---|
| Pinned upstream build and layering scripts | [`integration-tests/fixtures/spring-petclinic`](https://github.com/brewlet/integration-tests/tree/main/fixtures/spring-petclinic) |
| End-to-end orchestration | [`integration-tests/e2e/tier7-petclinic.sh`](https://github.com/brewlet/integration-tests/blob/main/e2e/tier7-petclinic.sh) |
| CLI and shim | [`brewlet/brewlet`](https://github.com/brewlet/brewlet) |
| `JavaApplication` descriptor and operator | [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes) |
| Architecture contract | [`brewlet/specs`](https://github.com/brewlet/specs) |

Nothing about PetClinic is special to Brewlet. Its ordinary Spring Boot JAR is
published as an OCI artifact and launched with `java -jar` on a node-resident JDK.

## Run the complete proof

Clone the core, Kubernetes, and integration-test repositories as siblings:

```bash
mkdir brewlet-workspace
cd brewlet-workspace
git clone https://github.com/brewlet/brewlet.git
git clone https://github.com/brewlet/kubernetes.git
git clone https://github.com/brewlet/integration-tests.git

export JAVA_HOME="$HOME/.sdkman/candidates/java/current"   # any full JDK 21+
./integration-tests/e2e/run.sh --reset --tier 7
```

For a non-sibling layout, set `BREWLET_CORE_DIR` and
`BREWLET_KUBERNETES_DIR` explicitly. The harness does not change component
branches.

Tier 7:

1. Builds the CLI from the selected core checkout.
2. Uses the integration-test fixture to clone a pinned Spring PetClinic revision
   and build its fat JAR.
3. Pushes and inspects only that JAR as an OCI artifact.
4. Runs it through the core shim and `runc` under real cgroup limits when Docker
   is available.
5. Builds the operator from the Kubernetes checkout and reconciles the PetClinic
   `JavaApplication` when a cluster is reachable.
6. Splits the application into deterministic classpath layers and verifies that
   a business-code rebuild reuses the dependency layer.

Unavailable optional prerequisites produce explicit skips. Build or runtime
failures in an exercised path fail the tier.

## Build and inspect the fixture manually

From the workspace directory:

```bash
export BREWLET_CORE_DIR="$PWD/brewlet"
export FIXTURE_DIR="$PWD/integration-tests/fixtures/spring-petclinic"
export WORK_DIR="$PWD/.brewlet-petclinic"
mkdir -p "$WORK_DIR"

"$FIXTURE_DIR/build.sh"
(cd "$BREWLET_CORE_DIR" && go build -o "$WORK_DIR/brewlet" ./cmd/brewlet)

"$WORK_DIR/brewlet" push \
  "$FIXTURE_DIR/target/spring-petclinic.jar" \
  demo/petclinic:1.0.0 \
  --store "$WORK_DIR/oci" \
  --format=artifact

"$WORK_DIR/brewlet" inspect demo/petclinic:1.0.0 \
  --store "$WORK_DIR/oci"
```

The launch config reports `entry.mode: jar` and
`mainJar: spring-petclinic.jar`. JDK selection, ports, resource limits, and JVM
tuning remain deployment concerns and are not embedded in the artifact.

## Deploy with `JavaApplication`

Publish to a registry reachable by your cluster, then use the descriptor owned by
the Kubernetes repository:

```bash
"$WORK_DIR/brewlet" push \
  "$FIXTURE_DIR/target/spring-petclinic.jar" \
  registry.example.com/demo/petclinic:1.0.0

kubectl apply -f kubernetes/deploy/petclinic-javaapplication.yaml
kubectl get javaapplication,deploy,svc -n petclinic
```

Update the placeholder registry in the descriptor first. The operator reconciles
the resource into a Deployment with `runtimeClassName: brewlet`, a Service, and
optional autoscaling.

See the
[`petclinic-javaapplication.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/petclinic-javaapplication.yaml)
source for replicas, cgroup limits, actuator probes, the H2 profile, and
autoscaling.

## Layered classpath delivery

A fat JAR is one large blob, so any code change produces a new blob. The fixture's
[`layered-build.sh`](https://github.com/brewlet/integration-tests/blob/main/fixtures/spring-petclinic/layered-build.sh)
maps Spring Boot's structure onto Brewlet's framework-neutral layers:

- application classes and resources become a thin application JAR;
- release dependencies become a deterministic classpath tar;
- snapshot dependencies, when present, become a separate volatile tar; and
- launch mode changes from `java -jar` to
  `java -cp app.jar:lib/* <MainClass>`.

Run the split:

```bash
"$FIXTURE_DIR/layered-build.sh"
find "$FIXTURE_DIR/target/layered" -maxdepth 1 -type f
```

Then publish the generated files:

```bash
"$WORK_DIR/brewlet" push \
  "$FIXTURE_DIR/target/layered/spring-petclinic-app.jar" \
  demo/petclinic-layered:1.0.0 \
  --store "$WORK_DIR/oci" \
  --format=artifact \
  --config "$FIXTURE_DIR/target/layered/jvm-config.json" \
  --classpath-layer "$FIXTURE_DIR/target/layered/deps-dependencies.tar"
```

The dependency tar uses sorted entries and normalized timestamps. An unchanged
dependency set therefore retains the same digest across application rebuilds and
is not transferred again. Tier 7 validates this property and runs the layered
artifact through the shim/runc path.

This mapping does not require Brewlet to parse Spring Boot's `layers.idx`.
Any framework or build that can provide compiled application classes and a
directory of dependency JARs can produce the same generic Brewlet layout.
