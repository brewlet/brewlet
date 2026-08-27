package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brewlet/brewlet/internal/artifact"
)

func TestAppCDSTrainingArgsFatJar(t *testing.T) {
	cfg := artifact.JVMConfig{
		SchemaVersion:    1,
		MainJar:          "app.jar",
		Entry:            artifact.Entry{Mode: "jar"},
		EnablePreview:    true,
		AddModules:       []string{"jdk.incubator.vector"},
		AddOpens:         []string{"java.base/java.lang=ALL-UNNAMED"},
		AddExports:       []string{"java.base/sun.nio.ch=ALL-UNNAMED"},
		SystemProperties: map[string]string{"b": "2", "a": "1"},
	}
	got, err := AppCDSTrainingArgs(cfg, "app.jar", "/tmp/app.jsa", []string{"--selftest"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--enable-preview",
		"--add-modules", "jdk.incubator.vector",
		"--add-opens", "java.base/java.lang=ALL-UNNAMED",
		"--add-exports", "java.base/sun.nio.ch=ALL-UNNAMED",
		"-Da=1", "-Db=2",
		"-XX:ArchiveClassesAtExit=/tmp/app.jsa",
		"-jar", "app.jar",
		"--selftest",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestAppCDSTrainingArgsMinimal(t *testing.T) {
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar"}
	got, err := AppCDSTrainingArgs(cfg, "app.jar", "out.jsa", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-XX:ArchiveClassesAtExit=out.jsa", "-jar", "app.jar"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("args mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestAppCDSTrainingArgsRejectsNonJar(t *testing.T) {
	for _, mode := range []string{"classpath", "module"} {
		cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: mode}}
		if mode == "classpath" {
			cfg.Entry.MainClass = "com.example.Main"
		} else {
			cfg.Entry.Module = "com.example"
		}
		if _, err := AppCDSTrainingArgs(cfg, "app.jar", "out.jsa", nil); err == nil {
			t.Fatalf("expected error for entry.mode=%q", mode)
		}
	}
}

func TestGenerateAppCDSArchiveTimeout(t *testing.T) {
	// A "java" that sleeps well past the timeout stands in for a non-terminating
	// workload; the run must be reported as a timeout, not a success.
	fakeJava := filepath.Join(t.TempDir(), "java")
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(fakeJava, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jar, []byte("PK\x03\x04dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	out := filepath.Join(t.TempDir(), "app.jsa")
	err := GenerateAppCDSArchive(cfg, jar, fakeJava, out, 200*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestGenerateAppCDSArchiveWritesArchive(t *testing.T) {
	// A fake "java" that just writes a non-empty file at the ArchiveClassesAtExit
	// path proves the staging + arg wiring + success detection, without a JDK.
	fakeJava := filepath.Join(t.TempDir(), "java")
	// Extract the ArchiveClassesAtExit path (=<path>) from argv and write to it.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"    -XX:ArchiveClassesAtExit=*) echo CDS > \"${a#-XX:ArchiveClassesAtExit=}\" ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(fakeJava, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jar, []byte("PK\x03\x04dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	out := filepath.Join(t.TempDir(), "nested", "app.jsa")
	if err := GenerateAppCDSArchive(cfg, jar, fakeJava, out, 5*time.Second, nil); err != nil {
		t.Fatalf("GenerateAppCDSArchive: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("expected non-empty archive at %s (err=%v)", out, err)
	}
}

func TestGenerateAppCDSArchiveNoArchiveFails(t *testing.T) {
	// A "java" that exits 0 but writes nothing must be reported as failure.
	fakeJava := filepath.Join(t.TempDir(), "java")
	if err := os.WriteFile(fakeJava, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jar, []byte("PK\x03\x04dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	out := filepath.Join(t.TempDir(), "app.jsa")
	err := GenerateAppCDSArchive(cfg, jar, fakeJava, out, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "no archive") {
		t.Fatalf("expected no-archive error, got %v", err)
	}
}
