package controller

import (
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testConfig() Config {
	return Config{
		Namespace:        "brewlet",
		ProvisionerImage: "ghcr.io/brewlet/node-provisioner:test",
		JDKs:             "temurin-21,microsoft-25",
		Launchers:        "jaz",
	}
}

func TestBuildRuntimeClass(t *testing.T) {
	rc := buildRuntimeClass()
	if rc.Name != brewlet.RuntimeClassName {
		t.Fatalf("name = %q, want %q", rc.Name, brewlet.RuntimeClassName)
	}
	if rc.Handler != brewlet.RuntimeClassName {
		t.Fatalf("handler = %q, want %q", rc.Handler, brewlet.RuntimeClassName)
	}
	if rc.Scheduling == nil || rc.Scheduling.NodeSelector[brewlet.LabelRuntimeReady] != brewlet.ValueReady {
		t.Fatalf("RuntimeClass must schedule only onto ready nodes, got %+v", rc.Scheduling)
	}
	if rc.Overhead == nil {
		t.Fatal("RuntimeClass must declare pod overhead for the JVM baseline")
	}
	if _, ok := rc.Overhead.PodFixed[corev1.ResourceMemory]; !ok {
		t.Fatal("overhead must reserve memory")
	}
}

func TestBuildProfileDaemonSet(t *testing.T) {
	cfg := testConfig()
	profile := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "general"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"general"}, Key: "cloud.google.com/gke-nodepool"},
			JDKs: []nodev1alpha1.JDKRef{
				{Distribution: "temurin", Feature: 21},
				{Distribution: "microsoft", Feature: 25},
			},
			Launchers: []string{"jaz"},
			Registry:  &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"mcr.microsoft.com": "registry.internal/mcr"}},
		},
	}
	ds := buildProfileDaemonSet(cfg, profile, "cloud.google.com/gke-nodepool", nil)

	if want := brewlet.ProfileDaemonSetName("general"); ds.Name != want {
		t.Fatalf("ds name = %q, want %q", ds.Name, want)
	}
	if ds.Namespace != cfg.Namespace {
		t.Fatalf("ds namespace = %q", ds.Namespace)
	}
	if ds.Spec.Template.Spec.ServiceAccountName != brewlet.ProvisionerName {
		t.Fatalf("serviceAccount = %q", ds.Spec.Template.Spec.ServiceAccountName)
	}
	if got := ds.Spec.Template.Labels[brewlet.LabelNodeProfile]; got != "general" {
		t.Fatalf("profile label = %q, want general", got)
	}

	spec := ds.Spec.Template.Spec
	if !spec.HostPID {
		t.Error("provisioner must run with hostPID to signal containerd")
	}
	if spec.ServiceAccountName != brewlet.ProvisionerName {
		t.Errorf("serviceAccount = %q", spec.ServiceAccountName)
	}

	// nodeAffinity must select the named pool on the resolved key.
	term := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if term.Key != "cloud.google.com/gke-nodepool" || term.Operator != corev1.NodeSelectorOpIn {
		t.Fatalf("affinity = %+v, want In on gke-nodepool", term)
	}
	if len(term.Values) != 1 || term.Values[0] != "general" {
		t.Fatalf("affinity values = %v, want [general]", term.Values)
	}

	// Inventory + mirror env come from the profile.
	env := map[string]string{}
	for _, e := range spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["JDKS"] != "temurin-21,microsoft-25" {
		t.Errorf("JDKS env = %q", env["JDKS"])
	}
	if env["LAUNCHERS"] != "jaz" {
		t.Errorf("LAUNCHERS env = %q", env["LAUNCHERS"])
	}
	if env["MIRRORS"] != "mcr.microsoft.com=registry.internal/mcr" {
		t.Errorf("MIRRORS env = %q", env["MIRRORS"])
	}
	if env["BREWLET_CONTAINERD_RESTART"] != "validated" {
		t.Errorf("BREWLET_CONTAINERD_RESTART env = %q, want validated (default)", env["BREWLET_CONTAINERD_RESTART"])
	}
}

func TestProfileAffinityCatchAll(t *testing.T) {
	def := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}}},
	}

	// With sibling named pools, the catch-all default excludes them via NotIn.
	aff := profileAffinity(def, "cloud.google.com/gke-nodepool", []string{"batch", "edge"})
	term := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if term.Operator != corev1.NodeSelectorOpNotIn {
		t.Fatalf("catch-all op = %v, want NotIn", term.Operator)
	}
	if len(term.Values) != 2 || term.Values[0] != "batch" || term.Values[1] != "edge" {
		t.Fatalf("catch-all NotIn values = %v, want sorted [batch edge]", term.Values)
	}

	// A lone default (no named pools) matches every node: no affinity.
	if aff := profileAffinity(def, "", nil); aff != nil {
		t.Fatalf("lone default affinity = %+v, want nil (every node)", aff)
	}
}

func TestPrettyInventory(t *testing.T) {
	cases := map[string]string{
		"":                        "none",
		"temurin-21":              "temurin-21",
		"temurin-21,microsoft-25": "temurin-21, microsoft-25",
	}
	for in, want := range cases {
		if got := prettyInventory(in); got != want {
			t.Errorf("prettyInventory(%q) = %q, want %q", in, got, want)
		}
	}
}
