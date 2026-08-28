# Brewlet documentation

**Run Java applications on Kubernetes the way you run WebAssembly — ship just your
app (a fat JAR, a layered classpath, or a module), no Dockerfile, no base image.**

This directory is the complete, task-oriented documentation for Brewlet. If you
want the elevator pitch and the "why", start with the [project landing page](/);
if you want the deep architecture and design rationale, read the
[SPECIFICATION](https://github.com/brewlet/brewlet/blob/main/specs/SPECIFICATION.md). These pages sit in between: they tell you how
to actually **install, configure, deploy, tune, and operate** Brewlet.

Brewlet is developed in the
[`brewlet/brewlet`](https://github.com/brewlet/brewlet) monorepo. Runtime code
lives at its root, Kubernetes resources in
[`kubernetes/`](https://github.com/brewlet/brewlet/tree/main/kubernetes), Maven
goals in [`maven-plugin/`](https://github.com/brewlet/brewlet/tree/main/maven-plugin),
architecture contracts in
[`specs/`](https://github.com/brewlet/brewlet/tree/main/specs), and runnable
examples in
[`integration-tests/`](https://github.com/brewlet/brewlet/tree/main/integration-tests).

> ⚠️ Brewlet is not production-ready. These docs cover functionality available in
> the current release. Future work is kept separately in the
> [roadmap](https://github.com/brewlet/brewlet/blob/main/ROADMAP.md).

---

## Where to start

| If you are a… | Start here |
|---|---|
| **Developer** shipping a Java service | [Building & publishing application artifacts](building-and-publishing.md) → [Deploying workloads](deploying-workloads.md) |
| **Platform / cluster operator** enabling Brewlet on a cluster | [Installation](installation.md) → [Configuration](configuration.md) → [JDK management](jdk-management.md) |
| **Anyone** who wants to try the released CLI locally | [Getting started](getting-started.md) |
| **Someone evaluating** the idea | [Concepts & architecture](concepts.md) |
| **Someone tracking planned work** | [Roadmap](https://github.com/brewlet/brewlet/blob/main/ROADMAP.md) |

---

## Table of contents

### Understand it
- **[Concepts & architecture](concepts.md)** — the model, the SpinKube comparison, the
  component inventory, and the end-to-end build/run flow.

### Try it
- **[Getting started](getting-started.md)** — download the released CLI, build
  the demo JAR, package and inspect it as an OCI artifact, run it with a
  node-resident JDK, and preview the shim's runtime bundle.
- **[Example: Spring PetClinic](spring-petclinic.md)** — the same flow with the
  **real upstream Spring Boot app**: build the fat JAR, ship only the JAR, run it
  via `shim → runc` under cgroups, and deploy it as a `JavaApplication`.

### Run it on a cluster
- **[Installation](installation.md)** — prerequisites, the SpinKube-style `helm
  install`, the manual (no-Helm) path, and how to verify the fleet is ready.
- **[Configuration](configuration.md)** — every knob: Helm values, provisioner
  env vars, operator/admission flags, the RuntimeClass, and precedence rules.
- **[JDK management](jdk-management.md)** — installing, versioning, patching, and
  multi-arch JDK runtime roots on nodes (copy-from-image).
- **[Launchers](launchers.md)** — vanilla `java` vs. `jaz`, installing launcher
  layers, choosing one, and how launcher selection is resolved.

### Ship workloads
- **[Building & publishing application artifacts](building-and-publishing.md)** — build a
  fat JAR (or a layered classpath app), author the launch config, and push it with the `brewlet` CLI or ORAS.
- **[Deploying workloads](deploying-workloads.md)** — the raw `Deployment` path,
  the `JavaApplication` CRD, and requesting a specific JDK/launcher via annotations.
- **[Resource requests, limits & JVM tuning](resource-tuning.md)** — how requests
  affect scheduling/HPA, limits become cgroup constraints, and the container-aware
  JVM (and `jaz`) react.

### Operate it
- **[Security](security.md)** — isolation model, non-root defaults, artifact
  integrity, and the sharp edge of privileged node provisioning.
- **[Observability & day‑2](observability.md)** — networking, logs, metrics,
  probes, JDK upgrades, and multi-arch operations.
- **[Multi-architecture fleets](multi-arch.md)** — run portable JARs across
  `amd64` and `arm64`, and constrain workloads that bundle native libraries.
- **[Troubleshooting](troubleshooting.md)** — failure modes, what they look like,
  and how to fix them.

### Reference
- **[CLI reference](cli-reference.md)** — `brewlet push / inspect / run / bundle / jdks`.
- **[Reference](reference.md)** — labels & annotations, OCI media types, the
  artifact & launch-config schema, well-known paths, and a glossary.
- **[JPMS support](jpms-support.md)** — how Brewlet runs modular
  (JPMS) apps on the module path rather than only fat JARs; `entry.mode: module` and the
  optional module/classpath layer.
- **[Layered classpath deployment](layered-classpath-deployment.md)** —
  splitting an app into stable dependency layers + a thin app layer for registry
  dedup and faster pulls; the `classpath.layer.v1+tar` layer and `entry.classPath`.
- **[Runnable-image delivery](runnable-image.md)** — `brewlet push --format=image`
  publishes the JAR as a standard, kubelet-pullable OCI image so a
  `runtimeClassName: brewlet` pod can set `image: <ref>` and let kubelet pull +
  unpack it (the WASI/SpinKube pull path), instead of custom media types that
  `ImagePullBackOff`.

### Planned work

- **[Roadmap](https://github.com/brewlet/brewlet/blob/main/ROADMAP.md)** — proposed capabilities and known follow-up work.
  Roadmap items are not part of the shipped feature set.

---

## How the pieces fit together (one diagram)

```mermaid
flowchart LR
    subgraph dev["Developer / CI"]
        A["mvn package → app.jar"] --> B["brewlet push (only the JAR)"]
    end
    subgraph reg["OCI Registry"]
        R[("OCI artifact + launch config")]
    end
    subgraph cp["Kubernetes control plane"]
        OP["brewlet-operator + admission webhook"]
        POD["Pod: runtimeClassName: brewlet"]
        OP -->|reconcile / stamp / steer| POD
    end
    subgraph node["Provisioned node"]
        CD["containerd"] --> SHIM["containerd-shim-brewlet-v2"] --> RUNC["runc sandbox"] --> JVM["java -jar /app/app.jar"]
        JDK[["node JDK installation (shared, RO)"]] -.->|mounted RO| JVM
    end
    B -->|push| R
    OP -.->|provision| node
    POD -->|scheduled| CD
    R ==>|shim pulls JAR| SHIM
```

See [Concepts & architecture](concepts.md) for the full walkthrough.
