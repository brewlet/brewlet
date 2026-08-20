package controller

import (
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config carries the operator-level knobs that shape the managed provisioner
// DaemonSet. In a real deployment these come from Helm values / operator flags
// (see cmd/manager). They mirror the provisioner's env contract (§5.3/§5.4).
type Config struct {
	// Namespace is where the operator manages the provisioner DaemonSet.
	Namespace string
	// ProvisionerImage is the brewlet-node-provisioner image to run.
	ProvisionerImage string
	// JDKs is the comma-separated <dist>-<feature> inventory that seeds the
	// chart-rendered default NodeProfile (deprecated flag path, §5.3). Per-pool
	// inventories now come from NodeProfile objects, not this field.
	JDKs string
	// Launchers is the comma-separated launcher inventory seeding the default
	// profile (deprecated flag path, §5.3).
	Launchers string
}

// buildRuntimeClass returns the desired brewlet RuntimeClass: it schedules only
// onto provisioned nodes (LabelRuntimeReady=ready) and reserves JVM overhead so
// the scheduler accounts for the runtime baseline (§7).
func buildRuntimeClass() *nodev1.RuntimeClass {
	overheadCPU := resource.MustParse("50m")
	overheadMem := resource.MustParse("64Mi")
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   brewlet.RuntimeClassName,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "brewlet-operator"},
		},
		Handler: brewlet.RuntimeClassName,
		Scheduling: &nodev1.Scheduling{
			NodeSelector: map[string]string{
				brewlet.LabelRuntimeReady: brewlet.ValueReady,
			},
		},
		Overhead: &nodev1.Overhead{
			PodFixed: corev1.ResourceList{
				corev1.ResourceCPU:    overheadCPU,
				corev1.ResourceMemory: overheadMem,
			},
		},
	}
}

func hostPathVolume(name, path string, t *corev1.HostPathType) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: t},
		},
	}
}
