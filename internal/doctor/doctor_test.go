package doctor

import (
	"errors"
	"strings"
	"testing"
)

const healthyNodes = `{
  "items": [{
    "metadata": {
      "name": "node-1",
      "labels": {"brewlet.sh/runtime": "ready"},
      "annotations": {"brewlet.sh/jdks": "temurin-21"}
    },
    "spec": {"unschedulable": false},
    "status": {"nodeInfo": {"containerRuntimeVersion": "containerd://2.1.3"}}
  }]
}`

func TestRunHealthyCluster(t *testing.T) {
	exec := func(args ...string) ([]byte, error) {
		switch command(args) {
		case "config current-context":
			return []byte("kind-brewlet\n"), nil
		case "get --raw=/readyz":
			return []byte("ok\n"), nil
		case "get runtimeclass brewlet -o name":
			return []byte("runtimeclass.node.k8s.io/brewlet\n"), nil
		case "get crd javaapplications.apps.brewlet.sh -o name":
			return []byte("customresourcedefinition.apiextensions.k8s.io/javaapplications.apps.brewlet.sh\n"), nil
		case "get nodes -o json":
			return []byte(healthyNodes), nil
		case "auth can-i create javaapplications.apps.brewlet.sh -n apps":
			return []byte("yes\n"), nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return nil, nil
		}
	}

	report := Run(exec, Options{Namespace: "apps"})
	if !report.OK() {
		t.Fatalf("report should be healthy: %+v", report)
	}
	if len(report.Checks) != 7 {
		t.Fatalf("checks = %d, want 7", len(report.Checks))
	}
}

func TestRunReportsMissingPlatform(t *testing.T) {
	exec := func(args ...string) ([]byte, error) {
		switch command(args) {
		case "config current-context":
			return []byte("dev\n"), nil
		case "get --raw=/readyz":
			return []byte("ok\n"), nil
		case "get runtimeclass brewlet -o name":
			return []byte(`Error from server (NotFound): runtimeclasses.node.k8s.io "brewlet" not found`), errors.New("exit status 1")
		case "get crd javaapplications.apps.brewlet.sh -o name":
			return []byte("not found"), errors.New("exit status 1")
		case "get nodes -o json":
			return []byte(`{"items":[]}`), nil
		case "auth can-i create javaapplications.apps.brewlet.sh -n default":
			return []byte("no\n"), nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return nil, nil
		}
	}

	report := Run(exec, Options{})
	if report.OK() {
		t.Fatalf("report should contain failures: %+v", report)
	}
	failures := 0
	for _, check := range report.Checks {
		if check.Status == Fail {
			failures++
		}
	}
	if failures != 5 {
		t.Fatalf("failures = %d, want 5: %+v", failures, report)
	}
}

func TestRunPassesKubectlOptions(t *testing.T) {
	exec := func(args ...string) ([]byte, error) {
		prefix := "--kubeconfig /tmp/k --context prod "
		got := command(args)
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("command %q missing prefix %q", got, prefix)
		}
		switch strings.TrimPrefix(got, prefix) {
		case "config current-context":
			return []byte("prod\n"), nil
		case "get --raw=/readyz":
			return []byte("ok\n"), nil
		case "get runtimeclass brewlet -o name", "get crd javaapplications.apps.brewlet.sh -o name":
			return []byte("resource\n"), nil
		case "get nodes -o json":
			return []byte(healthyNodes), nil
		case "auth can-i create javaapplications.apps.brewlet.sh -n apps":
			return []byte("yes\n"), nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return nil, nil
		}
	}

	if report := Run(exec, Options{Kubeconfig: "/tmp/k", Context: "prod", Namespace: "apps"}); !report.OK() {
		t.Fatalf("report should be healthy: %+v", report)
	}
}

func command(args []string) string {
	return strings.Join(args, " ")
}
