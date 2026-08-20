package controller

import (
	"context"
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newJavaAppReconciler builds a reconciler bound to the live envtest apiserver.
func newJavaAppReconciler(c client.Client) *JavaApplicationReconciler {
	return &JavaApplicationReconciler{
		Client:   c,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(100),
	}
}

// createNamespace makes a uniquely-named namespace and returns its name.
func createNamespace(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "japp-test-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("creating test namespace: %v", err)
	}
	name := ns.Name
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return name
}

// reconcileJavaApp runs one Reconcile pass for the named object.
func reconcileJavaApp(t *testing.T, ctx context.Context, r *JavaApplicationReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("Reconcile(%s/%s): %v", ns, name, err)
	}
}

func newJavaApp(ns, name string) *appsv1alpha1.JavaApplication {
	replicas := int32(2)
	return &appsv1alpha1.JavaApplication{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1alpha1.JavaApplicationSpec{
			Artifact: appsv1alpha1.ArtifactSpec{Image: "registry.example.com/team/orders:1.4.2"},
			Replicas: &replicas,
			JVM:      appsv1alpha1.JVMSpec{Version: 21, Launcher: "jaz"},
			Ports:    []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		},
	}
}

func TestReconcileCreatesManagedObjects(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)

	app := newJavaApp(ns, "orders-api")
	if err := c.Create(ctx, app); err != nil {
		t.Fatalf("creating JavaApplication: %v", err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)

	// Deployment: created, runtimeClassName=brewlet, owned by the app, replicas honored.
	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &dep); err != nil {
		t.Fatalf("managed Deployment not created: %v", err)
	}
	if rc := dep.Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != brewlet.RuntimeClassName {
		t.Fatalf("Deployment runtimeClassName = %v, want %q", rc, brewlet.RuntimeClassName)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("Deployment replicas = %v, want 2", dep.Spec.Replicas)
	}
	assertOwnedBy(t, dep.OwnerReferences, app)

	// Service: created (ports present, enabled by default) and owned.
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &svc); err != nil {
		t.Fatalf("managed Service not created: %v", err)
	}
	assertOwnedBy(t, svc.OwnerReferences, app)

	// HPA: not created (autoscaling disabled).
	var hpa autoscalingv1.HorizontalPodAutoscaler
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &hpa); !apierrors.IsNotFound(err) {
		t.Fatalf("HPA get err = %v, want NotFound (autoscaling disabled)", err)
	}

	// Status: observedGeneration, selectedJdk, and a Ready=False/Progressing
	// condition (no kubelet in envtest, so replicas never become Ready).
	var got appsv1alpha1.JavaApplication
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &got); err != nil {
		t.Fatalf("re-getting JavaApplication: %v", err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("status.observedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
	if got.Status.SelectedJdk != "21" {
		t.Fatalf("status.selectedJdk = %q, want %q", got.Status.SelectedJdk, "21")
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, appsv1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != appsv1alpha1.ReasonProgressing {
		t.Fatalf("Ready condition = %s/%s, want False/%s", cond.Status, cond.Reason, appsv1alpha1.ReasonProgressing)
	}
}

func TestReconcileServiceAddedThenRemoved(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)

	app := newJavaApp(ns, "web")
	if err := c.Create(ctx, app); err != nil {
		t.Fatalf("creating JavaApplication: %v", err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)

	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &svc); err != nil {
		t.Fatalf("Service should exist after first reconcile: %v", err)
	}

	// Disable the Service; the reconciler must delete the one it previously managed.
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, app); err != nil {
		t.Fatalf("re-getting JavaApplication: %v", err)
	}
	disabled := false
	app.Spec.Service.Enabled = &disabled
	if err := c.Update(ctx, app); err != nil {
		t.Fatalf("updating JavaApplication: %v", err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)

	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &svc); !apierrors.IsNotFound(err) {
		t.Fatalf("Service get err = %v, want NotFound after disabling", err)
	}
}

func TestReconcileHPAAddedThenRemoved(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)

	app := newJavaApp(ns, "scaler")
	min := int32(1)
	app.Spec.Autoscaling = appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: &min, MaxReplicas: 5}
	if err := c.Create(ctx, app); err != nil {
		t.Fatalf("creating JavaApplication: %v", err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)

	var hpa autoscalingv1.HorizontalPodAutoscaler
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &hpa); err != nil {
		t.Fatalf("HPA should exist when autoscaling enabled: %v", err)
	}
	assertOwnedBy(t, hpa.OwnerReferences, app)

	// With autoscaling on the HPA owns the replica count. Simulate the HPA
	// scaling the Deployment to 4, reconcile again, and assert the controller
	// leaves the live value untouched (desired replicas is nil) instead of
	// resetting it and fighting the HPA every reconcile.
	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &dep); err != nil {
		t.Fatalf("getting Deployment: %v", err)
	}
	four := int32(4)
	dep.Spec.Replicas = &four
	if err := c.Update(ctx, &dep); err != nil {
		t.Fatalf("simulating HPA scale: %v", err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &dep); err != nil {
		t.Fatalf("re-getting Deployment: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 4 {
		t.Fatalf("Deployment replicas = %v, want 4 (HPA-owned; controller must not reset it)", dep.Spec.Replicas)
	}

	// Disable autoscaling; the managed HPA must be removed.
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, app); err != nil {
		t.Fatalf("re-getting JavaApplication: %v", err)
	}
	app.Spec.Autoscaling = appsv1alpha1.AutoscalingSpec{Enabled: false}
	if err := c.Update(ctx, app); err != nil {
		t.Fatalf("updating JavaApplication: %v", err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)

	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &hpa); !apierrors.IsNotFound(err) {
		t.Fatalf("HPA get err = %v, want NotFound after disabling autoscaling", err)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)

	app := newJavaApp(ns, "steady")
	if err := c.Create(ctx, app); err != nil {
		t.Fatalf("creating JavaApplication: %v", err)
	}

	reconcileJavaApp(t, ctx, r, ns, app.Name)
	var first appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &first); err != nil {
		t.Fatalf("getting Deployment after first reconcile: %v", err)
	}

	// A second reconcile with no spec change must not error and must not churn
	// the Deployment (same UID, selector stays put — the selector is immutable).
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	var second appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: app.Name}, &second); err != nil {
		t.Fatalf("getting Deployment after second reconcile: %v", err)
	}
	if first.UID != second.UID {
		t.Fatalf("Deployment recreated across reconciles: %s -> %s", first.UID, second.UID)
	}
	want := selectorLabels(app)
	if got := second.Spec.Selector.MatchLabels; !mapsEqual(got, want) {
		t.Fatalf("Deployment selector = %v, want %v", got, want)
	}
}

func TestReconcileMissingObjectIsNoop(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)

	// A request for a JavaApplication that does not exist must be a clean no-op
	// (client.IgnoreNotFound), not an error.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "ghost"}}); err != nil {
		t.Fatalf("Reconcile of missing object returned err = %v, want nil", err)
	}
}

func assertOwnedBy(t *testing.T, refs []metav1.OwnerReference, app *appsv1alpha1.JavaApplication) {
	t.Helper()
	for _, ref := range refs {
		if ref.Kind == "JavaApplication" && ref.Name == app.Name {
			if ref.Controller == nil || !*ref.Controller {
				t.Fatalf("owner ref for %s is not a controller ref", app.Name)
			}
			return
		}
	}
	t.Fatalf("expected a controller owner reference to JavaApplication %s, got %+v", app.Name, refs)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
