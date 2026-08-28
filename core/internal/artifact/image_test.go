package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// gunzipTar decompresses a tar+gzip blob and returns its entry name->content.
func gunzipTar(t *testing.T, gz []byte) map[string]string {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gr.Close()
	out := map[string]string{}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry: %v", err)
		}
		out[hdr.Name] = string(b)
	}
	return out
}

func TestPushRunnableImageIsKubeletPullable(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 orders-fat-jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	writeTar(t, depsTar, map[string]string{"spring-core.jar": "aaa", "jackson.jar": "bbb"})

	cfg := JVMConfig{
		SchemaVersion: 1,
		MainJar:       "orders.jar",
		Entry:         Entry{Mode: "classpath", ClassPath: []string{"orders.jar", "lib/*"}, MainClass: "com.acme.Main"},
		SystemProperties: map[string]string{
			"spring.aot.enabled": "true",
		},
	}

	s := Store{Root: filepath.Join(dir, "oci")}
	idxDesc, err := s.PushRunnableImage("demo/orders:1.0.0", cfg, jarPath, []string{depsTar}, nil, "")
	if err != nil {
		t.Fatalf("PushRunnableImage: %v", err)
	}
	if idxDesc.MediaType != OCIImageIndexMediaType {
		t.Errorf("tagged descriptor mediaType = %q, want %q", idxDesc.MediaType, OCIImageIndexMediaType)
	}

	// The tagged object is a multi-arch image index (portable JAR -> amd64+arm64).
	idxRaw, err := s.ReadBlob(idxDesc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !IsIndexBlob(idxRaw) {
		t.Fatalf("tagged blob is not an image index")
	}
	var idx Index
	if err := json.Unmarshal(idxRaw, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 2 {
		t.Fatalf("index manifests = %d, want 2 (amd64, arm64)", len(idx.Manifests))
	}
	seen := map[string]bool{}
	for _, m := range idx.Manifests {
		if m.Platform == nil {
			t.Fatalf("index manifest %s has no platform", m.Digest)
		}
		if m.Platform.OS != "linux" {
			t.Errorf("platform os = %q, want linux", m.Platform.OS)
		}
		seen[m.Platform.Architecture] = true
	}
	if !seen["amd64"] || !seen["arm64"] {
		t.Errorf("index arches = %v, want amd64+arm64", seen)
	}

	// Resolve to a platform manifest and confirm it is a runnable image whose
	// launch config round-trips from the annotation.
	man, _, err := s.ResolveManifestByRef("demo/orders:1.0.0")
	if err != nil {
		t.Fatalf("ResolveManifestByRef: %v", err)
	}
	if !man.IsRunnableImage() {
		t.Fatalf("resolved manifest is not a runnable image")
	}
	if man.Config.MediaType != OCIImageConfigMediaType {
		t.Errorf("config mediaType = %q, want standard OCI image config %q", man.Config.MediaType, OCIImageConfigMediaType)
	}
	got, err := man.RunnableConfig()
	if err != nil {
		t.Fatalf("RunnableConfig: %v", err)
	}
	if got.MainJar != "orders.jar" || got.Entry.MainClass != "com.acme.Main" {
		t.Errorf("launch config did not round-trip: %+v", got)
	}
	if got.SystemProperties["spring.aot.enabled"] != "true" {
		t.Errorf("system properties lost: %+v", got.SystemProperties)
	}

	// Every layer is a STANDARD tar+gzip layer (so containerd/kubelet unpack it).
	for i, l := range man.Layers {
		if l.MediaType != OCILayerGzipMediaType {
			t.Errorf("layer[%d] mediaType = %q, want %q", i, l.MediaType, OCILayerGzipMediaType)
		}
		if l.Annotations[LayerRoleAnnotation] == "" {
			t.Errorf("layer[%d] missing %s annotation", i, LayerRoleAnnotation)
		}
	}

	// The image config's rootfs.diff_ids must equal the UNCOMPRESSED layer
	// digests, or containerd rejects the image on unpack.
	cfgRaw, err := s.ReadBlob(man.Config.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var imgCfg ociImageConfig
	if err := json.Unmarshal(cfgRaw, &imgCfg); err != nil {
		t.Fatal(err)
	}
	if len(imgCfg.RootFS.DiffIDs) != len(man.Layers) {
		t.Fatalf("diff_ids = %d, want %d (one per layer)", len(imgCfg.RootFS.DiffIDs), len(man.Layers))
	}
	if len(imgCfg.Config.Entrypoint) != 1 || imgCfg.Config.Entrypoint[0] != "/brewlet" {
		t.Fatalf("entrypoint = %v, want [/brewlet] placeholder for CRI", imgCfg.Config.Entrypoint)
	}
	for i, l := range man.Layers {
		gz, err := s.ReadBlob(l.Digest)
		if err != nil {
			t.Fatal(err)
		}
		gr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(gr)
		if err != nil {
			t.Fatal(err)
		}
		gr.Close()
		if want := digestOf(raw); imgCfg.RootFS.DiffIDs[i] != want {
			t.Errorf("layer[%d] diff_id = %s, want uncompressed digest %s", i, imgCfg.RootFS.DiffIDs[i], want)
		}
	}

	// The app layer carries the JAR (flat, named MainJar); the classpath layer
	// carries the dependency JARs.
	appLayer, err := man.RunnableAppLayer()
	if err != nil {
		t.Fatalf("RunnableAppLayer: %v", err)
	}
	appGz, _ := s.ReadBlob(appLayer.Digest)
	appFiles := gunzipTar(t, appGz)
	if appFiles["orders.jar"] != "PK\x03\x04 orders-fat-jar" {
		t.Errorf("app layer jar content = %q", appFiles["orders.jar"])
	}
	cps := man.RunnableClasspathLayers()
	if len(cps) != 1 {
		t.Fatalf("classpath layers = %d, want 1", len(cps))
	}
	cpGz, _ := s.ReadBlob(cps[0].Digest)
	cpFiles := gunzipTar(t, cpGz)
	if cpFiles["spring-core.jar"] != "aaa" || cpFiles["jackson.jar"] != "bbb" {
		t.Errorf("classpath layer files = %v", cpFiles)
	}
}

func TestPushRunnableImageNonPortableArch(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 native"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}, Arch: []string{"arm64"}}
	s := Store{Root: filepath.Join(dir, "oci")}
	if _, err := s.PushRunnableImage("demo/native:1", cfg, jarPath, nil, nil, ""); err != nil {
		t.Fatalf("PushRunnableImage: %v", err)
	}
	idxRaw, _ := os.ReadFile(filepath.Join(s.Root, "index.json"))
	var top Index
	if err := json.Unmarshal(idxRaw, &top); err != nil {
		t.Fatal(err)
	}
	// index.json tags the runnable index; read it and confirm a single arm64 arch.
	idxBlob, err := s.ReadBlob(top.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	var idx Index
	json.Unmarshal(idxBlob, &idx)
	if len(idx.Manifests) != 1 || idx.Manifests[0].Platform.Architecture != "arm64" {
		t.Errorf("non-portable image arches = %+v, want [arm64]", idx.Manifests)
	}
}
