# Brewlet roadmap

Brewlet's documentation describes the functionality available in current
releases. This roadmap collects ideas that are being explored but do not ship
today. Items are not commitments and have no release date until they are
accepted and implemented.

## Runtime and node operations

- **Safer containerd reconfiguration.** Validate generated containerd
  configuration, restart through a reversible path, restore the previous
  configuration after failure, and extend readiness checks to custom launchers.
  See [proposal 0002](specs/proposals/0002-validated-node-reconfig.md).
- **Stable capability labels.** Promote the existing JDK, launcher, runtime, and
  architecture labels from implementation details to a versioned public
  contract suitable for Cluster Autoscaler and Karpenter integrations. See
  [proposal 0003](specs/proposals/0003-capability-label-taxonomy.md).
- **Managed admission certificates.** Add an opt-in cert-manager integration for
  webhook certificate issuance and rotation. See
  [proposal 0004](specs/proposals/0004-cert-manager-admission.md).
- **Stronger sandbox options.** Evaluate supported gVisor and Kata Containers
  configurations for workloads that need stronger isolation than runc.
- **Node-level runtime metrics.** Export per-sandbox resource and JVM runtime
  metrics from the node runtime.
- **Node profile refinements.** Evaluate profile-managed taints and tolerations,
  an operator-synthesized default profile, a documented bare-metal pool label,
  and garbage collection for JDK roots no longer used by ready workloads.
- **Configuration cleanup.** Consolidate the simple Helm values and `NodeProfile`
  configuration paths before deprecating redundant global inventory flags.

## Workload delivery

- **Broader supply-chain admission.** A Brewlet-native example already ships in
  [`admission/`](admission/): a Ratify external verifier plugin plus Gatekeeper
  policy that admits digest-pinned `runtimeClassName: brewlet` pods only when
  their image carries a valid managed-dependency DSSE/in-toto attestation.
  Ecosystem-compatible cosign signatures, standard SLSA provenance, keyless
  identity, and admission policy for ordinary Brewlet runnable images remain
  future work; track that scope in [issue #3](https://github.com/brewlet/brewlet/issues/3).
- **Replica coalescing.** Allow an opt-in `JavaApplication` capacity model that
  can realize logical replicas as fewer, larger JVMs. See
  [proposal 0005](specs/proposals/0005-replica-coalescing.md).
- **Gradle plugin.** Provide the artifact build, publish, inspection, and
  manifest workflow currently available through the Maven plugin.
- **Additional multi-architecture guardrails.** Expand architecture coverage
  observability and safeguards for workloads with accelerator or native-library
  constraints.
- **Ahead-of-time startup options.** Track Project Leyden and related JDK
  capabilities as they become suitable for Brewlet workloads.

## Ecosystem questions

- Define the default JDK distribution policy and LTS upgrade model for node
  pools.
- Evaluate a standard media type for Java applications distributed through OCI
  registries.
- Establish cold-start service-level objectives that clarify when AppCDS
  archives are preferable to relying on the shared node JDK.

Detailed designs live in [`specs/proposals`](specs/proposals/). The
[specification](specs/SPECIFICATION.md) remains limited to behavior provided by
Brewlet today.
