package artifact

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeTar creates a plain tar at path containing the given name->content files.
func writeTar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
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

func TestPushWithLayersAndResolve(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 thin-app"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	snapTar := filepath.Join(dir, "snapshot-deps.tar")
	writeTar(t, depsTar, map[string]string{"spring-core.jar": "aaa", "jackson.jar": "bbb"})
	writeTar(t, snapTar, map[string]string{"internal-lib.jar": "ccc"})

	cfg := JVMConfig{
		SchemaVersion: 1,
		MainJar:       "app.jar",
		Entry:         Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}, MainClass: "com.acme.Main"},
	}

	s := Store{Root: filepath.Join(dir, "oci")}
	if _, err := s.PushWithLayers("demo/layered:1.0.0", cfg, jarPath, []string{depsTar, snapTar}); err != nil {
		t.Fatalf("PushWithLayers: %v", err)
	}

	man, got, err := s.Resolve("demo/layered:1.0.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Config round-trips including the new classPath.
	if got.Entry.Mode != "classpath" {
		t.Errorf("entry.mode = %q, want classpath", got.Entry.Mode)
	}
	if want := []string{"app.jar", "lib/*"}; len(got.Entry.ClassPath) != 2 || got.Entry.ClassPath[0] != want[0] || got.Entry.ClassPath[1] != want[1] {
		t.Errorf("entry.classPath = %v, want %v", got.Entry.ClassPath, want)
	}

	// Manifest has exactly one JAR layer + two classpath layers, in order.
	if _, err := man.JarLayer(); err != nil {
		t.Errorf("JarLayer: %v", err)
	}
	cps := man.ClasspathLayers()
	if len(cps) != 2 {
		t.Fatalf("ClasspathLayers len = %d, want 2", len(cps))
	}
	for i, l := range cps {
		if l.MediaType != ClasspathLayerMediaType {
			t.Errorf("layer[%d] mediaType = %q, want %q", i, l.MediaType, ClasspathLayerMediaType)
		}
		if _, err := s.ReadBlob(l.Digest); err != nil {
			t.Errorf("classpath blob %s not readable: %v", l.Digest, err)
		}
	}
	// Order is preserved: deps first, snapshot-deps second (by title annotation).
	if cps[0].Annotations["org.opencontainers.image.title"] != "deps.tar" {
		t.Errorf("first classpath layer title = %q, want deps.tar", cps[0].Annotations["org.opencontainers.image.title"])
	}
}

func TestPushNoClasspathLayersHasNone(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: filepath.Join(dir, "oci")}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}}
	if _, err := s.Push("demo/plain:1.0.0", cfg, jarPath); err != nil {
		t.Fatal(err)
	}
	man, _, err := s.Resolve("demo/plain:1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := man.ClasspathLayers(); len(got) != 0 {
		t.Errorf("ClasspathLayers = %v, want empty for a plain fat JAR", got)
	}
}

func TestPushWithModulepathLayersAndResolve(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 modular-app"), 0o644); err != nil {
		t.Fatal(err)
	}
	modsTar := filepath.Join(dir, "mods.tar")
	writeTar(t, modsTar, map[string]string{"guava.jar": "aaa", "slf4j.jar": "bbb"})

	cfg := JVMConfig{
		SchemaVersion: 1,
		MainJar:       "orders.jar",
		Entry:         Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}},
	}

	s := Store{Root: filepath.Join(dir, "oci")}
	if _, err := s.PushWithTypedLayers("demo/modular:1.0.0", cfg, jarPath, nil, []string{modsTar}); err != nil {
		t.Fatalf("PushWithTypedLayers: %v", err)
	}

	man, got, err := s.Resolve("demo/modular:1.0.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.Entry.Mode != "module" {
		t.Errorf("entry.mode = %q, want module", got.Entry.Mode)
	}
	if got.Entry.Module != "com.acme.orders" {
		t.Errorf("entry.module = %q, want com.acme.orders", got.Entry.Module)
	}
	if want := []string{"orders.jar", "mods"}; len(got.Entry.ModulePath) != 2 || got.Entry.ModulePath[0] != want[0] || got.Entry.ModulePath[1] != want[1] {
		t.Errorf("entry.modulePath = %v, want %v", got.Entry.ModulePath, want)
	}

	// Manifest carries the module layer, not a classpath layer.
	if got := man.ClasspathLayers(); len(got) != 0 {
		t.Errorf("ClasspathLayers = %v, want empty for a modular app", got)
	}
	mps := man.ModulepathLayers()
	if len(mps) != 1 {
		t.Fatalf("ModulepathLayers len = %d, want 1", len(mps))
	}
	if mps[0].MediaType != ModulepathLayerMediaType {
		t.Errorf("layer mediaType = %q, want %q", mps[0].MediaType, ModulepathLayerMediaType)
	}
	if _, err := s.ReadBlob(mps[0].Digest); err != nil {
		t.Errorf("modulepath blob %s not readable: %v", mps[0].Digest, err)
	}
	if mps[0].Annotations["org.opencontainers.image.title"] != "mods.tar" {
		t.Errorf("module layer title = %q, want mods.tar", mps[0].Annotations["org.opencontainers.image.title"])
	}
}

func TestEntryClassPathOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Entry{Mode: "jar"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("classPath")) {
		t.Errorf("empty classPath should be omitted, got %s", b)
	}
	b, _ = json.Marshal(Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}})
	if !bytes.Contains(b, []byte(`"classPath":["app.jar","lib/*"]`)) {
		t.Errorf("classPath not serialized as expected: %s", b)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     JVMConfig
		wantErr bool
	}{
		{"jar default", JVMConfig{Entry: Entry{Mode: "jar"}}, false},
		{"empty mode is jar", JVMConfig{Entry: Entry{}}, false},
		{"classpath with mainClass", JVMConfig{Entry: Entry{Mode: "classpath", MainClass: "M"}}, false},
		{"classpath missing mainClass", JVMConfig{Entry: Entry{Mode: "classpath"}}, true},
		{"jar with mainClass", JVMConfig{Entry: Entry{Mode: "jar", MainClass: "M"}}, true},
		{"jar with classPath", JVMConfig{Entry: Entry{Mode: "jar", ClassPath: []string{"lib/*"}}}, true},
		{"module with module name", JVMConfig{Entry: Entry{Mode: "module", Module: "com.acme.orders"}}, false},
		{"module with mainClass", JVMConfig{Entry: Entry{Mode: "module", Module: "com.acme.orders", MainClass: "M"}}, false},
		{"module with modulePath", JVMConfig{Entry: Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}}}, false},
		{"module missing module name", JVMConfig{Entry: Entry{Mode: "module"}}, true},
		{"module with classPath (mixed form)", JVMConfig{Entry: Entry{Mode: "module", Module: "com.acme.orders", ClassPath: []string{"lib/*"}}}, false},
		{"module with modulePath and classPath (mixed form)", JVMConfig{Entry: Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}, ClassPath: []string{"lib/*"}}}, false},
		{"jar with module", JVMConfig{Entry: Entry{Mode: "jar", Module: "com.acme.orders"}}, true},
		{"classpath with module", JVMConfig{Entry: Entry{Mode: "classpath", MainClass: "M", Module: "com.acme.orders"}}, true},
		{"unknown mode", JVMConfig{Entry: Entry{Mode: "bogus"}}, true},
		// Top-level JAR reference cross-check (only when MainJar is set).
		{"classpath refs matching mainJar", JVMConfig{MainJar: "app.jar", Entry: Entry{Mode: "classpath", MainClass: "M", ClassPath: []string{"app.jar", "lib/*"}}}, false},
		{"classpath refs mismatched top-level jar", JVMConfig{MainJar: "orders.jar", Entry: Entry{Mode: "classpath", MainClass: "M", ClassPath: []string{"app.jar", "lib/*"}}}, true},
		{"module refs matching mainJar", JVMConfig{MainJar: "orders.jar", Entry: Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}}}, false},
		{"module refs mismatched top-level jar", JVMConfig{MainJar: "orders.jar", Entry: Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"app.jar", "mods"}}}, true},
		{"mixed refs mismatched top-level jar", JVMConfig{MainJar: "orders.jar", Entry: Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}, ClassPath: []string{"legacy.jar"}}}, true},
		{"nested jar under lib is not a top-level ref", JVMConfig{MainJar: "orders.jar", Entry: Entry{Mode: "classpath", MainClass: "M", ClassPath: []string{"orders.jar", "lib/legacy.jar"}}}, false},
		{"mismatch ignored when mainJar unset", JVMConfig{Entry: Entry{Mode: "classpath", MainClass: "M", ClassPath: []string{"orders.jar", "lib/*"}}}, false},
		// Optional arch constraint (non-portable artifacts).
		{"arch unset is arch-neutral", JVMConfig{Entry: Entry{Mode: "jar"}}, false},
		{"arch amd64", JVMConfig{Entry: Entry{Mode: "jar"}, Arch: []string{"amd64"}}, false},
		{"arch amd64+arm64", JVMConfig{Entry: Entry{Mode: "jar"}, Arch: []string{"amd64", "arm64"}}, false},
		{"arch unknown token", JVMConfig{Entry: Entry{Mode: "jar"}, Arch: []string{"riscv64"}}, true},
		{"arch duplicate", JVMConfig{Entry: Entry{Mode: "jar"}, Arch: []string{"amd64", "amd64"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDecodeConfigRejectsUnknownField(t *testing.T) {
	// A stray field foreign to the schema must be rejected, not silently dropped.
	b := []byte(`{"schemaVersion":1,"mainJar":"app.jar","entry":{"mode":"classpath","mainClass":"M","bogusField":"x"}}`)
	if _, err := DecodeConfig(b); err == nil {
		t.Error("DecodeConfig accepted an unknown field, want error")
	}
}

func TestDecodeConfigModuleRoundTrip(t *testing.T) {
	b := []byte(`{"schemaVersion":1,"mainJar":"orders.jar","entry":{"mode":"module","module":"com.acme.orders","modulePath":["orders.jar","mods"]}}`)
	cfg, err := DecodeConfig(b)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if cfg.Entry.Mode != "module" {
		t.Errorf("mode = %q, want module", cfg.Entry.Mode)
	}
	if cfg.Entry.Module != "com.acme.orders" {
		t.Errorf("module = %q, want com.acme.orders", cfg.Entry.Module)
	}
	if got := cfg.Entry.ModulePath; len(got) != 2 || got[0] != "orders.jar" || got[1] != "mods" {
		t.Errorf("modulePath = %v, want [orders.jar mods]", got)
	}
}

func TestDecodeConfigArchRoundTrip(t *testing.T) {
	b := []byte(`{"schemaVersion":1,"mainJar":"app.jar","entry":{"mode":"jar"},"arch":["amd64"]}`)
	cfg, err := DecodeConfig(b)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	if got := cfg.Arch; len(got) != 1 || got[0] != "amd64" {
		t.Errorf("arch = %v, want [amd64]", got)
	}
}

func TestDecodeConfigRejectsUnknownArch(t *testing.T) {
	b := []byte(`{"schemaVersion":1,"mainJar":"app.jar","entry":{"mode":"jar"},"arch":["riscv64"]}`)
	if _, err := DecodeConfig(b); err == nil {
		t.Error("DecodeConfig accepted an unknown arch token, want validation error")
	}
}

func TestEntryModulePathOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Entry{Mode: "module", Module: "com.acme.orders"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("modulePath")) {
		t.Errorf("empty modulePath should be omitted, got %s", b)
	}
	if !bytes.Contains(b, []byte(`"module":"com.acme.orders"`)) {
		t.Errorf("module not serialized as expected: %s", b)
	}
}

func TestDecodeConfigRejectsInconsistentConfig(t *testing.T) {
	b := []byte(`{"schemaVersion":1,"mainJar":"app.jar","entry":{"mode":"jar","mainClass":"M"}}`)
	if _, err := DecodeConfig(b); err == nil {
		t.Error("DecodeConfig accepted mode=jar with mainClass, want validation error")
	}
}

func TestValidateCDS(t *testing.T) {
	base := func(cds *CDS) JVMConfig {
		return JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}, CDS: cds}
	}
	cases := []struct {
		name    string
		cds     *CDS
		wantErr bool
	}{
		{"nil ok", nil, false},
		{"bare filename dynamic", &CDS{Archive: "app.jsa", Mode: "dynamic"}, false},
		{"bare filename static", &CDS{Archive: "app.jsa", Mode: "static"}, false},
		{"bare filename no mode", &CDS{Archive: "app.jsa"}, false},
		{"empty archive", &CDS{Archive: ""}, true},
		{"whitespace archive", &CDS{Archive: "   "}, true},
		{"path separator", &CDS{Archive: "sub/app.jsa"}, true},
		{"parent ref", &CDS{Archive: ".."}, true},
		{"wildcard", &CDS{Archive: "*.jsa"}, true},
		{"unknown mode", &CDS{Archive: "app.jsa", Mode: "auto"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.cds).Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestPushWithCDSAndResolve(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsaPath := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaPath, []byte("JSA-ARCHIVE-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := JVMConfig{
		SchemaVersion: 1,
		MainJar:       "app.jar",
		Entry:         Entry{Mode: "jar"},
		CDS:           &CDS{Archive: "app.jsa", Mode: "dynamic"},
	}
	s := Store{Root: filepath.Join(dir, "oci")}
	if _, err := s.PushWithCDS("demo/cds:1.0.0", cfg, jarPath, nil, nil, jsaPath); err != nil {
		t.Fatalf("PushWithCDS: %v", err)
	}
	man, got, err := s.Resolve("demo/cds:1.0.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.CDS == nil || got.CDS.Archive != "app.jsa" || got.CDS.Mode != "dynamic" {
		t.Errorf("cfg.CDS round-trip = %+v, want {app.jsa dynamic}", got.CDS)
	}
	l, ok := man.CDSLayer()
	if !ok {
		t.Fatal("CDSLayer() not found, want present")
	}
	if l.MediaType != CDSLayerMediaType {
		t.Errorf("cds layer mediaType = %q, want %q", l.MediaType, CDSLayerMediaType)
	}
	if l.Annotations["org.opencontainers.image.title"] != "app.jsa" {
		t.Errorf("cds layer title = %q, want app.jsa", l.Annotations["org.opencontainers.image.title"])
	}
	if b, err := s.ReadBlob(l.Digest); err != nil || string(b) != "JSA-ARCHIVE-BYTES" {
		t.Errorf("cds blob content = %q (err %v), want the archive bytes", b, err)
	}
}

func TestPushNoCDSLayerWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: filepath.Join(dir, "oci")}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}}
	if _, err := s.Push("demo/plain:1.0.0", cfg, jarPath); err != nil {
		t.Fatal(err)
	}
	man, _, err := s.Resolve("demo/plain:1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := man.CDSLayer(); ok {
		t.Error("CDSLayer() present, want absent for an artifact with no CDS archive")
	}
}

func TestPushCDSPairingErrors(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsaPath := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaPath, []byte("JSA"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: filepath.Join(dir, "oci")}
	jarCfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}}

	// Config declares cds.archive but no archive file is shipped.
	cfgWithCDS := jarCfg
	cfgWithCDS.CDS = &CDS{Archive: "app.jsa"}
	if _, err := s.PushWithCDS("demo/a:1", cfgWithCDS, jarPath, nil, nil, ""); err == nil {
		t.Error("PushWithCDS accepted cds.archive with no shipped archive, want error")
	}

	// Archive file shipped but config has no cds hint.
	if _, err := s.PushWithCDS("demo/b:1", jarCfg, jarPath, nil, nil, jsaPath); err == nil {
		t.Error("PushWithCDS accepted a shipped archive with no cds hint, want error")
	}

	// Basename mismatch between file and cds.archive.
	mismatch := jarCfg
	mismatch.CDS = &CDS{Archive: "other.jsa"}
	if _, err := s.PushWithCDS("demo/c:1", mismatch, jarPath, nil, nil, jsaPath); err == nil {
		t.Error("PushWithCDS accepted mismatched archive basename, want error")
	}
}

func TestPushRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: filepath.Join(dir, "oci")}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "classpath"}}
	if _, err := s.PushWithLayers("demo/bad:1.0.0", cfg, jarPath, nil); err == nil {
		t.Error("PushWithLayers accepted an invalid config, want error")
	}
}
