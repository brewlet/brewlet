# Brewlet core runtime

Brewlet runs Java applications packaged as OCI artifacts with a node-resident
JDK. This repository contains the core CLI, OCI artifact implementation,
containerd Runtime v2 shim, and node provisioner.

- [Brewlet specification](https://github.com/brewlet/specs)
- [User documentation](https://github.com/brewlet/site)
- [Kubernetes platform components](https://github.com/brewlet/kubernetes)
- [Cross-repository integration tests](https://github.com/brewlet/integration-tests)

## Repository layout

| Path | Purpose |
| --- | --- |
| `cmd/brewlet/` | CLI for pushing, inspecting, running, and bundling Brewlet artifacts |
| `internal/artifact/` | OCI artifact formats and local OCI-layout storage |
| `internal/inventory/` | Node JDK inventory parsing and rendering |
| `internal/runtime/` | JVM launch configuration and OCI bundle assembly |
| `shim/` | containerd Runtime v2 shim and portable bundle preparation |
| `provisioner/` | Linux node-provisioner image and host installation entrypoint |

The Kubernetes operator, deployment manifests, and Helm chart live in
[`brewlet/kubernetes`](https://github.com/brewlet/kubernetes). Fixture applications
and end-to-end suites live in
[`brewlet/integration-tests`](https://github.com/brewlet/integration-tests).

## Requirements

- Go 1.26 or newer
- A JDK 17 or newer for the optional AppCDS integration test
- Docker with Buildx to build or publish the node-provisioner image

Linux-specific shim services retain their `linux` build constraints. On other
platforms, the portable bundle-assembly implementation is built instead.

## Build and test

```bash
make build
make test
make check
```

`make check` runs the same formatting, vet, build, and race-test checks as CI.
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

## License

This project is licensed under the terms in [LICENSE](LICENSE).
