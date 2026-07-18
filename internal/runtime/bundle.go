package runtime

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"github.com/brewlet/brewlet/internal/artifact"
)

// OCI runtime-spec subset sufficient to launch `java -jar` under runc.
type ociSpec struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname,omitempty"`
	Mounts     []ociMount `json:"mounts"`
	Linux      ociLinux   `json:"linux"`
}

type ociProcess struct {
	Terminal bool     `json:"terminal"`
	User     ociUser  `json:"user"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces"`
	Resources  ociResources   `json:"resources"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

type ociResources struct {
	Memory *ociMemory `json:"memory,omitempty"`
	CPU    *ociCPU    `json:"cpu,omitempty"`
}

type ociMemory struct {
	Limit *int64 `json:"limit,omitempty"`
}

type ociCPU struct {
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`
}

// GenerateBundle writes an OCI runtime bundle (config.json) that runc can run:
// rootfs = node JDK runtime root (lower) with the JAR mounted at /app. This is
// exactly what containerd-shim-brewlet-v2 produces on a Linux node.
func GenerateBundle(cfg artifact.JVMConfig, jdkRoot, jarHostPath, outDir string, res Resources, extraArgs []string) error {
	return GenerateBundleWithLauncher(cfg, jdkRoot, "", "", jarHostPath, nil, nil, outDir, res, extraArgs)
}

// GenerateBundleWithLauncher is GenerateBundle with an optional node-side
// launcher layer. launcherName is the launcher the deployment descriptor
// requested ("" or "java" for the vanilla OpenJDK launcher); launcherRoot is the
// host path of a read-only layer that the provisioner installed for a custom
// launcher (e.g. jaz); it is overlaid into the sandbox and prepended to PATH so
// the launcher binary resolves there. Pass "" for both for the vanilla `java`
// path.
//
// classpathTars are optional dependency-layer tars (see
// https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md). When present they are extracted into a
// host staging dir and bind-mounted read-only at /app/lib, so a class-mode
// `-cp /app/app.jar:/app/lib/*` resolves the dependency JARs.
//
// modulepathTars are optional library-module tars (see https://github.com/brewlet/site/blob/main/docs/jpms-support.md).
// When present they are extracted into a host staging dir and bind-mounted
// read-only at /app/mods, so a module-mode `-p /app/app.jar:/app/mods` resolves
// the library modules.
func GenerateBundleWithLauncher(cfg artifact.JVMConfig, jdkRoot, launcherRoot, launcherName, jarHostPath string, classpathTars, modulepathTars []string, outDir string, res Resources, extraArgs []string) error {
	return GenerateBundleWithCDS(cfg, jdkRoot, launcherRoot, launcherName, jarHostPath, classpathTars, modulepathTars, "", outDir, res, extraArgs)
}

// GenerateBundleWithCDS is GenerateBundleWithLauncher with an optional AppCDS
// archive. When cdsHostPath is non-empty the `.jsa` file it names is bind-mounted
// read-only at /app/<archive> (cfg.CDS.Archive), matching the -XX:SharedArchiveFile
// path BuildJVMArgs derives, so the JVM maps the app archive on startup. Pass ""
// for the common no-CDS case. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
func GenerateBundleWithCDS(cfg artifact.JVMConfig, jdkRoot, launcherRoot, launcherName, jarHostPath string, classpathTars, modulepathTars []string, cdsHostPath, outDir string, res Resources, extraArgs []string) error {
	return GenerateBundleWithRegen(cfg, jdkRoot, launcherRoot, launcherName, jarHostPath, classpathTars, modulepathTars, cdsHostPath, outDir, res, extraArgs, CDSRegenOptions{})
}

// CDSRegenOptions carries the node context needed for node-side AppCDS
// regeneration (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3, Phase B). Regeneration is a deployment/fleet
// decision (the brewlet.sh/cds-regenerate annotation / --appcds-regenerate flag),
// so Regenerate is passed in here rather than read from the artifact. It is only
// acted on when Regenerate is true AND ArtifactKey is non-empty; otherwise
// regeneration is skipped and launch falls back to base CDS (or the shipped
// archive, when the deployment did not opt in).
type CDSRegenOptions struct {
	// Regenerate reflects the deployment's node-side regeneration choice. When
	// false the shipped archive (if any) is consumed verbatim as before.
	Regenerate bool
	// ArtifactKey is the stable per-artifact identity (manifest digest in
	// production, ref otherwise) used to key the node archive cache. Empty
	// disables regeneration for this call.
	ArtifactKey string
	// CacheDir overrides the node cache directory (default DefaultCDSCacheDir).
	CacheDir string
	// MetricsDir, when set, receives best-effort node-local role records
	// (https://github.com/brewlet/site/blob/main/docs/metrics-exporter.md).
	MetricsDir string
}

// GenerateBundleWithRegen is GenerateBundleWithCDS plus node-side regeneration.
// When the deployment opts into regeneration (regen.Regenerate) and
// regen.ArtifactKey is set, it resolves a per-(artifact, JDK-build) archive from
// the node cache (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3): it bind-mounts the cache dir at
// InSandboxCDSDir (writable only for the elected writer), prepends the resolved
// -XX:+AutoCreateSharedArchive / -XX:SharedArchiveFile args, and treats any
// shipped archive as optional seed data rather than mounting it at /app/<archive>.
func GenerateBundleWithRegen(cfg artifact.JVMConfig, jdkRoot, launcherRoot, launcherName, jarHostPath string, classpathTars, modulepathTars []string, cdsHostPath, outDir string, res Resources, extraArgs []string, regen CDSRegenOptions) error {
	if err := os.MkdirAll(filepath.Join(outDir, "rootfs"), 0o755); err != nil {
		return err
	}

	// Re-derive the JVM args (resource mapping) but target in-sandbox paths.
	// The deployment's regeneration choice suppresses the shipped-archive args.
	inSandboxJar := "/app/" + nonEmpty(cfg.MainJar, "app.jar")
	jvmArgs, err := BuildJVMArgs(cfg, inSandboxJar, extraArgs, regen.Regenerate)
	if err != nil {
		return err
	}

	// Node-side regeneration: resolve the cache archive and prepend its launch
	// args. The cache dir is bind-mounted at InSandboxCDSDir (rw for the writer,
	// ro otherwise) so -XX:+AutoCreateSharedArchive can write it at JVM exit.
	var cdsCacheMount []ociMount
	if regen.Regenerate && regen.ArtifactKey != "" {
		seed := ""
		if cdsHostPath != "" && cfg.CDS != nil && cfg.CDS.Archive != "" {
			seed = cdsHostPath
		}
		cacheDir := regen.CacheDir
		if cacheDir == "" {
			cacheDir = DefaultCDSCacheDir
		}
		dec, derr := DecideCDSRegen(RegenParams{
			CacheDir:      cacheDir,
			JDKRoot:       jdkRoot,
			ArtifactKey:   regen.ArtifactKey,
			SeedArchive:   seed,
			ArchiveArgDir: InSandboxCDSDir,
			MetricsDir:    regen.MetricsDir,
		})
		if derr != nil {
			return derr
		}
		if len(dec.Args) > 0 {
			jvmArgs = append(append([]string{}, dec.Args...), jvmArgs...)
		}
		if dec.Role == RegenConsume || dec.Role == RegenWrite {
			opts := []string{"rbind"}
			if dec.MountRW {
				opts = append(opts, "rw")
			} else {
				opts = append(opts, "ro")
			}
			cdsCacheMount = []ociMount{{
				Destination: InSandboxCDSDir, Type: "bind",
				Source: cacheDir, Options: opts,
			}}
		}
	}

	// Pin the app JAR (and any staged lib/mods entries) to the canonical mtime
	// whenever CDS is in play — either a shipped archive or node-side
	// regeneration — so the archive maps rather than being silently rejected
	// under -Xshare:auto (see CDSModTime / https://github.com/brewlet/site/blob/main/docs/appcds.md §4.4).
	pinMtime := shipsCDS(cfg) || regen.Regenerate

	// Stage classpath dependency layers into a host dir bind-mounted at /app/lib.
	var libMount []ociMount
	if len(classpathTars) > 0 {
		libHost := filepath.Join(outDir, "lib")
		if err := StageClasspathLayers(classpathTars, libHost); err != nil {
			return err
		}
		if pinMtime {
			if err := PinCDSModTimesUnder(libHost); err != nil {
				return err
			}
		}
		libMount = []ociMount{{
			Destination: "/app/lib", Type: "bind",
			Source: libHost, Options: []string{"rbind", "ro"},
		}}
	}

	// Stage modulepath library-module layers into a host dir bind-mounted at
	// /app/mods, the module-path twin of /app/lib. See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
	var modsMount []ociMount
	if len(modulepathTars) > 0 {
		modsHost := filepath.Join(outDir, "mods")
		if err := StageModulepathLayers(modulepathTars, modsHost); err != nil {
			return err
		}
		if pinMtime {
			if err := PinCDSModTimesUnder(modsHost); err != nil {
				return err
			}
		}
		modsMount = []ociMount{{
			Destination: "/app/mods", Type: "bind",
			Source: modsHost, Options: []string{"rbind", "ro"},
		}}
	}

	// The app JAR is normally bind-mounted read-only straight from its host blob.
	// When CDS is in play (shipped archive or regeneration), mount a
	// canonical-mtime copy instead: the archive records the JAR's mtime and
	// HotSpot rejects it under -Xshare:auto if the on-disk mtime differs (see
	// CDSModTime), and a bind-mount inherits the source blob's non-deterministic
	// mtime.
	jarSource := jarHostPath
	if pinMtime {
		staged, err := StageCDSJar(jarHostPath, filepath.Join(outDir, "brewlet-app"), nonEmpty(cfg.MainJar, "app.jar"))
		if err != nil {
			return err
		}
		jarSource = staged
	}

	// Bind-mount the optional AppCDS archive read-only at /app/<archive> so the
	// -XX:SharedArchiveFile path BuildJVMArgs emitted resolves. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
	// Skipped when the deployment opts into node-side regeneration: there the
	// shipped archive is only seed data for the node cache (mounted at
	// InSandboxCDSDir instead).
	var cdsMount []ociMount
	if !regen.Regenerate && cdsHostPath != "" && cfg.CDS != nil && cfg.CDS.Archive != "" {
		cdsMount = []ociMount{{
			Destination: "/app/" + cfg.CDS.Archive, Type: "bind",
			Source: cdsHostPath, Options: []string{"rbind", "ro"},
		}}
	}

	// argv[0] is the launcher: "java" (vanilla, from /opt/jdk/bin) or a custom
	// launcher such as "jaz" resolved from the launcher layer on PATH.
	args := append([]string{artifact.LauncherName(launcherName)}, jvmArgs...)

	path := "/opt/jdk/bin:/usr/bin:/bin"
	var launcherMount []ociMount
	if !artifact.IsVanillaLauncher(launcherName) && launcherRoot != "" {
		// Overlay the launcher layer read-only and make it the first PATH entry.
		// The JDK root stays a generic, unmodified OpenJDK; the launcher is
		// composed in, not baked into the JDK image.
		launcherMount = []ociMount{{
			Destination: "/opt/brewlet/launcher", Type: "bind",
			Source: launcherRoot, Options: []string{"rbind", "ro"},
		}}
		path = "/opt/brewlet/launcher/bin:" + path
	}

	env := []string{
		"PATH=" + path,
		"JAVA_HOME=/opt/jdk",
	}
	for _, e := range cfg.Env {
		env = append(env, e.Name+"="+e.Value)
	}

	uid, gid := uint32(0), uint32(0)
	if cfg.User != nil {
		uid, gid = uint32(cfg.User.UID), uint32(cfg.User.GID)
	}

	spec := ociSpec{
		OCIVersion: "1.1.0",
		Process: ociProcess{
			User: ociUser{UID: uid, GID: gid},
			Args: args,
			Env:  env,
			Cwd:  "/app",
		},
		Root:     ociRoot{Path: "rootfs", Readonly: true},
		Hostname: "brewlet",
		Mounts: append([]ociMount{
			// Node-resident JDK mounted read-only (shared across all sandboxes).
			{Destination: "/opt/jdk", Type: "bind", Source: jdkRoot, Options: []string{"rbind", "ro"}},
			// The JAR payload from the artifact, read-only.
			{Destination: inSandboxJar, Type: "bind", Source: jarSource, Options: []string{"rbind", "ro"}},
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			// cgroup2 mounted read-only so the container-aware JVM can read its
			// own cpu.max/memory.max (via the cgroup namespace) and size the heap
			// and GC/JIT thread pools from the sandbox limits — exactly as it does
			// in an ordinary container. Without this the JVM sees the host's
			// resources and mis-sizes itself.
			{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev"}},
		}, append(append(append(append(libMount, modsMount...), cdsMount...), cdsCacheMount...), launcherMount...)...),
		Linux: ociLinux{
			// PoC omits the "network" namespace so it shares the host netns and
			// can be curled directly. The real shim instead JOINS the pod netns
			// that the kubelet/CNI prepared (so the JVM gets a normal pod IP).
			Namespaces: []ociNamespace{
				{Type: "pid"}, {Type: "ipc"}, {Type: "uts"},
				{Type: "mount"}, {Type: "cgroup"},
			},
			Resources: buildResources(res),
		},
	}

	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "config.json"), b, 0o644)
}

func buildResources(res Resources) ociResources {
	out := ociResources{}
	if res.MemoryLimit != "" {
		if bytes, err := parseMemory(res.MemoryLimit); err == nil {
			out.Memory = &ociMemory{Limit: &bytes}
		}
	}
	if res.CPULimit != "" {
		if millis, err := parseCPU(res.CPULimit); err == nil {
			period := uint64(100000)
			quota := int64(math.Round(float64(millis) / 1000.0 * float64(period)))
			out.CPU = &ociCPU{Quota: &quota, Period: &period}
		}
	}
	return out
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
