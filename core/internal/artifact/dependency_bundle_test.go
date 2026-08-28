package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDependencyBundleRoundTripAndRunnableReuse(t *testing.T) {
	dir := t.TempDir()
	layerPath := filepath.Join(dir, "spring-web.tar")
	spring := []byte("spring")
	jackson := []byte("jackson")
	writeOrderedTar(t, layerPath, []tarFile{
		{name: "jackson.jar", content: jackson},
		{name: "spring-core.jar", content: spring},
	})
	lock := DependencyLock{
		SchemaVersion: 1,
		Artifacts: []DependencyLockEntry{
			{GroupID: "org.springframework", ArtifactID: "spring-core", Version: "6.2.0", Type: "jar", Scope: "runtime", FileName: "spring-core.jar", SHA256: hexDigest(spring)},
			{GroupID: "com.fasterxml.jackson.core", ArtifactID: "jackson-databind", Version: "2.18.0", Type: "jar", Scope: "runtime", FileName: "jackson.jar", SHA256: hexDigest(jackson)},
		},
	}
	store := Store{Root: filepath.Join(dir, "oci")}
	bundleDesc, err := store.PushDependencyBundle("platform/spring-web:2026.08", DependencyBundleConfig{
		Name:           "spring-web",
		Version:        "2026.08",
		SourceBOM:      "com.example.platform:approved-spring-bom:2026.08",
		CompatibleJDKs: []int{25, 21},
	}, lock, layerPath)
	if err != nil {
		t.Fatalf("PushDependencyBundle: %v", err)
	}
	if bundleDesc.ArtifactType != DependencyBundleArtifactType {
		t.Fatalf("artifactType = %q", bundleDesc.ArtifactType)
	}

	bundle, err := store.ResolveDependencyBundle("platform/spring-web:2026.08")
	if err != nil {
		t.Fatalf("ResolveDependencyBundle: %v", err)
	}
	if bundle.Config.LayerDigest != bundle.Layer.Digest || bundle.Config.LayerDiffID == "" {
		t.Fatalf("bundle layer binding is incomplete: %+v", bundle.Config)
	}
	if got := bundle.Config.CompatibleJDKs; len(got) != 2 || got[0] != 21 || got[1] != 25 {
		t.Fatalf("compatibleJdks = %v", got)
	}

	jarPath := filepath.Join(dir, "orders.jar")
	writeZip(t, jarPath, map[string][]byte{"com/example/Orders.class": {0xca, 0xfe}})
	cfg := JVMConfig{
		SchemaVersion: 1,
		MainJar:       "orders.jar",
		Entry: Entry{
			Mode:      "classpath",
			MainClass: "com.example.Orders",
			ClassPath: []string{"orders.jar", "lib/*"},
		},
	}
	if _, err := store.PushRunnableImageWithOptions("apps/orders:1", cfg, jarPath, nil, nil, "", RunnableImageOptions{ManagedDependency: &bundle}); err != nil {
		t.Fatalf("PushRunnableImageWithOptions: %v", err)
	}
	manifest, _, err := store.ResolveManifestByRef("apps/orders:1")
	if err != nil {
		t.Fatal(err)
	}
	classpath := manifest.RunnableClasspathLayers()
	if len(classpath) != 1 || classpath[0].Digest != bundle.Layer.Digest {
		t.Fatalf("runnable image did not reuse bundle layer: %+v", classpath)
	}
	evidence, ok, err := manifest.ManagedDependencyEvidence()
	if err != nil || !ok {
		t.Fatalf("ManagedDependencyEvidence: ok=%v err=%v", ok, err)
	}
	if !evidence.ThinJar || evidence.DependencyBundleDigest != bundle.ManifestDigest || evidence.DependencyLockDigest != bundle.Config.LockDigest {
		t.Fatalf("managed dependency evidence = %+v", evidence)
	}
}

func TestPushDependencyBundleRejectsLockMismatch(t *testing.T) {
	dir := t.TempDir()
	layerPath := filepath.Join(dir, "deps.tar")
	writeOrderedTar(t, layerPath, []tarFile{{name: "spring.jar", content: []byte("actual")}})
	lock := DependencyLock{
		SchemaVersion: 1,
		Artifacts: []DependencyLockEntry{{
			GroupID: "org.springframework", ArtifactID: "spring-core", Version: "6.2.0",
			Type: "jar", Scope: "runtime", FileName: "spring.jar", SHA256: hexDigest([]byte("different")),
		}},
	}
	_, err := (Store{Root: filepath.Join(dir, "oci")}).PushDependencyBundle("bundle:test", DependencyBundleConfig{
		Name: "test", Version: "1", SourceBOM: "com.example:test-bom:1",
	}, lock, layerPath)
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

func TestPushDependencyBundleRejectsDuplicateLayerEntry(t *testing.T) {
	dir := t.TempDir()
	layerPath := filepath.Join(dir, "deps.tar")
	content := []byte("dependency")
	writeOrderedTar(t, layerPath, []tarFile{
		{name: "dependency.jar", content: content},
		{name: "dependency.jar", content: content},
	})
	lock := DependencyLock{
		SchemaVersion: 1,
		Artifacts: []DependencyLockEntry{{
			GroupID: "com.example", ArtifactID: "dependency", Version: "1",
			Type: "jar", Scope: "runtime", FileName: "dependency.jar", SHA256: hexDigest(content),
		}},
	}
	_, err := (Store{Root: filepath.Join(dir, "oci")}).PushDependencyBundle("bundle:test", DependencyBundleConfig{
		Name: "test", Version: "1", SourceBOM: "com.example:test-bom:1",
	}, lock, layerPath)
	if err == nil {
		t.Fatal("expected duplicate layer entry rejection")
	}
}

func TestResolveDependencyBundleRejectsTamperedBlob(t *testing.T) {
	dir := t.TempDir()
	layerPath := filepath.Join(dir, "deps.tar")
	content := []byte("dependency")
	writeOrderedTar(t, layerPath, []tarFile{{name: "dependency.jar", content: content}})
	lock := DependencyLock{
		SchemaVersion: 1,
		Artifacts: []DependencyLockEntry{{
			GroupID: "com.example", ArtifactID: "dependency", Version: "1",
			Type: "jar", Scope: "runtime", FileName: "dependency.jar", SHA256: hexDigest(content),
		}},
	}
	store := Store{Root: filepath.Join(dir, "oci")}
	if _, err := store.PushDependencyBundle("bundle:test", DependencyBundleConfig{
		Name: "test", Version: "1", SourceBOM: "com.example:test-bom:1",
	}, lock, layerPath); err != nil {
		t.Fatal(err)
	}
	bundle, err := store.ResolveDependencyBundle("bundle:test")
	if err != nil {
		t.Fatal(err)
	}
	layerBlob := filepath.Join(store.blobsDir(), strings.TrimPrefix(bundle.Layer.Digest, "sha256:"))
	raw, err := os.ReadFile(layerBlob)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(layerBlob, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveDependencyBundle("bundle:test"); err == nil {
		t.Fatal("expected tampered blob rejection")
	}
}

func TestVerifyDependencyLockRequiresExactGraph(t *testing.T) {
	entry := DependencyLockEntry{
		GroupID: "com.example", ArtifactID: "dependency", Version: "1",
		Type: "jar", Scope: "runtime", FileName: "dependency.jar",
		SHA256: strings.Repeat("a", 64),
	}
	expected := DependencyLock{SchemaVersion: 1, Artifacts: []DependencyLockEntry{entry}}
	if err := VerifyDependencyLock(expected, expected); err != nil {
		t.Fatalf("matching lock rejected: %v", err)
	}
	actual := expected
	actual.Artifacts = append([]DependencyLockEntry(nil), expected.Artifacts...)
	actual.Artifacts[0].Version = "2"
	if err := VerifyDependencyLock(expected, actual); err == nil {
		t.Fatal("expected graph mismatch")
	}
}

func TestManagedDependencyRequiresClasspathLaunchContract(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "orders.jar")
	writeZip(t, jarPath, map[string][]byte{"com/example/Orders.class": {1}})
	cfg := JVMConfig{
		SchemaVersion: 1,
		MainJar:       "orders.jar",
		Entry:         Entry{Mode: "jar"},
	}
	bundle := ResolvedDependencyBundle{}
	_, err := (Store{Root: filepath.Join(dir, "oci")}).PushRunnableImageWithOptions(
		"apps/orders:1", cfg, jarPath, nil, nil, "", RunnableImageOptions{ManagedDependency: &bundle},
	)
	if err == nil {
		t.Fatal("expected classpath launch contract rejection")
	}
}

func TestValidateThinJar(t *testing.T) {
	dir := t.TempDir()
	thin := filepath.Join(dir, "thin.jar")
	writeZip(t, thin, map[string][]byte{"com/example/Main.class": {1}})
	if err := ValidateThinJar(thin); err != nil {
		t.Fatalf("thin JAR rejected: %v", err)
	}

	fat := filepath.Join(dir, "fat.jar")
	writeZip(t, fat, map[string][]byte{"BOOT-INF/lib/spring.jar": {1}})
	if err := ValidateThinJar(fat); err == nil {
		t.Fatal("expected embedded dependency JAR rejection")
	}
}

type tarFile struct {
	name    string
	content []byte
}

func writeOrderedTar(t *testing.T, path string, files []tarFile) {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, file := range files {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(file.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hexDigest(content []byte) string {
	return string([]byte(digestOf(content))[len("sha256:"):])
}
