package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/brewlet/brewlet/internal/artifact"
)

func TestManagedDependencyBundleCLIFlow(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "oci")
	dependency := []byte("approved dependency")
	layer := filepath.Join(dir, "dependencies.tar")
	writeCLITar(t, layer, "approved.jar", dependency)
	lockPath := filepath.Join(dir, "dependency-lock.json")
	lock := artifact.DependencyLock{
		SchemaVersion: 1,
		Artifacts: []artifact.DependencyLockEntry{{
			GroupID: "com.example", ArtifactID: "approved", Version: "1.0.0",
			Type: "jar", Scope: "runtime", FileName: "approved.jar", SHA256: sha256Hex(dependency),
		}},
	}
	lockRaw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdDependencyBundle([]string{
		layer, "platform/approved:1",
		"--store", store,
		"--name", "approved",
		"--version", "1",
		"--source-bom", "com.example:approved-bom:1",
		"--lock", lockPath,
		"--compatible-jdks", "21,25",
	}); err != nil {
		t.Fatalf("cmdDependencyBundle: %v", err)
	}

	jar := filepath.Join(dir, "orders.jar")
	writeCLIZip(t, jar, "com/example/Orders.class")
	if err := cmdPush([]string{
		jar, "apps/orders:1",
		"--store", store,
		"--dependency-bundle", "platform/approved:1",
		"--dependency-lock", lockPath,
		"--main-class", "com.example.Orders",
	}); err != nil {
		t.Fatalf("cmdPush: %v", err)
	}

	manifest, _, err := (artifact.Store{Root: store}).ResolveManifestByRef("apps/orders:1")
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok, err := manifest.ManagedDependencyEvidence()
	if err != nil || !ok {
		t.Fatalf("managed evidence: ok=%v err=%v", ok, err)
	}
	if evidence.SourceBOM != "com.example:approved-bom:1" {
		t.Fatalf("sourceBom = %q", evidence.SourceBOM)
	}
}

func TestManagedDependencyBundleRejectsFatJar(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "fat.jar")
	writeCLIZip(t, jar, "BOOT-INF/lib/dependency.jar")
	err := cmdPush([]string{
		jar, "apps/fat:1",
		"--store", filepath.Join(dir, "missing-store"),
		"--dependency-bundle", "platform/approved:1",
		"--dependency-lock", filepath.Join(dir, "dependency-lock.json"),
		"--main-class", "com.example.Main",
	})
	if err == nil {
		t.Fatal("expected fat JAR rejection")
	}
}

func TestParseJDKFeaturesRejectsPartialInteger(t *testing.T) {
	if _, err := parseJDKFeatures("21x"); err == nil {
		t.Fatal("expected invalid JDK feature")
	}
}

func writeCLITar(t *testing.T, path, name string, content []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCLIZip(t *testing.T, path, name string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("content")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
