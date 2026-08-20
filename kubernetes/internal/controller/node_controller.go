package controller

import (
	"context"
	"fmt"
	"strings"

	"brewlet-operator/internal/brewlet"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NodeReconciler implements the brewlet-operator node lifecycle controller
// (https://github.com/brewlet/specs). It reflects each brewlet node's provisioning state
// (Provisioning/Ready/Failed) via an annotation and Kubernetes events. The
// RuntimeClass and the per-profile provisioner DaemonSets are owned by the
// NodeProfileReconciler (§5.2); this controller no longer manages them — it is
// the per-node state mirror only.
type NodeReconciler struct {
	client.Client
	Recorder record.EventRecorder
	Config   Config
}

// Reconcile is invoked for Node events (and, via a Pod watch, when a provisioner
// pod changes). It is idempotent and safe to call repeatedly.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only reflect state for brewlet nodes: those a NodeProfile targets, those
	// already advertising the runtime, or (legacy) an explicitly provision-opted
	// node. A node that is none of these is left completely untouched.
	targeted, err := r.isBrewletNode(ctx, &node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking node membership: %w", err)
	}
	if !targeted {
		return ctrl.Result{}, nil
	}

	// A provisioner-reported reconfig/validation failure (proposal 0002) is a
	// hard failure regardless of pod health.
	if e := node.Annotations[brewlet.AnnotationProvisionError]; e != "" {
		return ctrl.Result{}, r.markFailed(ctx, &node, e)
	}

	// Reflect the node's provisioning state.
	if node.Labels[brewlet.LabelRuntimeReady] == brewlet.ValueReady {
		return ctrl.Result{}, r.markReady(ctx, &node)
	}

	// Not ready yet: distinguish "still working" from "failing".
	failing, reason, err := r.provisionerFailing(ctx, node.Name)
	if err != nil {
		logger.Error(err, "inspecting provisioner pod", "node", node.Name)
	}
	if failing {
		return ctrl.Result{}, r.markFailed(ctx, &node, reason)
	}
	return ctrl.Result{}, r.markProvisioning(ctx, &node)
}

// isBrewletNode reports whether the operator should track this node's
// provisioning state: it is targeted by a NodeProfile pool, already advertises
// the runtime, or carries the legacy brewlet.sh/provision=true opt-in.
func (r *NodeReconciler) isBrewletNode(ctx context.Context, node *corev1.Node) (bool, error) {
	if node.Labels[brewlet.LabelProvision] == "true" ||
		node.Labels[brewlet.LabelRuntimeReady] == brewlet.ValueReady {
		return true, nil
	}
	var profiles nodev1alpha1.NodeProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return false, err
	}
	if len(profiles.Items) == 0 {
		return false, nil
	}
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return false, err
	}
	otherNamed := namedPoolsExcept(profiles.Items, "")
	for i := range profiles.Items {
		p := &profiles.Items[i]
		key := resolvePoolKey(p, nodes.Items)
		if profileClaimsNode(p, key, otherNamed, node) {
			return true, nil
		}
	}
	return false, nil
}

// profileClaimsNode mirrors NodeProfileReconciler.nodeAssigned without a
// receiver, for the node controller's membership check.
func profileClaimsNode(profile *nodev1alpha1.NodeProfile, resolvedKey string, allNamed []string, node *corev1.Node) bool {
	if !isDefaultProfile(profile) {
		return nodeInPool(node, resolvedKey, profile.Spec.NodePool.Names)
	}
	if resolvedKey != "" && len(allNamed) > 0 && nodeInPool(node, resolvedKey, allNamed) {
		return false
	}
	return true
}

// node is stuck (CrashLoopBackOff or repeated restarts), which maps to the
// ProvisionFailed event in §14.
func (r *NodeReconciler) provisionerFailing(ctx context.Context, nodeName string) (bool, string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(r.Config.Namespace),
		client.MatchingLabels{"app": brewlet.ProvisionerAppLabel},
	); err != nil {
		return false, "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != nodeName {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
				return true, fmt.Sprintf("provisioner pod %s is in CrashLoopBackOff", p.Name), nil
			}
			if cs.RestartCount >= 3 {
				return true, fmt.Sprintf("provisioner pod %s has restarted %d times", p.Name, cs.RestartCount), nil
			}
		}
	}
	return false, "", nil
}

func (r *NodeReconciler) markReady(ctx context.Context, node *corev1.Node) error {
	if node.Annotations[brewlet.AnnotationProvisionState] == brewlet.StateReady {
		return nil // already reconciled; avoid event spam
	}
	jdks := prettyInventory(node.Annotations[brewlet.AnnotationJDKs])
	launchers := prettyInventory(node.Annotations[brewlet.AnnotationLaunchers])
	r.Recorder.Eventf(node, corev1.EventTypeNormal, brewlet.ReasonNodeReady,
		"node provisioned; JDKs=[%s] launchers=[%s]", jdks, launchers)
	return r.setState(ctx, node, brewlet.StateReady)
}

func (r *NodeReconciler) markFailed(ctx context.Context, node *corev1.Node, reason string) error {
	if node.Annotations[brewlet.AnnotationProvisionState] != brewlet.StateFailed {
		r.Recorder.Event(node, corev1.EventTypeWarning, brewlet.ReasonProvisionFailed, reason)
	}
	return r.setState(ctx, node, brewlet.StateFailed)
}

func (r *NodeReconciler) markProvisioning(ctx context.Context, node *corev1.Node) error {
	if node.Annotations[brewlet.AnnotationProvisionState] != brewlet.StateProvisioning {
		r.Recorder.Event(node, corev1.EventTypeNormal, brewlet.ReasonProvisioning,
			"provisioning requested; waiting for the node to advertise the brewlet runtime")
	}
	return r.setState(ctx, node, brewlet.StateProvisioning)
}

// setState patches the node's provision-state annotation if it changed.
func (r *NodeReconciler) setState(ctx context.Context, node *corev1.Node, state string) error {
	if node.Annotations[brewlet.AnnotationProvisionState] == state {
		return nil
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[brewlet.AnnotationProvisionState] = state
	return r.Patch(ctx, node, patch)
}

// prettyInventory turns the on-node comma-separated inventory (e.g.
// "temurin-21,microsoft-25") into a spaced comma list for human-readable events.
func prettyInventory(v string) string {
	if v == "" {
		return "none"
	}
	return strings.ReplaceAll(v, ",", ", ")
}

// SetupWithManager wires the controller: it reconciles Nodes and also watches
// provisioner Pods, mapping each pod back to its node so provisioning
// progress/failure re-triggers reconciliation.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podToNode)).
		Named("brewlet-node").
		Complete(r)
}

func (r *NodeReconciler) podToNode(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Namespace != r.Config.Namespace {
		return nil
	}
	if pod.Labels["app"] != brewlet.ProvisionerAppLabel || pod.Spec.NodeName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: pod.Spec.NodeName}}}
}
