package observability

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	admissionRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "brewlet_admission_requests_total",
		Help: "Brewlet pod admission outcomes.",
	}, []string{"outcome", "reason"})
	nodeProfileNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "brewlet_nodeprofile_nodes",
		Help: "Nodes assigned to and ready for each Brewlet NodeProfile.",
	}, []string{"profile", "state"})
	nodeProfileCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "brewlet_nodeprofile_condition",
		Help: "Current NodeProfile readiness condition, labeled by reason and boolean status.",
	}, []string{"profile", "reason", "status"})
	provisionTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "brewlet_node_provision_transitions_total",
		Help: "Brewlet node provisioning state transitions.",
	}, []string{"state"})
)

func init() {
	metricsserver.Registry.MustRegister(
		admissionRequests,
		nodeProfileNodes,
		nodeProfileCondition,
		provisionTransitions,
	)
}

func ObserveAdmission(outcome, reason string) {
	if reason == "" {
		reason = "none"
	}
	admissionRequests.WithLabelValues(outcome, reason).Inc()
}

func SetNodeProfile(name string, assigned, ready int32, reason string, condition bool) {
	nodeProfileNodes.WithLabelValues(name, "assigned").Set(float64(assigned))
	nodeProfileNodes.WithLabelValues(name, "ready").Set(float64(ready))
	nodeProfileCondition.DeletePartialMatch(prometheus.Labels{"profile": name})
	nodeProfileCondition.WithLabelValues(name, reason, strconv.FormatBool(condition)).Set(1)
}

func DeleteNodeProfile(name string) {
	nodeProfileNodes.DeletePartialMatch(prometheus.Labels{"profile": name})
	nodeProfileCondition.DeletePartialMatch(prometheus.Labels{"profile": name})
}

func ObserveProvisionTransition(state string) {
	provisionTransitions.WithLabelValues(state).Inc()
}
