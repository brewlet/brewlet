# Brewlet Workshop ☕ — Run a Java app on Kubernetes with no Dockerfile

A hands-on, ~90-minute lab where you experience the Brewlet model end-to-end:
**ship just your Java app (here, an `app.jar`) as an OCI artifact, and have a
node-resident JVM run it with `java -jar` inside a resource-limited sandbox** — the
JVM analogue of
[KWasm](https://kwasm.sh/)/[SpinKube](https://www.spinkube.dev/).

If you have never seen Brewlet before, skim the [project README](./README.md)
first for the *why*. This workshop is the *how* — every command below is real
and runnable.

---

## What you'll learn

By the end of this lab you will have:

1. Built a self-executable demo `app.jar` **using only a JDK** (no Maven/Gradle).
2. Pushed **only the JAR** to an OCI artifact store — no Dockerfile, no base image.
3. Inspected the OCI artifact and its JVM launch config.
4. Run the artifact locally so a node JVM executes it straight from the JAR (Layer 1).
5. Run the **real mechanism** — shim → `runc` → `java -jar` under real cgroup
   CPU/memory limits (Layer 2).
6. Explored how a Kubernetes deployment descriptor maps to JVM behavior.
7. Run the **`brewlet-operator` out-of-cluster** and watched it reconcile a
   `JavaApplication` descriptor into a live `Deployment` (`runtimeClassName:
   brewlet`) + `Service` + `HorizontalPodAutoscaler` on your own cluster.
8. Exercised the **moat** — the capabilities a "slim base image + a JAR" cannot
   match: one shared, centrally-patched **JDK fleet** (`brewlet jdks`); **JPMS
   modules** run from the module path; **dependency-layer dedup** (a thin app
   layer over deterministic, deduplicated dependency layers); **AppCDS** faster
   cold starts; and **publish straight from a Maven build** — no CLI, no Dockerfile.

---

## Prerequisites

You need these on your machine before starting:

| Tool | Minimum version | Check | Notes |
|------|-----------------|-------|-------|
| **JDK** | 21+ | `java -version` | Builds the demo JAR and supplies the "node JVM". |
| **Go** | 1.26+ | `go version` | Builds the `brewlet` CLI and the shim. |
| **Docker** | any recent | `docker --version` | Needed **only** for the Layer 2 (`make e2e-linux`) real-cgroups demo. |
| **make** | any | `make --version` | Drives the lab targets. |
| **kubectl** | any | `kubectl version --client` | For the cluster-manifests section. |
| **curl** | any | `curl --version` | To hit the running JVM. |

A running **local Kubernetes cluster** (e.g. Docker Desktop → *Enable Kubernetes*,
minikube, or kind) is used in Part 5. Confirm it's healthy:

```bash
kubectl config current-context      # e.g. docker-desktop
kubectl get nodes                   # all nodes should be Ready
```

> **Tip:** on macOS with SDKMAN, point `JAVA_HOME` at any JDK 21+ before you start:
> ```bash
> export JAVA_HOME="$HOME/.sdkman/candidates/java/current"
> java -version
> ```
> The Makefile defaults `JAVA_HOME` to `$HOME/.sdkman/candidates/java/current`;
> override it if your JDK lives elsewhere.

---

## Setup

Clone the core, Kubernetes, and Maven plugin repositories as siblings (skip any
you already have). Core lab commands run from the `brewlet` repository root:

```bash
git clone https://github.com/brewlet/brewlet.git
git clone https://github.com/brewlet/kubernetes.git
git clone https://github.com/brewlet/maven-plugin.git
cd brewlet
```

---

## Part 1 — Build the demo JAR and the Brewlet tooling

The demo app is a tiny dependency-free Java HTTP server. Build it and the CLI/shim
binaries:

```bash
make app     # compiles demo-app -> demo-app/target/app.jar (javac + jar, no Maven)
make build   # builds ./bin/brewlet and ./bin/containerd-shim-brewlet-v2
```

**What to observe**

- `make app` prints the JAR manifest — note `Main-Class: com.example.Hello`. This
  is an ordinary self-executable JAR; nothing about it is Brewlet-specific.
- `make build` produces two Go binaries in `./bin`: the **`brewlet` CLI** (the
  developer tool) and the **containerd shim** (the node-side runtime).

---

## Part 2 — Layer 1: ship only a JAR, run it, curl it

This is the developer experience: push the JAR as an OCI artifact, then run it.

```bash
make demo
```

`make demo` runs [`demo.sh`](https://github.com/brewlet/brewlet/blob/main/demo.sh), which performs four steps:

1. **PUSH** — `brewlet push demo-app/target/app.jar demo/hello:1.0.0`
   ships **only the JAR** (plus a tiny launch config). No Dockerfile, no image build.
2. **INSPECT** — `brewlet inspect demo/hello:1.0.0` shows the artifact manifest and
   the JVM launch config (main JAR, entry mode, app-intrinsic launch knobs).
3. **RUN** — `brewlet run demo/hello:1.0.0` pulls the artifact and launches
   `java -jar` from the node JDK.
4. **CURL** — hits the live JVM.

**What to observe** — you should see output like:

```
$ curl -s localhost:8080/hello
Hello from a JAR running directly on the node via Brewlet!
$ curl -s localhost:8080/info
availableProcessors = ...     # the container-aware JVM reads its limits directly
```

> The **only** thing that moved was `app.jar`. The JDK came from the node.
> This local demo runs *unconstrained* — real cgroup enforcement is Part 3.

Try it yourself in a second terminal while it runs, or run the artifact manually:

```bash
make run REF=demo/hello:1.0.0     # runs in the foreground; Ctrl-C to stop
# in another terminal:
curl -s localhost:8080/hello
curl -s localhost:8080/info
curl -s localhost:8080/healthz
```

---

## Part 3 — Layer 2: the real mechanism (shim → runc → `java -jar` under cgroups)

This is what actually happens on a provisioned Kubernetes node. It requires Docker
(used to provide a Linux node with cgroups):

```bash
make e2e-linux
```

This target ([`e2e-linux.sh`](https://github.com/brewlet/brewlet/blob/main/e2e-linux.sh)) cross-compiles the CLI + shim for
Linux, then inside a privileged `eclipse-temurin:21` container it:

1. Installs `runc` and a JDK **runtime root** (`/opt/brewlet/jdks/temurin-21`) —
   simulating the node provisioner.
2. Hands the shim an image config with `cpuLimit: "1"` and `memoryLimit: "384Mi"`.
3. `shim prepare-bundle` **disassembles the artifact into an OCI runtime bundle**.
4. `runc run` launches `java -jar` as **PID 1** under real cgroup limits.

It then repeats the whole shim → `runc` flow for a **modular (JPMS) app**, so a
single target proves *both* deployment shapes under real cgroups:
`java -jar` **and** `java -p <module-path> -m <module>`.

**What to observe** — the JVM now sees the *enforced* limits:

```
--- /info (cgroup limits seen by the JVM under runc) ---
availableProcessors = 1       # real cgroup CPU limit (--cpus=1)
memory.max          = 384Mi   # real cgroup memory limit (--memory=384m)
--- /hello ---
Hello from a JAR running directly on the node via Brewlet!

== modular (JPMS) scenario: shim -> runc -> java -p ... -m ... ==
[demo-module-app] listening on :8080  (module=com.example.orders, java=21.0.11)
--- modular /hello (produced by the com.example.greeter module on the module path) ---
Hello from a MODULAR JPMS app on the module path via Brewlet!
jvm.input.args = [--module-path=/app/orders.jar:/app/mods, -Djdk.module.main=com.example.orders]
```

This is the key insight: **deployment resource limits → cgroup constraints →
the container-aware JVM sizes itself accordingly.** Brewlet injects no JVM flags;
the JDK reads the cgroup on its own. And the exact same shim → `runc` mechanism
runs a JPMS module on the module path — you explore that path further in Part 6.

---

## Part 4 — Explore the artifact and the OCI bundle

Peek under the hood with the CLI directly (from the core repository root):

```bash
# Inspect the artifact manifest + JVM launch config
./bin/brewlet inspect demo/hello:1.0.0

# Emit the exact OCI runtime bundle the shim feeds to runc, for a 2-CPU / 512Mi pod
make bundle REF=demo/hello:1.0.0
ls -R ./bundle
```

Open `./bundle/config.json` and find:

- `process.args` → `["java","-jar","/app/app.jar"]` (the canonical launch).
- the cgroup CPU/memory settings derived from `--cpu 2 --memory 512Mi`.
- the read-only JDK mount + the JAR mounted at `/app`.

This is the entire "novel" part of Brewlet: **artifact → bundle → args**. Isolation
(namespaces, cgroups, seccomp) is standard `runc`.

---

## Part 5 — The Kubernetes control plane: descriptor → live workload

Now see how Brewlet surfaces to a platform team and a developer on a real
cluster. The **`brewlet-operator`** turns a developer's `JavaApplication`
descriptor into standard Kubernetes objects — a `Deployment` (with
`runtimeClassName: brewlet`), a `Service`, and an optional
`HorizontalPodAutoscaler` — and garbage-collects them via owner references.

For the workshop we run the operator **out-of-cluster**: a plain binary that
talks to your cluster through your kubeconfig, so there's nothing to build into an
image or install on a node. This is the same mode the Tier-4 e2e test uses.

### 5.1 Register the RuntimeClass and the CRD

```bash
kubectl apply -f ../kubernetes/deploy/runtimeclass.yaml
kubectl apply -f ../kubernetes/deploy/javaapplication-crd.yaml
kubectl wait --for=condition=Established --timeout=30s \
  crd/javaapplications.apps.brewlet.sh
```

### 5.2 Build and start the operator (out-of-cluster)

```bash
(cd ../kubernetes/operator && go build -o ./bin/manager ./cmd/manager)

kubectl create namespace brewlet
../kubernetes/operator/bin/manager --namespace brewlet \
  --provisioner-image "demo/nonexistent:donotpull" \
  --jdks "temurin-21" --launchers "jaz" \
  --leader-elect=false --metrics-bind-address 0 \
  --health-probe-bind-address ":18081" \
  > /tmp/brewlet-operator.log 2>&1 &          # ← logs to a file, NOT your screen
sleep 5                                        # let the manager come up
```

> **Always redirect the operator's logs to a file.** It's a controller: it logs
> continuously, and on the first reconcile it may log a harmless, self-healing
> optimistic-lock conflict (`the object has been modified …`) as two overlapping
> reconciles race. controller-runtime just requeues and succeeds — redirecting
> keeps your terminal (and a live demo) clean.
>
> `--provisioner-image` is deliberately a **non-existent** ref so the operator
> never runs host-mutating provisioner pods on your machine (see the honesty
> note below).

### 5.3 Apply the descriptor and watch it expand

```bash
kubectl apply -f ../kubernetes/deploy/sample-javaapplication.yaml
sleep 3
kubectl get javaapplication,deploy,svc,hpa -n payments
```

> The sample is named **`orders-api`** and lives in the **`payments`** namespace
> (it creates that namespace itself). The operator reconciles it into a matching
> `Deployment`, `Service`, and `HPA`.

### 5.4 The payoff — a normal Kubernetes workload

```bash
# The ONLY Brewlet-specific line in the generated pod spec:
kubectl get deploy orders-api -n payments \
  -o jsonpath='runtimeClassName={.spec.template.spec.runtimeClassName}{"\n"}'
# → brewlet

# The image IS the OCI artifact (the JAR), not a container image:
kubectl get deploy orders-api -n payments \
  -o jsonpath='image={.spec.template.spec.containers[0].image}{"\n"}'

# The operator matched the requested JDK to its configured fleet:
kubectl get javaapplication orders-api -n payments \
  -o jsonpath='selectedJdk={.status.selectedJdk}{"\n"}'
# → 21

# The children are owned by the JavaApplication (so they GC with it):
kubectl get deploy orders-api -n payments \
  -o jsonpath='ownedBy={.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}{"\n"}'
```

Delete the descriptor and watch the Deployment/Service/HPA garbage-collect via
their owner references:

```bash
kubectl delete javaapplication orders-api -n payments
kubectl get deploy,svc,hpa -n payments        # all gone
```

Read the two developer-facing shapes:

- **[`deploy/raw-deployment.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/raw-deployment.yaml)** — a plain `Deployment` whose `image` **is the JAR
  artifact** and whose only Brewlet-specific line is `runtimeClassName: brewlet`.
- **[`deploy/sample-javaapplication.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/sample-javaapplication.yaml)** — the higher-level `JavaApplication` CRD:
  `artifact.image`, `replicas`, `resources`, and a `jvm:` block
  (`version`, `launcher`, and user `args` wired through via `JDK_JAVA_OPTIONS`).

Trace how a descriptor field becomes JVM behavior:

| Descriptor field | Becomes | JVM effect |
|---|---|---|
| `resources.limits.memory` | cgroup `memory.max` | JVM sizes heap via `-XX:MaxRAMPercentage` |
| `resources.limits.cpu` | cgroup `cpu.max` | GC/JIT threads scale to the CPU limit |
| `jvm.version: 21` | selects node JDK | shim mounts `/opt/brewlet/jdks/...-21` |

> ### ⚠️ What is and isn't proven on your laptop (important for the workshop)
> You just watched the **Kubernetes control plane work for real**: the operator
> reconciled a `JavaApplication` into a `Deployment` (`runtimeClassName: brewlet`)
> + `Service` + `HPA`, matched the requested JDK, populated status, and wired
> owner references for garbage collection.
>
> The pods, however, will show **`0/N Ready`** — and that is expected. The sample
> `artifact.image` is a placeholder ref, and this cluster's node has **no Brewlet
> runtime installed**. Actually executing a `runtimeClassName: brewlet` pod needs
> the shim + a JDK + the registered containerd runtime on the node, which the
> privileged `node-provisioner` DaemonSet installs (`deploy/node-provisioner.yaml`).
> **Do not apply that on a shared cluster** — it mutates hosts; read it, don't run
> it. (None of the components are stubbed: the containerd Runtime v2 TTRPC Task
> service is fully implemented on Linux — see [SPECIFICATION.md §6.4](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md);
> the provisioner really installs the shim + JDK roots — §5.5; the operator really
> manages the provisioner DaemonSet + RuntimeClass and tracks node readiness — §8.1.)
>
> So the two halves of the story are each proven separately and safely:
> **Part 3 (`make e2e-linux`)** proves the *actual execution* — the shim assembles a
> bundle and `runc` runs `java -jar` under enforced cgroups — and **Part 5** proves
> the *cluster orchestration* — descriptor → managed Kubernetes objects. Together
> they are the full end-to-end path.

---

## Part 6 — The moat: why this is more than "a base image with a JAR"

A skeptic could look at Part 2 and say *"you just copied a JAR onto a slim base
image."* You didn't — there is **no image and no OS/JVM layer** in what you
pushed. This part exercises the capabilities that a Dockerfile-per-app world
structurally cannot give you. Each sub-part is self-contained and runs from
the core repository root.

### 6.1 One shared, centrally-patched JDK fleet (`brewlet jdks`)

Nothing about the JDK is baked into your artifact — the **node** supplies it,
shared read-only across every pod and patched **once** for the whole fleet. The
CLI can inventory what the fleet advertises:

```bash
./bin/brewlet jdks               # table; add --output wide or --output json
```

On this fresh cluster you'll see:

```
No Brewlet JDK inventory found on any node.
```

That's expected — no `node-provisioner` has run here (Part 5's honesty note). On a
provisioned fleet each node advertises its installed JDKs/launchers as
`brewlet.sh/...` labels, and `brewlet jdks` lists them. This is the same inventory
the **admission webhook** matches a pod's requested `jvm.version` against — denying
`NoCompatibleJDK` if no node can serve it. You already saw the operator half of
this in Part 5.4: it matched the descriptor's `jvm.version: 21` and populated
`status.selectedJdk = 21`. **One `temurin-21 → temurin-21.0.12` node upgrade
patches every workload at once — no image rebuilds.**

### 6.2 Ship more than a fat JAR #1: JPMS modules on the module path

Brewlet runs Java *applications*, not just fat JARs. A modular (JPMS) app is
launched with `java -p <module-path> -m <module>` — never repackaged into a fat
JAR. Build the demo modular app (a JDK-only build: an `orders` app module that
`requires` a `greeter` library module) and ship it with a **module-path layer**:

```bash
JAVA_HOME="$JAVA_HOME" PATH="$JAVA_HOME/bin:$PATH" ./demo-module-app/build.sh   # javac + jar, no Maven
./bin/brewlet push demo-module-app/target/orders.jar demo/orders:1.0.0 \
  --module-layer demo-module-app/target/mods.tar
```

Inspect it — the launch config is `entry.mode: module`, not `jar`:

```bash
./bin/brewlet inspect demo/orders:1.0.0
```

```json
"entry": {
  "mode": "module",
  "mainClass": "com.example.orders.OrdersApp",
  "module": "com.example.orders",
  "modulePath": ["orders.jar", "mods"]
}
```

Run it and hit it:

```bash
./bin/brewlet run demo/orders:1.0.0        # launch: java -p .../orders.jar:.../mods -m com.example.orders/...
# in another terminal:
curl -s localhost:8080/hello               # served by the greeter module on the module path
```

> The **module-path layer** (`application/vnd.brewlet.modulepath.layer.v1+tar`)
> is a *separate* OCI layer from the app JAR — which is exactly the dedup lever in
> 6.3. Part 3's `make e2e-linux` runs this same modular app under **real cgroups**.

### 6.3 Ship more than a fat JAR #2: publish from a build + dependency-layer dedup

This is the direct rebuttal to *"isn't it just a base image?"*. Brewlet can split
an app into a **thin application layer** over **separate, deterministic dependency
layers**: change one line of business code and only the tiny app layer's digest
changes — the dependency layers keep the **same digest** and are skipped on push
*and* pull. And you never touch the CLI: the **Maven plugin** publishes the OCI
artifact straight from `mvn verify`.

Build the multi-module Maven demo (an `orders` app module over two library
modules), which is configured with `<layered>true</layered>`:

```bash
# 1. Install the plugin into your local Maven repo, then build the reactor:
mvn -f ../maven-plugin -q -DskipTests install
(cd demo-module-maven && mvn -q -DskipTests verify)      # emits an OCI layout under orders/target/brewlet/oci
```

Inspect the produced artifact — note the **two distinct layers**: a thin
`...jar.layer.v1+jar` (your compiled code) and a `...modulepath.layer.v1+tar`
(the dependency modules):

```bash
STORE=demo-module-maven/orders/target/brewlet/oci
./bin/brewlet inspect demo/orders-maven:1.0.0 --store "$STORE"
```

Now prove the dedup property — the dependency layer is built **deterministically**
(sorted entries, fixed mtime), so an unchanged dependency set yields a
**byte-identical digest**. Rebuild from clean and compare:

```bash
./bin/brewlet inspect demo/orders-maven:1.0.0 --store "$STORE" \
  | grep -A2 'modulepath.layer' | grep digest        # note the digest
(cd demo-module-maven && mvn -q -DskipTests verify)  # full clean rebuild
./bin/brewlet inspect demo/orders-maven:1.0.0 --store "$STORE" \
  | grep -A2 'modulepath.layer' | grep digest        # identical → deduped on push/pull
```

Run it (the plugin wired `-Dserver.port=8096` from the pom):

```bash
./bin/brewlet run demo/orders-maven:1.0.0 --store "$STORE"
curl -s localhost:8096/hello                          # output from BOTH library modules
```

> **Full Spring Boot proof:** `make petclinic-layered-e2e-linux` (Docker) takes
> the *real upstream* Spring PetClinic fat JAR, splits it into a thin app JAR +
> dependency layer(s), and runs it via shim → `runc` under cgroups — the same
> dedup win as Spring's `layertools`, via generic, framework-agnostic steps.

### 6.4 Faster cold starts with AppCDS

The classic objection to a per-pod JVM is cold start. Brewlet can attach a
build-time **Application Class-Data Sharing** archive (`cds.layer.v1+jsa`) as its
own layer, so the JVM memory-maps a pre-parsed class image instead of re-parsing
on every start. Run the JDK-integration check (auto-skips without a full JDK 17+):

```bash
make appcds-verify
```

```
--- PASS: TestAppCDSTrainThenMapIntegration
    --- PASS: .../maps_with_canonical_mtime
    --- PASS: .../refuses_with_drifted_mtime
```

It proves the *train → map* flow end-to-end: a CDS archive trained at build time
maps cleanly at run time, and is safely **refused** if the classpath drifts
(mtime mismatch) — so a stale archive can never silently mis-load. Node-side
regeneration is opt-in via `spec.jvm.cds.regenerate` /
`-XX:+AutoCreateSharedArchive` (see [docs/appcds.md](./docs/appcds.md)).

> **The moat, in one line:** the JDK, dependency layers, and CDS archive are all
> **shared, deduplicated, and patched centrally on the node** — not copied into a
> private image per app. That is the structural difference from a base image with
> a JAR copied in.

---

## Cleanup

```bash
# Stop the out-of-cluster operator (Part 5)
kill "$(pgrep -f 'kubernetes/operator/bin/manager')"

# Remove the cluster objects you applied
kubectl delete -f ../kubernetes/deploy/sample-javaapplication.yaml --ignore-not-found
kubectl delete -f ../kubernetes/deploy/javaapplication-crd.yaml --ignore-not-found
kubectl delete -f ../kubernetes/deploy/runtimeclass.yaml --ignore-not-found
kubectl delete namespace brewlet --ignore-not-found

# Remove local build outputs and demo artifacts
make clean
(cd demo-module-maven && mvn -q clean 2>/dev/null) || true   # Part 6.3 Maven outputs
```

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| `make app` fails with a Java error | Ensure `java -version` is **21+** and `JAVA_HOME` points at it: `export JAVA_HOME=$HOME/.sdkman/candidates/java/current`. |
| `make build` fails | Ensure `go version` is **1.26+**. |
| `curl: connection refused` in the demo | The JVM may still be starting — retry after a second, or hit `/healthz` first. |
| `make e2e-linux` fails to start | Docker must be running; the container needs `--privileged` (the Makefile sets it). |
| `make e2e-linux` crashes in `runc` (e.g. a Go panic in `runc/tty.go` / `asm_amd64.s`) | The container ran under the wrong CPU architecture (emulation). The Makefile now auto-detects your host arch (`uname -m`) and pins `docker run --platform`, so this should not happen. If you need to force an arch, run `make e2e-linux ARCH=amd64` (or `ARCH=arm64`). A globally exported `DOCKER_DEFAULT_PLATFORM` no longer affects this target. |
| Port `8080` already in use | Stop whatever owns it, or change the port in `demo.sh` (the demo app reads `-Dserver.port`). |
| `kubectl` targets the wrong cluster | `kubectl config use-context docker-desktop` (or your local context). |
| Part 5: `kubectl get deploy orders …` → `NotFound` | The generated Deployment is named **`orders-api`** and lives in the **`payments`** namespace — add `-n payments`. It also only exists while the operator (5.2) is running to reconcile the `JavaApplication`. |
| Part 5: operator logs flood your terminal | Start the manager with `> /tmp/brewlet-operator.log 2>&1 &` so it logs to a file, not the screen (see 5.2). |
| Part 5: operator log shows `the object has been modified …` | A harmless optimistic-lock conflict during the initial reconcile burst; controller-runtime retries and the objects reconcile correctly. Redirect logs to keep it off-screen. |
| Part 5: pods stay `0/N Ready` | Expected — the node has no Brewlet runtime installed and the sample image is a placeholder. Part 5 proves the *control-plane* reconciliation; actual pod *execution* is proven by Part 3 (`make e2e-linux`). |
| Part 6.1: `brewlet jdks` → `No Brewlet JDK inventory found` | Expected on an un-provisioned cluster — nodes only advertise JDK labels after the `node-provisioner` runs. It's reading real node labels via your kubeconfig, not stubbed. |
| Part 6.3: `mvn verify` can't find the plugin (`sh.brewlet:brewlet-maven-plugin`) | Install it into your local repo first: `mvn -f ../maven-plugin -DskipTests install` (run from the core repository root). |
| Part 6.3: the two `modulepath.layer` digests differ across rebuilds | They shouldn't — the dependency tar is built deterministically. Ensure you did a clean `mvn verify` (no source edits between runs); any change to a dependency module legitimately changes the digest. |
| Part 6.4: `make appcds-verify` prints `SKIP` | The archive step needs a full JDK 17+ on `JAVA_HOME`/`PATH`; a headless/cut-down JRE auto-skips. Point `JAVA_HOME` at a full JDK. |

---

## What you learned & where to go next

- A Java service on Kubernetes can be **just a JAR** — the JDK installation lives on the node,
  shared and patched centrally; no Dockerfile, no OS layer, no per-image JVM.
- Resource limits flow **descriptor → cgroup → container-aware JVM**, with no
  Brewlet-injected tuning flags.
- The runtime reuses the **same extension points** as the Wasm ecosystem:
  `RuntimeClass` + a containerd shim + a node provisioner.
- The **operator** turns a single `JavaApplication` descriptor into the standard
  Kubernetes objects a platform team already knows — `Deployment` +  `Service` +
  `HPA` — whose only Brewlet-specific line is `runtimeClassName: brewlet`.
- The **moat** is structural, not cosmetic: one shared, centrally-patched **JDK
  fleet** (`brewlet jdks`); **JPMS modules** on the module path; **dependency-layer
  dedup** (a thin app layer over deterministic, deduplicated dependency layers);
  **AppCDS** for faster cold starts; and **publish straight from a Maven build**.
  None of these exist in a "base image + a JAR" — because there is no image.

Keep exploring:

- **[SPECIFICATION.md](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)** — full architecture (artifact format
  §4, shim internals §6, node provisioning §5, security §11, roadmap §15).
- **[README.md](./README.md)** — the big-picture pitch and comparisons.
- **[Core repository README](https://github.com/brewlet/brewlet#readme)** — the PoC map and scope notes.

*Because your JAR should just run.* ☕
