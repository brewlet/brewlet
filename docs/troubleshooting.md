# Troubleshooting

A field guide to what can go wrong, what it looks like, and how to fix it. The
failure-mode summary is from [SPECIFICATION §14](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md).

---

## Failure modes at a glance

| Scenario | Behavior you'll see | Where to look |
|---|---|---|
| No compatible JDK on any ready node | Pod stays `Pending`; event `NoCompatibleJDK`; scheduler skips nodes | [→ JDK issues](#pod-is-pending-with-nocompatiblejdk) |
| Requested launcher not installed | Pod stays `Pending`; event `NoCompatibleLauncher`; scheduler skips nodes | [→ launcher issues](#pod-is-pending-with-nocompatiblelauncher) |
| OCI artifact missing/unauthorized | `ImagePull`-style failure on the pod | [→ artifact pull](#imagepull-style-failure) |
| Signature/provenance verification fails | Admission denied (if policy enabled) with a clear reason | [Security](security.md) |
| JVM OOM | `ExitOnOutOfMemoryError` → exit → kubelet restart | [→ OOM](#pod-restarts--oomkilled) |
| Node provisioning fails | Node not labeled `ready`; condition/event `ProvisionFailed` | [→ provisioning](#node-never-becomes-ready) |
| Shim crash | containerd reports task failure; pod restarts | [→ shim](#task--shim-failures) |
| cgroup v1-only node | Provisioner refuses; node not marked ready | [→ provisioning](#node-never-becomes-ready) |

---

## Node never becomes ready

**Symptom:** `kubectl get nodes -L brewlet.sh/runtime` shows no `ready`, or
`brewlet.sh/provision-state=Failed`.

```bash
# Is the node opted in?
kubectl get node <n> -o jsonpath='{.metadata.labels.brewlet\.sh/provision}{"\n"}'
# Provisioner pod on that node:
kubectl get pods -n brewlet -o wide | grep <n>
kubectl logs -n brewlet <provisioner-pod>
kubectl get events --field-selector reason=ProvisionFailed
```

**Common causes & fixes:**

- **cgroup v1-only node** — the provisioner refuses it (cgroup v2 is required). Move
  the node to a cgroup v2 kernel/config, or exclude it.
- **JDK copy-from-image failed** — the node can't reach the vendor JDK image, or an
  uncurated distribution was requested. Verify the containerd socket mount and image
  pull access, mirror the image, or request `temurin`/`microsoft`. See
  [JDK management](jdk-management.md#installation-copy-from-image).
- **can't reach the registry / socket** — verify the containerd
  socket mount and that the node can pull the JDK image.
- **containerd didn't pick up the runtime** — the provisioner reloads via `SIGHUP`;
  on some distros a full `systemctl restart containerd` is needed. Restart
  containerd on the node and re-check.
- **RBAC** — the provisioner needs `get`/`patch` on nodes to label them; confirm the
  ServiceAccount/ClusterRole from the manifest/chart are present.

---

## Pod is Pending with `NoCompatibleJDK`

**Symptom:** the pod won't schedule; a `NoCompatibleJDK` event is recorded.

The pod requested a JDK (`spec.jvm.version` via `JavaApplication`, or raw pod
annotation `brewlet.sh/jdk`) that no ready node advertises.

```bash
# What does the pod ask for?
kubectl get pod <pod> -o jsonpath='{.metadata.annotations.brewlet\.sh/jdk}{"\n"}'
# What do nodes offer?
kubectl get nodes -o custom-columns=NODE:.metadata.name,JDKS:.metadata.annotations.brewlet\\.sh/jdks
```

**Fix:** either request a JDK the fleet has, or add the JDK to the inventory:

```bash
helm upgrade brewlet ./charts/brewlet --set provisioner.jdks="temurin-21,temurin-25"
```

Then wait for nodes to re-provision (`brewlet.sh/jdks` updates) and re-deploy.

---

## Pod is Pending with `NoCompatibleLauncher`

Same as above, for launchers. The pod requested `spec.jvm.launcher` or
`brewlet.sh/launcher: <name>` that no ready node has.

```bash
kubectl get nodes -o custom-columns=NODE:.metadata.name,LAUNCHERS:.metadata.annotations.brewlet\\.sh/launchers
```

**Fix:** add the launcher to the inventory (`provisioner.launchers="jaz"`) and
re-provision, or drop the request to use the vanilla `java` launcher (omit the
annotation). See [Launchers](launchers.md#installing-jaz-on-nodes).

---

## ImagePull-style failure

**Symptom:** the pod can't fetch the OCI artifact.

- **Wrong ref / not pushed** — verify the artifact exists:
  `oras manifest fetch <ref>` (or `brewlet inspect <ref>` for the local layout).
- **Unauthorized** — add/verify `imagePullSecrets` (or `artifact.pullSecrets` in the
  `JavaApplication`).
- **Not a Brewlet artifact** — confirm it was pushed with the brewlet media types
  ([Reference](reference.md#oci-media-types)), not as a plain image.

---

## Pod restarts / OOMKilled

**Symptom:** the pod restarts under load or is `OOMKilled`.

- The heap likely has no non-heap headroom. Set
  `-XX:MaxRAMPercentage=75.0` (not 100%) so Metaspace/threads/code-cache/direct
  buffers fit under `limits.memory`. See [Resource tuning](resource-tuning.md#memory--leave-headroom-for-non-heap).
- Add `-XX:+ExitOnOutOfMemoryError` so an OOM is a clean restart, not a hang.
- Or switch to `jaz`, which sizes the heap from the cgroup automatically.
- Raise `limits.memory` if the workload genuinely needs more.

---

## Task / shim failures

**Symptom:** containerd reports a task failure; the pod restarts or won't start.

```bash
kubectl describe pod <pod>                 # events from kubelet/containerd
# On the node:
journalctl -u containerd | grep -i brewlet
ls -l /opt/brewlet/bin/containerd-shim-brewlet-v2
grep -A2 runtimes.brewlet /etc/containerd/config.toml
```

- **Shim not on containerd's PATH** — confirm it's installed to `/usr/local/bin` and
  the `runtimes.brewlet` block exists in `config.toml`; restart containerd.
- **JDK root missing** — confirm `/opt/brewlet/jdks/<dist>-<feature>/bin/java`
  exists on the node.
- **Arch mismatch** — the shim is arch-specific; ensure the provisioner image matches
  the node arch (the build compiles it per-arch).

---

## Webhook / admission problems

**Symptom:** artifact annotations aren't stamped, or scheduling isn't steered.

- With `admission.failurePolicy: Ignore` (default), a webhook outage silently lets
  pods through **without** stamping/steering — check the webhook is healthy:
  ```bash
  kubectl get pods -n brewlet -l app=brewlet-admission
  kubectl logs -n brewlet -l app=brewlet-admission
  ```
- **caBundle/cert mismatch** after a `helm upgrade` should self-heal (Helm rotates
  cert + `caBundle` together). In production use cert-manager. See
  [Configuration](configuration.md#admission-webhook).
- Non-brewlet pods are intentionally passed through untouched.

---

## Local PoC issues

- **The harness cannot find a component checkout** — clone `brewlet/brewlet` and
  `brewlet/kubernetes` beside `integration-tests`, or set `BREWLET_CORE_DIR` and
  `BREWLET_KUBERNETES_DIR`.
- **Tier 2 cannot build a fixture** — check `JAVA_HOME` points at a full JDK 21+.
- **Core or Kubernetes build fails** — Go 1.26+ is required.
- **Tier 3 skips or fails before runc** — Docker must be installed and reachable;
  the tier runs a privileged Linux container.
- **Non-Linux dev host** — only the portable bundle-assembly core builds locally;
  use integration-test tier 3 to exercise the Linux/runc path. See
  [Getting started](getting-started.md).

---

## Still stuck?

- Re-read the relevant guide: [Installation](installation.md),
  [Configuration](configuration.md), [JDK management](jdk-management.md),
  [Launchers](launchers.md), [Resource tuning](resource-tuning.md).
- The provisioner entrypoint is **idempotent** — safe to let it re-run after you fix
  node state.
- Component logs: `kubectl logs -n brewlet <operator|admission|provisioner pod>`.
