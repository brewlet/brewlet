package admission

import (
	"context"
	"encoding/json"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// PodMutator is the controller-runtime admission handler that applies the
// Brewlet admission/scheduling seam (§8/§14) to pods on CREATE. It reads the
// ready-node fleet through the manager's cached client, then delegates to the
// pure MutatePod logic.
type PodMutator struct {
	Client  client.Reader
	Decoder admission.Decoder
}

// Handle implements admission.Handler.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !IsBrewletPod(pod) {
		return admission.Allowed("not a brewlet pod")
	}

	fleet, err := m.fleet(ctx)
	if err != nil {
		// Fail open: never block scheduling because the webhook couldn't read
		// nodes. The shim still enforces JDK compatibility at runtime.
		log.Error(err, "listing nodes for fleet check; allowing pod without steering")
		return admission.Allowed("fleet unavailable")
	}

	res := MutatePod(pod, fleet)
	if res.DenyReason != "" {
		log.Info("denying brewlet pod", "reason", res.DenyReason, "message", res.DenyMessage)
		return denied(res.DenyReason, res.DenyMessage)
	}

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	log.Info("admitted brewlet pod",
		"artifactRef", res.ArtifactRef, "artifactDigest", res.ArtifactDigest)
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// fleet reads the current node inventory and projects it to capabilities.
func (m *PodMutator) fleet(ctx context.Context) ([]NodeCapability, error) {
	var nodes corev1.NodeList
	if err := m.Client.List(ctx, &nodes); err != nil {
		return nil, err
	}
	fleet := make([]NodeCapability, 0, len(nodes.Items))
	for i := range nodes.Items {
		fleet = append(fleet, NodeCapabilityFrom(&nodes.Items[i]))
	}
	return fleet, nil
}

// denied builds a Forbidden admission response carrying the NoCompatibleJDK /
// NoCompatibleLauncher reason (§14) so the pod-creating controller surfaces a
// clear cause.
func denied(reason, message string) admission.Response {
	return admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusForbidden,
				Reason:  metav1.StatusReason(reason),
				Message: message,
			},
		},
	}
}
