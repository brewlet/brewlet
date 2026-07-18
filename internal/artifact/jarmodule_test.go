package artifact

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"testing"
)

// A real module-info.class for `module com.acme.orders {}` repackaged with
// `jar --main-class com.acme.orders.Main`, so it carries both the Module and
// ModuleMainClass attributes. Generated once with the JDK and checked in so the
// parser test stays hermetic (no javac needed at test time).
//
//go:embed testdata/module-info.class
var moduleInfoClass []byte

func buildJar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectModuleJarModular(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "orders.jar")
	buildJar(t, jarPath, map[string][]byte{
		"module-info.class":          moduleInfoClass,
		"com/acme/orders/Main.class": []byte("ignored"),
		"META-INF/MANIFEST.MF":       []byte("Manifest-Version: 1.0\n"),
	})

	info, ok, err := InspectModuleJar(jarPath)
	if err != nil {
		t.Fatalf("InspectModuleJar: %v", err)
	}
	if !ok {
		t.Fatal("InspectModuleJar reported non-modular, want modular")
	}
	if info.Name != "com.acme.orders" {
		t.Errorf("module name = %q, want com.acme.orders", info.Name)
	}
	if info.MainClass != "com.acme.orders.Main" {
		t.Errorf("main class = %q, want com.acme.orders.Main", info.MainClass)
	}
}

func TestInspectModuleJarPlainJarNotModular(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "app.jar")
	buildJar(t, jarPath, map[string][]byte{
		"com/acme/App.class":   []byte("ignored"),
		"META-INF/MANIFEST.MF": []byte("Manifest-Version: 1.0\nMain-Class: com.acme.App\n"),
	})

	_, ok, err := InspectModuleJar(jarPath)
	if err != nil {
		t.Fatalf("InspectModuleJar: %v", err)
	}
	if ok {
		t.Error("InspectModuleJar reported modular for a plain JAR, want non-modular")
	}
}
