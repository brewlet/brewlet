// Package inventory reads and renders the JDK inventory that Brewlet nodes
// advertise, so developers can inspect which JDKs (vendor, major version, minor
// version, architecture) are available on a cluster and match dev/CI to prod.
//
// The node-provisioner annotates each opted-in Node with:
//
//	brewlet.sh/jdks-info = [{"distribution":"temurin","vendor":"Eclipse Adoptium",
//	                         "feature":21,"version":"21.0.5","arch":"amd64"}, ...]
//
// This package parses that annotation (from `kubectl get nodes -o json`),
// aggregates it across the fleet, and renders it as a table or JSON.
package inventory

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Annotation is the Node annotation key the provisioner writes the rich JDK
// inventory to (a JSON array of JDKInfo).
const Annotation = "brewlet.sh/jdks-info"

// LegacyAnnotation is the original coarse inventory annotation
// (`temurin-21,microsoft-25`); parsed as a fallback when the rich one is absent.
const LegacyAnnotation = "brewlet.sh/jdks"

// JDKInfo describes a single JDK runtime root installed on a node.
type JDKInfo struct {
	Distribution string `json:"distribution"` // brewlet token, e.g. "temurin"
	Vendor       string `json:"vendor"`       // vendor name, e.g. "Eclipse Adoptium"
	Feature      int    `json:"feature"`      // major/feature version, e.g. 21
	Version      string `json:"version"`      // full (minor) version, e.g. "21.0.5"
	Arch         string `json:"arch"`         // architecture, e.g. "amd64"
}

// key uniquely identifies a JDK for aggregation across nodes.
func (j JDKInfo) key() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", j.Distribution, j.Vendor, j.Feature, j.Version, j.Arch)
}

// NodeJDKs pairs a node name with the JDKs it advertises.
type NodeJDKs struct {
	Node string    `json:"node"`
	JDKs []JDKInfo `json:"jdks"`
}

// AggregatedJDK is one distinct JDK plus the nodes that provide it.
type AggregatedJDK struct {
	JDKInfo
	Nodes []string `json:"nodes"`
}

// nodeList is the subset of `kubectl get nodes -o json` we care about.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	} `json:"items"`
}

// ParseNodes parses `kubectl get nodes -o json` output into per-node JDK lists.
// Nodes that advertise no Brewlet JDK inventory are omitted. It reads the rich
// brewlet.sh/jdks-info annotation, falling back to the legacy brewlet.sh/jdks
// list when only that is present.
func ParseNodes(kubectlJSON []byte) ([]NodeJDKs, error) {
	var nl nodeList
	if err := json.Unmarshal(kubectlJSON, &nl); err != nil {
		return nil, fmt.Errorf("parsing kubectl nodes JSON: %w", err)
	}
	var out []NodeJDKs
	for _, item := range nl.Items {
		ann := item.Metadata.Annotations
		jdks, err := parseNodeAnnotations(ann)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", item.Metadata.Name, err)
		}
		if len(jdks) == 0 {
			continue
		}
		out = append(out, NodeJDKs{Node: item.Metadata.Name, JDKs: jdks})
	}
	return out, nil
}

func parseNodeAnnotations(ann map[string]string) ([]JDKInfo, error) {
	if raw, ok := ann[Annotation]; ok && strings.TrimSpace(raw) != "" {
		var jdks []JDKInfo
		if err := json.Unmarshal([]byte(raw), &jdks); err != nil {
			return nil, fmt.Errorf("parsing %s annotation: %w", Annotation, err)
		}
		return jdks, nil
	}
	// Fallback: the coarse "<dist>-<feature>,..." list (no vendor/minor/arch).
	if raw, ok := ann[LegacyAnnotation]; ok && strings.TrimSpace(raw) != "" {
		return parseLegacy(raw), nil
	}
	return nil, nil
}

// parseLegacy turns "temurin-21,microsoft-25" into JDKInfo with only the fields
// the coarse annotation carries (distribution + feature).
func parseLegacy(raw string) []JDKInfo {
	var jdks []JDKInfo
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		dist, feature := tok, 0
		if i := strings.LastIndex(tok, "-"); i >= 0 {
			dist = tok[:i]
			fmt.Sscanf(tok[i+1:], "%d", &feature)
		}
		jdks = append(jdks, JDKInfo{Distribution: dist, Feature: feature})
	}
	return jdks
}

// Aggregate collapses per-node JDKs into the distinct set available across the
// cluster, recording which nodes provide each. The result is sorted for stable
// output (distribution, feature, version, arch).
func Aggregate(nodes []NodeJDKs) []AggregatedJDK {
	byKey := map[string]*AggregatedJDK{}
	var order []string
	for _, n := range nodes {
		for _, j := range n.JDKs {
			k := j.key()
			agg, ok := byKey[k]
			if !ok {
				agg = &AggregatedJDK{JDKInfo: j}
				byKey[k] = agg
				order = append(order, k)
			}
			if !contains(agg.Nodes, n.Node) {
				agg.Nodes = append(agg.Nodes, n.Node)
			}
		}
	}
	out := make([]AggregatedJDK, 0, len(order))
	for _, k := range order {
		agg := byKey[k]
		sort.Strings(agg.Nodes)
		out = append(out, *agg)
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Distribution != y.Distribution {
			return x.Distribution < y.Distribution
		}
		if x.Feature != y.Feature {
			return x.Feature < y.Feature
		}
		if x.Version != y.Version {
			return x.Version < y.Version
		}
		return x.Arch < y.Arch
	})
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func majorStr(feature int) string {
	if feature == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", feature)
}

// RenderTable writes the cluster-wide distinct JDK inventory as an aligned table.
func RenderTable(w io.Writer, nodes []NodeJDKs) {
	agg := Aggregate(nodes)
	if len(agg) == 0 {
		fmt.Fprintln(w, "No Brewlet JDK inventory found on any node.")
		fmt.Fprintln(w, "(Nodes advertise JDKs once the node-provisioner has run; see https://github.com/brewlet/brewlet/blob/main/docs/jdk-management.md.)")
		return
	}
	rows := [][]string{{"VENDOR", "DISTRIBUTION", "MAJOR", "VERSION", "ARCH", "NODES"}}
	for _, a := range agg {
		rows = append(rows, []string{
			dash(a.Vendor),
			dash(a.Distribution),
			majorStr(a.Feature),
			dash(a.Version),
			dash(a.Arch),
			fmt.Sprintf("%d", len(a.Nodes)),
		})
	}
	writeTable(w, rows)
}

// RenderByNode writes a per-node listing of the JDKs each node advertises.
func RenderByNode(w io.Writer, nodes []NodeJDKs) {
	if len(nodes) == 0 {
		fmt.Fprintln(w, "No Brewlet JDK inventory found on any node.")
		return
	}
	rows := [][]string{{"NODE", "VENDOR", "DISTRIBUTION", "MAJOR", "VERSION", "ARCH"}}
	sorted := append([]NodeJDKs(nil), nodes...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Node < sorted[b].Node })
	for _, n := range sorted {
		for _, j := range n.JDKs {
			rows = append(rows, []string{
				n.Node,
				dash(j.Vendor),
				dash(j.Distribution),
				majorStr(j.Feature),
				dash(j.Version),
				dash(j.Arch),
			})
		}
	}
	writeTable(w, rows)
}

// RenderJSON writes the aggregated inventory as indented JSON.
func RenderJSON(w io.Writer, nodes []NodeJDKs) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(Aggregate(nodes))
}

// writeTable prints rows in space-padded columns (header row included).
func writeTable(w io.Writer, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i == len(r)-1 {
				b.WriteString(c)
			} else {
				fmt.Fprintf(&b, "%-*s   ", widths[i], c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}
