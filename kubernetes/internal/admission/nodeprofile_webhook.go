package admission

import (
	"context"
	"net/http"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/controller"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// NodeProfileValidator is the controller-runtime admission handler that rejects
// malformed NodeProfiles on CREATE/UPDATE (https://github.com/brewlet/specs): a non-empty
// JDK list, only curated distributions, a valid containerdRestart value, and no
// two profiles naming the same pool (ambiguous ownership). Catching these at
// admission keeps the reconcile loop from defending against garbage and gives
// users immediate feedback.
type NodeProfileValidator struct {
	Client  client.Reader
	Decoder admission.Decoder
}

// Handle implements admission.Handler.
func (v *NodeProfileValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	profile := &nodev1alpha1.NodeProfile{}
	if err := v.Decoder.Decode(req, profile); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if err := controller.ValidateNodeProfile(profile); err != nil {
		log.Info("rejecting NodeProfile", "name", profile.Name, "reason", err.Error())
		return denied("InvalidNodeProfile", "NodeProfile rejected: "+err.Error())
	}

	var existing nodev1alpha1.NodeProfileList
	if err := v.Client.List(ctx, &existing); err != nil {
		// Fail open on a read error: the reconcile loop still rejects a conflict
		// by refusing to double-own a pool, and the webhook must not wedge on a
		// transient apiserver hiccup.
		log.Error(err, "listing NodeProfiles for pool-conflict check; allowing")
		return admission.Allowed("profile list unavailable")
	}
	if err := controller.ValidateNoPoolConflicts(profile, existing.Items); err != nil {
		log.Info("rejecting NodeProfile", "name", profile.Name, "reason", err.Error())
		return denied("PoolConflict", "NodeProfile rejected: "+err.Error())
	}

	return admission.Allowed("valid NodeProfile")
}
