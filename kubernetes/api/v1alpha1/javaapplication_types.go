package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JavaApplicationSpec is the developer-facing deployment descriptor (§9). The
// controller (§8.2) reconciles it into a Deployment (+ optional Service/HPA).
type JavaApplicationSpec struct {
	// Artifact is the OCI artifact to run.
	Artifact ArtifactSpec `json:"artifact"`
	// Replicas is the desired replica count when autoscaling is disabled
	// (defaults to 1). Ignored once the HPA owns scaling.
	Replicas *int32 `json:"replicas,omitempty"`
	// Resources is copied verbatim onto the container and enforced as the
	// sandbox cgroup (§10). The container-aware JDK reads these limits directly.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// JVM selects the JDK/launcher and carries user JVM tuning (§9.3/§10).
	JVM JVMSpec `json:"jvm,omitempty"`
	// Env is wired through to the container verbatim.
	Env []corev1.EnvVar `json:"env,omitempty"`
	// Ports are exposed on the container (and the Service, when enabled).
	Ports []corev1.ContainerPort `json:"ports,omitempty"`
	// Service controls the generated Service.
	Service ServiceSpec `json:"service,omitempty"`
	// Probes are wired through to the container.
	Probes ProbesSpec `json:"probes,omitempty"`
	// Autoscaling controls the generated HorizontalPodAutoscaler.
	Autoscaling AutoscalingSpec `json:"autoscaling,omitempty"`
	// Arch is an OPTIONAL architecture constraint for NON-portable artifacts —
	// those bundling JNI native libraries or arch-specific dependencies (e.g.
	// netty-tcnative, RocksDB) that only run on the arch(es) whose natives were
	// bundled. Each entry is a GOARCH / kubernetes.io/arch token ("amd64" or
	// "arm64"). When set, the controller folds it into the brewlet.sh/arch pod
	// annotation so the admission webhook steers scheduling onto matching nodes
	// (kubernetes.io/arch In […]) and denies with NoCompatibleArch when no ready
	// node of a required arch exists. Leave it UNSET for the common case: a
	// pure-bytecode JAR is architecture-neutral and runs on any provisioned arch.
	Arch []string `json:"arch,omitempty"`
}

// ArtifactSpec references the OCI artifact (§4/§9).
type ArtifactSpec struct {
	// Image is the OCI ref to the artifact — a Java application packaged as an OCI artifact, not a container image (digest pin recommended).
	Image string `json:"image"`
	// PullPolicy for the artifact image (defaults to IfNotPresent).
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
	// PullSecrets are imagePullSecrets for a private registry.
	PullSecrets []string `json:"pullSecrets,omitempty"`
}

// JVMSpec selects the JDK/launcher and carries deployment-side JVM tuning. Per
// §8.2/§10 Brewlet injects no tuning of its own. Args here is the DEPLOYMENT's
// tuning (heap %, GC, agents, container flags), appended after the artifact's
// app-intrinsic launch knobs (enablePreview/addOpens/addExports/addModules/
// systemProperties). The artifact has no free-form JVM-args field, so all
// environment tuning lives here.
type JVMSpec struct {
	// Version is the JDK feature version to run on (e.g. 21). It must match a
	// node-installed JDK; the controller folds it (with Distribution, if set)
	// into the brewlet.sh/jdk pod annotation so the admission webhook validates
	// it and steers scheduling.
	Version int32 `json:"version,omitempty"`
	// Distribution optionally pins the JDK distribution (e.g. "temurin",
	// "microsoft"). When set together with Version it selects an exact
	// "<distribution>-<version>" node JDK (e.g. "microsoft-25"); when omitted,
	// any distribution providing the requested Version is acceptable and each
	// node selects the lexically-first installed distribution for it (no
	// built-in vendor preference). Ignored unless Version is also set.
	Distribution string `json:"distribution,omitempty"`
	// Launcher selects the JVM launcher: "java" (vanilla OpenJDK, default) or a
	// custom launcher such as "jaz" (§9.3). Stamped as the brewlet.sh/launcher
	// pod annotation.
	Launcher string `json:"launcher,omitempty"`
	// Args are user-supplied JVM flags, wired through to the JVM via
	// JDK_JAVA_OPTIONS (or JAVA_TOOL_OPTIONS on JDK 8, which lacks the former).
	// Tuning (heap %, GC, processor count) is the user's responsibility (§10).
	Args []string `json:"args,omitempty"`
	// CDS carries deployment-side AppCDS behavior. Node-side regeneration is a
	// fleet/operational decision (does this cluster maintain a self-healing
	// per-JDK-build archive cache?), not a property of the app, so it lives here
	// rather than in the artifact. The artifact carries only the optional shipped
	// *seed* archive (its cds.archive bytes). See https://github.com/brewlet/site.
	CDS CDSSpec `json:"cds,omitempty"`
}

// CDSSpec is the deployment-side AppCDS block (§4.3). Empty by default.
type CDSSpec struct {
	// Regenerate opts this deployment into node-side AppCDS regeneration
	// (see https://github.com/brewlet/site). When true the controller stamps the
	// brewlet.sh/cds-regenerate pod annotation; the node then maintains a
	// per-(artifact, JDK-build) archive cache and launches with
	// -XX:+AutoCreateSharedArchive, so the archive self-heals on every central
	// JDK patch instead of silently going stale. Any archive the artifact ships
	// (cds.archive) becomes optional *seed* data rather than the consumed
	// archive. Requires a JDK feature version that supports AutoCreateSharedArchive
	// (>= 19, i.e. Brewlet's JDK 21 floor); on older JDKs the node safely skips
	// regeneration and falls back to base CDS. Because AutoCreateSharedArchive
	// only writes the archive at JVM exit, the app-archive win lands on the second
	// rollout of a long-running server, not the first boot.
	Regenerate bool `json:"regenerate,omitempty"`
}

// ServiceSpec controls the generated Service.
type ServiceSpec struct {
	// Enabled toggles Service generation (defaults to true).
	Enabled *bool `json:"enabled,omitempty"`
	// Type is the Service type (defaults to ClusterIP).
	Type corev1.ServiceType `json:"type,omitempty"`
}

// ProbesSpec carries the readiness/liveness probes wired onto the container.
type ProbesSpec struct {
	Readiness *corev1.Probe `json:"readiness,omitempty"`
	Liveness  *corev1.Probe `json:"liveness,omitempty"`
}

// AutoscalingSpec controls the generated HorizontalPodAutoscaler (autoscaling/v1).
type AutoscalingSpec struct {
	// Enabled toggles HPA generation (defaults to false).
	Enabled bool `json:"enabled,omitempty"`
	// MinReplicas is the lower scaling bound.
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	// MaxReplicas is the upper scaling bound. Required (must be >= 1) when
	// Enabled is true; the controller rejects the JavaApplication otherwise
	// rather than emit an invalid HPA. The CRD also enforces this via CEL.
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
	// TargetCPUUtilizationPercentage is the CPU target that drives scaling.
	TargetCPUUtilizationPercentage *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

// JavaApplicationStatus reflects the reconciled state (§9).
type JavaApplicationStatus struct {
	// ObservedGeneration is the .metadata.generation the controller last acted on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ReadyReplicas mirrors the managed Deployment's readyReplicas.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// SelectedJdk is the requested JDK feature version (from jvm.version), for
	// human-readable status/printer columns. The concrete distribution is
	// resolved per-node by the shim, so it is not reflected here.
	SelectedJdk string `json:"selectedJdk,omitempty"`
	// Conditions carry the Ready condition and reconcile errors.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types and reasons surfaced on JavaApplication status.
const (
	// ConditionReady is True once the managed Deployment has all replicas ready.
	ConditionReady = "Ready"

	// ReasonReconciled — the managed objects were successfully reconciled.
	ReasonReconciled = "Reconciled"
	// ReasonProgressing — the Deployment has not yet reached its desired replicas.
	ReasonProgressing = "Progressing"
	// ReasonReconcileError — reconciling a managed object failed.
	ReasonReconcileError = "ReconcileError"
)

// JavaApplication is the developer-facing deployment descriptor for a JAR (§9).
type JavaApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JavaApplicationSpec   `json:"spec,omitempty"`
	Status JavaApplicationStatus `json:"status,omitempty"`
}

// JavaApplicationList is a list of JavaApplications.
type JavaApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []JavaApplication `json:"items"`
}
