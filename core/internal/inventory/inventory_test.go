package inventory

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const nodesJSON = `{
  "items": [
    {
      "metadata": {
        "name": "node-a",
        "annotations": {
          "brewlet.sh/jdks-info": "[{\"distribution\":\"temurin\",\"vendor\":\"Eclipse Adoptium\",\"feature\":21,\"version\":\"21.0.5\",\"arch\":\"amd64\"},{\"distribution\":\"microsoft\",\"vendor\":\"Microsoft\",\"feature\":25,\"version\":\"25\",\"arch\":\"amd64\"}]"
        }
      }
    },
    {
      "metadata": {
        "name": "node-b",
        "annotations": {
          "brewlet.sh/jdks-info": "[{\"distribution\":\"temurin\",\"vendor\":\"Eclipse Adoptium\",\"feature\":21,\"version\":\"21.0.5\",\"arch\":\"amd64\"}]"
        }
      }
    },
    {
      "metadata": {
        "name": "node-legacy",
        "annotations": {
          "brewlet.sh/jdks": "temurin-17,microsoft-25"
        }
      }
    },
    {
      "metadata": {
        "name": "node-none",
        "annotations": {}
      }
    }
  ]
}`

func TestParseNodes(t *testing.T) {
	nodes, err := ParseNodes([]byte(nodesJSON))
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	// node-none is omitted (no inventory).
	if len(nodes) != 3 {
		t.Fatalf("want 3 nodes with inventory, got %d: %+v", len(nodes), nodes)
	}

	byName := map[string][]JDKInfo{}
	for _, n := range nodes {
		byName[n.Node] = n.JDKs
	}
	if _, ok := byName["node-none"]; ok {
		t.Errorf("node-none should be omitted")
	}

	a := byName["node-a"]
	if len(a) != 2 {
		t.Fatalf("node-a: want 2 jdks, got %d", len(a))
	}
	if a[0].Vendor != "Eclipse Adoptium" || a[0].Version != "21.0.5" || a[0].Arch != "amd64" || a[0].Feature != 21 {
		t.Errorf("node-a[0] unexpected: %+v", a[0])
	}

	// Legacy annotation yields distribution+feature only.
	l := byName["node-legacy"]
	if len(l) != 2 {
		t.Fatalf("node-legacy: want 2 jdks, got %d", len(l))
	}
	if l[0].Distribution != "temurin" || l[0].Feature != 17 || l[0].Vendor != "" {
		t.Errorf("node-legacy[0] unexpected: %+v", l[0])
	}
}

func TestAggregate(t *testing.T) {
	nodes, err := ParseNodes([]byte(nodesJSON))
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	agg := Aggregate(nodes)

	// Distinct JDKs: temurin-17(legacy), temurin-21(amd64), microsoft-25(node-a rich),
	// microsoft-25(node-legacy coarse — different key: no vendor/arch).
	if len(agg) != 4 {
		t.Fatalf("want 4 distinct jdks, got %d: %+v", len(agg), agg)
	}

	// temurin-21 must be provided by node-a AND node-b.
	var found bool
	for _, a := range agg {
		if a.Distribution == "temurin" && a.Feature == 21 && a.Arch == "amd64" {
			found = true
			if len(a.Nodes) != 2 {
				t.Errorf("temurin-21 want 2 nodes, got %v", a.Nodes)
			}
			if a.Nodes[0] != "node-a" || a.Nodes[1] != "node-b" {
				t.Errorf("temurin-21 nodes not sorted: %v", a.Nodes)
			}
		}
	}
	if !found {
		t.Errorf("temurin-21 amd64 not found in aggregate")
	}

	// Sorted by distribution first: microsoft entries precede temurin entries.
	if agg[0].Distribution != "microsoft" {
		t.Errorf("expected microsoft first, got %q", agg[0].Distribution)
	}
}

func TestRenderTable(t *testing.T) {
	nodes, _ := ParseNodes([]byte(nodesJSON))
	var buf bytes.Buffer
	RenderTable(&buf, nodes)
	out := buf.String()
	for _, want := range []string{"VENDOR", "DISTRIBUTION", "MAJOR", "VERSION", "ARCH", "NODES", "Eclipse Adoptium", "21.0.5", "amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

func TestRenderTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	RenderTable(&buf, nil)
	if !strings.Contains(buf.String(), "No Brewlet JDK inventory") {
		t.Errorf("empty render missing message: %s", buf.String())
	}
}

func TestRenderByNode(t *testing.T) {
	nodes, _ := ParseNodes([]byte(nodesJSON))
	var buf bytes.Buffer
	RenderByNode(&buf, nodes)
	out := buf.String()
	if !strings.Contains(out, "node-a") || !strings.Contains(out, "node-b") || !strings.Contains(out, "node-legacy") {
		t.Errorf("by-node output missing a node:\n%s", out)
	}
}

func TestRenderJSON(t *testing.T) {
	nodes, _ := ParseNodes([]byte(nodesJSON))
	var buf bytes.Buffer
	if err := RenderJSON(&buf, nodes); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var agg []AggregatedJDK
	if err := json.Unmarshal(buf.Bytes(), &agg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(agg) != 4 {
		t.Errorf("want 4 aggregated jdks in JSON, got %d", len(agg))
	}
}

func TestParseNodesBadJSON(t *testing.T) {
	if _, err := ParseNodes([]byte("not json")); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestParseNodesBadAnnotation(t *testing.T) {
	bad := `{"items":[{"metadata":{"name":"n","annotations":{"brewlet.sh/jdks-info":"{not-an-array"}}}]}`
	if _, err := ParseNodes([]byte(bad)); err == nil {
		t.Errorf("expected error for malformed annotation JSON")
	}
}
