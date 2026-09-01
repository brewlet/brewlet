package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brewlet/brewlet/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type eventMetrics struct {
	launchPhases *prometheus.HistogramVec
	launches     *prometheus.CounterVec
	artifacts    *prometheus.HistogramVec
	cds          *prometheus.CounterVec
	invalid      prometheus.Counter
}

func newEventMetrics(reg prometheus.Registerer) *eventMetrics {
	m := &eventMetrics{
		launchPhases: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "brewlet_sandbox_launch_duration_seconds",
			Help:    "Time spent in each Brewlet sandbox launch phase.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"phase", "outcome"}),
		launches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "brewlet_sandbox_launches_total",
			Help: "Brewlet workload launch outcomes.",
		}, []string{"outcome", "reason", "entry_mode", "artifact_format"}),
		artifacts: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "brewlet_artifact_resolution_duration_seconds",
			Help:    "Time spent resolving Brewlet application content already available to the shim.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"backend", "artifact_format", "outcome"}),
		cds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "brewlet_cds_regeneration_decisions_total",
			Help: "Node-side AppCDS regeneration decisions.",
		}, []string{"role"}),
		invalid: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "brewlet_telemetry_events_invalid_total",
			Help: "Malformed or unsupported shim telemetry events.",
		}),
	}
	reg.MustRegister(m.launchPhases, m.launches, m.artifacts, m.cds, m.invalid)
	return m
}

func (m *eventMetrics) observe(payload []byte) {
	event, err := telemetry.Decode(payload)
	if err != nil {
		m.invalid.Inc()
		return
	}
	switch event.Kind {
	case telemetry.KindLaunchPhase:
		m.launchPhases.WithLabelValues(event.Phase, event.Outcome).Observe(event.DurationSeconds)
	case telemetry.KindLaunch:
		m.launches.WithLabelValues(event.Outcome, event.Reason, event.EntryMode, event.Format).Inc()
	case telemetry.KindArtifactResolution:
		m.artifacts.WithLabelValues(event.Backend, event.Format, event.Outcome).Observe(event.DurationSeconds)
	case telemetry.KindCDS:
		m.cds.WithLabelValues(event.CDSRole).Inc()
	}
}

type inventoryCollector struct {
	root         string
	jdkInfo      *prometheus.Desc
	jdkInstalled *prometheus.Desc
	launcherInfo *prometheus.Desc
}

func newInventoryCollector(root string) *inventoryCollector {
	return &inventoryCollector{
		root: root,
		jdkInfo: prometheus.NewDesc(
			"brewlet_jdk_info",
			"JDK builds installed and active on this Brewlet node.",
			[]string{"distribution", "feature", "version", "vendor", "arch", "source"}, nil,
		),
		jdkInstalled: prometheus.NewDesc(
			"brewlet_jdk_installed_timestamp_seconds",
			"Unix timestamp when this JDK root was installed on the node; this is not the upstream patch release date.",
			[]string{"distribution", "feature", "version"}, nil,
		),
		launcherInfo: prometheus.NewDesc(
			"brewlet_launcher_info",
			"JVM launchers installed on this Brewlet node.",
			[]string{"launcher"}, nil,
		),
	}
}

func (c *inventoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jdkInfo
	ch <- c.jdkInstalled
	ch <- c.launcherInfo
}

func (c *inventoryCollector) Collect(ch chan<- prometheus.Metric) {
	active := readWords(filepath.Join(c.root, "jdks", ".brewlet-active"))
	for _, token := range active {
		sep := strings.LastIndex(token, "-")
		if sep <= 0 || sep == len(token)-1 {
			continue
		}
		dist, feature := token[:sep], token[sep+1:]
		jdkRoot := filepath.Join(c.root, "jdks", token)
		release := readProperties(filepath.Join(jdkRoot, javaHome(jdkRoot), "release"))
		version := release["JAVA_VERSION"]
		vendor := release["IMPLEMENTOR"]
		arch := release["OS_ARCH"]
		source := firstLine(filepath.Join(jdkRoot, ".brewlet-source"))
		ch <- prometheus.MustNewConstMetric(c.jdkInfo, prometheus.GaugeValue, 1, dist, feature, version, vendor, arch, source)
		if installed := installedAt(jdkRoot); installed > 0 {
			ch <- prometheus.MustNewConstMetric(c.jdkInstalled, prometheus.GaugeValue, float64(installed), dist, feature, version)
		}
	}
	ch <- prometheus.MustNewConstMetric(c.launcherInfo, prometheus.GaugeValue, 1, "java")
	entries, _ := os.ReadDir(filepath.Join(c.root, "launchers"))
	for _, entry := range entries {
		if entry.IsDir() {
			ch <- prometheus.MustNewConstMetric(c.launcherInfo, prometheus.GaugeValue, 1, entry.Name())
		}
	}
}

func readWords(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

func javaHome(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".brewlet-java-home"))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "/")
}

func readProperties(path string) map[string]string {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return values
}

func firstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}

func installedAt(root string) int64 {
	data, err := os.ReadFile(filepath.Join(root, ".brewlet-installed-at"))
	if err == nil {
		if ts, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); parseErr == nil {
			return ts.Unix()
		}
		if unix, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); parseErr == nil {
			return unix
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

func serveDatagrams(ctx context.Context, path string, observe func([]byte)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		conn.Close()
		return err
	}
	defer func() {
		conn.Close()
		_ = os.Remove(path)
	}()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	buf := make([]byte, 64*1024)
	for {
		n, _, err := conn.ReadFromUnix(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		observe(append([]byte(nil), buf[:n]...))
	}
}

func main() {
	var (
		root       string
		listenAddr string
		socketPath string
	)
	flag.StringVar(&root, "root", "/opt/brewlet", "Brewlet host-state root")
	flag.StringVar(&listenAddr, "listen-address", ":9090", "HTTP metrics listen address")
	flag.StringVar(&socketPath, "socket-path", "", "shim telemetry Unix datagram socket")
	flag.Parse()
	if socketPath == "" {
		socketPath = filepath.Join(root, "metrics", "telemetry.sock")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	registry := prometheus.NewRegistry()
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	events := newEventMetrics(registry)
	registry.MustRegister(newInventoryCollector(root))

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errs := make(chan error, 2)
	go func() {
		log.Printf("serving metrics on %s", listenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errs <- fmt.Errorf("metrics server: %w", err)
		}
	}()
	go func() {
		log.Printf("listening for shim telemetry on %s", socketPath)
		if err := serveDatagrams(ctx, socketPath, events.observe); err != nil {
			errs <- fmt.Errorf("telemetry socket: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errs:
		log.Fatal(err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("metrics server shutdown: %v", err)
	}
}
