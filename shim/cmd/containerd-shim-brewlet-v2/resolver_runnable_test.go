package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brewlet/brewlet/internal/artifact"
)

// buildRunnableStore pushes a runnable OCI image into a local layout and returns
// the store root, the tagged ref, and the index digest (what containerd would
// have in its content store after a pull).
func buildRunnableStore(t *testing.T) (root, ref, indexDigest string) {
	t.Helper()
	root = t.TempDir()
	work := t.TempDir()
	jarPath := filepath.Join(work, "orders.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 orders"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(work, "deps.tar")
	writeTarBlob(t, depsTar, map[string]string{"spring-core.jar": "aaa"})

	cfg := artifact.JVMConfig{
		SchemaVersion: 1,
		MainJar:       "orders.jar",
		Entry:         artifact.Entry{Mode: "classpath", ClassPath: []string{"orders.jar", "lib/*"}, MainClass: "com.acme.Main"},
	}
	store := artifact.Store{Root: root}
	desc, err := store.PushRunnableImage("demo/orders:1", cfg, jarPath, []string{depsTar}, nil, "")
	if err != nil {
		t.Fatalf("PushRunnableImage: %v", err)
	}
	return root, "demo/orders:1", desc.Digest
}

func TestRunnableImageLayoutBackend(t *testing.T) {
	root, ref, _ := buildRunnableStore(t)
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())

	got, err := loadArtifactBlobs(imageConfig{StoreRoot: root, Ref: ref})
	if err != nil {
		t.Fatalf("layout runnable resolve: %v", err)
	}
	assertRunnableBlobs(t, got)
}

func TestRunnableImageContainerdBackend(t *testing.T) {
	// A layout store's blobs/ dir has the same shape as containerd's content
	// store, so pointing the containerd backend at it (by the index digest)
	// mirrors resolving a kubelet-pulled runnable image.
	root, _, indexDigest := buildRunnableStore(t)
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())

	got, err := loadArtifactBlobs(imageConfig{Backend: "containerd", ContentRoot: root, ManifestDigest: indexDigest})
	if err != nil {
		t.Fatalf("containerd runnable resolve: %v", err)
	}
	assertRunnableBlobs(t, got)
}

func assertRunnableBlobs(t *testing.T, got artifactBlobs) {
	t.Helper()
	if got.Config.MainJar != "orders.jar" || got.Config.Entry.MainClass != "com.acme.Main" {
		t.Errorf("launch config not recovered from annotation: %+v", got.Config)
	}
	// The JAR was gunzipped/untarred to a real file named after MainJar.
	if filepath.Base(got.JarHostPath) != "orders.jar" {
		t.Errorf("JarHostPath basename = %q, want orders.jar", filepath.Base(got.JarHostPath))
	}
	b, err := os.ReadFile(got.JarHostPath)
	if err != nil {
		t.Fatalf("staged jar not on disk: %v", err)
	}
	if string(b) != "PK\x03\x04 orders" {
		t.Errorf("staged jar content = %q", string(b))
	}
	// The classpath layer was staged as an uncompressed tar the existing
	// StageClasspathLayers path consumes.
	if len(got.ClasspathHostPaths) != 1 {
		t.Fatalf("classpath tars = %d, want 1", len(got.ClasspathHostPaths))
	}
	if _, err := os.Stat(got.ClasspathHostPaths[0]); err != nil {
		t.Errorf("staged classpath tar missing: %v", err)
	}
}
