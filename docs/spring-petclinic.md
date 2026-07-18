# Example: Spring PetClinic (a real Spring Boot app)

The [`demo-app/`](https://github.com/brewlet/brewlet/tree/main/demo-app/) example is deliberately tiny — a
dependency-free HTTP server built with the JDK alone. This example goes the other
way: it runs the **real, upstream [Spring PetClinic](https://github.com/spring-projects/spring-petclinic)**
— a genuine, dependency-heavy Spring Boot application whose repackaged fat JAR is
~63 MB — to prove that the Brewlet model works for the applications people
actually ship, not just toys.

Nothing about PetClinic is special to Brewlet: it is an ordinary Spring Boot fat
JAR whose `Main-Class` is Spring Boot's `JarLauncher`. Brewlet ships **only that
JAR** as an OCI artifact and runs it with `java -jar` on a node-resident JDK — no
Dockerfile, no base image, no JVM baked into the image.

- Example sources: [`spring-petclinic/`](https://github.com/brewlet/brewlet/tree/main/spring-petclinic/)
- Deployment descriptor: [`deploy/petclinic-javaapplication.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/petclinic-javaapplication.yaml)
- Layered-classpath split: [`spring-petclinic/layered-build.sh`](https://github.com/brewlet/brewlet/blob/main/spring-petclinic/layered-build.sh) — see [below](#layered-classpath-redeploy-only-your-business-code)
- Automated test: e2e **Tier 7** ([`integration-tests/tier7-petclinic.sh`](https://github.com/brewlet/integration-tests/blob/main/tier7-petclinic.sh))

---

## Prerequisites

| Tool | Why |
|---|---|
| **JDK 17+** | Builds the PetClinic fat JAR and acts as the "node JDK" for the local run. PetClinic targets Java 17. |
| **Go 1.26+** | Builds the `brewlet` CLI and the shim. |
| **Docker** | Runs the real-Linux `runc` path under cgroups (`make petclinic-e2e-linux`). |
| **Network** | The build clones upstream PetClinic and resolves its Maven dependencies. |

```bash
export JAVA_HOME="$HOME/.sdkman/candidates/java/current"   # any JDK 17+
git clone https://github.com/brewlet/brewlet.git
cd brewlet
```

---

## 1. Build the fat JAR

```bash
make petclinic     # clones spring-projects/spring-petclinic (pinned) + mvn package
```

This runs [`spring-petclinic/build.sh`](https://github.com/brewlet/brewlet/blob/main/spring-petclinic/build.sh), which
shallow-clones the upstream repo at a pinned commit, builds the repackaged Spring
Boot fat JAR (`./mvnw -DskipTests package`), and copies it to a stable path:

```
/spring-petclinic/target/spring-petclinic.jar
```

Overrides (all optional):

```bash
PETCLINIC_REF=main   make petclinic          # track upstream HEAD instead of the pin
PETCLINIC_JAR=/path/to/petclinic.jar make petclinic   # stage a pre-built JAR (skip the clone/build)
```

---

## 2. Ship ONLY the JAR as an OCI artifact

```bash
./bin/brewlet push spring-petclinic/target/spring-petclinic.jar \
    demo/petclinic:1.0.0

./bin/brewlet inspect demo/petclinic:1.0.0
```

`inspect` shows the launch config: `entry.mode: jar` (so the node runs
`java -jar`) and the main jar `spring-petclinic.jar`. JDK selection and ports
are deployment concerns, carried by the `JavaApplication` descriptor / CRI
metadata, not the artifact. No Dockerfile, no base image — just the JAR plus a
tiny JSON launch descriptor.

---

## 3. Run it — the real node mechanism (shim → runc → `java -jar`)

```bash
make petclinic-e2e-linux    # needs Docker
```

This is exactly what the containerd shim does on a provisioned node, run locally
inside a privileged `eclipse-temurin` container: the shim disassembles the
artifact into an OCI runtime bundle, and **`runc` runs the Spring Boot app as
PID 1 under real cgroup limits** (`--memory=768m --cpus=1`) using a
node-resident JDK. You should see:

```
--- /actuator/health (Spring Boot up under runc) ---
{"groups":["liveness","readiness"],"status":"UP"}
--- welcome page <title> (served by the real PetClinic app) ---
<title>PetClinic :: a Spring Framework demonstration</title>
--- /actuator/info (JVM is cgroup-aware: reads the sandbox limits directly) ---
{..."name":"system.cpu.count","measurements":[{"statistic":"VALUE","value":1.0}]...}
```

The `system.cpu.count = 1.0` proves the JVM read the sandbox's **cgroup CPU
limit** directly — Brewlet injects no tuning flags; the container-aware JDK sizes
itself from the limits in the deployment descriptor.

---

## 4. Deploy it on Kubernetes (the `JavaApplication` descriptor)

Push the artifact to your registry, then apply the descriptor. The
brewlet-operator expands it into a `Deployment` (with `runtimeClassName: brewlet`)
+ `Service` + optional HPA:

```bash
brewlet push /spring-petclinic/target/spring-petclinic.jar \
    registry.example.com/demo/petclinic:1.0.0

kubectl apply -f ../kubernetes/deploy/petclinic-javaapplication.yaml
kubectl get javaapplication,deploy,svc -n petclinic
```

See [`petclinic-javaapplication.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/petclinic-javaapplication.yaml)
for the full descriptor: replicas, cgroup limits, actuator readiness/liveness
probes (`/actuator/health/readiness`, `/actuator/health/liveness`), the H2
in-memory profile, and autoscaling.

> The container-aware JVM reads the pod's cgroup memory/CPU limits directly. The
> descriptor sets `-XX:MaxRAMPercentage=75.0` so the heap tracks `limits.memory`;
> swap in the [`jaz`](launchers.md) launcher to auto-tune heap/GC/CPU with no
> `-XX` flags at all.

---

## How it's tested

e2e **Tier 7** ([`integration-tests/tier7-petclinic.sh`](https://github.com/brewlet/integration-tests/blob/main/tier7-petclinic.sh))
automates everything above and asserts it stays green:

```bash
git clone https://github.com/brewlet/integration-tests.git
cd integration-tests && ./run.sh --tier 7
```

It builds the real fat JAR, pushes + inspects it, runs it through shim → runc
(asserting `/actuator/health` = `UP`, the welcome page, and the cgroup CPU
count), **and** reconciles a PetClinic `JavaApplication` on your cluster
(asserting the Deployment carries `runtimeClassName: brewlet`, the OCI artifact
image, and the requested replicas, plus a Service and owner-ref GC). It also
splits the fat JAR into a [layered classpath](#layered-classpath-redeploy-only-your-business-code)
deployment and asserts that rebuilding business code reuses (dedups) the
dependency layer while only the thin app-JAR layer changes, then runs that
layered artifact through shim → runc. Parts SKIP gracefully when a prerequisite
is missing (no network to build PetClinic, no Docker, no cluster). It also runs
in CI on a `kind` cluster.

---

## Layered classpath: redeploy only your business code

A fat JAR is a single ~63 MB blob: change one line of application code and the
whole 63 MB re-pushes and re-pulls. Brewlet's
[layered-classpath deployment](layered-classpath-deployment.md) lets PetClinic
travel as **separate OCI layers** instead — slow-moving dependencies in their own
layer(s), and only the compiled business classes in a thin application JAR. Then
`brewlet` launches `java -cp app.jar:lib/* <MainClass>` rather than `java -jar`.

Because a registry (and a node) dedups layers by digest, rebuilding your code
changes **only the small app-JAR layer**; the ~63 MB dependency layer keeps its
digest and is skipped. This delivers the same dedup win as Spring Boot's own
[layertools](https://docs.spring.io/spring-boot/reference/packaging/efficient.html)
model — but reached through **generic, framework-agnostic steps, without modifying
the upstream build and without Brewlet parsing `layers.idx`** (see
[Mapping any framework's layered output](#mapping-any-frameworks-layered-output) below).

### Split the fat JAR

```bash
make petclinic-layered    # runs spring-petclinic/layered-build.sh
```

This explodes the fat JAR and emits, under `spring-petclinic/target/layered/`:

| Output | Contents | Change frequency |
|---|---|---|
| `spring-petclinic-app.jar` | `BOOT-INF/classes` (your compiled code + resources) | **often** — ~390 KB |
| `deps-dependencies.tar` | release third-party JARs (`BOOT-INF/lib/*`, non-`SNAPSHOT`) | rarely — ~63 MB |
| `deps-snapshot-dependencies.tar` | `*-SNAPSHOT` deps (only if present) | sometimes |
| `jvm-config.json` | `entry.mode=classpath` launch config | — |

The dependency grouping uses a plain **filename convention** — a JAR whose name
carries `-SNAPSHOT` goes to the snapshot layer, everything else to the release layer.
It does **not** read `BOOT-INF/layers.idx`. The dependency tars are built
**deterministically** (sorted entries, fixed mtime), so an unchanged dependency set
produces a byte-identical tar — hence an identical layer digest — and is deduped
rather than re-pushed.

### Push and run it

```bash
./bin/brewlet push spring-petclinic/target/layered/spring-petclinic-app.jar \
    demo/petclinic-layered:1.0.0 \
    --config spring-petclinic/target/layered/jvm-config.json \
    --classpath-layer spring-petclinic/target/layered/deps-dependencies.tar

./bin/brewlet run demo/petclinic-layered:1.0.0    # java -cp app.jar:lib/* …
```

`inspect` shows the two layer kinds (`jar.layer.v1+jar` for the thin app JAR,
`classpath.layer.v1+tar` for the dependencies) and `entry.mode=classpath` with
`classPath: ["spring-petclinic-app.jar", "lib/*"]`.

### Proof: only the app layer redeploys

Rebuild just the business code, re-push to the same ref, and compare the layer
digests:

```
app-JAR layer:  sha256:e2284299…  ->  sha256:ee54a98b…   CHANGED
deps    layer:  sha256:7f37c50d…  ->  sha256:7f37c50d…   REUSED  (63 MB not re-pushed)
```

Tier 7 asserts exactly this (`layered: rebuilding business code REUSES the
dependency layer`), then runs the layered artifact through shim → runc so
`java -cp app.jar:lib/*` serves the live app as PID 1 under cgroups —
[`make petclinic-layered-e2e-linux`](https://github.com/brewlet/brewlet/blob/main/Makefile).

> On Kubernetes nothing changes in the descriptor: the same
> [`JavaApplication`](https://github.com/brewlet/kubernetes/blob/main/deploy/petclinic-javaapplication.yaml) simply points
> its `artifact.image` at `…/petclinic-layered:1.0.0`. The layering lives inside
> the artifact, so pods pull the shared dependency layer once and only re-pull the
> thin app-JAR layer on a code change.

---

## Mapping any framework's layered output

**Brewlet does not parse `layers.idx`** — or any other framework-specific layering
manifest. Doing so would oblige Brewlet to understand every framework's private
layering scheme. Brewlet's contract is deliberately narrow and generic:

- a thin application JAR (your compiled classes + resources) as the
  `application/vnd.brewlet.jar.layer.v1+jar` layer, and
- one or more dependency tars — JARs at the tar root — as
  `application/vnd.brewlet.classpath.layer.v1+tar` layers, unpacked to `/app/lib`,
- launched with `entry.mode=classpath` and `classPath: ["app.jar", "lib/*"]`.

The PetClinic [`layered-build.sh`](https://github.com/brewlet/brewlet/blob/main/spring-petclinic/layered-build.sh) shows
that **Spring Boot's repackaged layered output maps cleanly onto that generic
format using only structural, framework-agnostic steps** — no `layers.idx` reader:

1. Explode the repackaged fat JAR (`jar xf`).
2. Pack `BOOT-INF/classes` into the thin app JAR.
3. Group `BOOT-INF/lib/*.jar` into dependency tar(s) by a plain filename
   convention (`-SNAPSHOT` → a volatile snapshot layer, everything else → a stable
   release layer) and normalize each tar (sorted entries, fixed mtime) so unchanged
   inputs yield an identical digest.
4. Read `Start-Class` from the manifest for `entry.mainClass`.

The same recipe applies to **any framework that can emit an exploded classes
directory plus a directory of dependency JARs** (Spring Boot's `layertools`, a
`mvn dependency:copy-dependencies` output, Gradle's `bootJar`, etc.): keep your
compiled code in the thin JAR, tar the dependency JARs into stable → volatile
layers, and point `entry.classPath` at `["app.jar", "lib/*"]`. If you prefer the
tooling to build the dependency tars for you, the
[Maven plugin](building-and-publishing.md#layered-thin-jar-apps)
(`-Dbrewlet.layered=true`) does the split from the resolved POM dependency tree —
again without reading `layers.idx`.

> Consuming `layers.idx` (or any framework layering manifest) inside the CLI or
> Maven plugin is an explicit **non-goal** — see
> [layered classpath deployment §10](layered-classpath-deployment.md#10-recommendation--suggested-phasing).
