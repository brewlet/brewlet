package admission

import (
	"context"
	"encoding/json"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func profileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(nodev1alpha1.AddToScheme(s))
	return s
}

func profileRequest(t *testing.T, p *nodev1alpha1.NodeProfile) admission.Request {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func newValidator(t *testing.T, existing ...*nodev1alpha1.NodeProfile) *NodeProfileValidator {
	t.Helper()
	scheme := profileScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, p := range existing {
		builder = builder.WithObjects(p)
	}
	return &NodeProfileValidator{
		Client:  builder.Build(),
		Decoder: admission.NewDecoder(scheme),
	}
}

func TestNodeProfileValidator_RejectsCustomDistributionWithoutSource(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "bad"},
		Spec:       nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{Distribution: "corretto", Feature: 21}}},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed {
		t.Fatal("expected rejection for custom distribution without source")
	}
}

func TestNodeProfileValidator_RejectsEmptyJDKs(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "empty"}}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed {
		t.Fatal("expected rejection for empty jdks")
	}
}

func TestNodeProfileValidator_RejectsPoolConflict(t *testing.T) {
	existing := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}},
			JDKs:     []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}},
		},
	}
	v := newValidator(t, existing)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}},
			JDKs:     []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}},
		},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if res.Allowed {
		t.Fatal("expected rejection for two profiles naming the same pool")
	}
}

func TestNodeProfileValidator_AllowsValid(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: []string{"general"}},
			JDKs:     []nodev1alpha1.JDKRef{{Distribution: "microsoft", Feature: 25}},
		},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if !res.Allowed {
		t.Fatalf("expected valid profile to be allowed, got %+v", res.Result)
	}
}

func TestNodeProfileValidator_AllowsCustomDistributionWithSource(t *testing.T) {
	v := newValidator(t)
	p := &nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "zulu"},
		Spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
			Distribution: "zulu",
			Feature:      21,
			Source: &nodev1alpha1.JDKSource{
				Image:    "docker.io/library/azul-zulu:21",
				JavaHome: "/usr/lib/jvm/zulu21",
			},
		}}},
	}
	res := v.Handle(context.Background(), profileRequest(t, p))
	if !res.Allowed {
		t.Fatalf("expected custom JDK profile to be allowed, got %+v", res.Result)
	}
}
