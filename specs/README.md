# Brewlet specifications

This repository is the authoritative source for Brewlet architecture and
compatibility contracts. Implementations and user documentation must follow
[`SPECIFICATION.md`](SPECIFICATION.md); substantial changes should begin as a
reviewable design in [`proposals/`](proposals/).

## Versioning and citations

The specification is a living, versioned contract. Backward-incompatible
changes require an explicit version transition, while compatible clarifications
and additions may land incrementally. Brewlet repositories cite requirements by
section using the existing `§N` convention (for example, `§4.2`).

## Implementations

- [brewlet/brewlet](https://github.com/brewlet/brewlet) — core runtime, CLI,
  containerd shim, and node provisioner
- [brewlet/kubernetes](https://github.com/brewlet/kubernetes) — Kubernetes
  operator, admission webhooks, CRDs, manifests, and Helm chart
- [brewlet/maven-plugin](https://github.com/brewlet/maven-plugin) — Maven build
  and publishing integration

Changes here describe the contract; implementation-specific behavior belongs in
the repository that owns that component.

## Supporting repositories

- [brewlet/site](https://github.com/brewlet/site) — user and operator documentation
- [brewlet/integration-tests](https://github.com/brewlet/integration-tests) —
  cross-repository validation and fixture applications

The repositories are independently versioned. A specification change that affects
multiple implementations should use linked pull requests in each owning repository
and a corresponding integration-test update where appropriate.
