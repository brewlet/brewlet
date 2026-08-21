# Brewlet

Brewlet runs Java applications packaged as OCI artifacts with a node-resident
JDK. This monorepo contains the runtime, Kubernetes platform, Maven plugin,
specification, and end-to-end tests.

- [Brewlet specification](specs/SPECIFICATION.md)
- [User documentation](https://github.com/brewlet/site)

## Repository layout

| Path | Purpose |
| --- | --- |
| `cmd/brewlet/` | CLI for artifacts, runtime bundles, JDK inventory, and cluster diagnostics |
| `internal/artifact/` | OCI artifact formats and local OCI-layout storage |
| `internal/doctor/` | Kubernetes readiness and developer-access diagnostics |
| `internal/inventory/` | Node JDK inventory parsing and rendering |
| `internal/runtime/` | JVM launch configuration and OCI bundle assembly |
| `shim/` | containerd Runtime v2 shim and portable bundle preparation |
| `provisioner/` | Linux node-provisioner image and host installation entrypoint |
| `kubernetes/` | Operator, admission webhooks, CRDs, manifests, and Helm chart |
| `maven-plugin/` | Maven build and OCI publishing integration |
| `specs/` | Architecture, compatibility contracts, and proposals |
| `integration-tests/` | End-to-end harness and fixture applications |

## Requirements

- Go 1.26 or newer
- A JDK 17 or newer for the optional AppCDS integration test
- Docker with Buildx to build or publish the node-provisioner image

Linux-specific shim services retain their `linux` build constraints. On other
platforms, the portable bundle-assembly implementation is built instead.

## Build and test

```bash
make check-all
```

`make check-all` validates the core runtime, Kubernetes platform, Maven plugin,
and host-only integration tiers. Run `make check` for the core runtime alone.
The AppCDS integration test automatically skips when a suitable JDK is not
available.

Build binaries into `bin/`:

```bash
make binaries
```

Build or publish the Linux node-provisioner image:

```bash
make provisioner-image
make provisioner-image-push
```

Override `REGISTRY`, `TAG`, or `PROVISIONER_IMAGE` to publish elsewhere.

## Releases

Tags matching `v*` publish:

- multi-architecture `operator`, `admission`, and `node-provisioner` images to
  `ghcr.io/brewlet`;
- the Helm chart to `oci://ghcr.io/brewlet/charts/brewlet`; and
- signed-version CLI archives plus checksums on the GitHub release.

The chart's default component image tag follows its `appVersion`, so installing
a versioned chart selects the matching platform images automatically.

Install the current release:

```bash
helm upgrade --install brewlet oci://ghcr.io/brewlet/charts/brewlet \
  --version 0.1.0 \
  --namespace brewlet \
  --create-namespace \
  --set-string provisioner.jdks=temurin-21
```

Download the matching CLI archive for Linux or macOS from
[GitHub Releases](https://github.com/brewlet/brewlet/releases/tag/v0.1.0), then
run `brewlet doctor --namespace <developer-namespace>` before handing the
cluster to application developers.

## License

This project is licensed under the terms in [LICENSE](LICENSE).
