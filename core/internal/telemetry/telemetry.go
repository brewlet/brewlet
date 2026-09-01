package telemetry

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Version           = 1
	DefaultSocketPath = "/opt/brewlet/metrics/telemetry.sock"

	KindLaunchPhase        = "launch_phase"
	KindLaunch             = "launch"
	KindArtifactResolution = "artifact_resolution"
	KindCDS                = "cds"
)

// Event is the bounded-cardinality telemetry contract between the short-lived
// runtime shim and the node-local exporter.
type Event struct {
	Version         int     `json:"version"`
	Kind            string  `json:"kind"`
	Phase           string  `json:"phase,omitempty"`
	Outcome         string  `json:"outcome,omitempty"`
	Reason          string  `json:"reason,omitempty"`
	Backend         string  `json:"backend,omitempty"`
	Format          string  `json:"format,omitempty"`
	EntryMode       string  `json:"entryMode,omitempty"`
	CDSRole         string  `json:"cdsRole,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
}

func SocketPath() string {
	if path := strings.TrimSpace(os.Getenv("BREWLET_METRICS_SOCKET")); path != "" {
		return path
	}
	return DefaultSocketPath
}

// Emit sends one best-effort datagram. Callers deliberately ignore failures:
// observability must never change workload launch behavior.
func Emit(event Event) error {
	return EmitTo(SocketPath(), event)
}

func EmitTo(path string, event Event) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	event.Version = Version
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	addr := &net.UnixAddr{Name: filepath.Clean(path), Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	_, err = conn.Write(payload)
	return err
}

func Decode(payload []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return Event{}, err
	}
	if event.Version != Version {
		return Event{}, errors.New("unsupported telemetry event version")
	}
	switch event.Kind {
	case KindLaunchPhase:
		if !oneOf(event.Phase, "artifact_resolve", "bundle_prepare", "overlay_setup", "runc_create", "process_start") ||
			!oneOf(event.Outcome, "success", "error") {
			return Event{}, errors.New("invalid launch phase event")
		}
	case KindLaunch:
		if !oneOf(event.Outcome, "success", "error") ||
			!oneOf(event.Reason, "none", "NoCompatibleJDK", "NoCompatibleLauncher", "NoCompatibleArch", "ArtifactResolution", "OverlaySetup", "RuntimeCreate", "ProcessStart", "BundlePreparation") ||
			!oneOf(event.EntryMode, "jar", "classpath", "module", "unknown") ||
			!oneOf(event.Format, "native", "image", "unknown") {
			return Event{}, errors.New("invalid launch event")
		}
	case KindArtifactResolution:
		if !oneOf(event.Backend, "layout", "containerd") ||
			!oneOf(event.Format, "native", "image", "unknown") ||
			!oneOf(event.Outcome, "success", "error") {
			return Event{}, errors.New("invalid artifact resolution event")
		}
	case KindCDS:
		if !oneOf(event.CDSRole, "consume", "write", "defer", "skip") {
			return Event{}, errors.New("invalid CDS event")
		}
	default:
		return Event{}, errors.New("unknown telemetry event kind")
	}
	return event, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func Outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func Reason(err error) string {
	if err == nil {
		return "none"
	}
	message := err.Error()
	for _, reason := range []string{
		"NoCompatibleJDK",
		"NoCompatibleLauncher",
		"NoCompatibleArch",
	} {
		if strings.Contains(message, reason) {
			return reason
		}
	}
	switch {
	case strings.Contains(message, "artifact"), strings.Contains(message, "manifest"), strings.Contains(message, "blob"):
		return "ArtifactResolution"
	case strings.Contains(message, "overlay"):
		return "OverlaySetup"
	case strings.Contains(message, "runc"):
		return "RuntimeCreate"
	default:
		return "BundlePreparation"
	}
}
