# Proposal 0005 — Replica coalescing: fewer, larger JVMs

- **Status:** Draft
- **Target spec sections:** amend **§8.2**, **§9**, and **§10**
- **Related code:** [`brewlet/kubernetes`](https://github.com/brewlet/kubernetes):
  `api/v1alpha1`, `internal/controller`

> Enhancement proposal (KEP-style). This proposal introduces an opt-in capacity
> model in which the requested replica count represents logical capacity units that
> Brewlet may realize as fewer, larger JVMs.

---

## 1. Summary

Add an opt-in **replica coalescing** policy to `JavaApplication`. When multiple
otherwise-identical replicas can be placed on the same node, Brewlet may represent
them with one physical pod and one JVM whose CPU and memory requests and limits are
the sum of those replicas.

For example, four logical replicas with a per-replica limit of `1 CPU / 1 GiB` and
`maxFactor: 2` may be realized as:

- four physical JVMs of `1 CPU / 1 GiB`;
- two physical JVMs of `2 CPU / 2 GiB`; or
- one `2 CPU / 2 GiB` JVM and two `1 CPU / 1 GiB` JVMs.

All three layouts represent four **logical replicas**. Coalescing reduces duplicated
per-JVM costs such as heap reserve, Metaspace, code cache, thread stacks, GC
structures, JIT warmup, and RuntimeClass pod overhead. It deliberately trades some
failure isolation and scheduling flexibility for a potentially more efficient JVM.

Coalescing is not Vertical Pod Autoscaling (VPA). VPA derives a pod's resource size
from observed usage. Coalescing derives a physical JVM's size from an integer number
of declared logical replicas.

## 2. Motivation

Kubernetes normally realizes every Deployment replica as a separate pod and JVM.
When two such pods are scheduled on the same node, the node runs two independent
JVMs even if one JVM with their combined resources would be more efficient.

For suitable Java workloads, a larger JVM can:

- amortize fixed JVM and RuntimeClass overhead;
- provide a larger useful heap after non-heap headroom;
- share compiled code, class metadata, caches, and connection pools;
- give GC and JIT more room to optimize; and
- use CPU bursts within one cgroup rather than across separately capped JVMs.

The operator or developer may prefer this efficiency while retaining a declarative
logical capacity target. That preference must be explicit because a coalesced JVM is
also a larger failure unit.

## 3. Goals

- Let users request a logical replica count and opt into coalescing.
- Multiply CPU and memory requests and limits predictably by a coalescing factor.
- Permit a mixed layout in which physical JVMs have different factors.
- Preserve the requested total logical capacity whenever the cluster can schedule it.
- Expose both logical and physical capacity in status and metrics.
- Keep coalescing reversible as placement and capacity change.
- Preserve standard Kubernetes behavior when coalescing is disabled.

## 4. Non-goals

- Inferring resource requirements from historical usage; that is VPA's role.
- Combining arbitrary applications, containers, versions, configurations, or tenants.
- Merging running JVM processes or transferring live heap state between JVMs.
- Claiming that two logical capacity units always deliver exactly twice the throughput.
- Coalescing StatefulSets or workloads whose replicas have stable identity or
  replica-local persistent volumes in the initial implementation.
- Transparent weighted traffic distribution through a standard Kubernetes Service.

## 5. Terminology

- **Logical replica:** one unit of desired application capacity, carrying the base
  resource requests and limits declared in `spec.resources`.
- **Physical replica:** one scheduled pod containing one JVM.
- **Coalescing factor:** the number of logical replicas represented by a physical
  replica. An ordinary pod has factor `1`.
- **Plan:** the set of physical replicas and factors whose sum equals the desired
  logical replica count.

For a plan with factors `f1...fn` and desired logical replicas `R`:

```text
sum(f1...fn) = R
1 <= fi <= maxFactor
physicalReplicas = n
```

## 6. API

Extend `JavaApplicationSpec` with:

```yaml
apiVersion: brewlet.sh/v1alpha1
kind: JavaApplication
metadata:
  name: orders-api
spec:
  replicas: 4
  resources:
    requests: { cpu: "500m", memory: "512Mi" }
    limits:   { cpu: "1",    memory: "1Gi" }
  coalescing:
    enabled: true
    maxFactor: 2
    minPhysicalReplicas: 2
    strategy: PreferCoalesced
```

Proposed fields:

| Field | Default | Meaning |
|---|---:|---|
| `enabled` | `false` | Enables the logical-replica capacity model. |
| `maxFactor` | `2` | Maximum logical replicas represented by one JVM. |
| `minPhysicalReplicas` | `2` | Availability floor; Brewlet never plans fewer ready JVMs when the logical replica count permits it. |
| `strategy` | `PreferCoalesced` | `PreferCoalesced` chooses the largest schedulable factors; `Balanced` favors similarly sized physical replicas. |

Validation rules:

- `maxFactor >= 2`;
- `minPhysicalReplicas >= 1`;
- `minPhysicalReplicas <= replicas`;
- `autoscaling.enabled` and `coalescing.enabled` are mutually exclusive in the
  initial version;
- requests and limits, when present, must remain valid after multiplication;
- coalescing requires a stateless workload without replica-local persistent storage.

The existing meaning of `spec.replicas` is unchanged when coalescing is disabled.
When enabled, it becomes the desired number of logical replicas.

## 7. Resource semantics

For a physical replica with factor `f`, Brewlet computes:

```text
physical request CPU    = base request CPU    * f
physical request memory = base request memory * f
physical limit CPU      = base limit CPU      * f
physical limit memory   = base limit memory   * f
```

Other resources, including extended resources and huge pages, are not multiplied in
the initial version unless their resource-specific semantics are explicitly defined.

The RuntimeClass `podFixed` overhead remains charged once per physical pod. Brewlet
does not subtract estimated JVM savings from requests or limits: coalescing exposes
the full aggregate allocation to the larger JVM. The container-aware JDK reads the
resulting cgroup limits as described in §10.

Each physical pod receives:

```yaml
metadata:
  annotations:
    brewlet.sh/coalescing-factor: "2"
    brewlet.sh/logical-replicas: "2"
```

The factor is also available to the application as
`BREWLET_COALESCING_FACTOR`. Applications may use it to size worker pools or report
capacity, but correctness must not depend on reading it.

## 8. Planning and placement

Coalescing must occur **before** Kubernetes schedules the physical pod. Brewlet does
not attempt to merge already-running pods.

The ordinary controller currently emits one homogeneous Deployment, which cannot
represent mixed resource sizes. With coalescing enabled, a coalescing planner instead
renders one managed workload per factor present in the plan. Each workload has:

- the same artifact, configuration, Service labels, probes, and JDK constraints;
- a factor-specific pod template and resource allocation; and
- labels identifying the parent `JavaApplication`, revision, and factor.

For example, factors `[2, 1, 1]` may be represented by one Deployment with factor
`2` and two replicas in a Deployment with factor `1`.

The planner considers eligible Brewlet-ready nodes, their allocatable resources, the
application's scheduling constraints, and currently requested capacity. It may use a
coalesced factor only when a pod with the multiplied **requests** is schedulable.
Limits alone never establish schedulability.

`PreferCoalesced` starts with the largest permitted factor and falls back to smaller
factors when the larger pod cannot be placed. `Balanced` chooses factors whose sizes
differ by at most one where possible. Neither strategy binds to a node name; normal
Kubernetes scheduling remains authoritative.

Because the scheduler can race with other workloads, a plan is provisional. A
coalesced pod that remains unschedulable causes the controller to replan with smaller
factors after a bounded timeout. Replanning must never increase the sum of requested
logical capacity beyond `spec.replicas`.

## 9. Reconciliation and transitions

Plan changes use a capacity-aware rolling transition:

1. Create replacement physical replicas for the desired plan.
2. Wait for replacements to become Ready.
3. Remove superseded replicas while keeping the sum of ready logical factors at or
   above the configured availability floor.

Where the cluster lacks temporary headroom, the controller surfaces
`CoalescingTransitionBlocked` rather than deleting healthy capacity first. An
explicit future disruption policy may permit delete-first transitions; it is not the
default.

Coalescing and de-coalescing restart JVMs. No live process or heap merge is implied.
Normal Deployment revision, probe, termination-grace-period, and owner-reference
semantics continue to apply to every managed workload.

## 10. Availability and disruption

A factor-`f` physical replica is one failure domain whose loss removes `f` logical
capacity units. `minPhysicalReplicas` prevents an operator from accidentally turning
a highly replicated application into one JVM.

Status and readiness must not describe only pod count. The `JavaApplication` is Ready
when:

- the ready logical capacity equals the desired logical replica count;
- the ready physical replica count meets `minPhysicalReplicas`; and
- no managed revision transition is incomplete.

PodDisruptionBudget generation, if added, must protect physical replicas and account
for factor-weighted logical capacity. A conventional PDB cannot express weighted
pods, so the initial implementation documents this limitation and conservatively
sets `minAvailable` from `minPhysicalReplicas`.

## 11. Services and load distribution

All physical replicas remain endpoints of the generated Service. Kubernetes Services
distribute connections among endpoints without considering CPU, memory, or
coalescing factor. A factor-`2` endpoint is therefore not guaranteed to receive twice
the traffic of a factor-`1` endpoint.

This is naturally suitable for workloads that pull from shared queues or otherwise
perform their own demand-driven work distribution. For request/response services:

- `Balanced` or a uniform-factor plan is recommended; and
- proportional endpoint weighting requires a separate, explicitly supported ingress,
  Gateway API, or service-mesh integration.

Brewlet must document this limitation and must not claim proportional throughput for
mixed-factor Service endpoints.

## 12. Autoscaling and VPA

The initial version makes coalescing mutually exclusive with the generated HPA. HPA
scales physical pod count and averages utilization per pod; it has no concept of a
factor-weighted logical replica. Allowing both controllers to act independently
would produce unstable or misleading scaling decisions.

VPA is also not part of the initial coalescing contract. If users install VPA
separately, it must not mutate coalesced workloads because Brewlet owns the
factor-derived resources and would reconcile them back.

A future autoscaling design may:

- scale desired logical replicas from factor-weighted application metrics;
- use VPA recommendations to adjust the base per-logical-replica resources; and
- replan factors after either value changes.

Those decisions require a dedicated proposal.

## 13. Status and observability

Extend `JavaApplicationStatus` with:

```yaml
status:
  desiredLogicalReplicas: 4
  readyLogicalReplicas: 4
  physicalReplicas: 3
  readyPhysicalReplicas: 3
  coalescing:
    factors:
      "1": 2
      "2": 1
```

Expose metrics for desired/ready logical replicas, physical replicas, factor
distribution, replans, unschedulable coalesced pods, and transition duration.
Events explain every plan change and fallback.

## 14. Risks and mitigations

| Risk | Mitigation |
|---|---|
| A larger JVM creates a larger failure domain. | Explicit opt-in, `minPhysicalReplicas`, factor-aware status, and conservative transitions. |
| Large pods are harder to schedule and may increase fragmentation. | Bound `maxFactor`, verify requests against eligible nodes, and fall back to smaller factors. |
| Mixed factors receive equal Service endpoint treatment. | Document the limitation; recommend queue-driven workloads, uniform factors, or weighted routing. |
| JVM throughput does not scale linearly with resources. | Treat factors as allocated capacity, not a throughput guarantee; require workload benchmarking. |
| Rollouts need temporary aggregate capacity. | Never delete healthy capacity by default; report a blocked transition. |
| HPA/VPA fight with factor-derived resources. | Reject unsupported combinations in the initial API. |
| Metrics and alerts confuse physical and logical replicas. | Use explicit field and metric names; never overload `readyReplicas`. |

## 15. Rollout

1. Add the API fields, validation, status, annotations, and metrics behind a disabled
   feature gate.
2. Implement planning and factor-specific managed workloads in
   `brewlet/kubernetes`, with unit tests for resource arithmetic and plan invariants.
3. Add integration tests for mixed factors, insufficient node capacity, replanning,
   transitions, node loss, and Service endpoint membership.
4. Publish operator guidance describing workload eligibility, availability tradeoffs,
   routing limitations, and benchmarking.
5. Promote the feature only after production evidence shows a measurable JVM
   efficiency benefit without unacceptable availability or scheduling regressions.

## 16. Alternatives considered

### Vertical Pod Autoscaler

VPA right-sizes individual pods from observed usage but does not combine replica
capacity or reduce JVM count. It is complementary input to a future design, not an
implementation of this proposal.

### Require users to configure fewer, larger replicas manually

This is available today and remains the simplest option. It does not preserve a
logical capacity target, adapt the physical layout to node capacity, or expose the
tradeoff as an auditable platform policy.

### Merge pods after scheduling

Kubernetes cannot merge running pods or JVM state. Replacing colocated pods after the
fact introduces avoidable disruption and may deadlock when the replacement needs the
resources held by the pods it would replace. This proposal plans physical replicas
before scheduling instead.

### Scheduler plugin only

A scheduler plugin can choose nodes but cannot change a pod into a differently sized
JVM or alter the number of workload objects safely. Scheduling may inform planning,
but lifecycle and capacity accounting remain controller responsibilities.

## Appendix — spec integration points

- **§8.2:** describe the coalescing planner and factor-specific managed workloads as
  an opt-in alternative to the ordinary Deployment/HPA path.
- **§9:** add `spec.coalescing`, define logical replica semantics, validation, and
  factor-aware status.
- **§10:** define multiplication of base requests and limits and clarify that the JDK
  observes the expanded physical-pod cgroup.
