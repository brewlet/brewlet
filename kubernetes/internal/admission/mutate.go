package admission

import (
	"strconv"
	"strings"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

// MutationResult reports what MutatePod did.
type MutationResult struct {
	// Applies is true when the pod targets the brewlet RuntimeClass and was
	// therefore considered. When false the pod is left untouched.
	Applies bool
	// DenyReason / DenyMessage are set (from CheckFleet) when the pod requested a
	// JDK/launcher no ready node provides. When DenyReason is empty the mutation
	// succeeded and the (possibly modified) pod should be admitted.
	DenyReason  string
	DenyMessage string
	// ArtifactRef / ArtifactDigest are the values stamped onto the pod (for
	// logging).
	ArtifactRef    string
	ArtifactDigest string
}

// IsBrewletPod reports whether a pod is routed to the brewlet runtime.
func IsBrewletPod(pod *corev1.Pod) bool {
	return pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == brewlet.RuntimeClassName
}

// MutatePod applies the admission/scheduling seam to a brewlet pod in place:
//
//  1. stamps brewlet.sh/artifact-ref (from the brewlet container image) and, when
//     the ref is digest-pinned, brewlet.sh/artifact-digest — the annotations the
//     shim resolves;
//  2. validates any explicit JDK/launcher request against the ready fleet,
//     returning a NoCompatibleJDK / NoCompatibleLauncher denial when unsatisfiable;
//  3. injects nodeAffinity so the scheduler only lands the pod on nodes that
//     advertise the requested JDK/launcher.
//
// Non-brewlet pods are left untouched (Applies=false). The function is pure over
// (pod, fleet): it mutates only the passed pod and is unit-tested without a
// cluster.
func MutatePod(pod *corev1.Pod, fleet []NodeCapability) MutationResult {
	if !IsBrewletPod(pod) {
		return MutationResult{}
	}
	res := MutationResult{Applies: true}

	// 1. Stamp the artifact ref/digest the shim reads.
	ref := podArtifactRef(pod)
	if ref != "" {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		if pod.Annotations[brewlet.AnnotationArtifactRef] == "" {
			pod.Annotations[brewlet.AnnotationArtifactRef] = ref
		}
		res.ArtifactRef = pod.Annotations[brewlet.AnnotationArtifactRef]
		if digest := refDigest(res.ArtifactRef); digest != "" && pod.Annotations[brewlet.AnnotationArtifactDigest] == "" {
			pod.Annotations[brewlet.AnnotationArtifactDigest] = digest
		}
		res.ArtifactDigest = pod.Annotations[brewlet.AnnotationArtifactDigest]
	}

	// 2. Validate the requested JDK/launcher/arch against the ready fleet.
	jdk := strings.TrimSpace(pod.Annotations[brewlet.AnnotationRequestedJDK])
	launcher := strings.TrimSpace(pod.Annotations[brewlet.AnnotationRequestedLauncher])
	arch := splitArch(pod.Annotations[brewlet.AnnotationRequestedArch])
	if fr := CheckFleet(fleet, jdk, launcher, arch); !fr.Compatible {
		res.DenyReason = fr.DenyReason
		res.DenyMessage = fr.Message
		return res
	}

	// 3. Steer scheduling onto capable nodes.
	injectNodeAffinity(pod, jdk, launcher, arch)
	return res
}

// splitArch parses the comma-separated brewlet.sh/arch annotation into a
// trimmed, non-empty token slice. An empty annotation yields nil (arch-neutral).
func splitArch(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// podArtifactRef returns the OCI artifact reference for a brewlet pod: the
// image of the container named by brewlet.sh/artifact-container if set, else the
// first container's image.
func podArtifactRef(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	if name := pod.Annotations["brewlet.sh/artifact-container"]; name != "" {
		for _, c := range pod.Spec.Containers {
			if c.Name == name {
				return c.Image
			}
		}
	}
	return pod.Spec.Containers[0].Image
}

// refDigest returns the "sha256:…" manifest digest of a digest-pinned reference
// (repo@sha256:…), or "" for a tag-based reference.
func refDigest(ref string) string {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ""
	}
	digest := ref[at+1:]
	if !strings.HasPrefix(digest, "sha256:") || len(digest) <= len("sha256:") {
		return ""
	}
	return digest
}

// requiredCapabilityLabels returns the node-label selector requirements a pod
// needs, derived from its explicit JDK/launcher/arch request. JDK and launcher
// map to presence matches (Operator: Exists) against provisioner-emitted
// capability labels; arch maps to an In match against the standard
// kubernetes.io/arch label. An empty request contributes nothing.
func requiredCapabilityLabels(jdk, launcher string, arch []string) []corev1.NodeSelectorRequirement {
	var reqs []corev1.NodeSelectorRequirement
	if jdk != "" {
		key := brewlet.LabelJDKPrefix + jdk
		if feature, ok := bareFeature(jdk); ok {
			key = brewlet.LabelJDKFeaturePrefix + strconv.Itoa(feature)
		}
		reqs = append(reqs, corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpExists})
	}
	if launcher != "" && launcher != brewlet.VanillaLauncher {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key: brewlet.LabelLauncherPrefix + launcher, Operator: corev1.NodeSelectorOpExists,
		})
	}
	if len(arch) > 0 {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key: brewlet.LabelArch, Operator: corev1.NodeSelectorOpIn, Values: arch,
		})
	}
	return reqs
}

// injectNodeAffinity ANDs the pod's capability requirements into its
// requiredDuringSchedulingIgnoredDuringExecution nodeAffinity. Because
// matchExpressions within a term are ANDed while terms are ORed, the
// requirements are appended to every existing term (and a fresh term is created
// when the pod has none), preserving any operator/user-supplied affinity.
func injectNodeAffinity(pod *corev1.Pod, jdk, launcher string, arch []string) {
	reqs := requiredCapabilityLabels(jdk, launcher, arch)
	if len(reqs) == 0 {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	sel := na.RequiredDuringSchedulingIgnoredDuringExecution
	if len(sel.NodeSelectorTerms) == 0 {
		sel.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	for i := range sel.NodeSelectorTerms {
		sel.NodeSelectorTerms[i].MatchExpressions = append(sel.NodeSelectorTerms[i].MatchExpressions, reqs...)
	}
}
