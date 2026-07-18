package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brewlet/brewlet/internal/artifact"
)

// fakeJDK writes a minimal JDK root with a `release` file advertising the given
// JAVA_VERSION, so readJDKIdentity can resolve the feature version without a real
// JVM.
func fakeJDK(t *testing.T, javaVersion string) string {
	t.Helper()
	root := t.TempDir()
	rel := "IMPLEMENTOR=\"Eclipse Adoptium\"\n" +
		"JAVA_VERSION=\"" + javaVersion + "\"\n" +
		"JAVA_RUNTIME_VERSION=\"" + javaVersion + "+11-LTS\"\n" +
		"OS_ARCH=\"aarch64\"\n"
	if err := os.WriteFile(filepath.Join(root, "release"), []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBuildJVMArgsSuppressesShippedArgsWhenRegenerate(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); strings.Contains(got, "SharedArchiveFile") || strings.Contains(got, "AutoCreate") {
		t.Errorf("args = %q, want no CDS archive flags in regen mode (node injects them)", got)
	}
}

func TestParseFeature(t *testing.T) {
	cases := map[string]int{
		"21.0.5":    21,
		"17":        17,
		"1.8.0_412": 8,
		"25.0.3":    25,
		"":          0,
		"garbage":   0,
	}
	for in, want := range cases {
		if got := parseFeature(in); got != want {
			t.Errorf("parseFeature(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseFeatureFromInternal(t *testing.T) {
	s := "OpenJDK 64-Bit Server VM (21.0.5+11-LTS) for linux-aarch64 JRE (21.0.5+11-LTS)"
	if got := parseFeatureFromInternal(s); got != 21 {
		t.Errorf("parseFeatureFromInternal = %d, want 21", got)
	}
}

func TestReadJDKIdentityFromRelease(t *testing.T) {
	root := fakeJDK(t, "21.0.5")
	feature, buildID, err := readJDKIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if feature != 21 {
		t.Errorf("feature = %d, want 21", feature)
	}
	if !strings.Contains(buildID, "21.0.5+11-LTS") || !strings.Contains(buildID, "Eclipse Adoptium") {
		t.Errorf("buildID = %q, want it to include runtime version + implementor", buildID)
	}
}

func TestDecideCDSRegenGatesOldJDK(t *testing.T) {
	dec, err := DecideCDSRegen(RegenParams{
		CacheDir:    t.TempDir(),
		JDKRoot:     fakeJDK(t, "17.0.10"),
		ArtifactKey: "sha256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenSkip {
		t.Errorf("role = %q, want skip on JDK 17 (no AutoCreateSharedArchive)", dec.Role)
	}
	if len(dec.Args) != 0 {
		t.Errorf("args = %v, want none when skipped", dec.Args)
	}
}

func TestDecideCDSRegenElectsWriterThenConsumes(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	params := RegenParams{
		CacheDir:      cache,
		JDKRoot:       jdk,
		ArtifactKey:   "sha256:abc",
		ArchiveArgDir: InSandboxCDSDir,
	}

	// First launch: empty cache -> elected writer with AutoCreateSharedArchive.
	dec, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenWrite {
		t.Fatalf("role = %q, want write on empty cache", dec.Role)
	}
	if !dec.MountRW {
		t.Error("writer must mount the cache read-write")
	}
	joined := strings.Join(dec.Args, " ")
	if !strings.Contains(joined, "-XX:+AutoCreateSharedArchive") {
		t.Errorf("writer args = %q, want -XX:+AutoCreateSharedArchive", joined)
	}
	wantPath := InSandboxCDSDir + "/" + dec.Key + ".jsa"
	if !strings.Contains(joined, "-XX:SharedArchiveFile="+wantPath) {
		t.Errorf("writer args = %q, want -XX:SharedArchiveFile=%s", joined, wantPath)
	}

	// Simulate the writer having produced the archive at exit.
	if err := os.WriteFile(dec.HostArchive, []byte("jsa-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Next launch: valid archive present -> consume read-only, no AutoCreate.
	dec2, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if dec2.Role != RegenConsume {
		t.Fatalf("role = %q, want consume when a cached archive exists", dec2.Role)
	}
	if dec2.MountRW {
		t.Error("consumer must mount the cache read-only")
	}
	joined2 := strings.Join(dec2.Args, " ")
	if strings.Contains(joined2, "AutoCreate") {
		t.Errorf("consumer args = %q, must not recreate an existing archive", joined2)
	}
	if !strings.Contains(joined2, "-Xshare:auto") || !strings.Contains(joined2, "-XX:SharedArchiveFile="+wantPath) {
		t.Errorf("consumer args = %q, want -Xshare:auto -XX:SharedArchiveFile=%s", joined2, wantPath)
	}
}

func TestDecideCDSRegenDefersSecondWriter(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	params := RegenParams{CacheDir: cache, JDKRoot: jdk, ArtifactKey: "sha256:abc"}

	first, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RegenWrite {
		t.Fatalf("first role = %q, want write", first.Role)
	}
	// Archive not yet written; a concurrent launch must defer to base CDS.
	second, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != RegenDefer {
		t.Errorf("second role = %q, want defer while a writer is in flight", second.Role)
	}
	if len(second.Args) != 0 {
		t.Errorf("defer args = %v, want none (base CDS)", second.Args)
	}
}

func TestDecideCDSRegenStaleMarkerReelects(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	base := RegenParams{CacheDir: cache, JDKRoot: jdk, ArtifactKey: "sha256:abc", LockTTL: time.Minute}

	first, err := DecideCDSRegen(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RegenWrite {
		t.Fatalf("first role = %q, want write", first.Role)
	}
	// Age the writer marker past the TTL -> the next launch reclaims the key.
	marker := first.HostArchive + writerMarkerSuffix
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	reelect, err := DecideCDSRegen(base)
	if err != nil {
		t.Fatal(err)
	}
	if reelect.Role != RegenWrite {
		t.Errorf("role = %q, want write after reclaiming a stale marker", reelect.Role)
	}
}

func TestDecideCDSRegenSeeds(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	seed := filepath.Join(t.TempDir(), "app.jsa")
	if err := os.WriteFile(seed, []byte("seed-archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	dec, err := DecideCDSRegen(RegenParams{
		CacheDir:    cache,
		JDKRoot:     jdk,
		ArtifactKey: "sha256:abc",
		SeedArchive: seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenWrite {
		t.Fatalf("role = %q, want write", dec.Role)
	}
	got, err := os.ReadFile(dec.HostArchive)
	if err != nil {
		t.Fatalf("seed not copied into cache: %v", err)
	}
	if string(got) != "seed-archive" {
		t.Errorf("cache archive = %q, want the seed bytes", got)
	}
}

func TestDecideCDSRegenEvictsStale(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	stale := filepath.Join(cache, "deadbeef.jsa")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := DecideCDSRegen(RegenParams{
		CacheDir:    cache,
		JDKRoot:     jdk,
		ArtifactKey: "sha256:abc",
		EvictTTL:    14 * 24 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale archive should have been evicted, stat err = %v", err)
	}
}

func TestDecideCDSRegenWritesMetric(t *testing.T) {
	cache := t.TempDir()
	metrics := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	if _, err := DecideCDSRegen(RegenParams{
		CacheDir:    cache,
		JDKRoot:     jdk,
		ArtifactKey: "sha256:abc",
		MetricsDir:  metrics,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected a metric file to be written")
	}
	b, err := os.ReadFile(filepath.Join(metrics, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "brewlet_cds_archive_mapped") {
		t.Errorf("metric file = %q, want brewlet_cds_archive_mapped", b)
	}
}

func TestRegenKeyChangesWithBuild(t *testing.T) {
	a := regenKey("sha256:abc", "temurin|21.0.5+11")
	b := regenKey("sha256:abc", "temurin|21.0.6+7") // patch bump
	c := regenKey("sha256:xyz", "temurin|21.0.5+11")
	if a == b {
		t.Error("key must change when the JDK build changes")
	}
	if a == c {
		t.Error("key must change when the artifact changes")
	}
	if len(a) != 32 {
		t.Errorf("key length = %d, want 32", len(a))
	}
}

func TestGenerateBundleWithRegenMountsCache(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := fakeJDK(t, "21.0.5")
	jarHost := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarHost, []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	out := filepath.Join(dir, "bundle")
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	if err := GenerateBundleWithRegen(cfg, jdkRoot, "", "", jarHost, nil, nil, "", out, Resources{}, nil,
		CDSRegenOptions{Regenerate: true, ArtifactKey: "sha256:abc", CacheDir: cache}); err != nil {
		t.Fatalf("GenerateBundleWithRegen: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := string(b)
	if !strings.Contains(cfgJSON, "-XX:+AutoCreateSharedArchive") {
		t.Errorf("config.json missing regen argv:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, InSandboxCDSDir) {
		t.Errorf("config.json missing cache mount at %s:\n%s", InSandboxCDSDir, cfgJSON)
	}
	// A regenerating artifact with no shipped archive must not mount /app/*.jsa.
	if strings.Contains(cfgJSON, "/app/app.jsa") {
		t.Errorf("config.json should not mount a /app archive in pure regen mode:\n%s", cfgJSON)
	}
}
