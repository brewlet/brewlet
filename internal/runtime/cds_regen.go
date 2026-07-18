package runtime

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Node-side AppCDS regeneration (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3, Phase B). When an artifact
// sets cds.regenerate, the node maintains a per-(artifact, JDK-build) archive
// cache and launches with -XX:+AutoCreateSharedArchive so the archive self-heals
// on every central JDK patch — decoupling the archive from the shipped artifact.
// The whole feature is *best-effort*: any inability to determine the JDK build,
// create the cache, or elect a writer degrades to base CDS and never fails a
// launch (mirroring -Xshare:auto's safe-fallback posture).
const (
	// DefaultCDSCacheDir is the node-local directory the regeneration cache lives
	// in. The provisioner (https://github.com/brewlet/specs §5.2) creates it; entries are
	// per-(artifact-digest, JDK-build) `.jsa` files shared across sandboxes.
	DefaultCDSCacheDir = "/opt/brewlet/cds"
	// InSandboxCDSDir is where the shim/bundle bind-mounts the node cache dir
	// inside the sandbox, so -XX:SharedArchiveFile resolves to a stable path
	// regardless of the host cache location.
	InSandboxCDSDir = "/run/brewlet/cds"
	// DefaultWriterTTL bounds how long a writer-election marker is trusted before
	// a crashed/never-exiting writer is assumed dead and the key is re-elected.
	DefaultWriterTTL = 10 * time.Minute
	// DefaultEvictTTL is the max age (by mtime) a cached archive is kept before a
	// best-effort prune removes it — old JDK-build keys accumulate after patches.
	DefaultEvictTTL = 14 * 24 * time.Hour
	// minRegenFeature is the lowest JDK feature version that ships
	// -XX:+AutoCreateSharedArchive (JDK 19). Below it, emitting the flag would be
	// a *fatal* unrecognized-option error, so regeneration is skipped instead.
	minRegenFeature = 19

	writerMarkerSuffix = ".writer"
)

// RegenRole is the launch treatment the node chose for an artifact that opted
// into node-side AppCDS regeneration.
type RegenRole string

const (
	// RegenSkip: regeneration does not apply (feature disabled, unsupported JDK,
	// or JDK identity undeterminable). Launch as if no regeneration were set.
	RegenSkip RegenRole = "skip"
	// RegenConsume: a valid cached archive exists for this (artifact, JDK-build);
	// map it read-only with -Xshare:auto -XX:SharedArchiveFile.
	RegenConsume RegenRole = "consume"
	// RegenWrite: this launch was elected to (re)generate the archive; it runs
	// with -XX:+AutoCreateSharedArchive and writes the archive to the node cache
	// at JVM exit (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3 — the win lands on the next rollout).
	RegenWrite RegenRole = "write"
	// RegenDefer: another launch is already generating this key; run on base CDS
	// this boot and pick up the app archive on a later restart (thundering-herd
	// control).
	RegenDefer RegenRole = "defer"
)

// RegenParams are the host-side inputs to a node-side regeneration decision.
type RegenParams struct {
	// CacheDir is the host cache directory; "" => DefaultCDSCacheDir.
	CacheDir string
	// JDKRoot is the selected node JDK root, read for build identity + the
	// feature-version gate.
	JDKRoot string
	// ArtifactKey is a stable per-artifact identity (the manifest digest in
	// production; the ref otherwise). It must be stable across replicas so they
	// share one cache entry, and change when the app changes.
	ArtifactKey string
	// SeedArchive is the host path of a shipped `.jsa` to seed a fresh cache
	// entry with, or "". A seed gives the very first boot a warm archive;
	// -XX:+AutoCreateSharedArchive transparently recreates it if JDK-stale.
	SeedArchive string
	// ArchiveArgDir is the directory the JVM sees the archive under (the
	// in-sandbox mount point). "" means the JVM uses the host CacheDir directly
	// (the local `run` path, which is not sandboxed).
	ArchiveArgDir string
	// MetricsDir, when set, receives a best-effort node-local role record the
	// metrics exporter (https://github.com/brewlet/site/blob/main/docs/metrics-exporter.md, Option A) can aggregate.
	MetricsDir string
	// Now is an injectable clock for tests; zero => time.Now().
	Now time.Time
	// LockTTL / EvictTTL override the defaults when non-zero.
	LockTTL  time.Duration
	EvictTTL time.Duration
}

// RegenDecision is the outcome of DecideCDSRegen.
type RegenDecision struct {
	Role RegenRole
	// Key is the per-(artifact, JDK-build) cache key (hash), or "" when skipped.
	Key string
	// HostArchive is the host path of the cache archive to mount, or "" for
	// skip/defer (defer maps nothing; skip is handled by the shipped-archive
	// path).
	HostArchive string
	// ArgArchive is the archive path passed to -XX:SharedArchiveFile (the
	// in-sandbox path when ArchiveArgDir is set, else the host path).
	ArgArchive string
	// Args are the CDS launch args to prepend to the JVM argv (nil for
	// skip/defer).
	Args []string
	// MountRW reports whether the cache mount must be writable (writer only).
	MountRW bool
}

// DecideCDSRegen resolves how a regeneration-enabled artifact should launch on
// this node. It never returns a hard error for expected conditions (unsupported
// JDK, unwritable cache, lost election); those degrade to RegenSkip/RegenDefer so
// the launch proceeds on base CDS. It returns an error only for programmer
// misuse (empty JDKRoot/ArtifactKey).
func DecideCDSRegen(p RegenParams) (RegenDecision, error) {
	if p.JDKRoot == "" || p.ArtifactKey == "" {
		return RegenDecision{}, fmt.Errorf("cds regen: JDKRoot and ArtifactKey are required")
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	cacheDir := p.CacheDir
	if cacheDir == "" {
		cacheDir = DefaultCDSCacheDir
	}
	lockTTL := p.LockTTL
	if lockTTL <= 0 {
		lockTTL = DefaultWriterTTL
	}
	evictTTL := p.EvictTTL
	if evictTTL <= 0 {
		evictTTL = DefaultEvictTTL
	}

	// JDK build identity + feature gate. Any failure => skip (base CDS).
	feature, buildID, err := readJDKIdentity(p.JDKRoot)
	if err != nil || feature < minRegenFeature {
		return decisionSkip(p, cacheDir), nil
	}

	key := regenKey(p.ArtifactKey, buildID)
	hostArchive := filepath.Join(cacheDir, key+".jsa")
	argArchive := hostArchive
	if p.ArchiveArgDir != "" {
		// Nodes are Linux; join with "/" so the in-sandbox path is correct
		// regardless of the dev OS generating a bundle.
		argArchive = strings.TrimRight(p.ArchiveArgDir, "/") + "/" + key + ".jsa"
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		// Can't manage a cache here — fall back to base CDS.
		return decisionSkip(p, cacheDir), nil
	}
	evictStaleArchives(cacheDir, key, evictTTL, now)

	// A valid cached archive already exists → consume it read-only.
	if fi, statErr := os.Stat(hostArchive); statErr == nil && fi.Size() > 0 {
		d := RegenDecision{
			Role:        RegenConsume,
			Key:         key,
			HostArchive: hostArchive,
			ArgArchive:  argArchive,
			Args:        []string{"-Xshare:auto", "-XX:SharedArchiveFile=" + argArchive},
		}
		recordRegenMetric(p.MetricsDir, key, d.Role, now)
		return d, nil
	}

	// No archive yet: elect a single writer per key; others defer to base CDS.
	if claimWriter(hostArchive+writerMarkerSuffix, now, lockTTL) {
		// Seed a fresh cache entry from the shipped archive if one is available.
		// -XX:+AutoCreateSharedArchive validates it and recreates at exit if the
		// seed is JDK-stale, so seeding is always safe.
		if p.SeedArchive != "" {
			_ = seedArchive(p.SeedArchive, hostArchive)
		}
		d := RegenDecision{
			Role:        RegenWrite,
			Key:         key,
			HostArchive: hostArchive,
			ArgArchive:  argArchive,
			Args:        []string{"-XX:+AutoCreateSharedArchive", "-XX:SharedArchiveFile=" + argArchive, "-Xshare:auto"},
			MountRW:     true,
		}
		recordRegenMetric(p.MetricsDir, key, d.Role, now)
		return d, nil
	}

	d := RegenDecision{Role: RegenDefer, Key: key}
	recordRegenMetric(p.MetricsDir, key, d.Role, now)
	return d, nil
}

func decisionSkip(p RegenParams, cacheDir string) RegenDecision {
	d := RegenDecision{Role: RegenSkip}
	recordRegenMetric(p.MetricsDir, "", d.Role, timeOrNow(p.Now))
	return d
}

func timeOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// claimWriter attempts to atomically become the sole generator for a key by
// creating marker with O_EXCL. If the marker already exists but is older than
// ttl, the previous writer is assumed dead and the claim is reclaimed. Best
// effort: any unexpected error yields false (defer to base CDS).
func claimWriter(marker string, now time.Time, ttl time.Duration) bool {
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "pid=%d\ntime=%s\n", os.Getpid(), now.UTC().Format(time.RFC3339))
		_ = f.Close()
		return true
	}
	if !errors.Is(err, os.ErrExist) {
		return false
	}
	fi, statErr := os.Stat(marker)
	if statErr != nil {
		return false
	}
	if now.Sub(fi.ModTime()) <= ttl {
		return false // a live writer holds the key
	}
	// Stale marker: reclaim by refreshing its mtime in place.
	if err := os.Chtimes(marker, now, now); err != nil {
		return false
	}
	return true
}

// seedArchive copies src to dst (the cache path) only when dst does not yet
// exist, so a concurrent writer's in-progress archive is never clobbered.
func seedArchive(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	return copyFileContents(src, dst)
}

// evictStaleArchives removes cache `.jsa` files (and their writer markers) whose
// mtime is older than ttl, except the key currently in use. Best-effort: all
// errors are ignored. Prevents old JDK-build keys from accumulating after
// patches.
func evictStaleArchives(cacheDir, keepKey string, ttl time.Duration, now time.Time) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsa") {
			continue
		}
		if strings.TrimSuffix(name, ".jsa") == keepKey {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= ttl {
			continue
		}
		p := filepath.Join(cacheDir, name)
		_ = os.Remove(p)
		_ = os.Remove(p + writerMarkerSuffix)
	}
}

// regenKey derives the per-(artifact, JDK-build) cache key. Both inputs feed a
// SHA-256 so the key changes iff the app or the JDK build changes; the hex is
// truncated to a filename-friendly length.
func regenKey(artifactKey, buildID string) string {
	h := sha256.Sum256([]byte(artifactKey + "\x00" + buildID))
	return hex.EncodeToString(h[:])[:32]
}

// readJDKIdentity returns the JDK feature version and a build-identity token for
// the JDK rooted at jdkRoot. It prefers the `release` file (present in every
// standard distribution); when that is missing/unparseable it falls back to
// `bin/java -Xinternalversion`, whose output is exactly the string HotSpot
// records in a CDS archive header. The build token need only *change* on any
// JDK patch (that is what re-keys the cache); AutoCreateSharedArchive itself
// enforces the exact-build match at map time.
func readJDKIdentity(jdkRoot string) (feature int, buildID string, err error) {
	rel := filepath.Join(jdkRoot, "release")
	if kv, rerr := parseReleaseFile(rel); rerr == nil {
		jv := kv["JAVA_VERSION"]
		if f := parseFeature(jv); f > 0 {
			parts := []string{kv["IMPLEMENTOR"], kv["IMPLEMENTOR_VERSION"], firstNonEmpty(kv["JAVA_RUNTIME_VERSION"], jv), kv["OS_ARCH"]}
			return f, strings.Join(parts, "|"), nil
		}
	}
	// Fallback: ask the JVM directly.
	ident, ierr := javaInternalVersion(jdkRoot)
	if ierr != nil {
		return 0, "", ierr
	}
	f := parseFeatureFromInternal(ident)
	if f == 0 {
		return 0, "", fmt.Errorf("cds regen: cannot determine JDK feature for %q", jdkRoot)
	}
	return f, ident, nil
}

func javaInternalVersion(jdkRoot string) (string, error) {
	bin := filepath.Join(jdkRoot, "bin", "java")
	out, err := exec.Command(bin, "-Xinternalversion").Output()
	if err != nil {
		return "", fmt.Errorf("cds regen: %s -Xinternalversion: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseReleaseFile reads a JDK `release` file into a map, stripping the
// surrounding double quotes from values (KEY="VALUE" per JEP 220).
func parseReleaseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return kv, nil
}

// parseFeature extracts the JDK feature version from a version string such as
// "21.0.5" (=> 21), "1.8.0_412" (=> 8), or "17" (=> 17).
func parseFeature(v string) int {
	v = strings.Trim(strings.TrimSpace(v), `"`)
	if v == "" {
		return 0
	}
	comps := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' || r == '+' || r == '-' })
	if len(comps) == 0 {
		return 0
	}
	if comps[0] == "1" && len(comps) > 1 {
		// Legacy 1.x.y scheme (1.8 => 8).
		if n, err := strconv.Atoi(leadingDigits(comps[1])); err == nil {
			return n
		}
		return 0
	}
	if n, err := strconv.Atoi(leadingDigits(comps[0])); err == nil {
		return n
	}
	return 0
}

// parseFeatureFromInternal pulls the feature version out of a HotSpot
// -Xinternalversion string, which contains a parenthesized version like
// "(21.0.5+11-LTS)".
func parseFeatureFromInternal(s string) int {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return 0
	}
	rest := s[open+1:]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return parseFeature(rest)
	}
	return parseFeature(rest[:end])
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SupportsCDSRegen reports whether the JDK at jdkRoot supports node-side AppCDS
// regeneration (-XX:+AutoCreateSharedArchive, JDK feature >= 19). It is a
// convenience wrapper over the identity read DecideCDSRegen performs internally;
// callers that already invoke DecideCDSRegen do not need it. Whether to
// regenerate at all is a deployment decision (the brewlet.sh/cds-regenerate
// annotation / --appcds-regenerate flag), not a property of the artifact.
func SupportsCDSRegen(jdkRoot string) bool {
	feature, _, err := readJDKIdentity(jdkRoot)
	if err != nil {
		return false
	}
	return feature >= minRegenFeature
}
