package controller

import (
	"context"
	"fmt"
	"strconv"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NodeProfileReconciler reconciles NodeProfiles (https://github.com/brewlet/specs,
// 0001): it ensures the cluster-singleton brewlet RuntimeClass and one
// provisioner DaemonSet per profile (nodeAffinity selected on the resolved node
// pool), reflects assigned/ready node counts on status, and — via the
// node.brewlet.sh/cleanup finalizer — runs host cleanup before letting owner-ref
// GC drop the managed DaemonSet.
type NodeProfileReconciler struct {
	client.Client
	Recorder record.EventRecorder
	Config   Config
}

// Reconcile is invoked for NodeProfile events (and, via a Node watch, when the
// fleet changes so pool membership/readiness is re-evaluated).
func (r *NodeProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var profile nodev1alpha1.NodeProfile
	if err := r.Get(ctx, req.NamespacedName, &profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing nodes: %w", err)
	}
	var profiles nodev1alpha1.NodeProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing profiles: %w", err)
	}

	resolvedKey := resolvePoolKey(&profile, nodes.Items)
	otherPools := namedPoolsExcept(profiles.Items, profile.Name)

	// Deletion: run cleanup behind the finalizer before owner-ref GC (§5.6).
	if !profile.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &profile, resolvedKey, otherPools, nodes.Items)
	}

	if controllerutil.AddFinalizer(&profile, brewlet.FinalizerCleanup) {
		if err := r.Update(ctx, &profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	if err := r.ensureRuntimeClass(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring RuntimeClass: %w", err)
	}
	if err := r.ensureProfileDaemonSet(ctx, &profile, resolvedKey, otherPools); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring provisioner DaemonSet: %w", err)
	}

	return ctrl.Result{}, r.updateStatus(ctx, &profile, resolvedKey, otherPools, nodes.Items)
}

// reconcileDelete launches the cleanup DaemonSet, and only removes the finalizer
// (unblocking owner-ref GC of the managed provisioner DaemonSet) once cleanup
// has completed on every assigned node.
func (r *NodeProfileReconciler) reconcileDelete(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(profile, brewlet.FinalizerCleanup) {
		return ctrl.Result{}, nil
	}

	assigned, _ := r.poolCounts(profile, resolvedKey, otherPools, nodes)

	done, err := r.ensureCleanupComplete(ctx, profile, resolvedKey, otherPools, assigned)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		r.setDeleting(ctx, profile)
		logger.Info("cleanup in progress; holding finalizer", "profile", profile.Name)
		return ctrl.Result{Requeue: true}, nil
	}

	// Cleanup finished: tear down the cleanup DaemonSet and release the finalizer.
	if err := r.deleteCleanupDaemonSet(ctx, profile); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(profile, brewlet.FinalizerCleanup)
	if err := r.Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// ensureCleanupComplete makes sure the cleanup DaemonSet exists and reports
// whether it has finished on every assigned node. With no assigned nodes there
// is nothing to clean, so it completes immediately.
func (r *NodeProfileReconciler) ensureCleanupComplete(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, assigned int32) (bool, error) {
	if assigned == 0 {
		return true, nil
	}
	desired := buildCleanupDaemonSet(r.Config, profile, resolvedKey, otherPools)
	ds := &appsv1.DaemonSet{}
	ds.Name = desired.Name
	ds.Namespace = desired.Namespace
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		if err := controllerutil.SetControllerReference(profile, ds, r.Scheme()); err != nil {
			return err
		}
		ds.Labels = desired.Labels
		ds.Spec = desired.Spec
		return nil
	}); err != nil {
		return false, fmt.Errorf("ensuring cleanup DaemonSet: %w", err)
	}
	// Complete once the cleanup DaemonSet has run to ready on all its nodes.
	if ds.Status.DesiredNumberScheduled > 0 && ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled {
		return true, nil
	}
	return false, nil
}

func (r *NodeProfileReconciler) deleteCleanupDaemonSet(ctx context.Context, profile *nodev1alpha1.NodeProfile) error {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:      brewlet.CleanupDaemonSetName(profile.Name),
		Namespace: r.Config.Namespace,
	}}
	if err := r.Delete(ctx, ds); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting cleanup DaemonSet: %w", err)
	}
	return nil
}

func (r *NodeProfileReconciler) ensureRuntimeClass(ctx context.Context) error {
	desired := buildRuntimeClass()
	rc := &nodev1.RuntimeClass{}
	rc.Name = desired.Name
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rc, func() error {
		rc.Handler = desired.Handler
		rc.Scheduling = desired.Scheduling
		rc.Overhead = desired.Overhead
		if rc.Labels == nil {
			rc.Labels = map[string]string{}
		}
		for k, v := range desired.Labels {
			rc.Labels[k] = v
		}
		return nil
	})
	return err
}

func (r *NodeProfileReconciler) ensureProfileDaemonSet(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) error {
	desired := buildProfileDaemonSet(r.Config, profile, resolvedKey, otherPools)
	ds := &appsv1.DaemonSet{}
	ds.Name = desired.Name
	ds.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		if err := controllerutil.SetControllerReference(profile, ds, r.Scheme()); err != nil {
			return err
		}
		ds.Labels = desired.Labels
		ds.Spec = desired.Spec
		return nil
	})
	return err
}

// poolCounts returns the number of nodes assigned to a profile and how many of
// those advertise the brewlet runtime. The catch-all default owns every node not
// claimed by a named pool (§5.6).
func (r *NodeProfileReconciler) poolCounts(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (assigned, ready int32) {
	for i := range nodes {
		node := &nodes[i]
		if !r.nodeAssigned(profile, resolvedKey, otherPools, node) {
			continue
		}
		assigned++
		appliedRevision := node.Annotations[brewlet.AnnotationProfile] == profile.Name &&
			node.Annotations[brewlet.AnnotationProfileGeneration] == strconv.FormatInt(profile.Generation, 10)
		if node.Labels[brewlet.LabelRuntimeReady] == brewlet.ValueReady &&
			(profile.Generation == 0 || appliedRevision) {
			ready++
		}
	}
	return assigned, ready
}

// nodeAssigned reports whether a node belongs to the given profile.
func (r *NodeProfileReconciler) nodeAssigned(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, node *corev1.Node) bool {
	if !isDefaultProfile(profile) {
		return nodeInPool(node, resolvedKey, profile.Spec.NodePool.Names)
	}
	// Catch-all default: every node not claimed by a named pool.
	if resolvedKey != "" && len(otherPools) > 0 && nodeInPool(node, resolvedKey, otherPools) {
		return false
	}
	return true
}

// updateStatus recomputes assigned/ready counts and the Ready condition, and
// emits NodeUnmatched when a named pool resolves to zero nodes (§14).
func (r *NodeProfileReconciler) updateStatus(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) error {
	assigned, ready := r.poolCounts(profile, resolvedKey, otherPools, nodes)
	failed, failReason := r.assignedNodeFailure(profile, resolvedKey, otherPools, nodes)

	base := profile.DeepCopy()
	profile.Status.ObservedGeneration = profile.Generation
	profile.Status.ResolvedPoolKey = resolvedKey
	profile.Status.AssignedNodes = assigned
	profile.Status.ReadyNodes = ready

	cond := metav1.Condition{Type: nodev1alpha1.ConditionReady, ObservedGeneration: profile.Generation}
	switch {
	case !isDefaultProfile(profile) && assigned == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = nodev1alpha1.ReasonEmptyPool
		cond.Message = fmt.Sprintf("no nodes match pool(s) %v on key %q", profile.Spec.NodePool.Names, resolvedKey)
		r.Recorder.Eventf(profile, corev1.EventTypeWarning, brewlet.ReasonNodeUnmatched, cond.Message)
	case failed:
		cond.Status = metav1.ConditionFalse
		cond.Reason = nodev1alpha1.ReasonNodeFailure
		cond.Message = failReason
	case ready >= assigned:
		cond.Status = metav1.ConditionTrue
		cond.Reason = nodev1alpha1.ReasonAllNodesProvisioned
		cond.Message = fmt.Sprintf("%d/%d assigned nodes provisioned", ready, assigned)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = nodev1alpha1.ReasonProvisioning
		cond.Message = fmt.Sprintf("%d/%d assigned nodes provisioned", ready, assigned)
	}
	meta.SetStatusCondition(&profile.Status.Conditions, cond)

	if equalStatus(&base.Status, &profile.Status) {
		return nil
	}
	return r.Status().Update(ctx, profile)
}

// assignedNodeFailure reports whether any assigned node carries a
// provision-error annotation (proposal 0002), propagating it as the profile's
// Degraded reason (§5.5).
func (r *NodeProfileReconciler) assignedNodeFailure(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (bool, string) {
	for i := range nodes {
		node := &nodes[i]
		if !r.nodeAssigned(profile, resolvedKey, otherPools, node) {
			continue
		}
		if e := node.Annotations[brewlet.AnnotationProvisionError]; e != "" {
			return true, fmt.Sprintf("node %s: %s", node.Name, e)
		}
	}
	return false, ""
}

// setDeleting best-effort marks the profile Ready=False/CleanupPending while its
// cleanup runs, so `kubectl get nodeprofile` shows the teardown in flight.
func (r *NodeProfileReconciler) setDeleting(ctx context.Context, profile *nodev1alpha1.NodeProfile) {
	base := profile.DeepCopy()
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               nodev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             nodev1alpha1.ReasonCleanupPending,
		Message:            "running host cleanup before deletion",
		ObservedGeneration: profile.Generation,
	})
	if equalStatus(&base.Status, &profile.Status) {
		return
	}
	_ = r.Status().Update(ctx, profile)
}

// namedPoolsExcept returns the union of pool names claimed by every profile
// except the named one.
func namedPoolsExcept(profiles []nodev1alpha1.NodeProfile, except string) []string {
	seen := map[string]struct{}{}
	var out []string
	for i := range profiles {
		p := &profiles[i]
		if p.Name == except {
			continue
		}
		for _, name := range p.Spec.NodePool.Names {
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, name)
			}
		}
	}
	return out
}

// equalStatus compares the fields updateStatus manages (ignoring condition
// timestamps) to avoid a hot status-write loop.
func equalStatus(a, b *nodev1alpha1.NodeProfileStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration ||
		a.ResolvedPoolKey != b.ResolvedPoolKey ||
		a.AssignedNodes != b.AssignedNodes ||
		a.ReadyNodes != b.ReadyNodes ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type || ac.Status != bc.Status ||
			ac.Reason != bc.Reason || ac.Message != bc.Message ||
			ac.ObservedGeneration != bc.ObservedGeneration {
			return false
		}
	}
	return true
}

// SetupWithManager wires the controller to reconcile NodeProfiles and re-run when
// the node fleet changes (so pool membership/readiness stays current).
func (r *NodeProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nodev1alpha1.NodeProfile{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToProfiles)).
		Named("brewlet-nodeprofile").
		Complete(r)
}

// nodeToProfiles enqueues every NodeProfile when a node changes: pool membership
// and readiness are cluster-wide inputs to each profile's status.
func (r *NodeProfileReconciler) nodeToProfiles(ctx context.Context, _ client.Object) []reconcile.Request {
	var profiles nodev1alpha1.NodeProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(profiles.Items))
	for i := range profiles.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: profiles.Items[i].Name},
		})
	}
	return reqs
}
