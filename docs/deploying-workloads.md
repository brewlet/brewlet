# Deploying workloads

Once Brewlet is installed on your cluster ([Installation](installation.md)) and your
application is published ([Building & publishing](building-and-publishing.md)), deploying is
just standard Kubernetes with **one extra line**: `runtimeClassName: brewlet`.

Two ways to deploy:

- **[Raw `Deployment`/`Pod`](#raw-deployment)** — works today, nothing but the
  RuntimeClass.
- **[`JavaApplication` CRD](#javaapplication-crd)** — a higher-level descriptor;
  the controller (delivered in Phase 2) is implemented and reconciles it into a
  `Deployment` (+ `Service`, + optional `HPA`).

---

## Raw Deployment

The image field references an **OCI artifact containing a Java application**, not a
container image. The only
Brewlet-specific line is `runtimeClassName: brewlet`.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: hello }
spec:
  replicas: 1
  selector: { matchLabels: { app: hello } }
  template:
    metadata: { labels: { app: hello } }
    spec:
      runtimeClassName: brewlet
      containers:
        - name: hello
          image: registry.example.com/demo/hello:1.0.0   # the OCI artifact
          resources:
            limits: { cpu: "1", memory: "512Mi" }         # → cgroup limits
          ports: [{ containerPort: 8080 }]
```

```bash
kubectl apply -f my-deployment.yaml
kubectl get pods -l app=hello
kubectl logs -l app=hello
```

Because the shim is runc-backed, this pod is a **first-class Kubernetes citizen**:

- real pod IP via CNI → Services/Ingress/NetworkPolicy work;
- `kubectl logs` / `kubectl exec` / ephemeral debug containers work;
- readiness/liveness/startup probes (`httpGet`, `tcpSocket`, `exec`) work;
- HPA and metrics-server work.

Add a Service exactly as usual:

```yaml
apiVersion: v1
kind: Service
metadata: { name: hello }
spec:
  selector: { app: hello }
  ports: [{ name: http, port: 80, targetPort: 8080 }]
```

---

## Requesting a specific JDK or launcher

For raw Kubernetes workloads, request the JDK/launcher with pod annotations. The
admission webhook validates them against the ready fleet, injects `nodeAffinity`,
and the shim reads the same propagated annotations at launch. If `brewlet.sh/jdk`
is absent, the shim defaults to feature 21 and picks the lexically-first
installed distribution for it; omit `brewlet.sh/launcher` for
vanilla `java`.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: hello }
spec:
  template:
    metadata:
      annotations:
        brewlet.sh/jdk: "21"          # bare feature (any distro) or "temurin-21"
        brewlet.sh/launcher: "jaz"    # optional; omit/"java" = vanilla launcher
    spec:
      runtimeClassName: brewlet
      containers:
        - name: hello
          image: registry.example.com/demo/hello:1.0.0
          resources: { limits: { cpu: "2", memory: "1Gi" } }
```

| Annotation | Values | Effect |
|---|---|---|
| `brewlet.sh/jdk` | `21` (any distro of that feature) or `temurin-21` (exact) | Validated against ready nodes; injects `nodeAffinity`. If none compatible → pod rejected with `NoCompatibleJDK`. |
| `brewlet.sh/launcher` | `jaz`, or empty/`java` | Same, but for launchers → `NoCompatibleLauncher`. |
| `brewlet.sh/arch` | `amd64`, or `amd64,arm64` | Optional; only for **non-portable JARs** bundling JNI natives. Injects `kubernetes.io/arch` nodeAffinity; if no ready node of a required arch exists → `NoCompatibleArch`. Omit for arch-neutral bytecode. |
| `brewlet.sh/artifact-container` | container name | Which container's `image` is the OCI artifact (defaults to the brewlet container). |

If you set **no** annotation, the pod is admitted (its artifact ref/digest are still
stamped) and the shim performs its own runtime JDK compatibility check. See
[Launchers](launchers.md) and [Troubleshooting](troubleshooting.md).

---

## JavaApplication CRD

The `JavaApplication` CRD is the developer-facing "deployment descriptor" — a single
manifest that a controller reconciles into a `Deployment` (+ `Service`, + optional
`HPA`) with `runtimeClassName: brewlet` wired in.

> **Status.** The CRD ships in [`deploy/javaapplication-crd.yaml`](https://github.com/brewlet/kubernetes/blob/main/deploy/javaapplication-crd.yaml)
> and the reconciling controller (`JavaApplicationReconciler`,
> [SPECIFICATION §8.2](https://github.com/brewlet/specs/blob/main/SPECIFICATION.md)) is implemented in the operator: apply
> a `JavaApplication` and it manages the `Deployment` (+ `Service`, + optional
> `HPA`) for you, garbage-collecting them when the descriptor is deleted. The
> The `charts/brewlet` Helm chart in
> [brewlet/kubernetes](https://github.com/brewlet/kubernetes) installs the CRD automatically.

```bash
kubectl apply -f deploy/javaapplication-crd.yaml
kubectl apply -f deploy/sample-javaapplication.yaml
```

### Minimal example

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata: { name: hello }
spec:
  artifact: { image: registry.example.com/demo/hello:1.0.0 }
  resources:
    limits: { cpu: "1", memory: "512Mi" }
  ports: [{ name: http, containerPort: 8080 }]
```

### Full example

```yaml
apiVersion: apps.brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders-api
  namespace: payments
spec:
  artifact:
    image: registry.example.com/team/orders:1.4.2   # digest-pinned recommended
    pullPolicy: IfNotPresent
    pullSecrets: [regcred]
  replicas: 3
  resources:
    requests: { cpu: "500m", memory: "512Mi" }
    limits:   { cpu: "2",    memory: "1Gi" }
  jvm:
    version: 21                  # JDK feature version
    launcher: java               # vanilla OpenJDK (default); or "jaz"
    args:                        # YOUR tuning — Brewlet injects none
      - "-XX:MaxRAMPercentage=75.0"
      - "-XX:+UseZGC"
      - "-XX:+ExitOnOutOfMemoryError"
    cds:
      regenerate: true           # opt into node-side AppCDS regeneration (fleet choice)
  env:
    - name: SPRING_PROFILES_ACTIVE
      value: prod
  ports:
    - name: http
      containerPort: 8080
  service:
    enabled: true
    type: ClusterIP
  probes:
    readiness: { httpGet: { path: /actuator/health/readiness, port: 8080 } }
    liveness:  { httpGet: { path: /actuator/health/liveness,  port: 8080 } }
  autoscaling:
    enabled: true
    minReplicas: 3
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
  arch: [amd64]                  # optional; only for non-portable JARs (JNI natives)
```

| Field group | Purpose |
|---|---|
| `artifact` | The OCI artifact ref + pull policy/secrets. |
| `replicas` / `autoscaling` | Deployment replica count / HPA. |
| `resources` | Requests/limits → sandbox cgroup ([Resource tuning](resource-tuning.md)). |
| `jvm.version` | JDK feature version to run on (e.g. `21`); must match a node-installed JDK. |
| `jvm.distribution` | Optional JDK distribution (`temurin`, `microsoft`). With `jvm.version` pins an exact `<distribution>-<feature>` node JDK; omit to accept any distribution of that feature. |
| `jvm.launcher` | `java` (default) or `jaz` ([Launchers](launchers.md)). |
| `jvm.args` | Your JVM tuning flags. Omit under `jaz`. |
| `jvm.cds.regenerate` | Opt into **node-side AppCDS regeneration** ([AppCDS §4.3](appcds.md)). When `true` the controller stamps the `brewlet.sh/cds-regenerate` pod annotation and the node maintains a per-`(artifact, JDK-build)` archive cache via `-XX:+AutoCreateSharedArchive` (JDK 19+), self-healing on every central JDK patch. Fleet/operational choice (depends on your JDK patch cadence), so it lives here rather than in the artifact; any shipped `cds.archive` becomes optional seed data. Default `false`. |
| `arch` | Optional architecture constraint (`amd64`, `arm64`). Only for **non-portable JARs** bundling JNI native libraries; steers scheduling to matching-arch nodes and denies admission with `NoCompatibleArch` when unsatisfiable. Omit for arch-neutral bytecode (runs on any arch). |
| `env` / `ports` / `service` / `probes` | Wired through to the generated objects. |

The `status` subresource surfaces `readyReplicas`, the `selectedJdk`, and `Ready`
conditions.

### Autoscaling

Set `spec.autoscaling.enabled: true` and the controller manages a
`HorizontalPodAutoscaler` (`autoscaling/v1`) targeting the generated Deployment:

```yaml
spec:
  # replicas is ignored while autoscaling is enabled — the HPA owns scaling
  autoscaling:
    enabled: true
    minReplicas: 3                    # lower bound
    maxReplicas: 10                   # upper bound (required)
    targetCPUUtilizationPercentage: 70
```

- While autoscaling is on, the controller does not reconcile the Deployment's `replicas`
  so the HPA is the sole owner of the replica count (no fighting between the two
  controllers).
- Flip `enabled` back to `false` and the managed HPA is deleted and the Deployment
  returns to `spec.replicas` (default `1`).
- The HPA is owned via a controller reference, so it is garbage-collected when the
  `JavaApplication` is deleted.
- Requires a running **metrics-server** in the cluster for CPU metrics.


---

## Coexisting with regular containers

Brewlet is **additive**. Only pods that set `runtimeClassName: brewlet` go through
the shim; every other pod runs on the default runtime unchanged. You can freely mix
Brewlet workloads and ordinary containers in the same namespace and cluster.

## Next steps

- **[Resource tuning](resource-tuning.md)** — get heap/GC/CPU right.
- **[Launchers](launchers.md)** — `java` vs `jaz`.
- **[Observability & day‑2](observability.md)** — logs, metrics, probes, upgrades.
- **[Troubleshooting](troubleshooting.md)** — when a pod won't schedule/start.
