//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	apitypes "github.com/containerd/containerd/api/types"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	runtimeoptions "github.com/containerd/containerd/pkg/runtimeoptions/v1"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/runtime/v2/runc/task"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/brewlet/brewlet/internal/artifact"
	kcruntime "github.com/brewlet/brewlet/internal/runtime"
)

// Node-level locations the provisioner (https://github.com/brewlet/brewlet/tree/main/specs §5.2) materializes on
// every opted-in node. Overridable via env for testing / non-standard layouts.
const (
	defaultJDKRootsDir      = "/opt/brewlet/jdks"
	defaultLauncherRootsDir = "/opt/brewlet/launchers"
)

// CRI stamps the container role onto every OCI spec it hands the shim. The pod
// sandbox (the "pause" container) carries annContainerType == sandbox; only the
// workload containers should be rewritten into a `java -jar` launch. See
// containerd's pkg/cri/annotations.
const (
	annContainerType     = "io.kubernetes.cri.container-type"
	containerTypeSandbox = "sandbox"
)

func init() {
	// Register the Brewlet task service as the shim's TTRPC "task" plugin. The
	// containerd shim framework (shim.RunManager -> run) walks the plugin graph
	// and serves whatever implements RegisterTTRPC as the Task service.
	plugin.Register(&plugin.Registration{
		Type: plugin.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugin.EventPlugin,
			plugin.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			pp, err := ic.GetByID(plugin.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(plugin.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			inner, err := task.NewTaskService(ic.Context, pp.(shim.Publisher), ss.(shutdown.Service))
			if err != nil {
				return nil, err
			}
			return &brewletTaskService{TaskService: inner}, nil
		},
	})
}

// brewletTaskService decorates containerd's runc-backed Task service. Every
// method (State/Start/Kill/Delete/Exec/Wait/…) is delegated to the embedded
// runc implementation, so `kubectl logs/exec`, probes, signals and Services all
// behave exactly like an ordinary container (https://github.com/brewlet/brewlet/tree/main/specs §6.3). The one
// Brewlet-specific step is Create(): before runc ever sees the bundle, we
// disassemble the OCI artifact and rewrite the OCI spec into a `java -jar`
// sandbox backed by the node-resident JDK.
type brewletTaskService struct {
	taskAPI.TaskService
}

// RegisterTTRPC binds THIS decorator (not the embedded runc service) so
// containerd dispatches Create() to our artifact-assembly hook.
func (s *brewletTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, s)
	return nil
}

// Create assembles the Brewlet bundle in place, then hands off to the runc task
// service which performs the real `runc create` (rootfs mounts, cgroups,
// namespaces). This is §6.1 steps 1-4; step 5 (delegate to runc) is the inner
// Create.
func (s *brewletTaskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	// containerd's CRI plugin only emits the runc-native options.Options for the
	// built-in runc runtime types; for a custom runtime handler like ours it
	// wraps the runtime config in a generic runtimeoptions.Options. The embedded
	// runc task service unconditionally type-asserts *options.Options, so we
	// must translate before delegating or the shim panics
	// ("interface conversion: *runtimeoptions_v1.Options, not *options.Options")
	// and no brewlet pod can start under kubelet/CRI.
	if err := normalizeRuncOptions(r); err != nil {
		return nil, err
	}
	// The pod sandbox (pause) container inherits the pod's brewlet.sh/*
	// annotations but must NOT be rewritten into a JVM launch — it just holds
	// the pod's namespaces. Only workload containers get the artifact treatment.
	sandbox, err := isSandboxBundle(r.Bundle)
	if err != nil {
		return nil, err
	}
	if !sandbox {
		if err := assembleBrewletBundle(r); err != nil {
			return nil, err
		}
	}
	return s.TaskService.Create(ctx, r)
}

// isSandboxBundle reports whether the OCI spec at <bundle>/config.json belongs
// to a CRI pod sandbox (the pause container). Non-CRI callers (the ctr / e2e
// harness paths) leave the annotation unset, so those bundles are always treated
// as workload containers and get the brewlet rewrite.
func isSandboxBundle(bundle string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return false, fmt.Errorf("read oci spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, fmt.Errorf("parse oci spec: %w", err)
	}
	return spec.Annotations[annContainerType] == containerTypeSandbox, nil
}

// normalizeRuncOptions rewrites a generic CRI runtimeoptions.Options request
// into the runc-native options.Options the embedded task service expects,
// preserving the runc knobs the provisioner configures on the brewlet runtime
// (notably SystemdCgroup, which must match the kubelet cgroup driver). Requests
// that already carry runc options, or none at all, are left untouched.
func normalizeRuncOptions(r *taskAPI.CreateTaskRequest) error {
	if r.Options == nil || r.Options.GetValue() == nil {
		return nil
	}
	v, err := typeurl.UnmarshalAny(r.Options)
	if err != nil {
		return fmt.Errorf("decode runtime options: %w", err)
	}
	switch o := v.(type) {
	case *runcoptions.Options:
		return nil
	case *runtimeoptions.Options:
		runcOpts, err := runcOptionsFromGeneric(o)
		if err != nil {
			return err
		}
		any, err := anypb.New(runcOpts)
		if err != nil {
			return fmt.Errorf("encode runc options: %w", err)
		}
		r.Options = any
		return nil
	default:
		return nil
	}
}

// runcOptionsFromGeneric decodes the runc option knobs the CRI plugin preserved
// in a generic runtimeoptions.Options. When no ConfigPath is set, containerd
// stashes the runtime's TOML options section verbatim in ConfigBody; otherwise
// it points at a file. Either way the keys mirror runc's options.Options fields.
func runcOptionsFromGeneric(o *runtimeoptions.Options) (*runcoptions.Options, error) {
	body := o.GetConfigBody()
	if len(body) == 0 && o.GetConfigPath() != "" {
		b, err := os.ReadFile(o.GetConfigPath())
		if err != nil {
			return nil, fmt.Errorf("read runtime config %q: %w", o.GetConfigPath(), err)
		}
		body = b
	}
	opts := &runcoptions.Options{}
	if len(body) == 0 {
		return opts, nil
	}
	var cfg struct {
		BinaryName    string
		Root          string
		SystemdCgroup bool
		NoPivotRoot   bool
		NoNewKeyring  bool
	}
	if err := toml.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse runc runtime options: %w", err)
	}
	opts.BinaryName = cfg.BinaryName
	opts.Root = cfg.Root
	opts.SystemdCgroup = cfg.SystemdCgroup
	opts.NoPivotRoot = cfg.NoPivotRoot
	opts.NoNewKeyring = cfg.NoNewKeyring
	return opts, nil
}

// assembleBrewletBundle rewrites <bundle>/config.json (the OCI spec containerd's
// CRI layer wrote from the pod) into the Brewlet launch: it resolves the JAR
// artifact, selects the node JDK/launcher, and injects the `java -jar` process,
// JDK/JAR mounts and JVM env — while preserving everything CRI set up
// (namespaces incl. the CNI-provided network namespace, cgroup resources, user,
// and standard pod mounts).
func assembleBrewletBundle(r *taskAPI.CreateTaskRequest) error {
	specPath := filepath.Join(r.Bundle, "config.json")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("read oci spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("parse oci spec: %w", err)
	}

	ref := artifactRef(&spec)
	if ref == "" {
		return fmt.Errorf("no Brewlet artifact reference on task %q (expected annotation %q)", r.ID, annArtifactRef)
	}

	ic := imageConfig{
		StoreRoot:        envOr("BREWLET_STORE_ROOT", ""),
		Ref:              ref,
		JDKRootsDir:      envOr("BREWLET_JDK_ROOTS", defaultJDKRootsDir),
		LauncherRootsDir: envOr("BREWLET_LAUNCHER_ROOTS", defaultLauncherRootsDir),
		// The JDK/launcher the deployment descriptor requested, carried on the
		// pod annotations and propagated onto the OCI spec. The artifact config
		// no longer carries these — the descriptor is the single source of truth.
		JDKRequest:   spec.Annotations[annRequestedJDK],
		LauncherName: spec.Annotations[annRequestedLauncher],
		// Content-store resolution: read the artifact straight from containerd's
		// own content store by manifest digest. Selected by default (no
		// BREWLET_STORE_ROOT); the PoC harness sets BREWLET_STORE_ROOT to fall
		// back to the Brewlet-local OCI layout.
		Backend:        envOr("BREWLET_STORE_BACKEND", ""),
		ContentRoot:    envOr("BREWLET_CONTENT_ROOT", defaultContentRoot),
		ManifestDigest: spec.Annotations[annArtifactDigest],
	}
	ra, err := resolveArtifact(ic)
	if err != nil {
		return err // NoCompatibleJDK / NoCompatibleLauncher surface as task-create failures
	}

	if err := applyBrewletLaunch(&spec, ra, r.Bundle); err != nil {
		return err
	}
	if err := setupOverlayRootfs(r, ra); err != nil {
		return err
	}
	if err := mountClasspathLayers(&spec, ra, r.Bundle); err != nil {
		return err
	}
	if err := mountModulepathLayers(&spec, ra, r.Bundle); err != nil {
		return err
	}
	spec.Root = &specs.Root{Path: "rootfs", Readonly: false}

	out, err := json.MarshalIndent(&spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(specPath, out, 0o644)
}

// setupOverlayRootfs implements §6.1 step 3: the sandbox rootfs is an overlay
// whose read-only lower layer is the shared node JDK runtime root and whose
// upper/work layers are per-container writable scratch. containerd's runc
// container setup mounts r.Rootfs onto <bundle>/rootfs, so replacing the
// (nonexistent, for an OCI artifact) snapshot mounts with this overlay gives the
// JVM a writable root backed by the shared JDK userland — no container image or
// snapshot required, exactly the KWasm property.
func setupOverlayRootfs(r *taskAPI.CreateTaskRequest, ra resolvedArtifact) error {
	scratch := filepath.Join(r.Bundle, "brewlet")
	upper := filepath.Join(scratch, "upper")
	work := filepath.Join(scratch, "work")
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("overlay scratch: %w", err)
		}
	}
	// lowerdir is highest-priority first: a custom launcher layer (if any)
	// overlays the JDK userland so its binaries win on PATH.
	lowers := []string{ra.JDKRoot}
	if !artifact.IsVanillaLauncher(ra.LauncherName) && ra.LauncherRoot != "" {
		lowers = append([]string{ra.LauncherRoot}, lowers...)
	}
	r.Rootfs = []*apitypes.Mount{{
		Type:   "overlay",
		Source: "overlay",
		Options: []string{
			"lowerdir=" + strings.Join(lowers, ":"),
			"upperdir=" + upper,
			"workdir=" + work,
		},
	}}
	return nil
}

// applyBrewletLaunch mutates spec in place to run the JAR under the selected
// JDK, mirroring the mount/env/arg layout of runtime.GenerateBundleWithLauncher
// but leaving CRI's namespaces, resources and pod mounts untouched. bundleDir is
// the container bundle root (r.Bundle), used to stage a canonical-mtime copy of
// the app JAR when the artifact ships an AppCDS archive (see below).
func applyBrewletLaunch(spec *specs.Spec, ra resolvedArtifact, bundleDir string) error {
	mainJar := ra.Config.MainJar
	if mainJar == "" {
		mainJar = "app.jar"
	}
	inSandboxJar := "/app/" + mainJar

	// Node-side AppCDS regeneration is a deployment/fleet decision, carried on the
	// pod as brewlet.sh/cds-regenerate (set by the operator from
	// spec.jvm.cds.regenerate). Suppress the shipped-archive args when it is set;
	// the regen args are injected below.
	regenerate := spec.Annotations[annCDSRegenerate] == "true"

	jvmArgs, _ := kcruntime.BuildJVMArgs(ra.Config, inSandboxJar, nil, regenerate)

	// Node-side AppCDS regeneration (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3): when the deployment
	// opts in, resolve a per-(artifact, JDK-build) archive from the node cache and
	// prepend its -XX:+AutoCreateSharedArchive / -XX:SharedArchiveFile args. The
	// cache dir is bind-mounted at InSandboxCDSDir (rw only for the elected
	// writer), and a shipped archive becomes seed data rather than a /app mount.
	var cdsCacheMount []specs.Mount
	if regenerate {
		artifactKey := spec.Annotations[annArtifactDigest]
		if artifactKey == "" {
			artifactKey = artifactRef(spec)
		}
		if artifactKey != "" {
			cacheDir := envOr("BREWLET_CDS_CACHE", kcruntime.DefaultCDSCacheDir)
			seed := ""
			if ra.CDSHostPath != "" && ra.Config.CDS != nil && ra.Config.CDS.Archive != "" {
				seed = ra.CDSHostPath
			}
			dec, err := kcruntime.DecideCDSRegen(kcruntime.RegenParams{
				CacheDir:      cacheDir,
				JDKRoot:       ra.JDKHome,
				ArtifactKey:   artifactKey,
				SeedArchive:   seed,
				ArchiveArgDir: kcruntime.InSandboxCDSDir,
				MetricsDir:    envOr("BREWLET_METRICS_DIR", ""),
			})
			if err != nil {
				return err
			}
			if len(dec.Args) > 0 {
				jvmArgs = append(append([]string{}, dec.Args...), jvmArgs...)
			}
			if dec.Role == kcruntime.RegenConsume || dec.Role == kcruntime.RegenWrite {
				opts := []string{"rbind"}
				if dec.MountRW {
					opts = append(opts, "rw")
				} else {
					opts = append(opts, "ro")
				}
				cdsCacheMount = []specs.Mount{{
					Destination: kcruntime.InSandboxCDSDir,
					Type:        "bind",
					Source:      cacheDir,
					Options:     opts,
				}}
			}
		}
	}

	if spec.Process == nil {
		spec.Process = &specs.Process{}
	}
	spec.Process.Args = append([]string{artifact.LauncherName(ra.LauncherName)}, jvmArgs...)
	spec.Process.Cwd = "/app"
	if ra.Config.User != nil {
		spec.Process.User.UID = uint32(ra.Config.User.UID)
		spec.Process.User.GID = uint32(ra.Config.User.GID)
	}

	// PATH: prepend the custom launcher layer when present so e.g. `jaz`
	// resolves ahead of the JDK's own bin.
	path := "/opt/jdk/bin:/usr/bin:/bin"
	var launcherMount []specs.Mount
	if !artifact.IsVanillaLauncher(ra.LauncherName) && ra.LauncherRoot != "" {
		launcherMount = []specs.Mount{{
			Destination: "/opt/brewlet/launcher",
			Type:        "bind",
			Source:      ra.LauncherRoot,
			Options:     []string{"rbind", "ro"},
		}}
		path = "/opt/brewlet/launcher/bin:" + path
	}

	env := []string{"PATH=" + path, "JAVA_HOME=/opt/jdk"}
	for _, e := range ra.Config.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	spec.Process.Env = mergeEnv(spec.Process.Env, env)

	// The app JAR is normally bind-mounted read-only straight from its
	// content-store blob. When the artifact ships an AppCDS archive, mount a
	// canonical-mtime copy instead: HotSpot validates the archive against the
	// JAR's mtime and rejects it (silently, under -Xshare:auto) if it differs
	// from dump time, and the blob's mtime is the node's non-deterministic pull
	// time (see runtime.CDSModTime).
	jarSource := ra.JarHostPath
	if ra.Config.CDS != nil || regenerate {
		staged, err := kcruntime.StageCDSJar(ra.JarHostPath, filepath.Join(bundleDir, "brewlet", "app"), mainJar)
		if err != nil {
			return fmt.Errorf("stage cds jar: %w", err)
		}
		jarSource = staged
	}

	// The JDK runtime root doubles as the overlay rootfs lower layer
	// (setupOverlayRootfs), so it already supplies the base userland the JVM
	// links against — §5.3 requires it to be a self-contained minimal
	// userland+JDK. We bind the source image's declared Java home at /opt/jdk so
	// JAVA_HOME stays stable regardless of the runtime's internal image path.
	// The JAR payload is mounted read-only at /app.
	brewletMounts := []specs.Mount{
		{Destination: "/opt/jdk", Type: "bind", Source: ra.JDKHome, Options: []string{"rbind", "ro"}},
		{Destination: inSandboxJar, Type: "bind", Source: jarSource, Options: []string{"rbind", "ro"}},
	}
	// Optional AppCDS archive: bind-mount read-only at /app/<archive> so the
	// -XX:SharedArchiveFile path BuildJVMArgs emitted resolves. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
	// Skipped under node-side regeneration: there the shipped archive is only seed
	// data for the node cache (bind-mounted at InSandboxCDSDir instead).
	if !regenerate && ra.CDSHostPath != "" && ra.Config.CDS != nil && ra.Config.CDS.Archive != "" {
		brewletMounts = append(brewletMounts, specs.Mount{
			Destination: "/app/" + ra.Config.CDS.Archive,
			Type:        "bind",
			Source:      ra.CDSHostPath,
			Options:     []string{"rbind", "ro"},
		})
	}
	brewletMounts = append(brewletMounts, cdsCacheMount...)
	brewletMounts = append(brewletMounts, launcherMount...)
	spec.Mounts = append(spec.Mounts, brewletMounts...)
	return nil
}

// mountClasspathLayers implements the §6.1 rootfs step for layered-classpath
// deployment (https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md): it stages each optional
// classpath.layer.v1+tar into a per-container host dir and bind-mounts it
// read-only at /app/lib, so a `-cp /app/app.jar:/app/lib/*` launch resolves the
// dependency JARs. This mirrors runtime.GenerateBundleWithLauncher so the
// production shim and the CLI/harness bundle path behave identically. A no-op
// when the artifact carries no classpath layers.
func mountClasspathLayers(spec *specs.Spec, ra resolvedArtifact, bundleDir string) error {
	if len(ra.ClasspathHostPaths) == 0 {
		return nil
	}
	libHost := filepath.Join(bundleDir, "brewlet", "lib")
	if err := kcruntime.StageClasspathLayers(ra.ClasspathHostPaths, libHost); err != nil {
		return err
	}
	// Pin dependency-JAR mtimes to the canonical CDS value when the artifact
	// ships an AppCDS archive, so each classpath entry matches the archive's
	// recorded timestamps (see runtime.CDSModTime).
	if ra.Config.CDS != nil {
		if err := kcruntime.PinCDSModTimesUnder(libHost); err != nil {
			return err
		}
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/app/lib",
		Type:        "bind",
		Source:      libHost,
		Options:     []string{"rbind", "ro"},
	})
	return nil
}

// mountModulepathLayers is the module-path twin of mountClasspathLayers: it
// stages each optional modulepath.layer.v1+tar into a per-container host dir and
// bind-mounts it read-only at /app/mods, so a `-p /app/app.jar:/app/mods` launch
// resolves the library modules. This mirrors runtime.GenerateBundleWithLauncher
// so the production shim and the CLI/harness bundle path behave identically. A
// no-op when the artifact carries no modulepath layers. See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
func mountModulepathLayers(spec *specs.Spec, ra resolvedArtifact, bundleDir string) error {
	if len(ra.ModulepathHostPaths) == 0 {
		return nil
	}
	modsHost := filepath.Join(bundleDir, "brewlet", "mods")
	if err := kcruntime.StageModulepathLayers(ra.ModulepathHostPaths, modsHost); err != nil {
		return err
	}
	if ra.Config.CDS != nil {
		if err := kcruntime.PinCDSModTimesUnder(modsHost); err != nil {
			return err
		}
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/app/mods",
		Type:        "bind",
		Source:      modsHost,
		Options:     []string{"rbind", "ro"},
	})
	return nil
}

// mergeEnv overlays Brewlet's launch env onto whatever CRI provided: Brewlet's
// PATH/JAVA_HOME/launcher/app values win over pod defaults for the same key,
// but pod-specified variables Brewlet doesn't set are preserved.
func mergeEnv(base, overlay []string) []string {
	idx := make(map[string]int, len(base))
	out := append([]string(nil), base...)
	for i, kv := range out {
		if k := envKey(kv); k != "" {
			idx[k] = i
		}
	}
	for _, kv := range overlay {
		k := envKey(kv)
		if k == "" {
			continue
		}
		if i, ok := idx[k]; ok {
			out[i] = kv
		} else {
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

func envKey(kv string) string {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i]
		}
	}
	return kv
}

// artifactRef pulls the OCI artifact reference from the OCI spec annotations,
// preferring the Brewlet-native key and falling back to the CRI image name.
func artifactRef(spec *specs.Spec) string {
	if spec.Annotations != nil {
		if v := spec.Annotations[annArtifactRef]; v != "" {
			return v
		}
		if v := spec.Annotations[annCRIImage]; v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// compile-time check that the decorator still satisfies the Task service.
var _ taskAPI.TaskService = (*brewletTaskService)(nil)
