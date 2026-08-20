# Proposal 0002 — Validated containerd reconfiguration & readiness smoke gate

- **Status:** Partially implemented
- **Target spec sections:** amend **§5.2** (steps 4–5) + **§14** (failure modes)
- **Related code:** [`brewlet/brewlet`](https://github.com/brewlet/brewlet):
  `provisioner/entrypoint.sh`; [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes):
  `internal/controller` (node lifecycle) and `internal/brewlet`
  (labels/annotations vocabulary)
- **Split from:** the original [0001 (node profiles)](0001-node-profiles.md) draft. This is
  independent of the `NodeProfile` CRD and can ship on the current global-inventory
  model; 0001 consumes it via the `rollout.containerdRestart` / `rollout.validate`
  knobs (0001 §5.1–§5.2) once profiles land.

> **Partially implemented.** The readiness smoke gate and the restart-mode knob
> landed with 0001: the provisioner honors `BREWLET_VALIDATE` (rendered from
> `spec.rollout.validate`) to smoke-test `java -version` from every installed JDK
> root before flipping `runtime=ready`, exposes
> `BREWLET_CONTAINERD_RESTART=validated|sighup|none` (rendered from
> `spec.rollout.containerdRestart`), and records `brewlet.sh/provision-error` on the
> Node for the operator to surface as `ProvisionFailed`.
> **Still outstanding** (this proposal's remaining scope):
> 1. the *reversible* containerd reconfiguration — drop-in + `containerd config dump`
>    validation + `systemctl restart` + backup restore on failure; today `validated`
>    mode still reloads via `SIGHUP` with no automatic rollback, and
> 2. the launcher `<launcher> -version` probe in the smoke gate (only `java -version`
>    runs today).

---

## 1. Summary

Make the provisioner's host mutation **safe to fail**: validate the containerd
reconfiguration before committing it, restart containerd through a validated,
reversible path (drop-in + `config dump` + backup restore on failure), and gate the
`brewlet.sh/runtime=ready` label behind an explicit `java -version` (and, if a
launcher is installed, `<launcher> -version`) **smoke test** run from each installed
root. On any failure the node is left in its prior state and reported `Failed`
instead of half-provisioned.

## 2. Motivation — what today's provisioner does and doesn't guarantee

`entrypoint.sh` already does more than "file exists":

- It runs `java -version` while installing each root (`install_jdk`) and reads
  `-XshowSettings:properties` to build `brewlet.sh/jdks-info`.
- It writes a backup of `config.toml` to `config.toml.brewlet.bak` before appending
  the `runtimes.brewlet` block.

But two real gaps remain:

| Gap | Today | Why it hurts |
|---|---|---|
| **Non-reversible restart** | The block is appended directly to `config.toml`; containerd is reloaded via `SIGHUP` to the host process (`reload_containerd`). A backup is written but **nothing restores it** on a bad edit, and `SIGHUP` is unreliable on some distros. | A malformed edit or a distro that ignores `SIGHUP` can disturb `runc` workloads on the node with no automatic recovery. |
| **Ready label not gated on a final smoke test** | `java -version` runs at *install* time, but the terminal `label_node` step flips `brewlet.sh/runtime=ready` without re-verifying the JDK executes, and never exec-tests launcher binaries. | A root that installs but can't execute (or a broken launcher layer) still advertises the node as ready, and the RuntimeClass schedules onto it. |

## 3. Design

### 3.1 Validated containerd reconfiguration

Add a `containerdRestart` mode to the provisioner (env `BREWLET_CONTAINERD_RESTART`,
default `validated`):

- **`validated`** (default):
  1. Write the `runtimes.brewlet` block to a **drop-in**
     (`/etc/containerd/config.toml.d/`) where the distro's containerd supports it,
     falling back to an in-place append (current behavior) otherwise. Keep the
     existing `config.toml.brewlet.bak` backup.
  2. **Validate** with `containerd config dump` (parse must succeed and contain the
     `runtimes.brewlet` handler).
  3. **Restart** via `nsenter … systemctl restart containerd` (host PID namespace)
     rather than `SIGHUP`, then health-probe that the shim handler resolves.
  4. **On any failure, restore the backup / remove the drop-in, restart containerd
     back to the known-good config,** and exit non-zero so the node is **not**
     marked ready. This is the runtime-class-manager pattern.
- **`sighup`**: today's behavior, retained for environments where a full restart is
  undesirable.
- **`none`**: skip host reconfiguration entirely (the runtime is baked into the node
  image); run only the smoke gate and labelling. This is the label-only / immutable
  path 0001 selects via `rollout.containerdRestart: none`.

### 3.2 Readiness smoke gate

Add `validate` (env `BREWLET_VALIDATE`, default `true`). Before `label_node` flips
`brewlet.sh/runtime=ready`, run a one-shot:

- `java -version` from **every** installed `/opt/brewlet/jdks/<dist>-<feature>/bin/java`, and
- `<launcher> -version` (or the launcher's equivalent probe) for every installed
  launcher layer.

Only if all probes pass does the node get labelled ready. A failure exits non-zero,
leaving the node unready.

### 3.3 Reporting failure back to the operator

The provisioner is a self-labelling DaemonSet; the operator can't see a bash-level
failure. On any validation/reconfig failure the provisioner writes
`brewlet.sh/provision-error=<short-reason>` on the Node (and does **not** set
`runtime=ready`). `NodeReconciler` reads it: it already distinguishes
"working" from "failing" via the provisioner pod state; the annotation makes the
reason explicit in the `ProvisionFailed` event (§14) instead of only inferring
`CrashLoopBackOff`.

## 4. Rollout

1. Provisioner grows `BREWLET_CONTAINERD_RESTART` + `BREWLET_VALIDATE` env (default `validated` /
   `true`); operator surfaces them as flags/Helm values.
2. `NodeReconciler` consumes `brewlet.sh/provision-error` for richer
   `ProvisionFailed` events.
3. Docs (installation / troubleshooting) document the modes.

## 5. Risks & mitigations

- **`systemctl restart` heavier than `SIGHUP`.** Mitigate: it only runs on the first
  successful reconfig (idempotent skip afterwards), restart is validated + reversible,
  and `sighup`/`none` remain available.
- **Drop-in support varies by distro.** Mitigate: fall back to validated in-place
  append; the `config dump` gate + backup restore apply either way.

## Appendix — spec integration points

- **§5.2 step 4:** validated restart with `config dump` + backup restore (not
  `SIGHUP`-and-hope).
- **§5.2 step 5:** `runtime=ready` only after the `java -version` /
  `<launcher> -version` smoke gate passes.
- **§14:** add `brewlet.sh/provision-error` as the reason source for
  `ProvisionFailed`.
