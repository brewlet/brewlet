package runtime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brewlet/brewlet/internal/artifact"
)

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

func TestBuildJVMArgsJarMode(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "-jar /app/app.jar" {
		t.Errorf("args = %q, want -jar /app/app.jar", got)
	}
}

func TestBuildJVMArgsStructuredLaunchKnobs(t *testing.T) {
	cfg := artifact.JVMConfig{
		Entry:         artifact.Entry{Mode: "jar"},
		EnablePreview: true,
		AddModules:    []string{"jdk.incubator.vector", "java.se"},
		AddOpens:      []string{"java.base/java.lang=ALL-UNNAMED"},
		AddExports:    []string{"java.base/sun.nio.ch=ALL-UNNAMED"},
		// Deliberately out of lexical order to prove deterministic sorting.
		SystemProperties: map[string]string{"z.prop": "2", "a.prop": "1"},
	}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", []string{"-Xmx512m"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "--enable-preview " +
		"--add-modules jdk.incubator.vector,java.se " +
		"--add-opens java.base/java.lang=ALL-UNNAMED " +
		"--add-exports java.base/sun.nio.ch=ALL-UNNAMED " +
		"-Da.prop=1 -Dz.prop=2 " + // sorted by key
		"-Xmx512m " + // extraArgs (deployment tuning) after artifact flags
		"-jar /app/app.jar" // entry always last
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args =\n  %q\nwant\n  %q", got, want)
	}
}

func TestBuildJVMArgsCDS(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "-Xshare:auto -XX:SharedArchiveFile=/app/app.jsa -jar /app/app.jar"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestBuildJVMArgsCDSArchivePathTracksJar(t *testing.T) {
	// The archive is resolved beside the JAR, so a non-default main JAR path
	// still yields /<dir>/<archive>.
	cfg := artifact.JVMConfig{MainJar: "orders.jar", Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "orders.jsa"}}
	args, err := BuildJVMArgs(cfg, "/app/orders.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "-XX:SharedArchiveFile=/app/orders.jsa") {
		t.Errorf("args = %q, want -XX:SharedArchiveFile=/app/orders.jsa", got)
	}
}

func TestBuildJVMArgsNoCDS(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); strings.Contains(got, "SharedArchiveFile") || strings.Contains(got, "Xshare") {
		t.Errorf("args = %q, want no CDS flags when cds is unset", got)
	}
}

func TestGenerateBundleWithCDS(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := filepath.Join(dir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	jarHost := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsaHost := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaHost, []byte("JSA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}}
	out := filepath.Join(dir, "bundle")
	if err := GenerateBundleWithCDS(cfg, jdkRoot, "", "", jarHost, nil, nil, jsaHost, out, Resources{}, nil); err != nil {
		t.Fatalf("GenerateBundleWithCDS: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := string(b)
	if !strings.Contains(cfgJSON, `"/app/app.jsa"`) {
		t.Errorf("config.json missing /app/app.jsa mount:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, "-XX:SharedArchiveFile=/app/app.jsa") {
		t.Errorf("config.json missing -XX:SharedArchiveFile argv:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, "-Xshare:auto") {
		t.Errorf("config.json missing -Xshare:auto argv:\n%s", cfgJSON)
	}
}

func TestAssembleSandboxWithCDS(t *testing.T) {
	dir := t.TempDir()
	jarSrc := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarSrc, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsaSrc := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(jsaSrc, []byte("JSA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "app.jsa"}}
	sandbox, jarPath, err := AssembleSandboxWithCDS(cfg, jarSrc, nil, nil, jsaSrc, false)
	if err != nil {
		t.Fatalf("AssembleSandboxWithCDS: %v", err)
	}
	defer os.RemoveAll(sandbox)
	jsaPath := filepath.Join(filepath.Dir(jarPath), "app.jsa")
	if b, err := os.ReadFile(jsaPath); err != nil || string(b) != "JSA" {
		t.Errorf("cds archive not copied to /app/app.jsa: content %q err %v", b, err)
	}
}

func TestBuildJVMArgsEnablePreviewOnly(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}, EnablePreview: true}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "--enable-preview -jar /app/app.jar" {
		t.Errorf("args = %q, want --enable-preview -jar /app/app.jar", got)
	}
}

func TestBuildJVMArgsRejectsBlankAddOpens(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}, AddOpens: []string{"  "}}
	if _, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false); err == nil {
		t.Error("expected error for blank addOpens entry, got nil")
	}
}

func TestBuildJVMArgsClasspathModeSingleJar(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "classpath", MainClass: "com.acme.Main"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "-cp /app/app.jar com.acme.Main" {
		t.Errorf("args = %q, want -cp /app/app.jar com.acme.Main", got)
	}
}

func TestBuildJVMArgsClasspathModeWithClassPath(t *testing.T) {
	cfg := artifact.JVMConfig{
		Entry: artifact.Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}, MainClass: "com.acme.orders.Main"},
	}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "-cp /app/app.jar:/app/lib/* com.acme.orders.Main"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestBuildJVMArgsClassPathOrderPreserved(t *testing.T) {
	cfg := artifact.JVMConfig{
		Entry: artifact.Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/a.jar", "lib/b.jar"}, MainClass: "M"},
	}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := args[1]; got != "/app/app.jar:/app/lib/a.jar:/app/lib/b.jar" {
		t.Errorf("classpath = %q, order not preserved", got)
	}
}

func TestBuildJVMArgsRejectsInconsistentConfig(t *testing.T) {
	// mode=jar with a stray mainClass must fail fast rather than silently
	// running the manifest's Main-Class and ignoring mainClass.
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar", MainClass: "com.acme.Main"}}
	if _, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false); err == nil {
		t.Error("BuildJVMArgs accepted mode=jar with mainClass, want error")
	}
}

func TestBuildJVMArgsModuleModeSingleJar(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "module", Module: "com.acme.orders"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "-p /app/app.jar -m com.acme.orders" {
		t.Errorf("args = %q, want -p /app/app.jar -m com.acme.orders", got)
	}
}

func TestBuildJVMArgsModuleModeWithMainClass(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "module", Module: "com.acme.orders", MainClass: "com.acme.orders.Main"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "-p /app/app.jar -m com.acme.orders/com.acme.orders.Main"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestBuildJVMArgsModuleModeWithModulePath(t *testing.T) {
	cfg := artifact.JVMConfig{
		Entry: artifact.Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}},
	}
	args, err := BuildJVMArgs(cfg, "/app/orders.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "-p /app/orders.jar:/app/mods -m com.acme.orders"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestBuildJVMArgsMixedModuleAndClassPath(t *testing.T) {
	cfg := artifact.JVMConfig{
		Entry: artifact.Entry{
			Mode:       "module",
			Module:     "com.acme.orders",
			ModulePath: []string{"orders.jar", "mods"},
			ClassPath:  []string{"lib/*"},
		},
	}
	args, err := BuildJVMArgs(cfg, "/app/orders.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// The supplementary class path precedes the module path; -m stays terminal.
	want := "-cp /app/lib/* -p /app/orders.jar:/app/mods -m com.acme.orders"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestBuildJVMArgsMixedClassPathOrderPreserved(t *testing.T) {
	cfg := artifact.JVMConfig{
		Entry: artifact.Entry{
			Mode:       "module",
			Module:     "com.acme.orders",
			MainClass:  "com.acme.orders.Main",
			ModulePath: []string{"mods"},
			ClassPath:  []string{"legacy/a.jar", "legacy/b.jar", "lib/*"},
		},
	}
	args, err := BuildJVMArgs(cfg, "/app/orders.jar", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "-cp /app/legacy/a.jar:/app/legacy/b.jar:/app/lib/* -p /app/mods -m com.acme.orders/com.acme.orders.Main"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestBuildJVMArgsModuleModeRejectsMissingModule(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "module"}}
	if _, err := BuildJVMArgs(cfg, "/app/app.jar", nil, false); err == nil {
		t.Error("BuildJVMArgs accepted mode=module without module name, want error")
	}
}

func TestExtractTar(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "deps.tar")
	writeTar(t, tarPath, map[string]string{"spring-core.jar": "x", "nested/jackson.jar": "y"})

	dest := filepath.Join(dir, "lib")
	if err := extractTar(tarPath, dest); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "spring-core.jar")); err != nil || string(b) != "x" {
		t.Errorf("spring-core.jar not extracted correctly: %v %q", err, b)
	}
	if _, err := os.Stat(filepath.Join(dest, "nested", "jackson.jar")); err != nil {
		t.Errorf("nested jar not extracted: %v", err)
	}
}

func TestExtractTarRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "evil.tar")
	writeTar(t, tarPath, map[string]string{"../../escape.jar": "boom"})
	if err := extractTar(tarPath, filepath.Join(dir, "lib")); err == nil {
		t.Error("expected extractTar to reject a path-traversal entry")
	}
}

func TestAssembleSandboxWithClasspathLayers(t *testing.T) {
	dir := t.TempDir()
	jarSrc := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarSrc, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	writeTar(t, depsTar, map[string]string{"spring-core.jar": "x", "jackson.jar": "y"})

	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}, MainClass: "M"}}
	sandbox, jarPath, err := AssembleSandbox(cfg, jarSrc, []string{depsTar}, nil)
	if err != nil {
		t.Fatalf("AssembleSandbox: %v", err)
	}
	defer os.RemoveAll(sandbox)

	if _, err := os.Stat(jarPath); err != nil {
		t.Errorf("main jar not staged: %v", err)
	}
	libDir := filepath.Join(filepath.Dir(jarPath), "lib")
	for _, j := range []string{"spring-core.jar", "jackson.jar"} {
		if _, err := os.Stat(filepath.Join(libDir, j)); err != nil {
			t.Errorf("dependency %s not in /app/lib: %v", j, err)
		}
	}
}

func TestGenerateBundleWithClasspathLayers(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := filepath.Join(dir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	jarHost := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	writeTar(t, depsTar, map[string]string{"spring-core.jar": "x"})

	cfg := artifact.JVMConfig{
		MainJar: "app.jar",
		Entry:   artifact.Entry{Mode: "classpath", ClassPath: []string{"app.jar", "lib/*"}, MainClass: "com.acme.Main"},
	}
	out := filepath.Join(dir, "bundle")
	if err := GenerateBundleWithLauncher(cfg, jdkRoot, "", "", jarHost, []string{depsTar}, nil, out, Resources{}, nil); err != nil {
		t.Fatalf("GenerateBundleWithLauncher: %v", err)
	}

	// The dependency layer was extracted to the host staging dir.
	if _, err := os.Stat(filepath.Join(out, "lib", "spring-core.jar")); err != nil {
		t.Errorf("dependency not extracted into bundle lib dir: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := string(b)
	if !strings.Contains(cfgJSON, `"/app/lib"`) {
		t.Errorf("config.json missing /app/lib mount:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, "/app/app.jar:/app/lib/*") {
		t.Errorf("config.json missing layered classpath argv:\n%s", cfgJSON)
	}
}

func TestAssembleSandboxWithModulepathLayers(t *testing.T) {
	dir := t.TempDir()
	jarSrc := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarSrc, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	modsTar := filepath.Join(dir, "mods.tar")
	writeTar(t, modsTar, map[string]string{"guava.jar": "x", "slf4j.jar": "y"})

	cfg := artifact.JVMConfig{MainJar: "orders.jar", Entry: artifact.Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}}}
	sandbox, jarPath, err := AssembleSandbox(cfg, jarSrc, nil, []string{modsTar})
	if err != nil {
		t.Fatalf("AssembleSandbox: %v", err)
	}
	defer os.RemoveAll(sandbox)

	if _, err := os.Stat(jarPath); err != nil {
		t.Errorf("main jar not staged: %v", err)
	}
	modsDir := filepath.Join(filepath.Dir(jarPath), "mods")
	for _, j := range []string{"guava.jar", "slf4j.jar"} {
		if _, err := os.Stat(filepath.Join(modsDir, j)); err != nil {
			t.Errorf("module %s not in /app/mods: %v", j, err)
		}
	}
}

func TestGenerateBundleWithModulepathLayers(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := filepath.Join(dir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	jarHost := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	modsTar := filepath.Join(dir, "mods.tar")
	writeTar(t, modsTar, map[string]string{"guava.jar": "x"})

	cfg := artifact.JVMConfig{
		MainJar: "orders.jar",
		Entry:   artifact.Entry{Mode: "module", Module: "com.acme.orders", ModulePath: []string{"orders.jar", "mods"}},
	}
	out := filepath.Join(dir, "bundle")
	if err := GenerateBundleWithLauncher(cfg, jdkRoot, "", "", jarHost, nil, []string{modsTar}, out, Resources{}, nil); err != nil {
		t.Fatalf("GenerateBundleWithLauncher: %v", err)
	}

	// The module layer was extracted to the host staging dir.
	if _, err := os.Stat(filepath.Join(out, "mods", "guava.jar")); err != nil {
		t.Errorf("module not extracted into bundle mods dir: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := string(b)
	if !strings.Contains(cfgJSON, `"/app/mods"`) {
		t.Errorf("config.json missing /app/mods mount:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, `"/app/orders.jar:/app/mods"`) {
		t.Errorf("config.json missing module-path argv:\n%s", cfgJSON)
	}
}

func TestAssembleSandboxWithMixedLayers(t *testing.T) {
	dir := t.TempDir()
	jarSrc := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarSrc, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	writeTar(t, depsTar, map[string]string{"legacy.jar": "x"})
	modsTar := filepath.Join(dir, "mods.tar")
	writeTar(t, modsTar, map[string]string{"guava.jar": "y"})

	cfg := artifact.JVMConfig{
		MainJar: "orders.jar",
		Entry: artifact.Entry{
			Mode:       "module",
			Module:     "com.acme.orders",
			ModulePath: []string{"orders.jar", "mods"},
			ClassPath:  []string{"lib/*"},
		},
	}
	sandbox, jarPath, err := AssembleSandbox(cfg, jarSrc, []string{depsTar}, []string{modsTar})
	if err != nil {
		t.Fatalf("AssembleSandbox: %v", err)
	}
	defer os.RemoveAll(sandbox)

	appDir := filepath.Dir(jarPath)
	if _, err := os.Stat(filepath.Join(appDir, "lib", "legacy.jar")); err != nil {
		t.Errorf("class-path dependency not in /app/lib: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "mods", "guava.jar")); err != nil {
		t.Errorf("library module not in /app/mods: %v", err)
	}
}

func TestGenerateBundleWithMixedLayers(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := filepath.Join(dir, "jdk")
	if err := os.MkdirAll(jdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	jarHost := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	depsTar := filepath.Join(dir, "deps.tar")
	writeTar(t, depsTar, map[string]string{"legacy.jar": "x"})
	modsTar := filepath.Join(dir, "mods.tar")
	writeTar(t, modsTar, map[string]string{"guava.jar": "y"})

	cfg := artifact.JVMConfig{
		MainJar: "orders.jar",
		Entry: artifact.Entry{
			Mode:       "module",
			Module:     "com.acme.orders",
			ModulePath: []string{"orders.jar", "mods"},
			ClassPath:  []string{"lib/*"},
		},
	}
	out := filepath.Join(dir, "bundle")
	if err := GenerateBundleWithLauncher(cfg, jdkRoot, "", "", jarHost, []string{depsTar}, []string{modsTar}, out, Resources{}, nil); err != nil {
		t.Fatalf("GenerateBundleWithLauncher: %v", err)
	}

	// Both layer kinds were extracted to their host staging dirs.
	if _, err := os.Stat(filepath.Join(out, "lib", "legacy.jar")); err != nil {
		t.Errorf("class-path dependency not extracted into bundle lib dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "mods", "guava.jar")); err != nil {
		t.Errorf("library module not extracted into bundle mods dir: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := string(b)
	if !strings.Contains(cfgJSON, `"/app/lib"`) {
		t.Errorf("config.json missing /app/lib mount:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, `"/app/mods"`) {
		t.Errorf("config.json missing /app/mods mount:\n%s", cfgJSON)
	}
	// Mixed argv: -cp precedes -p, and -m stays terminal.
	for _, want := range []string{`"-cp"`, `"/app/lib/*"`, `"-p"`, `"/app/orders.jar:/app/mods"`, `"-m"`, `"com.acme.orders"`} {
		if !strings.Contains(cfgJSON, want) {
			t.Errorf("config.json missing mixed argv token %s:\n%s", want, cfgJSON)
		}
	}
}
