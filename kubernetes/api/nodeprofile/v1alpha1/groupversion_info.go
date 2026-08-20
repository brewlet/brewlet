// Package v1alpha1 contains the Go API types for the node.brewlet.sh/v1alpha1
// group — the cluster-scoped NodeProfile CRD (https://github.com/brewlet/specs,
// 0001). A NodeProfile binds a node pool to a JDK/launcher inventory (plus a
// rollout policy and an optional registry override); the operator reconciles one
// provisioner DaemonSet per profile. The wire schema mirrors
// deploy/nodeprofile-crd.yaml.
//
// This is a SEPARATE API group from apps.brewlet.sh/v1alpha1 (the developer-facing
// JavaApplication): node-prep is a platform-team concern keyed on cluster-scoped
// Nodes, so it gets its own group and its own scheme builder.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group/version served by these types.
var GroupVersion = schema.GroupVersion{Group: "node.brewlet.sh", Version: "v1alpha1"}

// SchemeBuilder registers the group's types with a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the group's types to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&NodeProfile{}, &NodeProfileList{})
}
