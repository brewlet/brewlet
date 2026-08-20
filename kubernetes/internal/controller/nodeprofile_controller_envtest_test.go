package controller

import (
	"context"
	"strconv"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newProfileReconciler(c client.Client, ns string) *NodeProfileReconciler {
	return &NodeProfileReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(100),
		Config: Config{
			Namespace:        ns,
			ProvisionerImage: "ghcr.io/brewlet/node-provisioner:test",
		},
	}
}

// createProfile creates a cluster-scoped NodeProfile and registers cleanup that
// clears the finalizer so the object can actually be removed after the test.
func createProfile(t *testing.T, ctx context.Context, c client.Client, name string, spec nodev1alpha1.NodeProfileSpec) *nodev1alpha1.NodeProfile {
	t.Helper()
	p := &nodev1alpha1.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("creating profile: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		var cur nodev1alpha1.NodeProfile
		if err := c.Get(bg, types.NamespacedName{Name: name}, &cur); err == nil {
			cur.Finalizers = nil
			_ = c.Update(bg, &cur)
			_ = c.Delete(bg, &cur)
		}
	})
	return p
}

func reconcileProfile(t *testing.T, ctx context.Context, r *NodeProfileReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("Reconcile(profile %s): %v", name, err)
	}
	return res
}

func getProfile(t *testing.T, ctx context.Context, c client.Client, name string) nodev1alpha1.NodeProfile {
	t.Helper()
	var p nodev1alpha1.NodeProfile
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
		t.Fatalf("getting profile %s: %v", name, err)
	}
	return p
}

func TestNodeProfileReconcileCreatesDaemonSet(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	node := createNode(t, ctx, c, map[string]string{poolKey: "batch"})
	name := uniqueName("batch")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})

	reconcileProfile(t, ctx, r, name)

	// RuntimeClass ensured.
	var rc nodev1.RuntimeClass
	if err := c.Get(ctx, types.NamespacedName{Name: brewlet.RuntimeClassName}, &rc); err != nil {
		t.Fatalf("RuntimeClass not ensured: %v", err)
	}

	// Per-profile DaemonSet created, owned by the profile.
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.ProfileDaemonSetName(name)}, &ds); err != nil {
		t.Fatalf("profile DaemonSet not created: %v", err)
	}
	if len(ds.OwnerReferences) != 1 || ds.OwnerReferences[0].Kind != "NodeProfile" {
		t.Fatalf("DaemonSet owner = %+v, want NodeProfile controller ref", ds.OwnerReferences)
	}

	// Status: one assigned node, not yet ready -> Provisioning.
	p := getProfile(t, ctx, c, name)
	if p.Status.AssignedNodes != 1 || p.Status.ReadyNodes != 0 {
		t.Fatalf("status assigned/ready = %d/%d, want 1/0", p.Status.AssignedNodes, p.Status.ReadyNodes)
	}
	if p.Status.ResolvedPoolKey != poolKey {
		t.Fatalf("resolvedPoolKey = %q, want %q", p.Status.ResolvedPoolKey, poolKey)
	}
	if r := conditionReason(p.Status.Conditions); r != nodev1alpha1.ReasonProvisioning {
		t.Fatalf("condition reason = %q, want Provisioning", r)
	}

	// Node advertises the runtime -> Ready.
	markNodeReady(t, ctx, c, node, name, p.Generation)
	reconcileProfile(t, ctx, r, name)
	p = getProfile(t, ctx, c, name)
	if p.Status.ReadyNodes != 1 {
		t.Fatalf("readyNodes = %d, want 1", p.Status.ReadyNodes)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonAllNodesProvisioned {
		t.Fatalf("condition reason = %q, want AllNodesProvisioned", reason)
	}
}

func TestNodeProfileEmptyPoolDegraded(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	// A node exists in a different pool, so the provider key resolves, but the
	// profile names a pool that matches nothing.
	createNode(t, ctx, c, map[string]string{"cloud.google.com/gke-nodepool": "general"})
	name := uniqueName("typo")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"does-not-exist"}, Key: "cloud.google.com/gke-nodepool"},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})

	reconcileProfile(t, ctx, r, name)

	p := getProfile(t, ctx, c, name)
	if p.Status.AssignedNodes != 0 {
		t.Fatalf("assignedNodes = %d, want 0", p.Status.AssignedNodes)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonEmptyPool {
		t.Fatalf("condition reason = %q, want EmptyPool", reason)
	}
}

func TestNodeProfileFinalizerBlocksGC(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	createNode(t, ctx, c, map[string]string{poolKey: "batch"})
	name := uniqueName("reversal")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})

	// First reconcile adds the finalizer + the provisioner DaemonSet.
	reconcileProfile(t, ctx, r, name)
	p := getProfile(t, ctx, c, name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("finalizer not added: %v", p.Finalizers)
	}

	// Delete: the finalizer keeps the object alive; cleanup DaemonSet appears.
	if err := c.Delete(ctx, &p); err != nil {
		t.Fatalf("deleting profile: %v", err)
	}
	reconcileProfile(t, ctx, r, name)

	p = getProfile(t, ctx, c, name) // still present: finalizer blocks GC
	if p.DeletionTimestamp.IsZero() {
		t.Fatal("expected deletionTimestamp to be set")
	}
	var cleanup appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.CleanupDaemonSetName(name)}, &cleanup); err != nil {
		t.Fatalf("cleanup DaemonSet not created: %v", err)
	}

	// Simulate the cleanup DaemonSet finishing on its node.
	cleanup.Status.DesiredNumberScheduled = 1
	cleanup.Status.NumberReady = 1
	if err := c.Status().Update(ctx, &cleanup); err != nil {
		t.Fatalf("updating cleanup status: %v", err)
	}

	// Next reconcile removes the finalizer, so the object can finally be GC'd.
	reconcileProfile(t, ctx, r, name)
	var gone nodev1alpha1.NodeProfile
	err := c.Get(ctx, types.NamespacedName{Name: name}, &gone)
	if err == nil && !gone.DeletionTimestamp.IsZero() && len(gone.Finalizers) == 0 {
		// finalizer removed; apiserver will finalize deletion — acceptable.
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error getting profile after cleanup: %v", err)
	} else if err == nil && containsString(gone.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("finalizer still present after cleanup completed: %v", gone.Finalizers)
	}
}

func markNodeReady(t *testing.T, ctx context.Context, c client.Client, name, profile string, generation int64) {
	t.Helper()
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatalf("getting node: %v", err)
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
	node.Annotations[brewlet.AnnotationProfile] = profile
	node.Annotations[brewlet.AnnotationProfileGeneration] = strconv.FormatInt(generation, 10)
	if err := c.Patch(ctx, &node, patch); err != nil {
		t.Fatalf("marking node ready: %v", err)
	}
}

func conditionReason(conds []metav1.Condition) string {
	for _, c := range conds {
		if c.Type == nodev1alpha1.ConditionReady {
			return c.Reason
		}
	}
	return ""
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

var nameCounter int

func uniqueName(prefix string) string {
	nameCounter++
	return prefix + "-" + itoaTest(nameCounter)
}

func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
