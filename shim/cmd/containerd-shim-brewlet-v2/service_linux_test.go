//go:build linux

package main

import (
	"archive/tar"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	runtimeoptions "github.com/containerd/containerd/pkg/runtimeoptions/v1"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/brewlet/brewlet/internal/artifact"
	kcruntime "github.com/brewlet/brewlet/internal/runtime"
)

func testResolved() resolvedArtifact {
	return resolvedArtifact{
		Config: artifact.JVMConfig{
			MainJar: "app.jar",
			Entry:   artifact.Entry{Mode: "jar"},
			Env:     []artifact.EnvVar{{Name: "FOO", Value: "bar"}},
		},
		JarHostPath: "/var/lib/containerd/.../blobs/sha256/deadbeef",
		JDKRoot:     "/opt/brewlet/jdks/temurin-21",
	}
}

func TestSetupOverlayRootfs(t *testing.T) {
	bundle := t.TempDir()
	r := &taskAPI.CreateTaskRequest{ID: "task-1", Bundle: bundle}

	if err := setupOverlayRootfs(r, testResolved()); err != nil {
		t.Fatalf("setupOverlayRootfs: %v", err)
	}
	if len(r.Rootfs) != 1 {
		t.Fatalf("want 1 rootfs mount, got %d", len(r.Rootfs))
	}
	m := r.Rootfs[0]
	if m.Type != "overlay" {
		t.Errorf("type = %q, want overlay", m.Type)
	}
	joined := strings.Join(m.Options, ",")
	if !strings.Contains(joined, "lowerdir=/opt/brewlet/jdks/temurin-21") {
		t.Errorf("missing JDK lowerdir in %q", joined)
	}
	for _, want := range []string{"upperdir=" + filepath.Join(bundle, "brewlet", "upper"), "workdir=" + filepath.Join(bundle, "brewlet", "work")} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "brewlet", "upper")); err != nil {
		t.Errorf("upper dir not created: %v", err)
	}
}

func TestSetupOverlayRootfsLauncherLower(t *testing.T) {
	bundle := t.TempDir()
	r := &taskAPI.CreateTaskRequest{ID: "task-2", Bundle: bundle}
	ra := testResolved()
	ra.LauncherName = "jaz"
	ra.LauncherRoot = "/opt/brewlet/launchers/jaz"

	if err := setupOverlayRootfs(r, ra); err != nil {
		t.Fatal(err)
	}
	opts := strings.Join(r.Rootfs[0].Options, ",")
	// launcher layer must be higher priority (listed before the JDK) in lowerdir.
	if !strings.Contains(opts, "lowerdir=/opt/brewlet/launchers/jaz:/opt/brewlet/jdks/temurin-21") {
		t.Errorf("launcher not layered above JDK: %q", opts)
	}
}

func TestApplyBrewletLaunch(t *testing.T) {
	spec := &specs.Spec{Process: &specs.Process{Env: []string{"POD_VAR=keepme", "JAVA_HOME=/wrong"}}}
	if err := applyBrewletLaunch(spec, testResolved(), t.TempDir()); err != nil {
		t.Fatalf("applyBrewletLaunch: %v", err)
	}

	if got := spec.Process.Args[0]; got != "java" {
		t.Errorf("args[0] = %q, want java", got)
	}
	if spec.Process.Args[len(spec.Process.Args)-1] != "/app/app.jar" {
		t.Errorf("last arg = %q, want /app/app.jar", spec.Process.Args[len(spec.Process.Args)-1])
	}
	if spec.Process.Cwd != "/app" {
		t.Errorf("cwd = %q", spec.Process.Cwd)
	}

	env := map[string]string{}
	for _, kv := range spec.Process.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["JAVA_HOME"] != "/opt/jdk" {
		t.Errorf("JAVA_HOME = %q, want /opt/jdk (Brewlet must win over pod default)", env["JAVA_HOME"])
	}
	if env["POD_VAR"] != "keepme" {
		t.Errorf("pod env not preserved: %q", env["POD_VAR"])
	}
	if env["FOO"] != "bar" {
		t.Errorf("artifact env FOO = %q, want bar", env["FOO"])
	}

	// JDK + JAR must be mounted.
	var haveJDK, haveJar bool
	for _, m := range spec.Mounts {
		if m.Destination == "/opt/jdk" {
			haveJDK = true
		}
		if m.Destination == "/app/app.jar" {
			haveJar = true
		}
	}
	if !haveJDK || !haveJar {
		t.Errorf("missing mounts: jdk=%v jar=%v", haveJDK, haveJar)
	}
}

func TestApplyBrewletLaunchWithCDS(t *testing.T) {
	// StageCDSJar copies the real JAR bytes, so back JarHostPath with a file.
	jarHost := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	ra := testResolved()
	ra.JarHostPath = jarHost
	ra.Config.CDS = &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}
	ra.CDSHostPath = "/var/lib/containerd/.../blobs/sha256/cafe"
	bundleDir := t.TempDir()
	spec := &specs.Spec{Process: &specs.Process{}}
	if err := applyBrewletLaunch(spec, ra, bundleDir); err != nil {
		t.Fatalf("applyBrewletLaunch: %v", err)
	}

	// The archive must be bind-mounted at /app/app.jsa...
	var haveCDS bool
	var jarSource string
	for _, m := range spec.Mounts {
		if m.Destination == "/app/app.jsa" && m.Source == ra.CDSHostPath {
			haveCDS = true
		}
		if m.Destination == "/app/app.jar" {
			jarSource = m.Source
		}
	}
	if !haveCDS {
		t.Errorf("missing /app/app.jsa mount: %+v", spec.Mounts)
	}
	// The JAR must be a canonical-mtime staged copy, NOT the raw blob, so the
	// archive's recorded JAR timestamp matches at load time.
	if jarSource == ra.JarHostPath {
		t.Errorf("jar mount source = raw blob %q, want a staged CDS copy", jarSource)
	}
	if fi, err := os.Stat(jarSource); err != nil {
		t.Errorf("stat staged jar: %v", err)
	} else if !fi.ModTime().Equal(kcruntime.CDSModTime) {
		t.Errorf("staged jar mtime = %v, want canonical %v", fi.ModTime(), kcruntime.CDSModTime)
	}
	// ...and the launch argv must reference it with the safe -Xshare:auto.
	argv := strings.Join(spec.Process.Args, " ")
	if !strings.Contains(argv, "-Xshare:auto") || !strings.Contains(argv, "-XX:SharedArchiveFile=/app/app.jsa") {
		t.Errorf("argv missing CDS flags: %q", argv)
	}
}

func TestApplyBrewletLaunchNoCDS(t *testing.T) {
	spec := &specs.Spec{Process: &specs.Process{}}
	ra := testResolved()
	if err := applyBrewletLaunch(spec, ra, t.TempDir()); err != nil {
		t.Fatalf("applyBrewletLaunch: %v", err)
	}
	for _, m := range spec.Mounts {
		if strings.HasSuffix(m.Destination, ".jsa") {
			t.Errorf("unexpected CDS mount for artifact with no archive: %q", m.Destination)
		}
		// Without CDS the JAR is bind-mounted straight from the blob (no copy).
		if m.Destination == "/app/app.jar" && m.Source != ra.JarHostPath {
			t.Errorf("jar mount source = %q, want raw blob %q (no copy without CDS)", m.Source, ra.JarHostPath)
		}
	}
	if argv := strings.Join(spec.Process.Args, " "); strings.Contains(argv, "SharedArchiveFile") {
		t.Errorf("unexpected CDS flag in argv: %q", argv)
	}
}

func TestMountClasspathLayers(t *testing.T) {
	// A classpath.layer.v1+tar staged on the host must be unpacked and
	// bind-mounted read-only at /app/lib so `-cp /app/app.jar:/app/lib/*`
	// resolves the dependency JARs in the production shim path (parity with the
	// CLI/harness bundle assembly).
	src := t.TempDir()
	tarPath := filepath.Join(src, "deps.tar")
	writeTar(t, tarPath, map[string][]byte{"dep.jar": []byte("JAR")})

	bundle := t.TempDir()
	spec := &specs.Spec{Process: &specs.Process{}}
	ra := testResolved()
	ra.ClasspathHostPaths = []string{tarPath}

	if err := mountClasspathLayers(spec, ra, bundle); err != nil {
		t.Fatalf("mountClasspathLayers: %v", err)
	}

	libHost := filepath.Join(bundle, "brewlet", "lib")
	var libMount *specs.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == "/app/lib" {
			libMount = &spec.Mounts[i]
		}
	}
	if libMount == nil {
		t.Fatal("no /app/lib mount added")
	}
	if libMount.Source != libHost {
		t.Errorf("lib mount source = %q, want %q", libMount.Source, libHost)
	}
	if strings.Join(libMount.Options, ",") != "rbind,ro" {
		t.Errorf("lib mount options = %v, want [rbind ro]", libMount.Options)
	}
	if _, err := os.Stat(filepath.Join(libHost, "dep.jar")); err != nil {
		t.Errorf("dependency JAR not staged: %v", err)
	}
}

func TestMountClasspathLayersNoop(t *testing.T) {
	// No classpath layers => no /app/lib mount.
	spec := &specs.Spec{Process: &specs.Process{}}
	if err := mountClasspathLayers(spec, testResolved(), t.TempDir()); err != nil {
		t.Fatalf("mountClasspathLayers: %v", err)
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/app/lib" {
			t.Errorf("unexpected /app/lib mount for artifact with no classpath layers")
		}
	}
}

func TestMountModulepathLayers(t *testing.T) {
	// A modulepath.layer.v1+tar staged on the host must be unpacked and
	// bind-mounted read-only at /app/mods so `-p /app/orders.jar:/app/mods`
	// resolves the library modules in the production shim path.
	src := t.TempDir()
	tarPath := filepath.Join(src, "mods.tar")
	writeTar(t, tarPath, map[string][]byte{"guava.jar": []byte("JAR")})

	bundle := t.TempDir()
	spec := &specs.Spec{Process: &specs.Process{}}
	ra := testResolved()
	ra.ModulepathHostPaths = []string{tarPath}

	if err := mountModulepathLayers(spec, ra, bundle); err != nil {
		t.Fatalf("mountModulepathLayers: %v", err)
	}

	modsHost := filepath.Join(bundle, "brewlet", "mods")
	var modsMount *specs.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == "/app/mods" {
			modsMount = &spec.Mounts[i]
		}
	}
	if modsMount == nil {
		t.Fatal("no /app/mods mount added")
	}
	if modsMount.Source != modsHost {
		t.Errorf("mods mount source = %q, want %q", modsMount.Source, modsHost)
	}
	if strings.Join(modsMount.Options, ",") != "rbind,ro" {
		t.Errorf("mods mount options = %v, want [rbind ro]", modsMount.Options)
	}
	if _, err := os.Stat(filepath.Join(modsHost, "guava.jar")); err != nil {
		t.Errorf("module JAR not staged: %v", err)
	}
}

func TestMountModulepathLayersNoop(t *testing.T) {
	// No module layers => no /app/mods mount.
	spec := &specs.Spec{Process: &specs.Process{}}
	if err := mountModulepathLayers(spec, testResolved(), t.TempDir()); err != nil {
		t.Fatalf("mountModulepathLayers: %v", err)
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/app/mods" {
			t.Errorf("unexpected /app/mods mount for artifact with no module layers")
		}
	}
}

func writeTar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactRefAnnotations(t *testing.T) {
	if got := artifactRef(&specs.Spec{Annotations: map[string]string{annArtifactRef: "demo/hello:1"}}); got != "demo/hello:1" {
		t.Errorf("native ref = %q", got)
	}
	if got := artifactRef(&specs.Spec{Annotations: map[string]string{annCRIImage: "cri/img:2"}}); got != "cri/img:2" {
		t.Errorf("cri fallback = %q", got)
	}
	if got := artifactRef(&specs.Spec{}); got != "" {
		t.Errorf("want empty ref, got %q", got)
	}
}

func writeSpecJSON(t *testing.T, bundle string, spec specs.Spec) {
	t.Helper()
	raw, err := json.Marshal(&spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), raw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
}

func TestIsSandboxBundle(t *testing.T) {
	cases := []struct {
		name string
		anns map[string]string
		want bool
	}{
		{"sandbox", map[string]string{annContainerType: containerTypeSandbox}, true},
		{"workload", map[string]string{annContainerType: "container"}, false},
		{"unset (non-CRI/harness path)", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			writeSpecJSON(t, bundle, specs.Spec{Annotations: tc.anns})
			got, err := isSandboxBundle(bundle)
			if err != nil {
				t.Fatalf("isSandboxBundle: %v", err)
			}
			if got != tc.want {
				t.Errorf("isSandboxBundle = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNormalizeRuncOptionsGeneric proves the fix for the CRI panic: containerd's
// CRI plugin wraps a custom runtime handler's config in a generic
// runtimeoptions.Options, but the embedded runc task service hard-asserts
// options.Options. We must translate, preserving SystemdCgroup.
func TestNormalizeRuncOptionsGeneric(t *testing.T) {
	generic := &runtimeoptions.Options{
		ConfigBody: []byte("BinaryName = \"runc\"\nSystemdCgroup = true\n"),
	}
	anyGeneric, err := anypb.New(generic)
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	r := &taskAPI.CreateTaskRequest{Options: anyGeneric}

	if err := normalizeRuncOptions(r); err != nil {
		t.Fatalf("normalizeRuncOptions: %v", err)
	}
	v, err := typeurl.UnmarshalAny(r.Options)
	if err != nil {
		t.Fatalf("unmarshal normalized options: %v", err)
	}
	opts, ok := v.(*runcoptions.Options)
	if !ok {
		t.Fatalf("normalized options type = %T, want *runcoptions.Options", v)
	}
	if !opts.SystemdCgroup {
		t.Errorf("SystemdCgroup not preserved through translation")
	}
	if opts.BinaryName != "runc" {
		t.Errorf("BinaryName = %q, want runc", opts.BinaryName)
	}
}

func TestNormalizeRuncOptionsPassthrough(t *testing.T) {
	// Already runc-native options must be left untouched.
	runcAny, err := anypb.New(&runcoptions.Options{SystemdCgroup: true, BinaryName: "runc"})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	r := &taskAPI.CreateTaskRequest{Options: runcAny}
	before := r.Options
	if err := normalizeRuncOptions(r); err != nil {
		t.Fatalf("normalizeRuncOptions: %v", err)
	}
	if r.Options != before {
		t.Errorf("runc-native options were rewritten")
	}

	// A request without options must not panic and must stay nil.
	empty := &taskAPI.CreateTaskRequest{}
	if err := normalizeRuncOptions(empty); err != nil {
		t.Fatalf("normalizeRuncOptions(nil options): %v", err)
	}
	if empty.Options != nil {
		t.Errorf("nil options became %v", empty.Options)
	}
}
