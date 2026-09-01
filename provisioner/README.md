# Brewlet node provisioner

[![CI](https://github.com/brewlet/brewlet/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/brewlet/brewlet/actions/workflows/ci.yml)
[![Release](https://github.com/brewlet/brewlet/actions/workflows/release.yml/badge.svg)](https://github.com/brewlet/brewlet/actions/workflows/release.yml)
[![node-provisioner image](https://img.shields.io/badge/ghcr.io-brewlet%2Fnode--provisioner-blue?logo=docker)](https://github.com/brewlet/brewlet/pkgs/container/node-provisioner)
[![License](https://img.shields.io/github/license/brewlet/brewlet)](../LICENSE)

The node provisioner prepares a Linux Kubernetes node to run Brewlet workloads.
Its privileged entrypoint installs the containerd shim, materializes configured
JDK runtime roots and optional launcher layers, registers the `brewlet`
containerd runtime, validates the installation, and advertises node readiness.

See the [Brewlet specification](../specs/SPECIFICATION.md) for the node
provisioning model and the [user documentation](https://github.com/brewlet/site)
for installation and operations guidance.

## Build

From the repository root:

```bash
make provisioner-image
make provisioner-image-push
```

Override the destination when needed:

```bash
make provisioner-image PROVISIONER_IMAGE=ghcr.io/acme/brewlet-provisioner:1.2.3
```

The multi-stage Docker build compiles
`containerd-shim-brewlet-v2` and `brewlet-metrics-exporter` for the target Linux
architecture and packages them with the host installation entrypoint.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `JDKS` | `temurin-21` | Comma-separated `<distribution>-<feature>` JDK roots |
| `JDK_CUSTOM_SOURCE_COUNT` | `0` | Number of indexed custom JDK image sources |
| `JDK_CUSTOM_SOURCE_<n>_TOKEN` | empty | Custom inventory token, such as `zulu-21` |
| `JDK_CUSTOM_SOURCE_<n>_IMAGE` | empty | Fully qualified OCI image reference containing the runtime and its userland |
| `JDK_CUSTOM_SOURCE_<n>_JAVA_HOME` | empty | Absolute JDK or jlink runtime path inside that image |
| `LAUNCHERS` | empty | Optional launcher layers, such as `jaz` |
| `NODE_NAME` | downward API | Kubernetes node to label |
| `BREWLET_PREFIX` | `/opt/brewlet` | Host installation prefix |
| `CONTAINERD_CONFIG` | `/etc/containerd/config.toml` | containerd configuration |
| `CONTAINERD_DROPIN_DIR` | `/etc/containerd/config.toml.d` | Host drop-in directory used when the primary config imports it |
| `CONTAINERD_DROPIN_FILE` | `<drop-in-dir>/99-brewlet.toml` | Brewlet-managed runtime drop-in |
| `CONTAINERD_ADDRESS` | `/run/containerd/containerd.sock` | containerd socket |
| `CONTAINERD_NAMESPACE` | `k8s.io` | containerd namespace |
| `BREWLET_MODE` | `provision` | `provision` installs; `cleanup` reverses it |
| `BREWLET_CONTAINERD_RESTART` | `validated` | `validated`, `sighup`, or `none` |
| `BREWLET_VALIDATE` | `true` | Run JDK and launcher smoke tests before readiness |
| `MIRRORS` | empty | Registry mirror mappings |

The curated JDK distributions are `temurin` and `microsoft`. A `NodeProfile` can
also supply an image and Java home for another distribution, including a
platform-built jlink runtime; the operator renders the indexed custom-source
variables automatically. The provisioner copies the image's complete root
filesystem so the sandbox has the loader and native libraries required by
`bin/java`. The source image need not contain shell or copy tools.

Provisioning is idempotent, records the source image and Java home, and
reinstalls a token when either changes. Runtime roots retain the source image's
filesystem modes; the shim keeps the shared lower layer and Java-home bind mount
read-only for workloads.

## Containerd configuration

The default `validated` mode checks whether the host's primary containerd
configuration imports `/etc/containerd/config.toml.d/*.toml`. When it does,
Brewlet writes only `99-brewlet.toml`; otherwise it appends the same runtime
block to the primary configuration and preserves the original as
`config.toml.brewlet.bak`.

Before activation, the provisioner runs
`containerd --config /etc/containerd/config.toml config dump` in the host mount
namespace. Provisioning fails unless the configuration parses and the dumped
effective configuration contains the `brewlet` runtime handler. A failed render
is removed or restored before exit, the node remains unready, and
`brewlet.sh/provision-error` reports either a rejected config dump or a missing
handler.

In the default `validated` restart mode, a changed containerd configuration is
activated with `systemctl restart containerd` from the host PID namespace. The
provisioner then checks the containerd socket and queries the live CRI status to
verify that the `brewlet` runtime handler is registered. A restart or
health-check failure restores the primary configuration backup (or removes the
renderer's drop-in), restarts containerd, verifies ordinary containerd recovery,
leaves the node unready, and sets an actionable
`brewlet.sh/provision-error`. A recovery failure is reported separately as
`rollback-failed`.

Re-running an unchanged valid render still verifies the effective configuration
and health-checks containerd without an unnecessary restart. `sighup` retains
the legacy in-place render and reload behavior without the config-dump gate,
while `none` is the immutable-image mode and does not mutate or signal
containerd.

When Helm runtime metrics are enabled, the profile-managed DaemonSet includes a
best-effort exporter sidecar that serves `/metrics` and listens for shim
telemetry on `/opt/brewlet/metrics/telemetry.sock`. It also reads the installed
JDK and launcher roots directly, including each JDK's exact `release` metadata,
source image, and node installation timestamp. The sidecar is disabled by
default.

Copy-from-image commands run through the bundled `ctr` client in the host mount
namespace. This is required because the provisioner connects to the host
containerd socket and unpack mounts must be visible in the node's namespace.

## Readiness validation

With `BREWLET_VALIDATE=true`, the provisioner runs a deterministic one-shot
probe for every configured runtime component before it advertises runtime or
capability labels:

- each JDK root runs `bin/java -version` inside its installed root;
- each `jaz` launcher layer runs with `JAZ_PRINT_VERSION=1` and
  `JAZ_EXIT_WITHOUT_FLUSH=1`.

A missing or non-executable launcher, or a failed probe, leaves the node
unready and records a bounded launcher-specific reason such as
`launcher-jaz-probe-failed` in `brewlet.sh/provision-error`. No JDK or launcher
capability labels are retained after failure. `BREWLET_VALIDATE=false` skips
both JDK and launcher probes and preserves the opt-out behavior.

> The provisioner is privileged and host-mutating. Run it only on nodes
> controlled by the platform team.
