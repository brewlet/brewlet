# Roadmap

> **Nothing on this page is available in the current Brewlet release.**
> The rest of the documentation describes functionality that ships today.

This page collects proposed capabilities and known follow-up work. Priorities and
designs may change before implementation. For accepted architecture contracts, see
the [Brewlet specification](https://github.com/brewlet/brewlet/blob/main/specs/SPECIFICATION.md);
for active design work, see the
[proposals directory](https://github.com/brewlet/brewlet/tree/main/specs/proposals).

## Developer tooling

- **Gradle plugin:** provide the build, push, manifest, inspect, layering, AppCDS,
  and runnable-image workflows available in the Maven plugin.
- **CLI dependency splitting:** build reproducible classpath layers directly from
  a dependency directory instead of requiring prebuilt tar files.

## Runtime delivery and lifecycle

- **Native-artifact pre-puller:** distribute native Brewlet artifacts to nodes
  without manual content-store imports.
- **JDK root garbage collection:** retire unused node JDK roots after workloads
  have migrated.
- **Validated containerd reconfiguration:** add reversible configuration updates,
  full-restart support where `SIGHUP` is insufficient, and launcher probes. See
  [proposal 0002](https://github.com/brewlet/brewlet/blob/main/specs/proposals/0002-validated-node-reconfig.md).

## Security and isolation

- **Signature and provenance admission:** verify cosign signatures and SLSA
  provenance against digest-pinned artifacts, preferably through an existing
  policy engine, with a fail-closed Brewlet webhook as an optional integration.
- **Stronger sandbox tiers:** evaluate gVisor for an OCI-compatible isolation tier
  and Kata Containers for workloads that require a virtual-machine boundary.
- **cert-manager integration:** manage admission-webhook serving certificates and
  CA injection. See
  [proposal 0004](https://github.com/brewlet/brewlet/blob/main/specs/proposals/0004-cert-manager-admission.md).

## Observability

- **Brewlet-specific Prometheus metrics:** expose launch timing, artifact cache
  outcomes, JDK inventory and patch age, admission decisions, and AppCDS archive
  effectiveness.
- **Fleet coverage views:** report architecture-by-JDK availability so operators
  can identify scheduling gaps before deployment.

## Portability guardrails

- **Accelerator validation:** prevent architecture-specific accelerators such as
  shipped AppCDS archives from silently narrowing an otherwise portable artifact.
- **Capability-label contract:** stabilize autoscaler-friendly JDK and launcher
  labels. See
  [proposal 0003](https://github.com/brewlet/brewlet/blob/main/specs/proposals/0003-capability-label-taxonomy.md).

## Additional runtimes

- **.NET runtime family:** generalize the artifact, provisioner, shim, admission,
  and workload APIs for framework-dependent .NET applications without forking the
  Java path. Node.js and Python remain lower-priority candidates because their
  dependency and native-extension models require more packaging policy.

## Performance

- **Project Leyden / AOT cache integration:** evaluate newer JDK AOT cache
  mechanisms after their compatibility and lifecycle behavior is stable enough for
  centrally patched node runtimes.

## Verification

- **Registry-backed runnable-image coverage:** exercise kubelet pulling a Brewlet
  runnable image from a registry in the end-to-end suite, complementing the current
  content-store import path.
