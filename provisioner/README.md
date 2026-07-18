# Brewlet node provisioner

The node provisioner prepares a Linux Kubernetes node to run Brewlet workloads.
Its privileged entrypoint installs the containerd shim, materializes configured
JDK runtime roots and optional launcher layers, registers the `brewlet`
containerd runtime, validates the installation, and advertises node readiness.

See the [Brewlet specification](https://github.com/brewlet/specs) for the node
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
`containerd-shim-brewlet-v2` for the target Linux architecture and packages it
with the host installation entrypoint.

## Configuration

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `JDKS` | `temurin-21` | Comma-separated `<distribution>-<feature>` JDK roots |
| `LAUNCHERS` | empty | Optional launcher layers, such as `jaz` |
| `NODE_NAME` | downward API | Kubernetes node to label |
| `BREWLET_PREFIX` | `/opt/brewlet` | Host installation prefix |
| `CONTAINERD_CONFIG` | `/etc/containerd/config.toml` | containerd configuration |
| `CONTAINERD_ADDRESS` | `/run/containerd/containerd.sock` | containerd socket |
| `CONTAINERD_NAMESPACE` | `k8s.io` | containerd namespace |
| `BREWLET_MODE` | `provision` | `provision` installs; `cleanup` reverses it |
| `BREWLET_CONTAINERD_RESTART` | `validated` | `validated`, `sighup`, or `none` |
| `BREWLET_VALIDATE` | `true` | Run JDK smoke tests before readiness |
| `MIRRORS` | empty | Registry mirror mappings |

The curated JDK distributions are `temurin` and `microsoft`. Provisioning is
idempotent, and installed JDK roots are made read-only for workload sharing.

> The provisioner is privileged and host-mutating. Run it only on nodes
> controlled by the platform team.
