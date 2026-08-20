package controller

import (
	"context"
	"fmt"
	"strconv"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// JavaApplicationReconciler implements the brewlet-operator developer-ergonomics
// controller (https://github.com/brewlet/specs). It reconciles each JavaApplication into
// a managed Deployment (runtimeClassName: brewlet) plus an optional Service and
// HorizontalPodAutoscaler, owns them via controller references (so they are
// garbage-collected with the JavaApplication), and reflects readiness on status.
type JavaApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile brings the managed objects in line with the JavaApplication and
// updates its status. It is idempotent and safe to call repeatedly.
func (r *JavaApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var app appsv1alpha1.JavaApplication
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := validateSpec(&app); err != nil {
		return r.fail(ctx, &app, "validating spec", err)
	}

	if err := r.reconcileDeployment(ctx, &app); err != nil {
		return r.fail(ctx, &app, "reconciling Deployment", err)
	}
	if err := r.reconcileService(ctx, &app); err != nil {
		return r.fail(ctx, &app, "reconciling Service", err)
	}
	if err := r.reconcileHPA(ctx, &app); err != nil {
		return r.fail(ctx, &app, "reconciling HorizontalPodAutoscaler", err)
	}

	if err := r.updateStatus(ctx, &app); err != nil {
		logger.Error(err, "updating JavaApplication status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *JavaApplicationReconciler) reconcileDeployment(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	desired := buildDeployment(app)
	dep := &appsv1.Deployment{}
	dep.Name, dep.Namespace = desired.Name, desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = desired.Labels
		// The selector is immutable; set it only on creation.
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = desired.Spec.Selector
		}
		// When autoscaling is on, desired.Spec.Replicas is nil and the HPA owns
		// the count — leave the live value untouched.
		if desired.Spec.Replicas != nil {
			dep.Spec.Replicas = desired.Spec.Replicas
		}
		dep.Spec.Template = desired.Spec.Template
		return controllerutil.SetControllerReference(app, dep, r.Scheme)
	})
	return err
}

func (r *JavaApplicationReconciler) reconcileService(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	desired := buildService(app)
	if desired == nil {
		// Service disabled (or no ports): remove any previously managed one.
		return r.deleteIfExists(ctx, &corev1.Service{}, app.Namespace, app.Name)
	}
	svc := &corev1.Service{}
	svc.Name, svc.Namespace = desired.Name, desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desired.Labels
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Type = desired.Spec.Type
		svc.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(app, svc, r.Scheme)
	})
	return err
}

func (r *JavaApplicationReconciler) reconcileHPA(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	desired := buildHPA(app)
	if desired == nil {
		return r.deleteIfExists(ctx, &autoscalingv1.HorizontalPodAutoscaler{}, app.Namespace, app.Name)
	}
	hpa := &autoscalingv1.HorizontalPodAutoscaler{}
	hpa.Name, hpa.Namespace = desired.Name, desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
		hpa.Labels = desired.Labels
		hpa.Spec = desired.Spec
		return controllerutil.SetControllerReference(app, hpa, r.Scheme)
	})
	return err
}

// validateSpec enforces spec invariants that the developer-facing contract
// documents (https://github.com/brewlet/site) but that a structural CRD schema
// cannot fully express on its own. It runs before any object is built so an
// invalid spec surfaces as a clear Ready=False/event instead of a rejected or
// nonsensical HorizontalPodAutoscaler. It is belt-and-suspenders behind the
// CRD's CEL validation for API servers that do not evaluate CEL.
func validateSpec(app *appsv1alpha1.JavaApplication) error {
	as := app.Spec.Autoscaling
	if !as.Enabled {
		return nil
	}
	if as.MaxReplicas < 1 {
		return fmt.Errorf("autoscaling.maxReplicas is required and must be >= 1 when autoscaling.enabled is true")
	}
	if as.MinReplicas != nil {
		if *as.MinReplicas < 1 {
			return fmt.Errorf("autoscaling.minReplicas must be >= 1, got %d", *as.MinReplicas)
		}
		if *as.MinReplicas > as.MaxReplicas {
			return fmt.Errorf("autoscaling.minReplicas (%d) must not exceed autoscaling.maxReplicas (%d)", *as.MinReplicas, as.MaxReplicas)
		}
	}
	return nil
}

// deleteIfExists deletes a managed object by name, ignoring a NotFound.
func (r *JavaApplicationReconciler) deleteIfExists(ctx context.Context, obj client.Object, namespace, name string) error {
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// updateStatus refreshes the JavaApplication status from the managed Deployment.
func (r *JavaApplicationReconciler) updateStatus(ctx context.Context, app *appsv1alpha1.JavaApplication) error {
	var dep appsv1.Deployment
	depErr := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &dep)
	if depErr != nil && !apierrors.IsNotFound(depErr) {
		return depErr
	}

	app.Status.ObservedGeneration = app.Generation
	app.Status.ReadyReplicas = dep.Status.ReadyReplicas
	if v := app.Spec.JVM.Version; v > 0 {
		app.Status.SelectedJdk = strconv.Itoa(int(v))
	} else {
		app.Status.SelectedJdk = ""
	}

	ready, reason, msg := deploymentReady(&dep, depErr == nil)
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               appsv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: app.Generation,
	})

	return r.Status().Update(ctx, app)
}

// deploymentReady reports whether the managed Deployment has reached its desired
// replica count, and a reason/message for the Ready condition.
func deploymentReady(dep *appsv1.Deployment, found bool) (bool, string, string) {
	if !found {
		return false, appsv1alpha1.ReasonProgressing, "Deployment not created yet"
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.ReadyReplicas >= want && dep.Status.ObservedGeneration >= dep.Generation {
		return true, appsv1alpha1.ReasonReconciled,
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, want)
	}
	return false, appsv1alpha1.ReasonProgressing,
		fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, want)
}

// fail records the error on status/events and returns it so the request requeues.
func (r *JavaApplicationReconciler) fail(ctx context.Context, app *appsv1alpha1.JavaApplication, action string, cause error) (ctrl.Result, error) {
	err := fmt.Errorf("%s: %w", action, cause)
	r.Recorder.Event(app, corev1.EventTypeWarning, appsv1alpha1.ReasonReconcileError, err.Error())
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               appsv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             appsv1alpha1.ReasonReconcileError,
		Message:            err.Error(),
		ObservedGeneration: app.Generation,
	})
	// Best-effort status write; return the original error to requeue.
	_ = r.Status().Update(ctx, app)
	return ctrl.Result{}, err
}

// SetupWithManager wires the controller to reconcile JavaApplications and the
// Deployment/Service/HPA objects it owns.
func (r *JavaApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.JavaApplication{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv1.HorizontalPodAutoscaler{}).
		Named("brewlet-javaapplication").
		Complete(r)
}
