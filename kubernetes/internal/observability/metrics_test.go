package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAdmissionAndNodeProfileMetrics(t *testing.T) {
	admissionRequests.Reset()
	nodeProfileNodes.Reset()
	nodeProfileCondition.Reset()

	ObserveAdmission("denied", "NoCompatibleArch")
	if got := testutil.ToFloat64(admissionRequests.WithLabelValues("denied", "NoCompatibleArch")); got != 1 {
		t.Fatalf("admission counter = %v", got)
	}

	SetNodeProfile("batch", 3, 2, "Provisioning", false)
	if got := testutil.ToFloat64(nodeProfileNodes.WithLabelValues("batch", "assigned")); got != 3 {
		t.Fatalf("assigned gauge = %v", got)
	}
	if got := testutil.ToFloat64(nodeProfileNodes.WithLabelValues("batch", "ready")); got != 2 {
		t.Fatalf("ready gauge = %v", got)
	}
}
