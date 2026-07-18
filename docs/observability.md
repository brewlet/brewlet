# Observability & day‑2 operations

Because the shim is runc-backed and the workload is an ordinary pod, everything you
already do to observe and operate Kubernetes workloads works unchanged. This page
covers what to expect and the Brewlet-specific day‑2 tasks.

See also [SPECIFICATION §12](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md).

---

## Networking

- **Normal pod IP via CNI** — runc owns the netns the kubelet/containerd provide.
- **Services, Ingress, NetworkPolicy** all behave exactly as for any pod.
- No special CNI configuration is required.

```bash
kubectl get pod <pod> -o wide          # real pod IP
kubectl expose deployment hello --port 80 --target-port 8080
```

---

## Logs

The JVM's stdout/stderr flow through containerd just like any container:

```bash
kubectl logs -l app=hello
kubectl logs -f <pod>
kubectl logs <pod> --previous          # after a restart
```

---

## Metrics & tracing

- **JMX, Micrometer, OpenTelemetry** work as usual — the JVM is a normal process in
  a normal sandbox.
- **JFR (Java Flight Recorder)** can be enabled via `jvm.args`
  (e.g. `-XX:StartFlightRecording=...`).
- **metrics-server / HPA** work because the sandbox is a real cgroup-backed
  container.
- The shim can export node-level per-sandbox JVM metrics (RSS, GC) *(roadmap)*.
  See the [metrics exporter research note](metrics-exporter.md) for the proposed
  Brewlet-specific telemetry (cold-start phases, JDK inventory/patch age, admission
  outcomes) and how it would be exposed as Prometheus.

```yaml
jvm:
  args:
    - "-XX:StartFlightRecording=filename=/tmp/app.jfr,dumponexit=true"
```

---

## Probes & exec

All probe types and interactive debugging work because runc backs the sandbox:

- readiness / liveness / startup probes: `httpGet`, `tcpSocket`, `exec`;
- `kubectl exec` into the JVM sandbox;
- ephemeral debug containers.

```yaml
readinessProbe: { httpGet: { path: /actuator/health/readiness, port: 8080 } }
livenessProbe:  { httpGet: { path: /actuator/health/liveness,  port: 8080 } }
```

```bash
kubectl exec -it <pod> -- jcmd 1 VM.flags      # inspect the running JVM
```

---

## Signals & graceful shutdown

- `SIGTERM` is forwarded to the JVM (PID 1) → **shutdown hooks run**; Brewlet honors
  `terminationGracePeriodSeconds` and `preStop`.
- The `java` process's exit code is the container exit code, which drives the pod's
  `restartPolicy` (so `-XX:+ExitOnOutOfMemoryError` → clean restart).

---

## Autoscaling

HPA works against CPU/memory or custom/Prometheus metrics as usual. With the
`JavaApplication` CRD you can declare autoscaling inline and the controller (§8.2)
creates the `HorizontalPodAutoscaler` for you; with raw Deployments, attach a
standard `HorizontalPodAutoscaler`.

---

## Day‑2: JDK upgrades

JDK roots on nodes are **versioned and additive**. The upgrade choreography — add a
new root, migrate workloads, retire the old root — is covered in detail in
[JDK management → Patching & upgrading](jdk-management.md#patching--upgrading-jdks).

Key properties:

- New roots install **additively**; running pods keep their JDK until they restart.
- Old roots are retained until no workload references them, then GC'd by the
  provisioner *(GC is a provisioner responsibility; today, retire by removing from
  the inventory and cleaning the node)*.
- Patching one node JDK patches every workload that uses it — centralized CVE
  management.

---

## Day‑2: multi-arch fleets

- Install one JDK root per node architecture (amd64/arm64).
- The **OCI artifact is arch-independent** — the same artifact runs on any
  provisioned arch, so multi-arch is transparent to developers.
- The provisioner image and shim are compiled per-arch; use the multi-arch
  `*-image-push` build targets. See [JDK management → multi-arch](jdk-management.md#architecture-mapping-multi-arch).
- **Non-portable (JNI) JARs** and arch-coupled accelerators (AppCDS archives) are the
  exception — see the [multi-arch note](multi-arch.md) for the optional `arch`
  scheduling constraint.

---

## Watching the fleet

```bash
# Node readiness and operator state:
kubectl get nodes -L brewlet.sh/runtime
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/provision-state}{"\n"}'

# What each node offers:
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/jdks}{"\n"}'
kubectl get node <n> -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'

# Operator/provisioner events:
kubectl get events --field-selector reason=NodeReady
kubectl get events --field-selector reason=ProvisionFailed

# Component health:
kubectl get pods -n brewlet
```

The operator exposes metrics (`--metrics-bind-address`, default `:8080`) and a
health/readiness endpoint (`--health-probe-bind-address`, default `:8081`).

## Next steps

- **[Troubleshooting](troubleshooting.md)** — when something's wrong.
- **[Security](security.md)** — hardening and supply chain.
