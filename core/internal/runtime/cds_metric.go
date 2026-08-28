package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// recordRegenMetric writes a best-effort node-local record of the AppCDS
// regeneration role chosen for a launch. It is the shim-side half of the metrics
// design in https://github.com/brewlet/site/blob/main/docs/metrics-exporter.md (Option A): the long-lived provisioner
// exporter aggregates these files into `brewlet_cds_archive_mapped_total` /
// `_stale_total`, letting operators watch the accelerator's payoff erode after a
// JDK patch (https://github.com/brewlet/site/blob/main/docs/appcds.md §7). It NEVER fails a launch: every error is
// swallowed, and an empty dir disables emission entirely.
//
// One file is written per launch decision at
// <dir>/cds-<key-or-skip>-<unixnano>.prom, in Prometheus text-exposition format,
// so the exporter can textfile-collect and delete them.
func recordRegenMetric(dir, key string, role RegenRole, now time.Time) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	label := key
	if label == "" {
		label = "none"
	}
	// mapped=1 for consume (the archive win landed), 0 otherwise; role carried as
	// a label so the exporter can also break down write/defer/skip.
	mapped := 0
	if role == RegenConsume {
		mapped = 1
	}
	line := fmt.Sprintf(
		"# HELP brewlet_cds_archive_mapped Whether a node-side AppCDS archive was mapped for this launch (1) or not (0).\n"+
			"# TYPE brewlet_cds_archive_mapped gauge\n"+
			"brewlet_cds_archive_mapped{key=%q,role=%q} %d\n",
		label, string(role), mapped,
	)
	name := fmt.Sprintf("cds-%s-%d.prom", label, now.UnixNano())
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, name))
}
