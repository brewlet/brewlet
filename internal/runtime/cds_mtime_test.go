package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brewlet/brewlet/internal/artifact"
)

// mtime returns path's modification time, failing the test on error.
func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime()
}

func TestStageCDSJarPinsModTime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jar")
	if err := os.WriteFile(src, []byte("PKcontent"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Give the source a clearly non-canonical mtime.
	drift := time.Date(2030, 6, 6, 6, 6, 6, 0, time.UTC)
	if err := os.Chtimes(src, drift, drift); err != nil {
		t.Fatal(err)
	}

	dst, err := StageCDSJar(src, filepath.Join(dir, "staged"), "app.jar")
	if err != nil {
		t.Fatalf("StageCDSJar: %v", err)
	}
	if filepath.Base(dst) != "app.jar" {
		t.Errorf("staged name = %q, want app.jar", filepath.Base(dst))
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "PKcontent" {
		t.Errorf("staged content = %q err %v, want PKcontent", b, err)
	}
	if got := mtime(t, dst); !got.Equal(CDSModTime) {
		t.Errorf("staged mtime = %v, want canonical %v", got, CDSModTime)
	}
}

func TestPinCDSModTimesUnder(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "lib", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(dir, "lib", "a.jar"),
		filepath.Join(sub, "b.jar"),
	}
	drift := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, drift, drift); err != nil {
			t.Fatal(err)
		}
	}

	if err := PinCDSModTimesUnder(filepath.Join(dir, "lib")); err != nil {
		t.Fatalf("PinCDSModTimesUnder: %v", err)
	}
	for _, f := range files {
		if got := mtime(t, f); !got.Equal(CDSModTime) {
			t.Errorf("%s mtime = %v, want canonical %v", f, got, CDSModTime)
		}
	}

	// A missing directory is a no-op, not an error.
	if err := PinCDSModTimesUnder(filepath.Join(dir, "does-not-exist")); err != nil {
		t.Errorf("PinCDSModTimesUnder(missing) = %v, want nil", err)
	}
}

func TestAssembleSandboxWithCDSPinsJarModTime(t *testing.T) {
	dir := t.TempDir()
	jarSrc := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarSrc, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift := time.Date(2029, 9, 9, 9, 9, 9, 0, time.UTC)
	if err := os.Chtimes(jarSrc, drift, drift); err != nil {
		t.Fatal(err)
	}
	// A dependency layer so the /app/lib pinning path is exercised too.
	libTar := filepath.Join(dir, "deps.tar")
	writeTar(t, libTar, map[string]string{"dep.jar": "DEP"})
	jsaSrc := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaSrc, []byte("JSA"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := artifact.JVMConfig{
		MainJar: "app.jar",
		Entry:   artifact.Entry{Mode: "classpath", MainClass: "com.acme.Main", ClassPath: []string{"app.jar", "lib/*"}},
		CDS:     &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"},
	}
	sandbox, jarPath, err := AssembleSandboxWithCDS(cfg, jarSrc, []string{libTar}, nil, jsaSrc, false)
	if err != nil {
		t.Fatalf("AssembleSandboxWithCDS: %v", err)
	}
	defer os.RemoveAll(sandbox)

	if got := mtime(t, jarPath); !got.Equal(CDSModTime) {
		t.Errorf("staged jar mtime = %v, want canonical %v", got, CDSModTime)
	}
	depPath := filepath.Join(filepath.Dir(jarPath), "lib", "dep.jar")
	if got := mtime(t, depPath); !got.Equal(CDSModTime) {
		t.Errorf("staged dep.jar mtime = %v, want canonical %v", got, CDSModTime)
	}
}

func TestAssembleSandboxNoCDSKeepsJarModTime(t *testing.T) {
	dir := t.TempDir()
	jarSrc := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarSrc, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	sandbox, jarPath, err := AssembleSandbox(cfg, jarSrc, nil, nil)
	if err != nil {
		t.Fatalf("AssembleSandbox: %v", err)
	}
	defer os.RemoveAll(sandbox)
	// Without CDS the staged jar must NOT be forced to the canonical mtime.
	if got := mtime(t, jarPath); got.Equal(CDSModTime) {
		t.Errorf("non-CDS staged jar mtime unexpectedly canonical %v", got)
	}
}

// bundleJarSource returns the host Source of the /app/<mainJar> bind mount in the
// generated bundle's config.json.
func bundleJarSource(t *testing.T, bundleConfig, inSandboxJar string) string {
	t.Helper()
	var spec struct {
		Mounts []struct {
			Destination string `json:"destination"`
			Source      string `json:"source"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal([]byte(bundleConfig), &spec); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	for _, m := range spec.Mounts {
		if m.Destination == inSandboxJar {
			return m.Source
		}
	}
	t.Fatalf("no mount for %s in config.json", inSandboxJar)
	return ""
}

func TestGenerateBundleWithCDSPinsJarModTime(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := filepath.Join(dir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	jarHost := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift := time.Date(2028, 8, 8, 8, 8, 8, 0, time.UTC)
	if err := os.Chtimes(jarHost, drift, drift); err != nil {
		t.Fatal(err)
	}
	jsaHost := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaHost, []byte("JSA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}}
	out := filepath.Join(dir, "bundle")
	if err := GenerateBundleWithCDS(cfg, jdkRoot, "", "", jarHost, nil, nil, jsaHost, out, Resources{}, nil); err != nil {
		t.Fatalf("GenerateBundleWithCDS: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	src := bundleJarSource(t, string(b), "/app/app.jar")
	// When CDS ships, the jar mount must NOT be the original blob (whose mtime is
	// non-deterministic) but a staged copy pinned to the canonical mtime.
	if src == jarHost {
		t.Errorf("jar mount source = original blob %q, want a staged CDS copy", src)
	}
	if got := mtime(t, src); !got.Equal(CDSModTime) {
		t.Errorf("staged jar mount mtime = %v, want canonical %v", got, CDSModTime)
	}
}

func TestGenerateBundleNoCDSMountsOriginalJar(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := filepath.Join(dir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	jarHost := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	out := filepath.Join(dir, "bundle")
	if err := GenerateBundle(cfg, jdkRoot, jarHost, out, Resources{}, nil); err != nil {
		t.Fatalf("GenerateBundle: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// No CDS: preserve the zero-copy bind mount straight from the blob.
	if src := bundleJarSource(t, string(b), "/app/app.jar"); src != jarHost {
		t.Errorf("jar mount source = %q, want original blob %q (no copy without CDS)", src, jarHost)
	}
}

func TestCDSModTimeValue(t *testing.T) {
	// Must match the Maven plugin's canonical value (epoch second 946684800 =
	// 2000-01-01T00:00:00Z). Keep the two in lockstep — see https://github.com/brewlet/site/blob/main/docs/appcds.md.
	if got := CDSModTime.Unix(); got != 946684800 {
		t.Errorf("CDSModTime = %d, want 946684800 (2000-01-01T00:00:00Z)", got)
	}
	if CDSModTime.Nanosecond() != 0 {
		t.Errorf("CDSModTime nanoseconds = %d, want 0", CDSModTime.Nanosecond())
	}
}
