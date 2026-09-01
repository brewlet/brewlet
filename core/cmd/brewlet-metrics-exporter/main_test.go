package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInventoryCollector(t *testing.T) {
	root := t.TempDir()
	jdk := filepath.Join(root, "jdks", "temurin-21", "opt", "java", "openjdk")
	if err := os.MkdirAll(jdk, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "jdks", ".brewlet-active"):                     "temurin-21\n",
		filepath.Join(root, "jdks", "temurin-21", ".brewlet-java-home"):    "/opt/java/openjdk\n",
		filepath.Join(root, "jdks", "temurin-21", ".brewlet-source"):       "docker.io/library/eclipse-temurin:21\n/opt/java/openjdk\n",
		filepath.Join(root, "jdks", "temurin-21", ".brewlet-installed-at"): "2026-08-31T12:00:00Z\n",
		filepath.Join(jdk, "release"):                                      "JAVA_VERSION=\"21.0.8\"\nIMPLEMENTOR=\"Eclipse Adoptium\"\nOS_ARCH=\"aarch64\"\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(newInventoryCollector(root))
	expected := `
# HELP brewlet_jdk_info JDK builds installed and active on this Brewlet node.
# TYPE brewlet_jdk_info gauge
brewlet_jdk_info{arch="aarch64",distribution="temurin",feature="21",source="docker.io/library/eclipse-temurin:21",vendor="Eclipse Adoptium",version="21.0.8"} 1
# HELP brewlet_jdk_installed_timestamp_seconds Unix timestamp when this JDK root was installed on the node; this is not the upstream patch release date.
# TYPE brewlet_jdk_installed_timestamp_seconds gauge
brewlet_jdk_installed_timestamp_seconds{distribution="temurin",feature="21",version="21.0.8"} 1.7881776e+09
# HELP brewlet_launcher_info JVM launchers installed on this Brewlet node.
# TYPE brewlet_launcher_info gauge
brewlet_launcher_info{launcher="java"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestEventMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newEventMetrics(reg)
	payload := []byte(`{"version":1,"kind":"launch","outcome":"error","reason":"NoCompatibleJDK","entryMode":"jar","format":"image"}`)
	metrics.observe(payload)
	if got := testutil.ToFloat64(metrics.launches.WithLabelValues("error", "NoCompatibleJDK", "jar", "image")); got != 1 {
		t.Fatalf("launch counter = %v", got)
	}
	metrics.observe([]byte(`{"version":99,"kind":"cds","cdsRole":"consume"}`))
	if got := testutil.ToFloat64(metrics.invalid); got != 1 {
		t.Fatalf("invalid counter = %v", got)
	}
}
