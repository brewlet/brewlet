# Getting started (local proof of concept)

The [core repository](https://github.com/brewlet/brewlet) proves the whole Brewlet model end-to-end on your
laptop — no Kubernetes cluster required. In a couple of minutes you will:

1. Build a dependency-free demo Java HTTP app into a self-executable `app.jar`.
2. Push **only that JAR** as an OCI artifact (no Dockerfile).
3. Run it directly from the artifact with a node-resident JDK.
4. Exercise the **real** mechanism — `shim → runc → java -jar` under real cgroup
   limits — exactly as it happens on a provisioned Kubernetes node.

---

## Prerequisites

| Tool | Why | Notes |
|---|---|---|
| **JDK 21+** | Builds the demo JAR and acts as the "node JDK" for the local run | Any OpenJDK build (Temurin, Microsoft, Corretto, Zulu…). |
| **Go 1.26+** | Builds the `brewlet` CLI and the shim | |
| **Docker** | Runs the real-Linux `runc` demo under cgroups | Only needed for `make e2e-linux`. |

Point `JAVA_HOME` at any JDK 21+:

```bash
export JAVA_HOME="$HOME/.sdkman/candidates/java/current"   # or any JDK 21+
```

Clone the core repository. All commands below run from its root:

---

## Layer 1 — the developer experience (ship only a JAR)

```bash
git clone https://github.com/brewlet/brewlet.git
cd brewlet

make app     # build demo-app/target/app.jar (the self-executable demo)
make build   # build ./bin/brewlet and ./bin/containerd-shim-brewlet-v2
make demo    # push the JAR as an artifact -> run it on this machine -> curl it
```

`make demo` runs three steps that mirror the real developer flow:

1. `brewlet push app.jar demo/hello:1.0.0` — ships **only the JAR** as an OCI
   artifact. No Dockerfile, no base image.
2. `brewlet inspect demo/hello:1.0.0` — shows the artifact manifest and the JVM
   launch config.
3. `brewlet run demo/hello:1.0.0` — pulls the artifact and launches `java -jar`
   using the node JDK (`JAVA_HOME`).

You should see something like:

```
[brewlet] launch: java -jar /…/app/app.jar
$ curl localhost:8080/hello
Hello from a JAR running directly on the node via Brewlet!
```

> **Note.** Resource *enforcement* needs cgroups, which the local run doesn't set
> up — that's what Layer 2 demonstrates. Brewlet injects no JVM tuning flags; the
> container-aware JDK reads the sandbox cgroup limits and any tuning is user-supplied
> via descriptor `jvm.args`. See [Resource tuning](resource-tuning.md).

Run an arbitrary artifact in the foreground and pass extra JVM args:

```bash
make run REF=demo/hello:1.0.0
# or, directly, with extra args after a literal --:
./bin/brewlet run demo/hello:1.0.0 -- -Dspring.profiles.active=dev
```

---

## Layer 2 — the real mechanism (shim → runc → java -jar under cgroups)

```bash
make e2e-linux   # requires Docker
```

This runs the actual shim core inside a privileged Linux container: it disassembles
the artifact into an OCI runtime bundle and executes it with **runc**, so `java -jar`
runs as **PID 1** under real cgroup limits and a node-resident JDK — exactly what the
containerd shim does on a provisioned node.

The demo pins `--cpus=1 --memory=384m`, and the app reports what the JVM actually
sees inside the sandbox:

```
shim Create(): disassemble artifact → OCI runtime bundle
shim Start():  runc run … → java -jar runs as PID 1 (Temurin 21.0.11)
$ curl localhost:8080/info
availableProcessors = 1       (real cgroup CPU limit, --cpus=1)
memory.max          = 384Mi   (real cgroup memory limit)
```

The architecture is auto-detected from your host (`arm64`/`amd64`), so this runs
natively on Apple Silicon and x86_64 alike. Override with `make e2e-linux ARCH=amd64`.

---

## Inspect the OCI runtime bundle by hand

Want to see exactly what the shim feeds to runc? Emit the bundle yourself:

```bash
make bundle REF=demo/hello:1.0.0
# equivalently:
./bin/brewlet bundle demo/hello:1.0.0 --cpu 2 --memory 512Mi --out ./bundle
cat ./bundle/config.json
```

On a Linux node the shim runs the equivalent of `runc run -b ./bundle brewlet-<id>`.

---

## Cluster artifacts (optional, from your laptop)

Even without the operator you can apply the raw cluster objects to a cluster:

```bash
kubectl apply -f deploy/runtimeclass.yaml
kubectl apply -f deploy/javaapplication-crd.yaml
kubectl apply -f deploy/sample-javaapplication.yaml   # or deploy/raw-deployment.yaml
```

For the full cluster experience (operator + provisioner + webhook via Helm), see
[Installation](installation.md).

---

## Clean up

```bash
make clean   # removes bin/, oci/, bundle/, demo-app/target/
```

---

## What just happened?

- You shipped **only** `app.jar` — the JDK came from the node in every case.
- The Layer 1 run used your `JAVA_HOME` JDK; the Layer 2 run used a JDK inside the
  Docker image, mounted read-only into a runc sandbox with real cgroup limits.
- This is the same code path the containerd shim uses on a real Kubernetes node.

## Next steps

- Understand the pieces: [Concepts & architecture](concepts.md).
- Publish your own app: [Building & publishing OCI artifacts](building-and-publishing.md).
- Enable Brewlet on a cluster: [Installation](installation.md).
- All CLI flags: [CLI reference](cli-reference.md).
