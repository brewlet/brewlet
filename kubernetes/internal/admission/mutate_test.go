package admission

import (
	"testing"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

func brewletPod(image string, annotations map[string]string) *corev1.Pod {
	rc := brewlet.RuntimeClassName
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			RuntimeClassName: &rc,
			Containers:       []corev1.Container{{Name: "app", Image: image}},
		},
	}
	pod.Annotations = annotations
	return pod
}

func readyFleet() []NodeCapability {
	return []NodeCapability{
		{Name: "a", Ready: true, JDKs: []string{"temurin-21"}, Launchers: []string{"java"}},
		{Name: "b", Ready: true, JDKs: []string{"microsoft-25"}, Launchers: []string{"java", "jaz"}},
	}
}

func TestMutatePod_NonBrewletUntouched(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "nginx"}}}}
	res := MutatePod(pod, readyFleet())
	if res.Applies {
		t.Fatal("non-brewlet pod must not be considered")
	}
	if pod.Annotations != nil {
		t.Fatal("non-brewlet pod must be untouched")
	}
}

func TestMutatePod_StampsRefAndDigest(t *testing.T) {
	pod := brewletPod("registry.example.com/demo/hello@sha256:"+
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	res := MutatePod(pod, readyFleet())
	if !res.Applies || res.DenyReason != "" {
		t.Fatalf("expected clean apply, got %+v", res)
	}
	if got := pod.Annotations[brewlet.AnnotationArtifactRef]; got == "" {
		t.Fatal("artifact-ref not stamped")
	}
	if got := pod.Annotations[brewlet.AnnotationArtifactDigest]; got != "sha256:"+
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("artifact-digest = %q", got)
	}
}

func TestMutatePod_TagRefNoDigest(t *testing.T) {
	pod := brewletPod("registry.example.com/demo/hello:1.0.0", nil)
	MutatePod(pod, readyFleet())
	if pod.Annotations[brewlet.AnnotationArtifactRef] != "registry.example.com/demo/hello:1.0.0" {
		t.Fatalf("ref = %q", pod.Annotations[brewlet.AnnotationArtifactRef])
	}
	if _, ok := pod.Annotations[brewlet.AnnotationArtifactDigest]; ok {
		t.Fatal("tag-based ref must not stamp a digest")
	}
}

func TestMutatePod_PreservesExistingRef(t *testing.T) {
	pod := brewletPod("first/image:1", map[string]string{
		brewlet.AnnotationArtifactRef: "override/ref:9",
	})
	MutatePod(pod, readyFleet())
	if pod.Annotations[brewlet.AnnotationArtifactRef] != "override/ref:9" {
		t.Fatalf("existing ref overwritten: %q", pod.Annotations[brewlet.AnnotationArtifactRef])
	}
}

func TestMutatePod_ArtifactContainerOverride(t *testing.T) {
	rc := brewlet.RuntimeClassName
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		RuntimeClassName: &rc,
		Containers: []corev1.Container{
			{Name: "sidecar", Image: "sidecar:1"},
			{Name: "app", Image: "the/jar:2"},
		},
	}}
	pod.Annotations = map[string]string{"brewlet.sh/artifact-container": "app"}
	MutatePod(pod, readyFleet())
	if pod.Annotations[brewlet.AnnotationArtifactRef] != "the/jar:2" {
		t.Fatalf("ref = %q, want the/jar:2", pod.Annotations[brewlet.AnnotationArtifactRef])
	}
}

func TestMutatePod_NoRequestNoAffinity(t *testing.T) {
	pod := brewletPod("demo/hello:1", nil)
	res := MutatePod(pod, readyFleet())
	if res.DenyReason != "" {
		t.Fatalf("unexpected deny: %+v", res)
	}
	if pod.Spec.Affinity != nil {
		t.Fatal("no explicit JDK/launcher => no affinity should be injected")
	}
}

func TestMutatePod_InjectsFeatureAffinity(t *testing.T) {
	pod := brewletPod("demo/hello:1", map[string]string{brewlet.AnnotationRequestedJDK: "21"})
	res := MutatePod(pod, readyFleet())
	if res.DenyReason != "" {
		t.Fatalf("unexpected deny: %+v", res)
	}
	reqs := requirementsFromPod(t, pod)
	if len(reqs) != 1 || reqs[0].Key != brewlet.LabelJDKFeaturePrefix+"21" ||
		reqs[0].Operator != corev1.NodeSelectorOpExists {
		t.Fatalf("feature affinity = %+v", reqs)
	}
}

func TestMutatePod_InjectsDistAndLauncherAffinity(t *testing.T) {
	pod := brewletPod("demo/hello:1", map[string]string{
		brewlet.AnnotationRequestedJDK:      "microsoft-25",
		brewlet.AnnotationRequestedLauncher: "jaz",
	})
	res := MutatePod(pod, readyFleet())
	if res.DenyReason != "" {
		t.Fatalf("unexpected deny: %+v", res)
	}
	keys := map[string]bool{}
	for _, r := range requirementsFromPod(t, pod) {
		keys[r.Key] = true
	}
	if !keys[brewlet.LabelJDKPrefix+"microsoft-25"] || !keys[brewlet.LabelLauncherPrefix+"jaz"] {
		t.Fatalf("expected dist + launcher labels, got %v", keys)
	}
}

func TestMutatePod_ANDsIntoExistingTerms(t *testing.T) {
	pod := brewletPod("demo/hello:1", map[string]string{brewlet.AnnotationRequestedJDK: "temurin-21"})
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"us-1"}},
				}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"us-2"}},
				}},
			},
		},
	}}
	MutatePod(pod, readyFleet())
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("expected 2 terms preserved, got %d", len(terms))
	}
	for i, term := range terms {
		found := false
		for _, e := range term.MatchExpressions {
			if e.Key == brewlet.LabelJDKPrefix+"temurin-21" {
				found = true
			}
		}
		if !found {
			t.Errorf("term %d missing the brewlet JDK requirement (must AND into every term)", i)
		}
	}
}

func TestMutatePod_DeniesNoCompatibleJDK(t *testing.T) {
	pod := brewletPod("demo/hello:1", map[string]string{brewlet.AnnotationRequestedJDK: "temurin-17"})
	res := MutatePod(pod, readyFleet())
	if res.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Fatalf("expected NoCompatibleJDK, got %+v", res)
	}
	if pod.Spec.Affinity != nil {
		t.Fatal("denied pod should not be mutated with affinity")
	}
}

func TestMutatePod_DeniesNoCompatibleLauncher(t *testing.T) {
	pod := brewletPod("demo/hello:1", map[string]string{
		brewlet.AnnotationRequestedJDK:      "temurin-21",
		brewlet.AnnotationRequestedLauncher: "jaz",
	})
	res := MutatePod(pod, readyFleet())
	if res.DenyReason != brewlet.ReasonNoCompatibleLauncher {
		t.Fatalf("expected NoCompatibleLauncher, got %+v", res)
	}
}

func TestMutatePod_InjectsArchAffinity(t *testing.T) {
	fleet := []NodeCapability{
		{Name: "a", Ready: true, Arch: "amd64", JDKs: []string{"temurin-21"}, Launchers: []string{"java"}},
		{Name: "b", Ready: true, Arch: "arm64", JDKs: []string{"temurin-21"}, Launchers: []string{"java"}},
	}
	pod := brewletPod("demo/hello:1", map[string]string{brewlet.AnnotationRequestedArch: "amd64"})
	res := MutatePod(pod, fleet)
	if res.DenyReason != "" {
		t.Fatalf("unexpected deny: %+v", res)
	}
	reqs := requirementsFromPod(t, pod)
	if len(reqs) != 1 || reqs[0].Key != brewlet.LabelArch ||
		reqs[0].Operator != corev1.NodeSelectorOpIn || len(reqs[0].Values) != 1 || reqs[0].Values[0] != "amd64" {
		t.Fatalf("arch affinity = %+v", reqs)
	}
}

func TestMutatePod_DeniesNoCompatibleArch(t *testing.T) {
	fleet := []NodeCapability{
		{Name: "a", Ready: true, Arch: "amd64", JDKs: []string{"temurin-21"}, Launchers: []string{"java"}},
	}
	pod := brewletPod("demo/hello:1", map[string]string{brewlet.AnnotationRequestedArch: "arm64"})
	res := MutatePod(pod, fleet)
	if res.DenyReason != brewlet.ReasonNoCompatibleArch {
		t.Fatalf("expected NoCompatibleArch, got %+v", res)
	}
	if pod.Spec.Affinity != nil {
		t.Fatal("denied pod should not be mutated with affinity")
	}
}

func TestRefDigest(t *testing.T) {
	cases := map[string]string{
		"repo/x:1":                 "",
		"repo/x@sha256:" + hex64(): "sha256:" + hex64(),
		"repo/x@sha512:abcd":       "", // only sha256 recognized
		"repo/x@sha256:":           "", // empty digest
	}
	for in, want := range cases {
		if got := refDigest(in); got != want {
			t.Errorf("refDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

func hex64() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

func requirementsFromPod(t *testing.T, pod *corev1.Pod) []corev1.NodeSelectorRequirement {
	t.Helper()
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("expected nodeAffinity to be injected")
	}
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 {
		t.Fatalf("expected 1 term, got %d", len(terms))
	}
	return terms[0].MatchExpressions
}
