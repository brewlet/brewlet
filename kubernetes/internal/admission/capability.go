// Package admission implements the Brewlet pod admission/scheduling seam
// (https://github.com/brewlet/specs). It is deliberately split into pure logic (this
// file + mutate.go, unit-tested without a cluster) and a thin controller-runtime
// webhook wrapper (webhook.go).
package admission

import (
	"sort"
	"strconv"
	"strings"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

// NodeCapability is the brewlet-relevant view of a Node: whether it is ready and
// which JDKs / launchers it advertises. It is derived from the labels and
// annotations the provisioner writes (see provisioner/entrypoint.sh, §5.2).
type NodeCapability struct {
	Name      string
	Ready     bool
	Arch      string   // kubernetes.io/arch value, e.g. "amd64" or "arm64"
	JDKs      []string // "<dist>-<feature>" tokens, e.g. ["temurin-21","microsoft-25"]
	Launchers []string // launcher names, e.g. ["java","jaz"]
}

// NodeCapabilityFrom extracts a NodeCapability from a Node object. Readiness is
// the provisioner-owned brewlet.sh/runtime=ready label; the JDK/launcher
// inventory comes from the comma-separated brewlet.sh/jdks|launchers
// annotations the provisioner advertises; the architecture comes from the
// standard kubelet-provided kubernetes.io/arch label.
func NodeCapabilityFrom(node *corev1.Node) NodeCapability {
	return NodeCapability{
		Name:      node.Name,
		Ready:     node.Labels[brewlet.LabelRuntimeReady] == brewlet.ValueReady,
		Arch:      node.Labels[brewlet.LabelArch],
		JDKs:      splitInventory(node.Annotations[brewlet.AnnotationJDKs]),
		Launchers: splitInventory(node.Annotations[brewlet.AnnotationLaunchers]),
	}
}

// splitInventory parses the on-node comma-separated inventory string the
// provisioner advertises.
func splitInventory(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// supportsJDK reports whether this node provides a JDK satisfying the request.
// A request is either a "<dist>-<feature>" token (exact match) or a bare feature
// like "21" (matches any distribution of that feature). An empty request matches
// any node.
func (c NodeCapability) supportsJDK(request string) bool {
	if request == "" {
		return true
	}
	if feature, ok := bareFeature(request); ok {
		for _, jdk := range c.JDKs {
			if jdkFeature(jdk) == feature {
				return true
			}
		}
		return false
	}
	for _, jdk := range c.JDKs {
		if jdk == request {
			return true
		}
	}
	return false
}

// supportsLauncher reports whether this node provides the requested launcher.
// The vanilla "java" launcher (empty or "java") is provided by every JDK root,
// so it is always available on a ready node.
func (c NodeCapability) supportsLauncher(request string) bool {
	if request == "" || request == brewlet.VanillaLauncher {
		return true
	}
	for _, l := range c.Launchers {
		if l == request {
			return true
		}
	}
	return false
}

// supportsArch reports whether this node's architecture satisfies the request.
// An empty request (arch-neutral artifact, the common case) matches any node.
func (c NodeCapability) supportsArch(request []string) bool {
	if len(request) == 0 {
		return true
	}
	for _, a := range request {
		if c.Arch == a {
			return true
		}
	}
	return false
}

// bareFeature reports whether s is a bare feature version (all digits) and, if
// so, returns it as an int.
func bareFeature(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// jdkFeature extracts the feature version from a "<dist>-<feature>" token, or 0
// if it has no numeric suffix.
func jdkFeature(jdk string) int {
	i := strings.LastIndex(jdk, "-")
	if i < 0 || i == len(jdk)-1 {
		return 0
	}
	n, err := strconv.Atoi(jdk[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// FleetResult is the outcome of checking a JDK/launcher request against the
// ready fleet.
type FleetResult struct {
	// Compatible is true if at least one ready node satisfies both the JDK and
	// launcher request (or the request was not explicit).
	Compatible bool
	// DenyReason is one of brewlet.ReasonNoCompatibleJDK /
	// ReasonNoCompatibleLauncher when Compatible is false; empty otherwise.
	DenyReason string
	// Message is a human-readable explanation for the admission response/event.
	Message string
}

// CheckFleet decides whether a pod requesting the given JDK/launcher/arch can
// run on the current ready fleet. It only ever denies for an *explicit* request
// (a non-empty jdk, a non-vanilla launcher, or a non-empty arch): with no
// request the pod is allowed and the shim keeps its runtime NoCompatibleJDK
// behavior. Among explicit requests, an unsatisfiable JDK is reported before an
// unsatisfiable launcher, then arch.
func CheckFleet(fleet []NodeCapability, jdk, launcher string, arch []string) FleetResult {
	ready := make([]NodeCapability, 0, len(fleet))
	for _, n := range fleet {
		if n.Ready {
			ready = append(ready, n)
		}
	}

	jdkExplicit := jdk != ""
	launcherExplicit := launcher != "" && launcher != brewlet.VanillaLauncher
	archExplicit := len(arch) > 0
	if !jdkExplicit && !launcherExplicit && !archExplicit {
		return FleetResult{Compatible: true}
	}

	for _, n := range ready {
		if n.supportsJDK(jdk) && n.supportsLauncher(launcher) && n.supportsArch(arch) {
			return FleetResult{Compatible: true}
		}
	}

	// Nothing matched. Attribute the denial: first to any axis that no ready node
	// satisfies at all, checked JDK -> launcher -> arch; then, for a joint failure
	// (each axis individually satisfiable but not on one node), to the narrowest
	// explicit axis, preferring arch, then launcher, then JDK.
	switch {
	case jdkExplicit && !anySupportsJDK(ready, jdk):
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleJDK,
			Message: "no ready brewlet node provides JDK " + strconv.Quote(jdk) +
				"; provisioned JDKs=" + strconv.Quote(fleetJDKs(ready)),
		}
	case launcherExplicit && !anySupportsLauncher(ready, launcher):
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleLauncher,
			Message: "no ready brewlet node provides launcher " + strconv.Quote(launcher) +
				"; provisioned launchers=" + strconv.Quote(fleetLaunchers(ready)),
		}
	case archExplicit && !anySupportsArch(ready, arch):
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleArch,
			Message: "no ready brewlet node provides architecture " + strconv.Quote(strings.Join(arch, ",")) +
				"; provisioned arches=" + strconv.Quote(fleetArches(ready)),
		}
	case archExplicit:
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleArch,
			Message: "no ready brewlet node jointly satisfies the request for architecture " +
				strconv.Quote(strings.Join(arch, ",")) + "; provisioned arches=" + strconv.Quote(fleetArches(ready)),
		}
	case launcherExplicit:
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleLauncher,
			Message: "no ready brewlet node jointly satisfies the request for launcher " +
				strconv.Quote(launcher) + "; provisioned launchers=" + strconv.Quote(fleetLaunchers(ready)),
		}
	default:
		return FleetResult{
			DenyReason: brewlet.ReasonNoCompatibleJDK,
			Message: "no ready brewlet node provides JDK " + strconv.Quote(jdk) +
				"; provisioned JDKs=" + strconv.Quote(fleetJDKs(ready)),
		}
	}
}

func anySupportsArch(fleet []NodeCapability, arch []string) bool {
	for _, n := range fleet {
		if n.supportsArch(arch) {
			return true
		}
	}
	return false
}

func anySupportsJDK(fleet []NodeCapability, jdk string) bool {
	for _, n := range fleet {
		if n.supportsJDK(jdk) {
			return true
		}
	}
	return false
}

func anySupportsLauncher(fleet []NodeCapability, launcher string) bool {
	for _, n := range fleet {
		if n.supportsLauncher(launcher) {
			return true
		}
	}
	return false
}

func fleetJDKs(fleet []NodeCapability) string {
	return joinInventory(fleet, func(n NodeCapability) []string { return n.JDKs })
}

func fleetLaunchers(fleet []NodeCapability) string {
	return joinInventory(fleet, func(n NodeCapability) []string { return n.Launchers })
}

func fleetArches(fleet []NodeCapability) string {
	return joinInventory(fleet, func(n NodeCapability) []string {
		if n.Arch == "" {
			return nil
		}
		return []string{n.Arch}
	})
}

// joinInventory returns the sorted, de-duplicated union of an inventory across
// the fleet, for human-readable messages.
func joinInventory(fleet []NodeCapability, pick func(NodeCapability) []string) string {
	set := map[string]struct{}{}
	for _, n := range fleet {
		for _, v := range pick(n) {
			set[v] = struct{}{}
		}
	}
	if len(set) == 0 {
		return "none"
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
