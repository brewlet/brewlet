// Package brewlet holds the label/annotation vocabulary shared across the
// brewlet-operator controllers. These keys are the contract between the node
// provisioner (which sets them, see src/provisioner) and the operator (which
// reads them) — keep them in sync with https://github.com/brewlet/specs.
package brewlet

const (
	// LabelProvision is the legacy per-node opt-in for brewlet provisioning.
	// The platform team sets it (e.g. `kubectl label node --all
	// brewlet.sh/provision=true`). It is modeled as a label — not an annotation —
	// so it can drive the nodeAffinity of the standalone
	// deploy/node-provisioner.yaml DaemonSet (the no-operator path).
	// Under the operator, DaemonSet placement is driven by NodeProfile pools
	// (§5.6), not this label; the operator only reads it as a fallback opt-in when
	// deciding whether to track a node's provisioning state (see isBrewletNode).
	LabelProvision = "brewlet.sh/provision"

	// LabelRuntimeReady is set on a node by the provisioner once the shim + a JDK
	// are installed and the containerd runtime is registered. The RuntimeClass
	// nodeSelector matches on it so workloads only schedule onto ready nodes.
	LabelRuntimeReady = "brewlet.sh/runtime"
	// ValueReady is the value LabelRuntimeReady carries when a node is ready.
	ValueReady = "ready"

	// AnnotationJDKs advertises the JDK roots the provisioner installed, as a
	// comma-separated list of <dist>-<feature> tokens, e.g.
	// "temurin-21,microsoft-25".
	AnnotationJDKs = "brewlet.sh/jdks"
	// AnnotationLaunchers advertises the installed launcher layers, similarly
	// comma-separated, e.g. "java,jaz".
	AnnotationLaunchers = "brewlet.sh/launchers"

	// AnnotationProvisionState reflects the operator's view of a node's
	// provisioning lifecycle: "Provisioning", "Ready", or "Failed". It is the
	// operator's own bookkeeping (distinct from LabelRuntimeReady, which the
	// provisioner owns).
	AnnotationProvisionState = "brewlet.sh/provision-state"

	// AnnotationProvisionError carries a machine-readable failure reason the
	// provisioner writes on a node when a reconfig/validation step fails
	// (proposal 0002). The NodeProfileReconciler reads it when computing
	// readyNodes and surfaces it as the owning profile's Degraded reason (§5.5).
	AnnotationProvisionError = "brewlet.sh/provision-error"
	// AnnotationProfile and AnnotationProfileGeneration identify the exact
	// NodeProfile revision the provisioner successfully applied to a node.
	AnnotationProfile           = "brewlet.sh/profile"
	AnnotationProfileGeneration = "brewlet.sh/profile-generation"
)

// Node-pool vocabulary for the NodeProfile model (§5.6 / proposal 0001).
const (
	// LabelNodeProfile is stamped on a managed provisioner/cleanup DaemonSet to
	// record which NodeProfile owns it, so the operator (and operators) can map
	// a DaemonSet back to its profile.
	LabelNodeProfile = "brewlet.sh/nodeprofile"

	// FinalizerCleanup gates NodeProfile deletion on host cleanup running before
	// owner-ref GC drops the managed DaemonSet (§5.6).
	FinalizerCleanup = "node.brewlet.sh/cleanup"
)

// ProviderPoolKeys are the node label keys that carry the node-pool name on the
// major providers, in auto-detection probe order (§5.1). The operator resolves a
// profile's pool key by probing these across the fleet unless spec.nodePool.key
// overrides it. A node belongs to exactly one pool, so these are disjoint.
var ProviderPoolKeys = []string{
	"cloud.google.com/gke-nodepool",  // GKE
	"kubernetes.azure.com/agentpool", // AKS
	"agentpool",                      // AKS (legacy)
	"eks.amazonaws.com/nodegroup",    // EKS managed node groups
	"karpenter.sh/nodepool",          // Karpenter
}

// CuratedDistributions have built-in copy-from-image mappings (§5.3). Other
// distributions are accepted when their NodeProfile supplies a custom source.
var CuratedDistributions = []string{"temurin", "microsoft"}

// ProfileDaemonSetName is the name of the provisioner DaemonSet the operator
// manages for a given profile (one DaemonSet per profile, §5.2).
func ProfileDaemonSetName(profile string) string {
	return ProvisionerName + "-" + profile
}

// CleanupDaemonSetName is the name of the short-lived cleanup DaemonSet the
// operator launches to reverse host state for a profile on deletion (§5.6).
func CleanupDaemonSetName(profile string) string {
	return "brewlet-cleanup-" + profile
}

// Pod-side vocabulary consumed by the admission/scheduling seam (§8 / §14).
const (
	// AnnotationArtifactRef is stamped by the admission webhook onto a brewlet
	// pod: the OCI artifact reference the shim resolves (mirrors the shim's
	// annArtifactRef). Sourced from the pod's brewlet container image.
	AnnotationArtifactRef = "brewlet.sh/artifact-ref"
	// AnnotationArtifactDigest is stamped when the artifact ref is digest-pinned
	// (repo@sha256:…); it lets the shim read the artifact straight from
	// containerd's content store (mirrors the shim's annArtifactDigest).
	AnnotationArtifactDigest = "brewlet.sh/artifact-digest"

	// AnnotationRequestedJDK optionally declares the JDK a pod needs, as either a
	// "<dist>-<feature>" token (e.g. "temurin-21") or a bare feature ("21").
	// When set, the webhook validates it against the ready fleet and steers
	// scheduling via nodeAffinity. When unset, no JDK constraint is imposed and
	// the shim surfaces NoCompatibleJDK at runtime as before.
	AnnotationRequestedJDK = "brewlet.sh/jdk"
	// AnnotationRequestedLauncher optionally declares the launcher a pod needs
	// (e.g. "jaz"). Empty or "java" means the vanilla OpenJDK launcher, which
	// every ready node provides.
	AnnotationRequestedLauncher = "brewlet.sh/launcher"
	// AnnotationRequestedArch optionally declares the architecture(s) a NON-portable
	// artifact needs (those bundling JNI natives / arch-specific deps), as a
	// comma-separated list of GOARCH / kubernetes.io/arch tokens (e.g. "amd64"
	// or "amd64,arm64"). When set, the webhook steers scheduling onto matching
	// nodes (kubernetes.io/arch In […]) and denies with NoCompatibleArch when no
	// ready node of a required arch exists. When unset, the artifact is treated
	// as architecture-neutral (the common case) and no arch constraint applies.
	AnnotationRequestedArch = "brewlet.sh/arch"

	// AnnotationCDSRegenerate optionally opts a pod into node-side AppCDS
	// regeneration (https://github.com/brewlet/site). Value "true" tells the shim to maintain
	// a per-(artifact, JDK-build) archive cache with -XX:+AutoCreateSharedArchive
	// instead of consuming a shipped archive verbatim. It imposes no scheduling
	// constraint (every ready node can regenerate, or safely skips on JDK < 19),
	// so unlike the JDK/launcher/arch requests the webhook does not validate it —
	// it merely flows through to the shim. The controller stamps it from
	// spec.jvm.cds.regenerate.
	AnnotationCDSRegenerate = "brewlet.sh/cds-regenerate"
)

// Per-capability node labels the provisioner emits so the scheduler can skip
// incompatible nodes (annotations can't drive nodeAffinity — see LabelProvision).
// Each is a boolean-presence label; the webhook matches with Operator: Exists.
const (
	// LabelJDKPrefix + "<dist>-<feature>" (e.g. brewlet.sh/jdk.temurin-21) marks a
	// node that has that exact JDK root installed.
	LabelJDKPrefix = "brewlet.sh/jdk."
	// LabelJDKFeaturePrefix + "<feature>" (e.g. brewlet.sh/jdk-feature.21) marks a
	// node that has some JDK of that feature version, for distribution-agnostic
	// requests.
	LabelJDKFeaturePrefix = "brewlet.sh/jdk-feature."
	// LabelLauncherPrefix + "<name>" (e.g. brewlet.sh/launcher.jaz) marks a node
	// that has that launcher layer installed.
	LabelLauncherPrefix = "brewlet.sh/launcher."
	// LabelArch is the standard, kubelet-provided node label carrying the node's
	// architecture (e.g. "amd64", "arm64"). Brewlet reuses it — rather than
	// emitting a provisioner label — to steer non-portable artifacts via the
	// AnnotationRequestedArch constraint.
	LabelArch = "kubernetes.io/arch"
)

// VanillaLauncher is the built-in OpenJDK launcher name; it needs no launcher
// layer and is available on every ready node.
const VanillaLauncher = "java"

// Provisioning-state values written to AnnotationProvisionState.
const (
	StateProvisioning = "Provisioning"
	StateReady        = "Ready"
	StateFailed       = "Failed"
)

// Well-known object names the operator manages.
const (
	// RuntimeClassName is the RuntimeClass (and containerd handler) name.
	RuntimeClassName = "brewlet"
	// ProvisionerName is the managed DaemonSet's name.
	ProvisionerName = "brewlet-node-provisioner"
	// ProvisionerAppLabel identifies the provisioner pods/DaemonSet.
	ProvisionerAppLabel = "brewlet-node-provisioner"
)

// Event reasons the operator records (see https://github.com/brewlet/specs).
const (
	// ReasonProvisioning — the operator has requested provisioning for a node.
	ReasonProvisioning = "Provisioning"
	// ReasonNodeReady — a node is provisioned and advertising the brewlet runtime.
	ReasonNodeReady = "NodeReady"
	// ReasonProvisionFailed — the provisioner pod on a node is failing.
	ReasonProvisionFailed = "ProvisionFailed"

	// ReasonNodeUnmatched — a NodeProfile named a pool that resolves to zero
	// nodes (typo, pool not yet created, wrong provider key); the profile goes
	// Degraded/EmptyPool (§14).
	ReasonNodeUnmatched = "NodeUnmatched"

	// ReasonNoCompatibleJDK — a brewlet pod requested a JDK no ready node
	// provides; the admission webhook denies it (§14).
	ReasonNoCompatibleJDK = "NoCompatibleJDK"
	// ReasonNoCompatibleLauncher — a brewlet pod requested a launcher no ready
	// node provides; the admission webhook denies it (§14).
	ReasonNoCompatibleLauncher = "NoCompatibleLauncher"
	// ReasonNoCompatibleArch — a non-portable brewlet pod requested an
	// architecture no ready node provides; the admission webhook denies it (§14).
	ReasonNoCompatibleArch = "NoCompatibleArch"
)
