package controller

import (
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sampleApp() *appsv1alpha1.JavaApplication {
	replicas := int32(3)
	return &appsv1alpha1.JavaApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-api", Namespace: "payments"},
		Spec: appsv1alpha1.JavaApplicationSpec{
			Artifact: appsv1alpha1.ArtifactSpec{
				Image:       "registry.example.com/team/orders:1.4.2",
				PullPolicy:  corev1.PullIfNotPresent,
				PullSecrets: []string{"regcred"},
			},
			Replicas: &replicas,
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			},
			JVM: appsv1alpha1.JVMSpec{
				Version:  21,
				Launcher: "jaz",
				Args:     []string{"-XX:MaxRAMPercentage=75.0", "-XX:+UseZGC"},
			},
			Env:   []corev1.EnvVar{{Name: "SPRING_PROFILES_ACTIVE", Value: "prod"}},
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		},
	}
}

func TestBuildDeployment(t *testing.T) {
	app := sampleApp()
	dep := buildDeployment(app)

	if dep.Name != "orders-api" || dep.Namespace != "payments" {
		t.Fatalf("identity = %s/%s", dep.Namespace, dep.Name)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %v, want 3", dep.Spec.Replicas)
	}

	pod := dep.Spec.Template.Spec
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != brewlet.RuntimeClassName {
		t.Fatalf("runtimeClassName must be %q", brewlet.RuntimeClassName)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("imagePullSecrets = %v", pod.ImagePullSecrets)
	}

	c := pod.Containers[0]
	if c.Image != app.Spec.Artifact.Image {
		t.Errorf("image = %q, want the OCI artifact ref", c.Image)
	}
	if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
		t.Error("container must carry the descriptor's memory limit (sandbox cgroup)")
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("ports = %v", c.Ports)
	}

	// JDK/launcher must be stamped for the admission webhook.
	ann := dep.Spec.Template.Annotations
	if ann[brewlet.AnnotationRequestedJDK] != "21" {
		t.Errorf("jdk annotation = %q, want 21", ann[brewlet.AnnotationRequestedJDK])
	}
	if ann[brewlet.AnnotationRequestedLauncher] != "jaz" {
		t.Errorf("launcher annotation = %q, want jaz", ann[brewlet.AnnotationRequestedLauncher])
	}

	// jvm.args must be wired through via JDK_JAVA_OPTIONS (version 21) alongside user env.
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["SPRING_PROFILES_ACTIVE"] != "prod" {
		t.Error("user env must be wired through")
	}
	if env[jdkJavaOptionsEnv] != "-XX:MaxRAMPercentage=75.0 -XX:+UseZGC" {
		t.Errorf("JDK_JAVA_OPTIONS = %q", env[jdkJavaOptionsEnv])
	}
	if _, ok := env[javaToolOptionsEnv]; ok {
		t.Error("must not set JAVA_TOOL_OPTIONS for a modern (>=9) JDK")
	}
}

func TestPodAnnotationsJDKDistribution(t *testing.T) {
	cases := []struct {
		name    string
		version int32
		dist    string
		want    string // "" means the annotation must be absent
	}{
		{"bare feature", 21, "", "21"},
		{"dist and feature", 25, "microsoft", "microsoft-25"},
		{"dist trimmed", 21, "  temurin  ", "temurin-21"},
		{"distribution without version is ignored", 0, "microsoft", ""},
		{"no jvm request", 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.JVM = appsv1alpha1.JVMSpec{Version: tc.version, Distribution: tc.dist}
			ann := buildDeployment(app).Spec.Template.Annotations
			got := ann[brewlet.AnnotationRequestedJDK]
			if got != tc.want {
				t.Errorf("brewlet.sh/jdk = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodAnnotationsArch(t *testing.T) {
	cases := []struct {
		name string
		arch []string
		want string // "" means the annotation must be absent
	}{
		{"unset is arch-neutral", nil, ""},
		{"single arch", []string{"amd64"}, "amd64"},
		{"multi arch joined", []string{"amd64", "arm64"}, "amd64,arm64"},
		{"blank tokens dropped", []string{"  amd64 ", "", "  "}, "amd64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.Arch = tc.arch
			ann := buildDeployment(app).Spec.Template.Annotations
			if got := ann[brewlet.AnnotationRequestedArch]; got != tc.want {
				t.Errorf("brewlet.sh/arch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodAnnotationsCDSRegenerate(t *testing.T) {
	cases := []struct {
		name       string
		regenerate bool
		want       string // "" means the annotation must be absent
	}{
		{"unset omits annotation", false, ""},
		{"regenerate stamps true", true, "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := sampleApp()
			app.Spec.JVM.CDS = appsv1alpha1.CDSSpec{Regenerate: tc.regenerate}
			ann := buildDeployment(app).Spec.Template.Annotations
			if got := ann[brewlet.AnnotationCDSRegenerate]; got != tc.want {
				t.Errorf("brewlet.sh/cds-regenerate = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildDeploymentJDK8UsesJavaToolOptions(t *testing.T) {
	app := sampleApp()
	app.Spec.JVM.Version = 8
	c := buildDeployment(app).Spec.Template.Spec.Containers[0]

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env[javaToolOptionsEnv] != "-XX:MaxRAMPercentage=75.0 -XX:+UseZGC" {
		t.Errorf("JAVA_TOOL_OPTIONS = %q, want the jvm.args (JDK_JAVA_OPTIONS is unsupported on 8)", env[javaToolOptionsEnv])
	}
	if _, ok := env[jdkJavaOptionsEnv]; ok {
		t.Error("must not set JDK_JAVA_OPTIONS on JDK 8")
	}
}

func TestJVMOptionsEnvName(t *testing.T) {
	cases := map[int32]string{
		0:  jdkJavaOptionsEnv,  // unspecified => assume modern
		8:  javaToolOptionsEnv, // JDK 8 has no JDK_JAVA_OPTIONS
		11: jdkJavaOptionsEnv,
		21: jdkJavaOptionsEnv,
	}
	for version, want := range cases {
		if got := jvmOptionsEnvName(version); got != want {
			t.Errorf("jvmOptionsEnvName(%d) = %q, want %q", version, got, want)
		}
	}
}

func TestBuildDeploymentDefaultsAndVanillaLauncher(t *testing.T) {
	app := &appsv1alpha1.JavaApplication{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"},
		Spec: appsv1alpha1.JavaApplicationSpec{
			Artifact: appsv1alpha1.ArtifactSpec{Image: "registry.example.com/demo/hello:1.0.0"},
			JVM:      appsv1alpha1.JVMSpec{Launcher: "java"},
		},
	}
	dep := buildDeployment(app)

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("replicas default = %v, want 1", dep.Spec.Replicas)
	}
	// No version and vanilla launcher => no request annotations at all.
	if ann := dep.Spec.Template.Annotations; ann != nil {
		t.Errorf("expected no pod annotations, got %v", ann)
	}
	if dep.Spec.Template.Spec.Containers[0].Env != nil {
		t.Error("no jvm.args/env => nil container env")
	}
}

func TestBuildDeploymentRespectsUserJVMOptions(t *testing.T) {
	// A user-set options var (either name) is respected: nothing is injected.
	for _, name := range []string{javaToolOptionsEnv, jdkJavaOptionsEnv} {
		app := sampleApp() // version 21
		app.Spec.Env = []corev1.EnvVar{{Name: name, Value: "-Dexplicit=1"}}
		got := map[string]int{}
		var value string
		for _, e := range buildDeployment(app).Spec.Template.Spec.Containers[0].Env {
			if e.Name == jdkJavaOptionsEnv || e.Name == javaToolOptionsEnv {
				got[e.Name]++
				value = e.Value
			}
		}
		if len(got) != 1 || got[name] != 1 {
			t.Errorf("with user-set %s, options vars = %v, want only that one", name, got)
		}
		if value != "-Dexplicit=1" {
			t.Errorf("explicit %s overwritten: %q", name, value)
		}
	}
}

func TestBuildDeploymentReplicasNilWhenAutoscaling(t *testing.T) {
	app := sampleApp()
	app.Spec.Autoscaling = appsv1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 10}
	if r := buildDeployment(app).Spec.Replicas; r != nil {
		t.Errorf("replicas = %v, want nil so the HPA owns scaling", r)
	}
}

func TestBuildService(t *testing.T) {
	app := sampleApp()
	svc := buildService(app)
	if svc == nil {
		t.Fatal("service should be generated by default when ports are set")
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("default service type = %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("service ports = %v", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Errorf("targetPort = %v, want 8080", svc.Spec.Ports[0].TargetPort)
	}
	if svc.Spec.Selector["app.kubernetes.io/name"] != app.Name {
		t.Error("service selector must match the deployment pods")
	}
}

func TestBuildServiceDisabled(t *testing.T) {
	app := sampleApp()
	disabled := false
	app.Spec.Service.Enabled = &disabled
	if buildService(app) != nil {
		t.Error("service must be nil when disabled")
	}

	app = sampleApp()
	app.Spec.Ports = nil
	if buildService(app) != nil {
		t.Error("service must be nil when the app exposes no ports")
	}
}

func TestBuildHPA(t *testing.T) {
	app := sampleApp()
	if buildHPA(app) != nil {
		t.Fatal("HPA must be nil when autoscaling is disabled")
	}

	min := int32(3)
	target := int32(70)
	app.Spec.Autoscaling = appsv1alpha1.AutoscalingSpec{
		Enabled:                        true,
		MinReplicas:                    &min,
		MaxReplicas:                    10,
		TargetCPUUtilizationPercentage: &target,
	}
	hpa := buildHPA(app)
	if hpa == nil {
		t.Fatal("HPA must be generated when autoscaling is enabled")
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" || hpa.Spec.ScaleTargetRef.Name != app.Name {
		t.Errorf("scaleTargetRef = %+v", hpa.Spec.ScaleTargetRef)
	}
	if hpa.Spec.MaxReplicas != 10 || hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 3 {
		t.Errorf("bounds = min:%v max:%d", hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
	}
	if hpa.Spec.TargetCPUUtilizationPercentage == nil || *hpa.Spec.TargetCPUUtilizationPercentage != 70 {
		t.Errorf("cpu target = %v", hpa.Spec.TargetCPUUtilizationPercentage)
	}
}

func TestDeploymentReady(t *testing.T) {
	// Not found yet.
	if ok, _, _ := deploymentReady(nil, false); ok {
		t.Error("missing deployment must not be Ready")
	}
}
