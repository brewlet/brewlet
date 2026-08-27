package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/brewlet/brewlet/internal/artifact"
)

// writeContentBlob writes b into an OCI-style content store and returns its
// "sha256:<hex>" digest, mirroring how containerd lays blobs on disk.
func writeContentBlob(t *testing.T, root string, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	digest := fmt.Sprintf("sha256:%x", sum)
	dir := filepath.Join(root, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest[len("sha256:"):]), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestContentStoreBlobs(t *testing.T) {
	root := t.TempDir()

	jarDigest := writeContentBlob(t, root, []byte("PK\x03\x04 fake-jar-bytes"))

	cfg := artifact.JVMConfig{
		SchemaVersion: 1,
		MainJar:       "app.jar",
		Entry:         artifact.Entry{Mode: "jar"},
	}
	cfgBytes, _ := json.Marshal(cfg)
	cfgDigest := writeContentBlob(t, root, cfgBytes)

	man := artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		ArtifactType:  artifact.ArtifactType,
		Config:        artifact.Descriptor{MediaType: artifact.ConfigMediaType, Digest: cfgDigest},
		Layers: []artifact.Descriptor{
			{MediaType: artifact.JarLayerMediaType, Digest: jarDigest},
		},
	}
	manBytes, _ := json.Marshal(man)
	manDigest := writeContentBlob(t, root, manBytes)

	blobs, err := contentStoreBlobs(root, manDigest)
	if err != nil {
		t.Fatalf("contentStoreBlobs: %v", err)
	}
	if blobs.Config.MainJar != "app.jar" {
		t.Errorf("MainJar = %q, want app.jar", blobs.Config.MainJar)
	}
	if want := contentBlobPath(root, jarDigest); blobs.JarHostPath != want {
		t.Errorf("JarHostPath = %q, want %q", blobs.JarHostPath, want)
	}
	if _, err := os.Stat(blobs.JarHostPath); err != nil {
		t.Errorf("resolved jar path not on disk: %v", err)
	}
}

func TestContentStoreBlobsErrors(t *testing.T) {
	root := t.TempDir()

	if _, err := contentStoreBlobs(root, ""); err == nil {
		t.Error("expected error for empty manifest digest")
	}
	if _, err := contentStoreBlobs(root, "sha256:deadbeef"); err == nil {
		t.Error("expected error for missing manifest blob")
	}

	// A non-Brewlet artifactType must be rejected.
	man := artifact.Manifest{ArtifactType: "application/vnd.oci.image.config.v1+json"}
	manBytes, _ := json.Marshal(man)
	badDigest := writeContentBlob(t, root, manBytes)
	if _, err := contentStoreBlobs(root, badDigest); err == nil {
		t.Error("expected error for non-Brewlet artifactType")
	}
}

func TestLoadArtifactBlobsBackendSelection(t *testing.T) {
	// Layout backend round-trips through the local OCI layout store.
	layoutRoot := t.TempDir()
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 layout-jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := artifact.Store{Root: layoutRoot}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	if _, err := store.Push("demo/hello:1.0.0", cfg, jarPath); err != nil {
		t.Fatal(err)
	}

	got, err := loadArtifactBlobs(imageConfig{StoreRoot: layoutRoot, Ref: "demo/hello:1.0.0"})
	if err != nil {
		t.Fatalf("layout backend: %v", err)
	}
	if got.Config.MainJar != "app.jar" {
		t.Errorf("layout MainJar = %q", got.Config.MainJar)
	}

	// Unknown backend is rejected.
	if _, err := loadArtifactBlobs(imageConfig{Backend: "bogus"}); err == nil {
		t.Error("expected error for unknown backend")
	}
}
