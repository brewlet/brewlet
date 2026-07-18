package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/brewlet/brewlet/internal/artifact"
)

func writeTarBlob(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLayoutBlobsWithClasspathLayers verifies the shim resolves the optional
// dependency layers' on-disk paths from a Brewlet OCI layout.
func TestLayoutBlobsWithClasspathLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	writeTarBlob(t, depsTar, map[string]string{"spring-core.jar": "x"})

	store := artifact.Store{Root: filepath.Join(dir, "oci")}
	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "app.jar",
		Entry: artifact.Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}, MainClass: "com.acme.Main"},
	}
	if _, err := store.PushWithLayers("demo/layered:1.0.0", cfg, jarPath, []string{depsTar}); err != nil {
		t.Fatal(err)
	}

	blobs, err := loadArtifactBlobs(imageConfig{StoreRoot: store.Root, Ref: "demo/layered:1.0.0"})
	if err != nil {
		t.Fatalf("loadArtifactBlobs: %v", err)
	}
	if len(blobs.ClasspathHostPaths) != 1 {
		t.Fatalf("ClasspathHostPaths len = %d, want 1", len(blobs.ClasspathHostPaths))
	}
	if _, err := os.Stat(blobs.ClasspathHostPaths[0]); err != nil {
		t.Errorf("resolved classpath blob not on disk: %v", err)
	}
}

// TestContentStoreBlobsWithClasspathLayers verifies the production containerd
// backend also surfaces the dependency layers.
func TestContentStoreBlobsWithClasspathLayers(t *testing.T) {
	root := t.TempDir()

	jarDigest := writeContentBlob(t, root, []byte("PK\x03\x04 jar"))
	depsBytes := []byte("fake-deps-tar")
	depsDigest := writeContentBlob(t, root, depsBytes)

	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "app.jar",
		Entry: artifact.Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}, MainClass: "com.acme.Main"},
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
			{MediaType: artifact.ClasspathLayerMediaType, Digest: depsDigest},
		},
	}
	manBytes, _ := json.Marshal(man)
	manDigest := writeContentBlob(t, root, manBytes)

	blobs, err := contentStoreBlobs(root, manDigest)
	if err != nil {
		t.Fatalf("contentStoreBlobs: %v", err)
	}
	if len(blobs.ClasspathHostPaths) != 1 {
		t.Fatalf("ClasspathHostPaths len = %d, want 1", len(blobs.ClasspathHostPaths))
	}
	if want := contentBlobPath(root, depsDigest); blobs.ClasspathHostPaths[0] != want {
		t.Errorf("classpath path = %q, want %q", blobs.ClasspathHostPaths[0], want)
	}
}

// TestLayoutBlobsWithModulepathLayers verifies the shim resolves the optional
// module layers' on-disk paths from a Brewlet OCI layout.
func TestLayoutBlobsWithModulepathLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	modsTar := filepath.Join(dir, "mods.tar")
	writeTarBlob(t, modsTar, map[string]string{"guava.jar": "x"})

	store := artifact.Store{Root: filepath.Join(dir, "oci")}
	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "orders.jar",
		Entry: artifact.Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}},
	}
	if _, err := store.PushWithTypedLayers("demo/modular:1.0.0", cfg, jarPath, nil, []string{modsTar}); err != nil {
		t.Fatal(err)
	}

	blobs, err := loadArtifactBlobs(imageConfig{StoreRoot: store.Root, Ref: "demo/modular:1.0.0"})
	if err != nil {
		t.Fatalf("loadArtifactBlobs: %v", err)
	}
	if len(blobs.ClasspathHostPaths) != 0 {
		t.Errorf("ClasspathHostPaths len = %d, want 0", len(blobs.ClasspathHostPaths))
	}
	if len(blobs.ModulepathHostPaths) != 1 {
		t.Fatalf("ModulepathHostPaths len = %d, want 1", len(blobs.ModulepathHostPaths))
	}
	if _, err := os.Stat(blobs.ModulepathHostPaths[0]); err != nil {
		t.Errorf("resolved modulepath blob not on disk: %v", err)
	}
}

// TestContentStoreBlobsWithModulepathLayers verifies the production containerd
// backend also surfaces the module layers.
func TestContentStoreBlobsWithModulepathLayers(t *testing.T) {
	root := t.TempDir()

	jarDigest := writeContentBlob(t, root, []byte("PK\x03\x04 jar"))
	modsBytes := []byte("fake-mods-tar")
	modsDigest := writeContentBlob(t, root, modsBytes)

	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "orders.jar",
		Entry: artifact.Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}},
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
			{MediaType: artifact.ModulepathLayerMediaType, Digest: modsDigest},
		},
	}
	manBytes, _ := json.Marshal(man)
	manDigest := writeContentBlob(t, root, manBytes)

	blobs, err := contentStoreBlobs(root, manDigest)
	if err != nil {
		t.Fatalf("contentStoreBlobs: %v", err)
	}
	if len(blobs.ClasspathHostPaths) != 0 {
		t.Errorf("ClasspathHostPaths len = %d, want 0", len(blobs.ClasspathHostPaths))
	}
	if len(blobs.ModulepathHostPaths) != 1 {
		t.Fatalf("ModulepathHostPaths len = %d, want 1", len(blobs.ModulepathHostPaths))
	}
	if want := contentBlobPath(root, modsDigest); blobs.ModulepathHostPaths[0] != want {
		t.Errorf("modulepath path = %q, want %q", blobs.ModulepathHostPaths[0], want)
	}
}
