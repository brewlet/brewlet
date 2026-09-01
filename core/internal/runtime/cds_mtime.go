package runtime

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/brewlet/brewlet/internal/artifact"
)

// CDSModTime is the fixed modification time Brewlet stamps on the application
// files that participate in an AppCDS archive — the app JAR and any
// classpath/module-path library JARs — whenever the artifact ships a CDS archive
// (cfg.CDS != nil).
//
// Why this exists: HotSpot validates a CDS archive against each app-classpath
// entry's basename, file size, and last-modified time; if any differ from what
// the training (dump) run recorded, the archive is rejected. Under Brewlet's
// -Xshare:auto default that rejection is silent — the JVM just falls back to base
// CDS and the app-archive win evaporates (verified; see https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §2.1).
//
// The node runs the exact same JAR bytes shipped in the artifact (same basename,
// same size), so the ONLY axis that would otherwise drift is the on-disk mtime:
// the JAR is bind-mounted from a content-store blob whose mtime is the node's
// pull time — never the build machine's training time. The path/directory does
// NOT matter to CDS (verified: an archive trained at one absolute path maps when
// consumed at another), so pinning the mtime to a shared canonical value on both
// the training side (the Maven plugin / CLI) and the node side (here) is
// sufficient and necessary for a build-time archive to actually map.
//
// Value: 2000-01-01T00:00:00Z (Unix 946684800, nanoseconds zero). The Maven
// plugin stamps the identical value on the JAR it trains against; keep the two in
// sync. Stamping happens ONLY when CDS is present, so the common no-CDS path is
// byte-for-byte unchanged.
var CDSModTime = time.Unix(946684800, 0).UTC()

// pinCDSModTime sets path's atime+mtime to CDSModTime. It is a no-op-safe helper
// used on the staged app JAR and library JARs when an artifact ships a CDS
// archive.
func pinCDSModTime(path string) error {
	return os.Chtimes(path, CDSModTime, CDSModTime)
}

// stageCDSJar copies the JAR at src into dstDir as name, pins the copy's mtime to
// CDSModTime, and returns the copy's path. The production shim and the portable
// bundle path bind-mount the app JAR read-only straight from a content blob whose
// mtime is non-deterministic; when the artifact ships a CDS archive they instead
// mount this canonical-mtime copy so the archive's recorded JAR timestamp matches
// (see CDSModTime). dstDir is created if needed.
func StageCDSJar(src, dstDir, name string) (string, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dstDir, name)
	if err := copyFileContents(src, dst); err != nil {
		return "", err
	}
	if err := pinCDSModTime(dst); err != nil {
		return "", err
	}
	return dst, nil
}

// pinCDSModTimesUnder walks dir and pins the mtime of every regular file to
// CDSModTime. It is applied to the staged /app/lib and /app/mods directories when
// the artifact ships a CDS archive so each library JAR on the classpath/module
// path matches the archive's recorded timestamps. A missing dir is not an error.
func PinCDSModTimesUnder(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return pinCDSModTime(path)
	})
}

// shipsCDS reports whether cfg's artifact carries an AppCDS archive, i.e. whether
// mtime pinning applies.
func shipsCDS(cfg artifact.JVMConfig) bool {
	return cfg.CDS != nil
}

// copyFileContents copies src to dst (truncating dst), preserving nothing but the
// bytes; callers pin the mtime explicitly afterwards.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil { //nolint:gosec // trusted artifact JAR
		out.Close()
		return err
	}
	return out.Close()
}
