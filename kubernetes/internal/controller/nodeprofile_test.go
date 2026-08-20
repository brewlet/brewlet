package controller

import (
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func jdk(dist string, feature int32) nodev1alpha1.JDKRef {
	return nodev1alpha1.JDKRef{Distribution: dist, Feature: feature}
}

func profileNamed(name string, names []string, jdks ...nodev1alpha1.JDKRef) nodev1alpha1.NodeProfile {
	return nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: names},
			JDKs:     jdks,
		},
	}
}

func labeledNode(name string, labels map[string]string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestValidateNodeProfile(t *testing.T) {
	cases := []struct {
		name    string
		spec    nodev1alpha1.NodeProfileSpec
		wantErr bool
	}{
		{
			name:    "valid",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)}},
			wantErr: false,
		},
		{
			name:    "empty jdks rejected",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: nil},
			wantErr: true,
		},
		{
			name:    "custom distribution without source rejected",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{jdk("corretto", 21)}},
			wantErr: true,
		},
		{
			name: "custom distribution accepted",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: &nodev1alpha1.JDKSource{
					Image:    "docker.io/library/azul-zulu:21",
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: false,
		},
		{
			name: "curated distribution override rejected",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "temurin",
				Feature:      21,
				Source: &nodev1alpha1.JDKSource{
					Image:    "registry.example.com/jdk:21",
					JavaHome: "/opt/jdk",
				},
			}}},
			wantErr: true,
		},
		{
			name: "custom image must be fully qualified",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: &nodev1alpha1.JDKSource{
					Image:    "azul-zulu:21",
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: true,
		},
		{
			name: "custom image must be a valid OCI reference",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: &nodev1alpha1.JDKSource{
					Image:    "registry.example.com/jdk:bad:tag",
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: true,
		},
		{
			name: "custom java home must be clean and absolute",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: &nodev1alpha1.JDKSource{
					Image:    "docker.io/library/azul-zulu:21",
					JavaHome: "/usr/lib/../jdk",
				},
			}}},
			wantErr: true,
		},
		{
			name: "JDK token must fit capability label",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "this-distribution-name-is-far-too-long-for-a-jdk-label-xx",
				Feature:      21,
				Source: &nodev1alpha1.JDKSource{
					Image:    "registry.example.com/jdk:21",
					JavaHome: "/opt/jdk",
				},
			}}},
			wantErr: true,
		},
		{
			name:    "non-positive feature rejected",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 0)}},
			wantErr: true,
		},
		{
			name: "bad containerdRestart rejected",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:    []nodev1alpha1.JDKRef{jdk("microsoft", 25)},
				Rollout: nodev1alpha1.RolloutSpec{ContainerdRestart: "reboot"},
			},
			wantErr: true,
		},
		{
			name: "label-only containerdRestart=none accepted",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:    []nodev1alpha1.JDKRef{jdk("microsoft", 25)},
				Rollout: nodev1alpha1.RolloutSpec{ContainerdRestart: "none"},
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &nodev1alpha1.NodeProfile{Spec: tc.spec}
			err := ValidateNodeProfile(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateNodeProfile() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateNoPoolConflicts(t *testing.T) {
	batchA := profileNamed("team-a", []string{"batch"}, jdk("temurin", 21))
	batchB := profileNamed("team-b", []string{"batch", "edge"}, jdk("temurin", 21))
	edge := profileNamed("edge", []string{"edge-only"}, jdk("temurin", 21))

	if err := ValidateNoPoolConflicts(&batchB, []nodev1alpha1.NodeProfile{batchA}); err == nil {
		t.Fatal("expected conflict: two profiles naming pool 'batch'")
	}
	if err := ValidateNoPoolConflicts(&edge, []nodev1alpha1.NodeProfile{batchA}); err != nil {
		t.Fatalf("disjoint pools must not conflict: %v", err)
	}
	// A profile compared against itself in the list is not a self-conflict.
	if err := ValidateNoPoolConflicts(&batchA, []nodev1alpha1.NodeProfile{batchA}); err != nil {
		t.Fatalf("self must not conflict: %v", err)
	}
}

func TestResolvePoolKey(t *testing.T) {
	gke := []corev1.Node{
		labeledNode("n1", map[string]string{"cloud.google.com/gke-nodepool": "general"}),
	}
	p := profileNamed("p", []string{"general"}, jdk("temurin", 21))
	if got := resolvePoolKey(&p, gke); got != "cloud.google.com/gke-nodepool" {
		t.Fatalf("GKE auto-detect = %q", got)
	}

	// Explicit key overrides auto-detection.
	pk := p
	pk.Spec.NodePool.Key = "brewlet.sh/pool"
	if got := resolvePoolKey(&pk, gke); got != "brewlet.sh/pool" {
		t.Fatalf("explicit key = %q, want brewlet.sh/pool", got)
	}

	// Bare-metal (no provider labels) resolves to empty.
	bare := []corev1.Node{labeledNode("b1", map[string]string{"kubernetes.io/arch": "amd64"})}
	if got := resolvePoolKey(&p, bare); got != "" {
		t.Fatalf("bare-metal key = %q, want empty", got)
	}
}

func TestNamedPoolsExcept(t *testing.T) {
	profiles := []nodev1alpha1.NodeProfile{
		profileNamed("a", []string{"batch"}, jdk("temurin", 21)),
		profileNamed("b", []string{"edge", "batch"}, jdk("temurin", 21)),
		profileNamed("default", nil, jdk("temurin", 21)),
	}
	got := namedPoolsExcept(profiles, "a")
	// Should include edge + batch (from b), deduped; exclude a's contribution.
	set := map[string]bool{}
	for _, s := range got {
		set[s] = true
	}
	if !set["edge"] || !set["batch"] {
		t.Fatalf("namedPoolsExcept = %v, want to contain edge+batch", got)
	}
}

func TestMirrorEnv(t *testing.T) {
	p := &nodev1alpha1.NodeProfile{Spec: nodev1alpha1.NodeProfileSpec{
		Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
			"mcr.microsoft.com": "registry.internal/mcr",
			"docker.io":         "registry.internal/dockerhub",
		}},
	}}
	// Deterministic (sorted by host).
	want := "docker.io=registry.internal/dockerhub,mcr.microsoft.com=registry.internal/mcr"
	if got := mirrorEnv(p); got != want {
		t.Fatalf("mirrorEnv = %q, want %q", got, want)
	}
	if got := mirrorEnv(&nodev1alpha1.NodeProfile{}); got != "" {
		t.Fatalf("no-registry mirrorEnv = %q, want empty", got)
	}
}

func TestJDKRefToken(t *testing.T) {
	if got := jdk("temurin", 21).Token(); got != "temurin-21" {
		t.Fatalf("Token() = %q, want temurin-21", got)
	}
}

func TestCustomJDKSourceEnv(t *testing.T) {
	p := profileNamed("custom", nil,
		jdk("temurin", 21),
		nodev1alpha1.JDKRef{
			Distribution: "zulu",
			Feature:      21,
			Source: &nodev1alpha1.JDKSource{
				Image:    "docker.io/library/azul-zulu:21",
				JavaHome: "/usr/lib/jvm/zulu21",
			},
		},
	)
	got := map[string]string{}
	for _, item := range customJDKSourceEnv(&p) {
		got[item.Name] = item.Value
	}
	want := map[string]string{
		"JDK_CUSTOM_SOURCE_COUNT":       "1",
		"JDK_CUSTOM_SOURCE_0_TOKEN":     "zulu-21",
		"JDK_CUSTOM_SOURCE_0_IMAGE":     "docker.io/library/azul-zulu:21",
		"JDK_CUSTOM_SOURCE_0_JAVA_HOME": "/usr/lib/jvm/zulu21",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func TestCleanupDaemonSetBuilder(t *testing.T) {
	cfg := testConfig()
	p := profileNamed("batch", []string{"batch"}, jdk("temurin", 21))
	ds := buildCleanupDaemonSet(cfg, &p, "cloud.google.com/gke-nodepool", nil)
	if ds.Name != brewlet.CleanupDaemonSetName("batch") {
		t.Fatalf("cleanup ds name = %q", ds.Name)
	}
	var mode string
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "BREWLET_MODE" {
			mode = e.Value
		}
	}
	if mode != "cleanup" {
		t.Fatalf("cleanup ds BREWLET_MODE = %q, want cleanup", mode)
	}
}
