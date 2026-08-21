// Package doctor diagnoses whether a Kubernetes context is ready to run Brewlet
// workloads without requiring an in-process Kubernetes client.
package doctor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brewlet/brewlet/internal/inventory"
)

type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	Checks []Check `json:"checks"`
}

func (r Report) OK() bool {
	for _, check := range r.Checks {
		if check.Status == Fail {
			return false
		}
	}
	return true
}

type Options struct {
	Kubeconfig string
	Context    string
	Namespace  string
}

// Executor runs kubectl with the supplied arguments and returns combined output.
type Executor func(args ...string) ([]byte, error)

func Run(exec Executor, opts Options) Report {
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	var checks []Check

	run := func(args ...string) ([]byte, error) {
		base := make([]string, 0, 4+len(args))
		if opts.Kubeconfig != "" {
			base = append(base, "--kubeconfig", opts.Kubeconfig)
		}
		if opts.Context != "" {
			base = append(base, "--context", opts.Context)
		}
		return exec(append(base, args...)...)
	}

	contextOut, err := run("config", "current-context")
	if err != nil {
		checks = append(checks, failed("cluster-context", err,
			"Install kubectl and select a valid Kubernetes context."))
		return Report{Checks: checks}
	}
	context := strings.TrimSpace(string(contextOut))
	if opts.Context != "" {
		context = opts.Context
	}
	checks = append(checks, Check{Name: "cluster-context", Status: Pass, Detail: context})

	if out, err := run("get", "--raw=/readyz"); err != nil {
		checks = append(checks, failed("api-server", commandError(out, err),
			"Check cluster connectivity and kubeconfig credentials."))
		return Report{Checks: checks}
	} else {
		checks = append(checks, Check{Name: "api-server", Status: Pass, Detail: strings.TrimSpace(string(out))})
	}

	if out, err := run("get", "runtimeclass", "brewlet", "-o", "name"); err != nil {
		checks = append(checks, failed("runtimeclass", commandError(out, err),
			"Ask the platform team to install Brewlet and create RuntimeClass brewlet."))
	} else {
		checks = append(checks, Check{Name: "runtimeclass", Status: Pass, Detail: strings.TrimSpace(string(out))})
	}

	if out, err := run("get", "crd", "javaapplications.apps.brewlet.sh", "-o", "name"); err != nil {
		checks = append(checks, failed("javaapplication-crd", commandError(out, err),
			"Install or upgrade the Brewlet Helm chart."))
	} else {
		checks = append(checks, Check{Name: "javaapplication-crd", Status: Pass, Detail: strings.TrimSpace(string(out))})
	}

	nodesOut, nodesErr := run("get", "nodes", "-o", "json")
	if nodesErr != nil {
		checks = append(checks, failed("brewlet-nodes", commandError(nodesOut, nodesErr),
			"Grant node read access or ask Ops to run brewlet doctor."))
	} else {
		nodeCheck, inventoryCheck := diagnoseNodes(nodesOut)
		checks = append(checks, nodeCheck, inventoryCheck)
	}

	canIOut, err := run("auth", "can-i", "create", "javaapplications.apps.brewlet.sh", "-n", opts.Namespace)
	if err != nil {
		checks = append(checks, failed("developer-rbac", commandError(canIOut, err),
			"Ask Ops for permission to create JavaApplication resources in the target namespace."))
	} else if strings.TrimSpace(string(canIOut)) != "yes" {
		checks = append(checks, Check{
			Name:        "developer-rbac",
			Status:      Fail,
			Detail:      fmt.Sprintf("cannot create JavaApplication resources in namespace %q", opts.Namespace),
			Remediation: "Ask Ops for the application-developer role in this namespace.",
		})
	} else {
		checks = append(checks, Check{
			Name:   "developer-rbac",
			Status: Pass,
			Detail: fmt.Sprintf("can create JavaApplication resources in namespace %q", opts.Namespace),
		})
	}

	return Report{Checks: checks}
}

type nodeList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
			Taints        []struct {
				Effect string `json:"effect"`
			} `json:"taints"`
		} `json:"spec"`
		Status struct {
			NodeInfo struct {
				ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

func diagnoseNodes(raw []byte) (Check, Check) {
	var nodes nodeList
	if err := json.Unmarshal(raw, &nodes); err != nil {
		failure := failed("brewlet-nodes", err, "Run kubectl get nodes -o json and inspect the response.")
		return failure, Check{Name: "jdk-inventory", Status: Fail, Detail: "node inventory unavailable"}
	}

	ready := 0
	var incompatible []string
	for _, node := range nodes.Items {
		if node.Metadata.Labels["brewlet.sh/runtime"] != "ready" ||
			node.Spec.Unschedulable || blocksScheduling(node.Spec.Taints) {
			continue
		}
		ready++
		if !strings.HasPrefix(node.Status.NodeInfo.ContainerRuntimeVersion, "containerd://") {
			incompatible = append(incompatible, node.Metadata.Name)
		}
	}

	nodeCheck := Check{
		Name:   "brewlet-nodes",
		Status: Pass,
		Detail: fmt.Sprintf("%d schedulable Brewlet-ready node(s)", ready),
	}
	switch {
	case ready == 0:
		nodeCheck = Check{
			Name:        "brewlet-nodes",
			Status:      Fail,
			Detail:      "no schedulable node has brewlet.sh/runtime=ready",
			Remediation: "Check the NodeProfile and node-provisioner status.",
		}
	case len(incompatible) > 0:
		nodeCheck = Check{
			Name:        "brewlet-nodes",
			Status:      Fail,
			Detail:      "Brewlet-ready nodes without containerd: " + strings.Join(incompatible, ", "),
			Remediation: "Brewlet requires containerd nodes.",
		}
	}

	nodeJDKs, err := inventory.ParseNodes(raw)
	if err != nil {
		return nodeCheck, failed("jdk-inventory", err, "Inspect the Brewlet JDK annotations on each node.")
	}
	jdks := inventory.Aggregate(nodeJDKs)
	if len(jdks) == 0 {
		return nodeCheck, Check{
			Name:        "jdk-inventory",
			Status:      Fail,
			Detail:      "no Brewlet JDK inventory is advertised",
			Remediation: "Configure at least one JDK in the active NodeProfile.",
		}
	}
	return nodeCheck, Check{
		Name:   "jdk-inventory",
		Status: Pass,
		Detail: fmt.Sprintf("%d distinct JDK runtime(s) advertised", len(jdks)),
	}
}

func blocksScheduling(taints []struct {
	Effect string `json:"effect"`
}) bool {
	for _, taint := range taints {
		if taint.Effect == "NoSchedule" || taint.Effect == "NoExecute" {
			return true
		}
	}
	return false
}

func failed(name string, err error, remediation string) Check {
	return Check{Name: name, Status: Fail, Detail: err.Error(), Remediation: remediation}
}

func commandError(out []byte, err error) error {
	if detail := strings.TrimSpace(string(out)); detail != "" {
		return fmt.Errorf("%s", detail)
	}
	return err
}
