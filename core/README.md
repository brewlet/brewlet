# Brewlet core

[![CI](https://github.com/brewlet/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/brewlet/brewlet/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/brewlet/brewlet?filename=core%2Fgo.mod)](go.mod)
[![Release](https://img.shields.io/github/v/release/brewlet/brewlet?sort=semver)](https://github.com/brewlet/brewlet/releases/latest)
[![License](https://img.shields.io/github/license/brewlet/brewlet)](../LICENSE)

This Go module contains the Brewlet command-line interface, OCI artifact
tooling, shared runtime packages, and containerd Runtime v2 shim.

```text
cmd/brewlet/                         Brewlet CLI
cmd/brewlet-metrics-exporter/        Node-local Prometheus exporter
internal/                            Artifact, diagnostics, inventory, and runtime packages
shim/cmd/containerd-shim-brewlet-v2/ containerd runtime shim
```

When runtime metrics are enabled, the shim emits best-effort launch and AppCDS
telemetry to the node exporter over a Unix datagram socket. The contract
intentionally excludes artifact references, digests, pod names, and arbitrary
errors from labels. Metrics are opt-in in the Helm chart.

See the [metrics exporter README](cmd/brewlet-metrics-exporter/README.md) for
live integration screenshots that can be reused by the documentation site.

`BREWLET_METRICS_DIR` is a legacy compatibility path for external Prometheus
textfile collectors. Such collectors must remove consumed files; the built-in
exporter uses the Unix socket and does not read that directory.

From the repository root, build and test the module with:

```bash
go -C core build ./...
go -C core test ./...
```

## Managed dependency bundles

Platform teams can publish an approved dependency lock and deterministic
classpath tar as a managed OCI bundle:

```bash
brewlet dependency-bundle dependencies.tar platform/spring-web:2026.08 \
  --store target/dependency-bundle-oci \
  --name spring-web \
  --version 2026.08 \
  --source-bom com.example.platform:approved-spring-bom:2026.08 \
  --lock dependency-lock.json \
  --signing-key platform-builder.pem \
  --signer-identity https://ci.example.com/platform-bundles \
  --compatible-jdks 21,25
```

Applications then compose that bundle with a thin JAR:

```bash
brewlet push target/orders.jar apps/orders:1.4.2 \
  --store target/application-oci \
  --dependency-bundle platform/spring-web:2026.08 \
  --dependency-lock target/dependency-lock.json \
  --trusted-public-key platform-builder.pub.pem \
  --trusted-signer-identity https://ci.example.com/platform-bundles \
  --signing-key application-builder.pem \
  --builder-identity https://ci.example.com/application-publisher \
  --main-class com.example.OrdersApplication
```

The application lock must describe its resolved Maven runtime graph and match
the bundle lock exactly. The bundle owns a standard gzip OCI classpath layer tagged with
`brewlet.sh/layer=classpath`. The application image reuses that exact descriptor
and blob, rather than repacking it. Brewlet rejects nested dependency JARs in the
application JAR and records versioned managed-dependency evidence on the final
image manifest. `brewlet inspect` displays the source BOM, bundle, layer, lock,
and application JAR digests.

These Go CLI commands read and write OCI layouts through `--store`; they do not
pull managed bundles directly from a registry. The Go CLI also does not resolve a
Maven graph: callers must supply the canonical lock with `--lock` or
`--dependency-lock`. Use the Maven plugin for BOM import, Maven graph resolution,
and direct registry publication/consumption.

Bundle publication always generates a CycloneDX 1.5 SBOM. Supplying signing
credentials additionally creates a DSSE/in-toto provenance referrer; when that
referrer exists, application publication requires trust credentials and validates
it before composition. Application signing is also optional and, when enabled,
attaches a managed-dependency attestation whose subject is the final image index
digest. Generate a local ECDSA P-256 key pair with:

```bash
brewlet keygen --private builder.pem --public builder.pub.pem
```

`brewlet inspect` always labels the manifest annotation as informational. Pass
`--trusted-public-key` and `--trusted-signer-identity` to cryptographically
verify the signed referrer and display its complete predicate.

The key-based DSSE format is compatible with the in-toto/Sigstore signing model,
but this implementation does not contact Fulcio or Rekor. Key custody, rotation,
and public-key distribution belong to the platform's signing policy.
