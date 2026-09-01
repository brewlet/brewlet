// Package runtime contains the Brewlet launch core shared by the CLI ("run"
// for local demo) and the containerd shim ("bundle" for the runc-backed path).
package runtime

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/brewlet/brewlet/internal/artifact"
)

// sortedKeys returns a map's keys in deterministic (lexical) order so the
// expanded -D flags are stable across builds.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Resources mirror the deployment-descriptor limits (https://github.com/brewlet/brewlet/tree/main/specs §10).
// They drive the sandbox cgroup only; Brewlet injects no JVM tuning flags.
type Resources struct {
	CPULimit    string // e.g. "2", "500m"  (empty = unlimited)
	MemoryLimit string // e.g. "512Mi", "1Gi" (empty = unlimited)
}

// Plan is the fully-resolved launch: which JDK, which args, which jar.
type Plan struct {
	JavaBin string
	JDKHome string
	Args    []string // full argv after the java binary
	Env     []string
	JarPath string
}

// BuildJVMArgs assembles the launcher argv and appends the entrypoint. Brewlet
// injects NO resource/environment tuning (heap/GC/CPU): that is the deployment
// descriptor's job via jvm.args. The only launch flags derived from the artifact
// are the app-intrinsic correctness knobs (preview features, module-system
// access, system properties) and, when present, the optional AppCDS archive
// (https://github.com/brewlet/brewlet/blob/main/docs/appcds.md). The container-aware JDK reads the sandbox cgroup limits
// directly. This performs no filesystem access, so it is safe for bundle
// generation targeting a remote node's JDK.
//
// Expansion order (entry always last): -Xshare:auto -XX:SharedArchiveFile (CDS),
// --enable-preview, --add-modules, --add-opens, --add-exports, -D<k>=<v>
// (sorted), then extraArgs (the local `-- …` args or the descriptor's jvm.args),
// then the entrypoint.
//
// regenerateCDS reflects the DEPLOYMENT's node-side regeneration choice (the
// brewlet.sh/cds-regenerate annotation / --appcds-regenerate flag), not anything
// in the artifact: when set, the shipped-archive args are suppressed here because
// the node injects -XX:+AutoCreateSharedArchive against its own cache separately
// (https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §4.3), and a shipped archive becomes seed data rather than a
// consumed /app/<archive> mount.
func BuildJVMArgs(cfg artifact.JVMConfig, jarPath string, extraArgs []string, regenerateCDS bool) ([]string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var args []string

	// AppCDS (optional): mount-point is /app/<archive>, i.e. beside the JAR, so
	// derive it from jarPath. `-Xshare:auto` (never `:on`) is deliberate — a
	// build/version/classpath mismatch falls back to base CDS instead of failing,
	// preserving Brewlet's safe-fallback posture. See https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §5, §7.
	//
	// Skipped when the deployment opts into node-side regeneration: in that case
	// the node injects the regeneration args (-XX:+AutoCreateSharedArchive against
	// the node cache) separately (https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §4.3), so BuildJVMArgs must not
	// also point -XX:SharedArchiveFile at a /app/<archive> that isn't mounted.
	if cfg.CDS != nil && !regenerateCDS && cfg.CDS.Archive != "" {
		archivePath := filepath.Join(filepath.Dir(jarPath), cfg.CDS.Archive)
		args = append(args, "-Xshare:auto", "-XX:SharedArchiveFile="+archivePath)
	}

	if cfg.EnablePreview {
		args = append(args, "--enable-preview")
	}
	if len(cfg.AddModules) > 0 {
		args = append(args, "--add-modules", strings.Join(cfg.AddModules, ","))
	}
	for _, o := range cfg.AddOpens {
		args = append(args, "--add-opens", o)
	}
	for _, x := range cfg.AddExports {
		args = append(args, "--add-exports", x)
	}
	for _, k := range sortedKeys(cfg.SystemProperties) {
		args = append(args, "-D"+k+"="+cfg.SystemProperties[k])
	}
	args = append(args, extraArgs...)

	// Entry: java -jar app.jar (default), java -cp app.jar MainClass, or
	// java [-cp <class-path>] -p <module-path> -m <module>[/MainClass] (JPMS module
	// mode, optionally with a supplementary class path for the mixed form).
	// Field/mode consistency is already enforced by cfg.Validate() above.
	switch cfg.Entry.Mode {
	case "", "jar":
		args = append(args, "-jar", jarPath)
	case "classpath":
		args = append(args, "-cp", classPath(cfg, jarPath), cfg.Entry.MainClass)
	case "module":
		// Mixed form: a supplementary class path (`-cp`) precedes the module path
		// so `-m <module>` stays terminal (everything after it is program args).
		if len(cfg.Entry.ClassPath) > 0 {
			args = append(args, "-cp", classPath(cfg, jarPath))
		}
		target := cfg.Entry.Module
		if cfg.Entry.MainClass != "" {
			target += "/" + cfg.Entry.MainClass
		}
		args = append(args, "-p", modulePath(cfg, jarPath), "-m", target)
	default:
		return nil, fmt.Errorf("unknown entry.mode %q", cfg.Entry.Mode)
	}
	return args, nil
}

// classPath builds the `-cp` value for classpath mode. With no entry.classPath it is
// just the main JAR (today's behavior). Otherwise each entry is resolved relative
// to the app directory (the directory holding jarPath, i.e. /app in the sandbox)
// and joined in order with the node path separator ":". The JVM expands any
// trailing "lib/*" wildcard itself. See https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md.
func classPath(cfg artifact.JVMConfig, jarPath string) string {
	return joinAppPaths(cfg.Entry.ClassPath, jarPath)
}

// modulePath builds the `-p` value for module mode. It is the module-path twin of
// classPath: with no entry.modulePath it is just the main JAR (the single modular
// JAR case); otherwise each /app-relative entry (e.g. "orders.jar", "mods") is
// resolved against the app directory and joined in order with ":". A directory
// entry (e.g. the /app/mods modulepath layer) contributes every JAR it holds.
// See https://github.com/brewlet/brewlet/blob/main/docs/jpms-support.md.
func modulePath(cfg artifact.JVMConfig, jarPath string) string {
	return joinAppPaths(cfg.Entry.ModulePath, jarPath)
}

// joinAppPaths resolves an ordered list of /app-relative entries against the app
// directory (the directory holding jarPath) and joins them with the node path
// separator ":". When entries is empty it falls back to the single jarPath.
// Nodes are Linux; ":" is used explicitly so bundles generated on any dev OS are
// correct for the target node.
func joinAppPaths(entries []string, jarPath string) string {
	if len(entries) == 0 {
		return jarPath
	}
	base := filepath.Dir(jarPath)
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = filepath.Join(base, e)
	}
	return strings.Join(parts, ":")
}

// BuildPlan resolves the node JDK/launcher and assembles the launch argv. The
// launcher name comes from the deployment descriptor (not the artifact); pass
// "" or "java" for the vanilla OpenJDK launcher. regenerateCDS reflects the
// deployment's node-side AppCDS regeneration choice (https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §4.3); the
// caller injects the regeneration args separately.
func BuildPlan(cfg artifact.JVMConfig, jarPath, jdkHome, launcherName string, extraArgs []string, regenerateCDS bool) (Plan, error) {
	_, home, err := resolveJDK(jdkHome)
	if err != nil {
		return Plan{}, err
	}

	launcherBin, err := resolveLauncher(launcherName, home)
	if err != nil {
		return Plan{}, err
	}

	args, err := BuildJVMArgs(cfg, jarPath, extraArgs, regenerateCDS)
	if err != nil {
		return Plan{}, err
	}

	env := os.Environ()
	// A custom launcher (e.g. jaz) locates the JVM via JAVA_HOME; pin it to the
	// selected node JDK. Harmless for the vanilla `java` path.
	env = append(env, "JAVA_HOME="+home)
	for _, e := range cfg.Env {
		env = append(env, e.Name+"="+e.Value)
	}

	return Plan{
		JavaBin: launcherBin,
		JDKHome: home,
		Args:    args,
		Env:     env,
		JarPath: jarPath,
	}, nil
}

// Run executes the plan in the foreground (the local-demo path; on Linux the
// shim hands the equivalent argv to runc inside a sandbox instead).
func (p Plan) Run() error {
	cmd := exec.Command(p.JavaBin, p.Args...)
	cmd.Env = p.Env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	for {
		select {
		case err := <-done:
			return err
		case sig := <-signals:
			if err := cmd.Process.Signal(sig); err != nil {
				return <-done
			}
		}
	}
}

// CommandLine returns a copy-pasteable representation of the launch.
func (p Plan) CommandLine() string {
	return p.JavaBin + " " + strings.Join(p.Args, " ")
}

func resolveJDK(jdkHome string) (javaBin, home string, err error) {
	if jdkHome == "" {
		jdkHome = os.Getenv("BREWLET_JDK_HOME")
	}
	if jdkHome == "" {
		jdkHome = os.Getenv("JAVA_HOME")
	}
	if jdkHome != "" {
		bin := filepath.Join(jdkHome, "bin", "java")
		if _, statErr := os.Stat(bin); statErr == nil {
			return bin, jdkHome, nil
		}
		return "", "", fmt.Errorf("no java under JDK home %q", jdkHome)
	}
	bin, lookErr := exec.LookPath("java")
	if lookErr != nil {
		return "", "", fmt.Errorf("no node-resident JDK found (set --jdk-root or JAVA_HOME)")
	}
	return bin, filepath.Dir(filepath.Dir(bin)), nil
}

// resolveLauncher returns the launcher binary that fronts the entrypoint. The
// vanilla launcher is the selected JDK's own `java`. A custom launcher (e.g.
// "jaz") is a separate node-installed package resolved as an absolute path or
// via PATH — independent of the JDK root. A missing launcher surfaces as
// NoCompatibleLauncher, mirroring NoCompatibleJDK. The launcher name is supplied
// by the deployment descriptor, not the artifact.
func resolveLauncher(launcherName, jdkHome string) (string, error) {
	if artifact.IsVanillaLauncher(launcherName) {
		return filepath.Join(jdkHome, "bin", "java"), nil
	}
	name := artifact.LauncherName(launcherName)
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err != nil {
			return "", fmt.Errorf("NoCompatibleLauncher: launcher %q not found: %w", name, err)
		}
		return name, nil
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleLauncher: launcher %q not on PATH", name)
	}
	return bin, nil
}

// AssembleSandbox creates a per-container working dir and links the JAR at
// /app/<mainJar>, mirroring the rootfs assembly the shim performs. Any classpath
// layer tars are extracted under /app/lib so `java -cp .../lib/*` resolves them,
// and any modulepath layer tars are extracted under /app/mods so
// `java -p .../mods` resolves the library modules.
func AssembleSandbox(cfg artifact.JVMConfig, jarSrc string, classpathTars, modulepathTars []string) (sandboxDir, jarPath string, err error) {
	return AssembleSandboxWithCDS(cfg, jarSrc, classpathTars, modulepathTars, "", false)
}

// AssembleSandboxWithCDS is AssembleSandbox with an optional AppCDS archive.
// When cdsSrc is non-empty the `.jsa` file it names is copied to /app/<archive>
// (cfg.CDS.Archive) so a `-XX:SharedArchiveFile=/app/<archive>` launch finds it,
// mirroring the shim's read-only archive bind-mount. Pass "" for the common
// no-CDS case. regenerate reflects the deployment's node-side regeneration
// choice (https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §4.3): when set, the JAR mtime is pinned to the
// canonical value even without a shipped archive, so a node-regenerated archive
// keeps mapping across runs (CDS validates classpath entries by basename+size+
// mtime). See https://github.com/brewlet/brewlet/blob/main/docs/appcds.md.
func AssembleSandboxWithCDS(cfg artifact.JVMConfig, jarSrc string, classpathTars, modulepathTars []string, cdsSrc string, regenerate bool) (sandboxDir, jarPath string, err error) {
	pinMtime := shipsCDS(cfg) || regenerate
	sandboxDir, err = os.MkdirTemp("", "brewlet-sandbox-*")
	if err != nil {
		return "", "", err
	}
	appDir := filepath.Join(sandboxDir, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return "", "", err
	}
	main := cfg.MainJar
	if main == "" {
		main = "app.jar"
	}
	jarPath = filepath.Join(appDir, main)
	data, err := os.ReadFile(jarSrc)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jarPath, data, 0o644); err != nil {
		return "", "", err
	}
	// When the artifact ships an AppCDS archive (or the deployment regenerates
	// one), pin the app JAR's mtime to the canonical value the archive was
	// trained against so it maps on load rather than being silently rejected
	// under -Xshare:auto (see CDSModTime).
	if pinMtime {
		if err := pinCDSModTime(jarPath); err != nil {
			return "", "", err
		}
	}
	if len(classpathTars) > 0 {
		libDir := filepath.Join(appDir, "lib")
		if err := StageClasspathLayers(classpathTars, libDir); err != nil {
			return "", "", err
		}
		if pinMtime {
			if err := PinCDSModTimesUnder(libDir); err != nil {
				return "", "", err
			}
		}
	}
	if len(modulepathTars) > 0 {
		modsDir := filepath.Join(appDir, "mods")
		if err := StageModulepathLayers(modulepathTars, modsDir); err != nil {
			return "", "", err
		}
		if pinMtime {
			if err := PinCDSModTimesUnder(modsDir); err != nil {
				return "", "", err
			}
		}
	}
	// Copy the shipped archive to /app/<archive> so a -XX:SharedArchiveFile=
	// /app/<archive> launch finds it. Skipped under node-side regeneration: there
	// the shipped archive is only seed data for the node cache (the caller feeds
	// cdsSrc to DecideCDSRegen), and the /app copy would go unread.
	if cdsSrc != "" && cfg.CDS != nil && !regenerate {
		cdsData, err := os.ReadFile(cdsSrc)
		if err != nil {
			return "", "", err
		}
		if err := os.WriteFile(filepath.Join(appDir, cfg.CDS.Archive), cdsData, 0o644); err != nil {
			return "", "", err
		}
	}
	return sandboxDir, jarPath, nil
}

// StageClasspathLayers extracts each dependency-layer tar into libDir (creating
// it if needed), so the directory can be bind-mounted read-only at /app/lib in
// the sandbox. It is the shared implementation used by both the CLI/harness
// bundle path (GenerateBundleWithLauncher) and the production containerd shim's
// Create() hook, keeping layered-classpath deployment
// (https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md) identical across both.
func StageClasspathLayers(classpathTars []string, libDir string) error {
	return stageLayers(classpathTars, libDir, "classpath")
}

// StageModulepathLayers extracts each modulepath-layer tar into modsDir (creating
// it if needed), so the directory can be bind-mounted read-only at /app/mods in
// the sandbox and fed to `java --module-path`. It is the module-path twin of
// StageClasspathLayers. See https://github.com/brewlet/brewlet/blob/main/docs/jpms-support.md.
func StageModulepathLayers(modulepathTars []string, modsDir string) error {
	return stageLayers(modulepathTars, modsDir, "modulepath")
}

// stageLayers extracts each layer tar into destDir (creating it if needed). kind
// names the layer type for error messages (e.g. "classpath", "modulepath").
func stageLayers(tars []string, destDir, kind string) error {
	if len(tars) == 0 {
		return nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, t := range tars {
		if err := extractTar(t, destDir); err != nil {
			return fmt.Errorf("extract %s layer %q: %w", kind, t, err)
		}
	}
	return nil
}

// extractTar unpacks a plain (uncompressed) tar file into destDir, creating
// intermediate directories. It rejects entries whose path would escape destDir.
// The Brewlet classpath layer media type is `+tar` (no gzip).
func extractTar(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		target := filepath.Join(destDir, name)
		rel, err := filepath.Rel(destDir, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // trusted layer content
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

// parseMemory parses Kubernetes-style quantities (Ki/Mi/Gi, K/M/G, bytes).
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := map[string]int64{
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"K": 1000, "M": 1000_000, "G": 1000_000_000,
		"k": 1000, "m": 1000_000, "g": 1000_000_000,
	}
	for _, suf := range []string{"Ki", "Mi", "Gi", "Ti", "K", "M", "G", "k", "m", "g"} {
		if strings.HasSuffix(s, suf) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, suf), 64)
			if err != nil {
				return 0, fmt.Errorf("bad memory %q: %w", s, err)
			}
			return int64(n * float64(mult[suf])), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad memory %q: %w", s, err)
	}
	return n, nil
}

// parseCPU parses "2", "1.5", or "500m" into millicores.
func parseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad cpu %q: %w", s, err)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad cpu %q: %w", s, err)
	}
	return int64(f * 1000), nil
}
