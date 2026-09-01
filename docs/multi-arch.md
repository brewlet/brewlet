# Multi-architecture fleets

Pure Java bytecode is architecture-neutral, so the same Brewlet artifact can run
on provisioned `amd64` and `arm64` nodes. Brewlet installs a JDK root for each
node's architecture and publishes its operator, provisioner, admission, and shim
components for both architectures.

## Portable JARs

For a JAR without native libraries, omit the architecture constraint. Kubernetes
can schedule the workload on any ready node that offers the requested JDK and
launcher:

```yaml
apiVersion: brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders
spec:
  artifact:
    image: registry.example.com/team/orders:1.4.2
  jvm:
    version: 21
```

The provisioner selects the matching JDK image platform for each node. No
architecture-specific application build is required.

## JARs with native libraries

A JAR that bundles JNI libraries or architecture-specific dependencies is not
portable. Set `spec.arch` on a `JavaApplication`, or `arch` in the launch config:

```yaml
spec:
  arch: [amd64]
```

```json
{
  "schemaVersion": 1,
  "mainJar": "app.jar",
  "entry": { "mode": "jar" },
  "arch": ["amd64"]
}
```

The admission webhook converts this constraint into required node affinity using
the standard `kubernetes.io/arch` label. If the cluster has no compatible ready
node, admission reports `NoCompatibleArch`.

The CLI and Maven plugin scan JARs for bundled `.so`, `.dll`, and `.dylib` files
and can infer the corresponding architecture constraint. Confirm the detected
value when a dependency packages native code under a nonstandard layout.

## AppCDS archives

AppCDS archives are tied to both the exact JDK build and the CPU architecture.
Prefer [node-side regeneration](appcds.md#43-node-side-regeneration-the-durable-answer-for-a-patched-fleet)
to preserve one-artifact-anywhere portability. If you ship a prebuilt archive,
set the artifact's architecture constraint and rely on `-Xshare:auto` for safe
fallback when the archive does not match.

## Operating a mixed fleet

- Keep the same requested JDK features available on every architecture you intend
  to serve.
- Use multi-platform component image references so each node pulls the matching
  operator, provisioner, admission, and shim image.
- Inspect node inventory with `brewlet jdks` and Kubernetes labels before rolling
  out an architecture-constrained application.
- See [JDK management](jdk-management.md#architecture-mapping-multi-arch) and
  [observability](observability.md#day2-multi-arch-fleets) for operational commands.
