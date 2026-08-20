package controller

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// profileLabels are the pod/selector labels for a profile's managed DaemonSet:
// the shared provisioner app label (so the node-state reflection and the
// admission fleet check still match) plus a per-profile label recording the
// owner.
func profileLabels(profile string) map[string]string {
	return map[string]string{
		"app":                    brewlet.ProvisionerAppLabel,
		brewlet.LabelNodeProfile: profile,
	}
}

// resolvePoolKey returns the node label key a profile's pool is matched on. An
// explicit spec.nodePool.key wins; otherwise the operator auto-detects the
// provider key by probing the known keys across the fleet (§5.1). It returns ""
// when no provider pool label is present anywhere (bare-metal / kubeadm), which
// the caller treats as "every node".
func resolvePoolKey(profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) string {
	if k := profile.Spec.NodePool.Key; k != "" {
		return k
	}
	for _, key := range brewlet.ProviderPoolKeys {
		for i := range nodes {
			if _, ok := nodes[i].Labels[key]; ok {
				return key
			}
		}
	}
	return ""
}

// isDefaultProfile reports whether a profile is the empty-pool catch-all (no
// pool names) that owns every node not claimed by a named-pool profile (§5.6).
func isDefaultProfile(profile *nodev1alpha1.NodeProfile) bool {
	return len(profile.Spec.NodePool.Names) == 0
}

// nodeInPool reports whether a node belongs to one of the given pool names on
// the resolved key.
func nodeInPool(node *corev1.Node, key string, names []string) bool {
	if key == "" || len(names) == 0 {
		return false
	}
	val, ok := node.Labels[key]
	if !ok {
		return false
	}
	for _, n := range names {
		if val == n {
			return true
		}
	}
	return false
}

// profileAffinity computes the nodeAffinity term for a profile's DaemonSet:
//   - named pool:      <resolvedKey> In [names]
//   - catch-all default with sibling named pools: <resolvedKey> NotIn [namedPools]
//   - catch-all default with no named pools / no pool key: nil (every node)
//
// otherPools is the set of pool names claimed by OTHER (named) profiles; it is
// what excludes the catch-all default from named pools (§5.6).
func profileAffinity(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) *corev1.Affinity {
	names := profile.Spec.NodePool.Names

	if len(names) > 0 {
		key := resolvedKey
		if key == "" {
			// Could not resolve a pool key for a named-pool profile: land the
			// DaemonSet nowhere (the reconciler surfaces EmptyPool) rather than
			// emit an invalid empty-key selector or provision every node.
			key = brewlet.ProviderPoolKeys[0]
		}
		return requiredNodeAffinity(key, corev1.NodeSelectorOpIn, names)
	}

	// Catch-all default.
	if resolvedKey != "" && len(otherPools) > 0 {
		return requiredNodeAffinity(resolvedKey, corev1.NodeSelectorOpNotIn, otherPools)
	}
	// Bare-metal / lone default: every node.
	return nil
}

func requiredNodeAffinity(key string, op corev1.NodeSelectorOperator, values []string) *corev1.Affinity {
	vals := append([]string(nil), values...)
	sort.Strings(vals)
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      key,
						Operator: op,
						Values:   vals,
					}},
				}},
			},
		},
	}
}

// jdkTokens renders a profile's JDK inventory as the comma-separated
// "<dist>-<feature>" list the provisioner env (JDKS) expects.
func jdkTokens(profile *nodev1alpha1.NodeProfile) string {
	toks := make([]string, 0, len(profile.Spec.JDKs))
	for _, j := range profile.Spec.JDKs {
		toks = append(toks, j.Token())
	}
	return strings.Join(toks, ",")
}

func customJDKSourceEnv(profile *nodev1alpha1.NodeProfile) []corev1.EnvVar {
	var sources []corev1.EnvVar
	count := 0
	for _, j := range profile.Spec.JDKs {
		if j.Source == nil {
			continue
		}
		prefix := fmt.Sprintf("JDK_CUSTOM_SOURCE_%d_", count)
		sources = append(sources,
			corev1.EnvVar{Name: prefix + "TOKEN", Value: j.Token()},
			corev1.EnvVar{Name: prefix + "IMAGE", Value: j.Source.Image},
			corev1.EnvVar{Name: prefix + "JAVA_HOME", Value: j.Source.JavaHome},
		)
		count++
	}
	if count == 0 {
		return nil
	}
	return append([]corev1.EnvVar{{Name: "JDK_CUSTOM_SOURCE_COUNT", Value: fmt.Sprintf("%d", count)}}, sources...)
}

// mirrorEnv encodes a profile's registry mirrors as a deterministic
// comma-separated "host=mirror" list for the provisioner's MIRRORS env (§5.6).
// Returns "" when no mirrors are configured.
func mirrorEnv(profile *nodev1alpha1.NodeProfile) string {
	if profile.Spec.Registry == nil || len(profile.Spec.Registry.Mirrors) == 0 {
		return ""
	}
	hosts := make([]string, 0, len(profile.Spec.Registry.Mirrors))
	for h := range profile.Spec.Registry.Mirrors {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	pairs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		pairs = append(pairs, h+"="+profile.Spec.Registry.Mirrors[h])
	}
	return strings.Join(pairs, ",")
}

// containerdRestart returns the effective containerd-restart mode for a profile,
// defaulting to "validated" (proposal 0002).
func containerdRestart(profile *nodev1alpha1.NodeProfile) string {
	if m := profile.Spec.Rollout.ContainerdRestart; m != "" {
		return m
	}
	return nodev1alpha1.ContainerdRestartValidated
}

// buildProfileDaemonSet returns the provisioner DaemonSet for one profile — the
// generalized buildDaemonSet: pod nodeAffinity comes from the profile's pool and
// JDKS/LAUNCHERS/MIRRORS env come from its inventory (§5.2). Pool disjointness is
// what enforces single ownership; there is no per-node assignment label.
func buildProfileDaemonSet(cfg Config, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) *appsv1.DaemonSet {
	privileged := true
	hostPathSocket := corev1.HostPathSocket
	lbls := profileLabels(profile.Name)

	env := []corev1.EnvVar{
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
		{Name: "JDKS", Value: jdkTokens(profile)},
		{Name: "LAUNCHERS", Value: strings.Join(profile.Spec.Launchers, ",")},
		{Name: "BREWLET_CONTAINERD_RESTART", Value: containerdRestart(profile)},
		{Name: "BREWLET_PROFILE_NAME", Value: profile.Name},
		{Name: "BREWLET_PROFILE_GENERATION", Value: strconv.FormatInt(profile.Generation, 10)},
	}
	env = append(env, customJDKSourceEnv(profile)...)
	if m := mirrorEnv(profile); m != "" {
		env = append(env, corev1.EnvVar{Name: "MIRRORS", Value: m})
	}
	if v := profile.Spec.Rollout.Validate; v != nil && !*v {
		env = append(env, corev1.EnvVar{Name: "BREWLET_VALIDATE", Value: "false"})
	}

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      brewlet.ProfileDaemonSetName(profile.Name),
			Namespace: cfg.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					ServiceAccountName: brewlet.ProvisionerName,
					HostPID:            true,
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Affinity:           profileAffinity(profile, resolvedKey, otherPools),
					Containers: []corev1.Container{{
						Name:            "provisioner",
						Image:           cfg.ProvisionerImage,
						SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
						Env:             env,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "host-opt", MountPath: "/opt/brewlet"},
							{Name: "containerd-conf", MountPath: "/etc/containerd"},
							{Name: "host-bin", MountPath: "/host/usr/local/bin"},
							{Name: "containerd-sock", MountPath: "/run/containerd/containerd.sock"},
						},
					}},
					Volumes: []corev1.Volume{
						hostPathVolume("host-opt", "/opt/brewlet", nil),
						hostPathVolume("containerd-conf", "/etc/containerd", nil),
						hostPathVolume("host-bin", "/usr/local/bin", nil),
						hostPathVolume("containerd-sock", "/run/containerd/containerd.sock", &hostPathSocket),
					},
				},
			},
		},
	}
	if mu := profile.Spec.Rollout.MaxUnavailable; mu != nil {
		ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
			Type:          appsv1.RollingUpdateDaemonSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: mu},
		}
	}
	return ds
}

// buildCleanupDaemonSet returns the short-lived brewlet-cleanup DaemonSet the
// operator launches to reverse host state for a deleted profile (§5.6). It runs
// the same provisioner image in BREWLET_MODE=cleanup, scoped to the profile's
// pool, and (following the kata-cleanup pattern) restores containerd config,
// removes the shim, and drops the runtime + capability labels.
func buildCleanupDaemonSet(cfg Config, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) *appsv1.DaemonSet {
	ds := buildProfileDaemonSet(cfg, profile, resolvedKey, otherPools)
	ds.Name = brewlet.CleanupDaemonSetName(profile.Name)
	lbls := ds.Spec.Selector.MatchLabels
	lbls["app"] = "brewlet-cleanup"
	ds.Labels = lbls
	ds.Spec.Template.ObjectMeta.Labels = lbls
	ds.Spec.Template.Spec.Containers[0].Env = append(
		ds.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BREWLET_MODE", Value: "cleanup"},
	)
	return ds
}
