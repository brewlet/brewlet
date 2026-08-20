// Package v1alpha1 contains the Go API types for the apps.brewlet.sh/v1alpha1
// group — currently the JavaApplication CRD (https://github.com/brewlet/specs), the
// developer-facing "deployment descriptor" reconciled by the JavaApplication
// controller. The wire schema mirrors deploy/javaapplication-crd.yaml.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group/version served by these types.
var GroupVersion = schema.GroupVersion{Group: "apps.brewlet.sh", Version: "v1alpha1"}

// SchemeBuilder registers the group's types with a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the group's types to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&JavaApplication{}, &JavaApplicationList{})
}
