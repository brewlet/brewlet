package controller

import (
	"strconv"
	"strings"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// appContainerName is the name of the single JAR container in generated pods.
const appContainerName = "app"

// JVM options env vars. Brewlet wires the user's jvm.args through one of these
// (it injects no tuning of its own — §8.2/§10). JDK_JAVA_OPTIONS is the modern,
// launcher-scoped variable (JDK 9+) and is preferred; it is unsupported on JDK 8,
// where JAVA_TOOL_OPTIONS (interpreted by the VM itself) is the only option.
// See https://bugs.openjdk.org/browse/JDK-8170832.
const (
	jdkJavaOptionsEnv  = "JDK_JAVA_OPTIONS"
	javaToolOptionsEnv = "JAVA_TOOL_OPTIONS"
)

// jvmOptionsEnvName returns the env var Brewlet should use to pass jvm.args for a
// given JDK feature version: JAVA_TOOL_OPTIONS on JDK 8 (JDK_JAVA_OPTIONS is not
// supported there), JDK_JAVA_OPTIONS otherwise (including when the version is
// unspecified, where a modern JDK is assumed).
func jvmOptionsEnvName(version int32) string {
	if version == 8 {
		return javaToolOptionsEnv
	}
	return jdkJavaOptionsEnv
}

// selectorLabels are the immutable pod-selector labels for a JavaApplication's
// managed objects. Kept minimal and stable (the Deployment selector is
// immutable) and unique per JavaApplication within its namespace.
func selectorLabels(app *appsv1alpha1.JavaApplication) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app.Name,
		"app.kubernetes.io/managed-by": "brewlet-operator",
	}
}

// buildDeployment renders the managed Deployment for a JavaApplication (§8.2):
// runtimeClassName=brewlet, container image = the OCI artifact, resources
// copied verbatim, JDK/launcher stamped as pod annotations for the admission
// webhook, and user env/ports/probes/jvm.args wired through.
func buildDeployment(app *appsv1alpha1.JavaApplication) *appsv1.Deployment {
	labels := selectorLabels(app)

	container := corev1.Container{
		Name:            appContainerName,
		Image:           app.Spec.Artifact.Image,
		ImagePullPolicy: app.Spec.Artifact.PullPolicy,
		Resources:       app.Spec.Resources,
		Ports:           app.Spec.Ports,
		Env:             buildEnv(app),
		ReadinessProbe:  app.Spec.Probes.Readiness,
		LivenessProbe:   app.Spec.Probes.Liveness,
	}

	runtimeClass := brewlet.RuntimeClassName
	podSpec := corev1.PodSpec{
		RuntimeClassName: &runtimeClass,
		Containers:       []corev1.Container{container},
	}
	for _, s := range app.Spec.Artifact.PullSecrets {
		podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: s})
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: desiredReplicas(app),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: podAnnotations(app),
				},
				Spec: podSpec,
			},
		},
	}
	return dep
}

// desiredReplicas returns the Deployment replica count. When autoscaling is
// enabled the HPA owns the replica count, so we leave it unset (nil) to avoid
// the controller and HPA fighting over it each reconcile.
func desiredReplicas(app *appsv1alpha1.JavaApplication) *int32 {
	if app.Spec.Autoscaling.Enabled {
		return nil
	}
	if app.Spec.Replicas != nil {
		return app.Spec.Replicas
	}
	one := int32(1)
	return &one
}

// podAnnotations stamps the JDK/launcher requests the admission webhook (§8.3)
// reads to validate compatibility and steer scheduling. A zero version or a
// vanilla/empty launcher contributes nothing (the webhook then imposes no
// constraint).
func podAnnotations(app *appsv1alpha1.JavaApplication) map[string]string {
	ann := map[string]string{}
	if v := app.Spec.JVM.Version; v > 0 {
		// Fold the optional distribution into the "<dist>-<feature>" request the
		// webhook and shim understand; a bare feature (no distribution) matches
		// any distribution of that version. Distribution alone (no version) is
		// not a schedulable request, so it is ignored.
		if dist := strings.TrimSpace(app.Spec.JVM.Distribution); dist != "" {
			ann[brewlet.AnnotationRequestedJDK] = dist + "-" + strconv.Itoa(int(v))
		} else {
			ann[brewlet.AnnotationRequestedJDK] = strconv.Itoa(int(v))
		}
	}
	if l := strings.TrimSpace(app.Spec.JVM.Launcher); l != "" && l != brewlet.VanillaLauncher {
		ann[brewlet.AnnotationRequestedLauncher] = l
	}
	// Fold the optional non-portable arch constraint into the brewlet.sh/arch
	// annotation the webhook reads (trimmed, non-empty tokens, comma-joined). An
	// unset/empty arch is arch-neutral and contributes nothing.
	var arch []string
	for _, a := range app.Spec.Arch {
		if a = strings.TrimSpace(a); a != "" {
			arch = append(arch, a)
		}
	}
	if len(arch) > 0 {
		ann[brewlet.AnnotationRequestedArch] = strings.Join(arch, ",")
	}
	// Opt into node-side AppCDS regeneration (https://github.com/brewlet/site). This is a
	// deployment/fleet decision, so it originates here and rides the pod as
	// brewlet.sh/cds-regenerate; the shim reads it. It carries no scheduling
	// constraint (the webhook ignores it).
	if app.Spec.JVM.CDS.Regenerate {
		ann[brewlet.AnnotationCDSRegenerate] = "true"
	}
	if len(ann) == 0 {
		return nil
	}
	return ann
}

// buildEnv wires the user env through and, when jvm.args are set, exposes them
// via the version-appropriate JVM options env var (JDK_JAVA_OPTIONS, or
// JAVA_TOOL_OPTIONS on JDK 8). If the user already set either options var
// explicitly, theirs is respected and nothing is injected.
func buildEnv(app *appsv1alpha1.JavaApplication) []corev1.EnvVar {
	env := make([]corev1.EnvVar, len(app.Spec.Env))
	copy(env, app.Spec.Env)

	if len(app.Spec.JVM.Args) == 0 {
		return nilIfEmpty(env)
	}
	for _, e := range env {
		if e.Name == jdkJavaOptionsEnv || e.Name == javaToolOptionsEnv {
			return env // respect an explicit user-set JVM options var
		}
	}
	env = append(env, corev1.EnvVar{
		Name:  jvmOptionsEnvName(app.Spec.JVM.Version),
		Value: strings.Join(app.Spec.JVM.Args, " "),
	})
	return env
}

func nilIfEmpty(env []corev1.EnvVar) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	return env
}

// serviceEnabled reports whether a Service should be generated (default true).
func serviceEnabled(app *appsv1alpha1.JavaApplication) bool {
	return app.Spec.Service.Enabled == nil || *app.Spec.Service.Enabled
}

// buildService renders the managed Service, or nil when disabled or when the app
// exposes no ports (a Service needs at least one port).
func buildService(app *appsv1alpha1.JavaApplication) *corev1.Service {
	if !serviceEnabled(app) || len(app.Spec.Ports) == 0 {
		return nil
	}
	labels := selectorLabels(app)

	svcType := app.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}

	ports := make([]corev1.ServicePort, 0, len(app.Spec.Ports))
	for _, p := range app.Spec.Ports {
		ports = append(ports, corev1.ServicePort{
			Name:       p.Name,
			Port:       p.ContainerPort,
			TargetPort: intstr.FromInt32(p.ContainerPort),
			Protocol:   servicePortProtocol(p.Protocol),
		})
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			Ports:    ports,
		},
	}
}

func servicePortProtocol(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
}

// buildHPA renders the managed HorizontalPodAutoscaler, or nil when autoscaling
// is disabled.
func buildHPA(app *appsv1alpha1.JavaApplication) *autoscalingv1.HorizontalPodAutoscaler {
	if !app.Spec.Autoscaling.Enabled {
		return nil
	}
	as := app.Spec.Autoscaling
	return &autoscalingv1.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
			Labels:    selectorLabels(app),
		},
		Spec: autoscalingv1.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       app.Name,
			},
			MinReplicas:                    as.MinReplicas,
			MaxReplicas:                    as.MaxReplicas,
			TargetCPUUtilizationPercentage: as.TargetCPUUtilizationPercentage,
		},
	}
}
