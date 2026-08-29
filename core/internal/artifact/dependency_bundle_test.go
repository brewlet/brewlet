package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestCanonicalDependencyTarNormalizesMetadataAndOrder(t *testing.T) {
	first := []byte("first")
	second := []byte("second")
	lock := DependencyLock{SchemaVersion: 1, Artifacts: []DependencyLockEntry{
		{
			GroupID: "com.example", ArtifactID: "second", Version: "1",
			Type: "jar", Scope: "runtime", FileName: "b.jar", SHA256: hexDigest(second),
		},
		{
			GroupID: "com.example", ArtifactID: "first", Version: "1",
			Type: "jar", Scope: "runtime", FileName: "a.jar", SHA256: hexDigest(first),
		},
	}}
	source := func(files []tarFile, mode int64, modTime time.Time) []byte {
		t.Helper()
		var buf bytes.Buffer
		writer := tar.NewWriter(&buf)
		for _, file := range files {
			header := &tar.Header{
				Name: file.name, Mode: mode, Size: int64(len(file.content)),
				Uid: 501, Gid: 20, ModTime: modTime, Typeflag: tar.TypeReg,
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(file.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	left, err := canonicalDependencyTar(source([]tarFile{
		{name: "b.jar", content: second}, {name: "a.jar", content: first},
	}, 0o755, time.Now()), lock)
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalDependencyTar(source([]tarFile{
		{name: "a.jar", content: first}, {name: "b.jar", content: second},
	}, 0o600, time.Unix(123456, 0)), lock)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(left, right) {
		t.Fatal("canonical tar depends on source ordering or metadata")
	}

	reader := tar.NewReader(bytes.NewReader(left))
	var names []string
	for {
		header, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.Mode != 0o644 || header.Uid != 0 || header.Gid != 0 ||
			!header.ModTime.Equal(time.Unix(0, 0)) || header.Format != tar.FormatUSTAR {
			t.Fatalf("non-canonical tar header: %+v", header)
		}
	}
	if !reflect.DeepEqual(names, []string{"a.jar", "b.jar"}) {
		t.Fatalf("entry order = %v", names)
	}
}

func TestDependencyLockUsesNormativeCoordinateOrder(t *testing.T) {
	raw := []byte(`{
	  "schemaVersion": 1,
	  "artifacts": [
	    {
	      "groupId": "com.example", "artifactId": "library", "version": "10",
	      "type": "jar", "scope": "runtime", "fileName": "a.jar",
	      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	    },
	    {
	      "groupId": "com.example", "artifactId": "library", "version": "1",
	      "type": "jar", "scope": "runtime", "fileName": "z.jar",
	      "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	    }
	  ]
	}`)
	lock, err := DecodeDependencyLock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Artifacts[0].Version; got != "1" {
		t.Fatalf("first version = %q, want normative lexical order to put 1 first", got)
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

func TestResolveDependencyBundleRejectsNonContractManifest(t *testing.T) {
	tests := map[string]func(map[string]any){
		"unknown field": func(manifest map[string]any) {
			manifest["unexpected"] = true
		},
		"schema version": func(manifest map[string]any) {
			manifest["schemaVersion"] = float64(3)
		},
		"media type": func(manifest map[string]any) {
			manifest["mediaType"] = "application/json"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			layerPath := filepath.Join(dir, "deps.tar")
			content := []byte("dependency")
			writeOrderedTar(t, layerPath, []tarFile{{name: "dependency.jar", content: content}})
			lock := DependencyLock{SchemaVersion: 1, Artifacts: []DependencyLockEntry{{
				GroupID: "com.example", ArtifactID: "dependency", Version: "1",
				Type: "jar", Scope: "runtime", FileName: "dependency.jar", SHA256: hexDigest(content),
			}}}
			store := Store{Root: filepath.Join(dir, "oci")}
			if _, err := store.PushDependencyBundle("bundle:test", DependencyBundleConfig{
				Name: "test", Version: "1", SourceBOM: "com.example:test-bom:1",
			}, lock, layerPath); err != nil {
				t.Fatal(err)
			}
			subject, err := store.DescriptorByRef("bundle:test")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := store.ReadBlob(subject.Digest)
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			mutate(manifest)
			raw, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			replacement, err := store.writeBlob(raw)
			if err != nil {
				t.Fatal(err)
			}
			replacement.MediaType = subject.MediaType
			replacement.ArtifactType = subject.ArtifactType
			replacement.Annotations = subject.Annotations
			index, err := store.readIndex()
			if err != nil {
				t.Fatal(err)
			}
			for i := range index.Manifests {
				if index.Manifests[i].Digest == subject.Digest {
					index.Manifests[i] = replacement
				}
			}
			indexRaw, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.indexPath(), indexRaw, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ResolveDependencyBundle("bundle:test"); err == nil {
				t.Fatal("expected non-contract manifest rejection")
			}
		})
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
	actual.Artifacts[0] = entry
	actual.Artifacts[0].Scope = "compile"
	if err := VerifyDependencyLock(expected, actual); err == nil {
		t.Fatal("expected scope-only graph mismatch")
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
