package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/brewlet/brewlet/internal/artifact"
)

// TestLayoutBlobsWithCDSLayer verifies the shim resolves the optional AppCDS
// archive's on-disk path from a Brewlet OCI layout.
func TestLayoutBlobsWithCDSLayer(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsaPath := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaPath, []byte("JSA"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := artifact.Store{Root: filepath.Join(dir, "oci")}
	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "app.jar",
		Entry: artifact.Entry{Mode: "jar"},
		CDS:   &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"},
	}
	if _, err := store.PushWithCDS("demo/cds:1.0.0", cfg, jarPath, nil, nil, jsaPath); err != nil {
		t.Fatal(err)
	}

	blobs, err := loadArtifactBlobs(imageConfig{StoreRoot: store.Root, Ref: "demo/cds:1.0.0"})
	if err != nil {
		t.Fatalf("loadArtifactBlobs: %v", err)
	}
	if blobs.CDSHostPath == "" {
		t.Fatal("CDSHostPath empty, want the resolved archive path")
	}
	if _, err := os.Stat(blobs.CDSHostPath); err != nil {
		t.Errorf("resolved cds blob not on disk: %v", err)
	}
	if blobs.Config.CDS == nil || blobs.Config.CDS.Archive != "app.jsa" {
		t.Errorf("resolved config cds = %+v, want archive app.jsa", blobs.Config.CDS)
	}
}

// TestContentStoreBlobsWithCDSLayer verifies the production containerd backend
// also surfaces the AppCDS archive.
func TestContentStoreBlobsWithCDSLayer(t *testing.T) {
	root := t.TempDir()

	jarDigest := writeContentBlob(t, root, []byte("PK\x03\x04 jar"))
	jsaDigest := writeContentBlob(t, root, []byte("JSA-BYTES"))

	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "app.jar",
		Entry: artifact.Entry{Mode: "jar"},
		CDS:   &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"},
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
			{MediaType: artifact.CDSLayerMediaType, Digest: jsaDigest},
		},
	}
	manBytes, _ := json.Marshal(man)
	manDigest := writeContentBlob(t, root, manBytes)

	blobs, err := contentStoreBlobs(root, manDigest)
	if err != nil {
		t.Fatalf("contentStoreBlobs: %v", err)
	}
	if want := contentBlobPath(root, jsaDigest); blobs.CDSHostPath != want {
		t.Errorf("cds path = %q, want %q", blobs.CDSHostPath, want)
	}
}

// TestLayoutBlobsNoCDSLayer confirms CDSHostPath stays empty for an artifact
// without an AppCDS archive.
func TestLayoutBlobsNoCDSLayer(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := artifact.Store{Root: filepath.Join(dir, "oci")}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	if _, err := store.Push("demo/plain:1.0.0", cfg, jarPath); err != nil {
		t.Fatal(err)
	}
	blobs, err := loadArtifactBlobs(imageConfig{StoreRoot: store.Root, Ref: "demo/plain:1.0.0"})
	if err != nil {
		t.Fatalf("loadArtifactBlobs: %v", err)
	}
	if blobs.CDSHostPath != "" {
		t.Errorf("CDSHostPath = %q, want empty for an artifact with no CDS archive", blobs.CDSHostPath)
	}
}
