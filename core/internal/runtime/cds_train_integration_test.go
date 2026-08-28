package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brewlet/brewlet/internal/artifact"
)

// This file turns the manual empirical verification of the AppCDS turnkey path
// (see https://github.com/brewlet/site/blob/main/docs/appcds.md §4.2/§4.4) into an automated, self-skipping integration
// test. It requires a real JDK (java + javac + jar, feature >= 17) and is skipped
// under `go test -short` or when no suitable JDK is found — so CI's Go-only job
// skips it while any developer/CI host with a JDK runs it for real.
//
// It asserts the load-bearing invariant behind the whole feature: an archive
// produced by GenerateAppCDSArchive (the `brewlet push --appcds` code path) maps
// under `-Xshare:on` (which is FATAL on mismatch, unlike the runtime's safe
// `-Xshare:auto`) from a DIFFERENT directory than it was trained in, as long as
// the JAR is presented with the canonical mtime — and, conversely, that a drifted
// mtime makes the very same archive fail to map. That negative case is what
// guarantees the deterministic-mtime normalization (StageCDSJar / CDSModTime) is
// actually pulling its weight.

var javaVersionRE = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

// locateJDK returns paths to java, javac, and jar for a JDK with feature version
// >= 17, honoring JAVA_HOME then PATH, or skips the test.
func locateJDK(t *testing.T) (javaBin, javacBin, jarBin string) {
	t.Helper()

	find := func(tool string) string {
		if jh := strings.TrimSpace(os.Getenv("JAVA_HOME")); jh != "" {
			cand := filepath.Join(jh, "bin", tool)
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
		if p, err := exec.LookPath(tool); err == nil {
			return p
		}
		return ""
	}

	javaBin, javacBin, jarBin = find("java"), find("javac"), find("jar")
	if javaBin == "" || javacBin == "" || jarBin == "" {
		t.Skip("AppCDS integration test needs a full JDK (java + javac + jar) on JAVA_HOME or PATH; skipping")
	}

	out, err := exec.Command(javaBin, "-version").CombinedOutput()
	if err != nil {
		t.Skipf("AppCDS integration test: %q -version failed (%v); skipping", javaBin, err)
	}
	m := javaVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		t.Skipf("AppCDS integration test: could not parse java version from %q; skipping", strings.TrimSpace(string(out)))
	}
	feature, _ := strconv.Atoi(m[1])
	if feature == 1 { // legacy "1.8" scheme
		feature, _ = strconv.Atoi(m[2])
	}
	if feature < 17 {
		t.Skipf("AppCDS integration test needs JDK >= 17, found feature %d; skipping", feature)
	}
	return javaBin, javacBin, jarBin
}

// buildTinyFatJar compiles a minimal main class that touches a few JDK classes
// (so the archive is non-trivial) and packages it as an executable fat JAR,
// returning its path.
func buildTinyFatJar(t *testing.T, javacBin, jarBin string) string {
	t.Helper()
	src := t.TempDir()
	main := filepath.Join(src, "Main.java")
	const prog = `public class Main {
  public static void main(String[] a) {
    java.util.List<String> l = new java.util.ArrayList<>();
    l.add("warm");
    java.util.regex.Pattern.compile("\\d+");
    System.out.println("APPCDS_TRAINED");
  }
}`
	if err := os.WriteFile(main, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(javacBin, "-d", src, main).CombinedOutput(); err != nil {
		t.Fatalf("javac failed: %v\n%s", err, out)
	}
	mf := filepath.Join(src, "manifest.txt")
	if err := os.WriteFile(mf, []byte("Main-Class: Main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(src, "app.jar")
	if out, err := exec.Command(jarBin, "cfm", jar, mf, "-C", src, "Main.class").CombinedOutput(); err != nil {
		t.Fatalf("jar failed: %v\n%s", err, out)
	}
	return jar
}

// mapUnderShareOn runs the app in runDir with `-Xshare:on` (fatal on mismatch)
// and the archive, returning the JVM's combined output and exit code.
func mapUnderShareOn(t *testing.T, javaBin, runDir string) (string, int) {
	t.Helper()
	cmd := exec.Command(javaBin,
		"-Xshare:on",
		"-XX:SharedArchiveFile=app.jsa",
		"-Xlog:cds=info",
		"-jar", "app.jar",
	)
	cmd.Dir = runDir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running java: %v\n%s", err, out)
		}
	}
	return string(out), code
}

func TestAppCDSTrainThenMapIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AppCDS JDK integration test in -short mode")
	}
	javaBin, javacBin, jarBin := locateJDK(t)

	jar := buildTinyFatJar(t, javacBin, jarBin)
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}

	// Produce the archive via the real CLI code path.
	archive := filepath.Join(t.TempDir(), "out", "app.jsa")
	if err := GenerateAppCDSArchive(cfg, jar, javaBin, archive, 90*time.Second, nil); err != nil {
		t.Fatalf("GenerateAppCDSArchive: %v", err)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Size() == 0 {
		t.Fatalf("expected non-empty archive at %s (err=%v)", archive, err)
	}

	// Positive: the shim presents the JAR with the canonical mtime in a fresh
	// directory (different from the training dir). The archive must map.
	t.Run("maps with canonical mtime", func(t *testing.T) {
		runDir := t.TempDir()
		if _, err := StageCDSJar(jar, runDir, "app.jar"); err != nil {
			t.Fatalf("StageCDSJar: %v", err)
		}
		if err := copyFileContents(archive, filepath.Join(runDir, "app.jsa")); err != nil {
			t.Fatal(err)
		}
		out, code := mapUnderShareOn(t, javaBin, runDir)
		if code != 0 {
			t.Fatalf("expected -Xshare:on to map (exit 0), got exit %d\n%s", code, out)
		}
		if !strings.Contains(out, "Mapped static") || !strings.Contains(out, "Mapped dynamic") {
			t.Fatalf("expected static+dynamic CDS regions to be mapped, got:\n%s", out)
		}
		if !strings.Contains(out, "APPCDS_TRAINED") {
			t.Fatalf("app did not run to completion under CDS:\n%s", out)
		}
	})

	// Negative: the exact same archive against a JAR whose mtime is NOT canonical
	// must be refused under -Xshare:on. This is what proves the canonical-mtime
	// normalization is load-bearing: without it the archive would silently fail
	// to map on the node.
	t.Run("refuses with drifted mtime", func(t *testing.T) {
		runDir := t.TempDir()
		if err := copyFileContents(jar, filepath.Join(runDir, "app.jar")); err != nil {
			t.Fatal(err)
		}
		drift := time.Date(2030, 6, 6, 6, 6, 6, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(runDir, "app.jar"), drift, drift); err != nil {
			t.Fatal(err)
		}
		if err := copyFileContents(archive, filepath.Join(runDir, "app.jsa")); err != nil {
			t.Fatal(err)
		}
		out, code := mapUnderShareOn(t, javaBin, runDir)
		if code == 0 {
			t.Fatalf("expected -Xshare:on to REFUSE a drifted-mtime JAR, but it mapped:\n%s", out)
		}
	})
}
