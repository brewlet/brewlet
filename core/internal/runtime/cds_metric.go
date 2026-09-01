package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brewlet/brewlet/internal/telemetry"
)

// recordRegenMetric writes a best-effort node-local record of the AppCDS
// regeneration role chosen for a launch. It is the shim-side half of the metrics
// design in https://github.com/brewlet/brewlet/blob/main/docs/metrics-exporter.md
// (Option A). The current exporter consumes the socket event. The optional file
// remains for compatibility with external Prometheus textfile collectors. It
// NEVER fails a launch: every error is swallowed, and an empty dir disables
// file emission entirely.
//
// One file is written per launch decision at
// <dir>/cds-<key-or-skip>-<unixnano>.prom, in Prometheus text-exposition format,
// External collectors using this compatibility path must delete consumed files.
func recordRegenMetric(dir, _ string, role RegenRole, now time.Time) {
	_ = telemetry.Emit(telemetry.Event{Kind: telemetry.KindCDS, CDSRole: string(role)})
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// mapped=1 for consume (the archive win landed), 0 otherwise; role carried as
	// a label so legacy textfile collectors can also break down write/defer/skip.
	mapped := 0
	if role == RegenConsume {
		mapped = 1
	}
	line := fmt.Sprintf(
		"# HELP brewlet_cds_archive_mapped Whether a node-side AppCDS archive was mapped for this launch (1) or not (0).\n"+
			"# TYPE brewlet_cds_archive_mapped gauge\n"+
			"brewlet_cds_archive_mapped{role=%q} %d\n",
		string(role), mapped,
	)
	name := fmt.Sprintf("cds-%d.prom", now.UnixNano())
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, name))
}
