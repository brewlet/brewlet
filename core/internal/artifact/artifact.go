// Package artifact implements the Brewlet OCI application artifact format and a
// minimal local OCI image-layout store, so a developer can publish ONLY a JAR
// (plus a small launch config) without building a container image.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Media types that define the Brewlet application artifact (see https://github.com/brewlet/brewlet/tree/main/specs §4).
const (
	ArtifactType      = "application/vnd.brewlet.app.v1+json"
	ConfigMediaType   = "application/vnd.brewlet.jvm.config.v1+json"
	JarLayerMediaType = "application/vnd.brewlet.jar.layer.v1+jar"
	// ClasspathLayerMediaType marks an optional layer carrying a tar of extra
	// JARs (dependency layers) that the shim unpacks under /app/lib for
	// layered class-path deployment. See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md.
	ClasspathLayerMediaType = "application/vnd.brewlet.classpath.layer.v1+tar"
	// ModulepathLayerMediaType marks an optional layer carrying a tar of library
	// module JARs that the shim unpacks under /app/mods for modular (JPMS)
	// deployment; the directory becomes part of the `--module-path`. It is the
	// module-path twin of ClasspathLayerMediaType. See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
	ModulepathLayerMediaType = "application/vnd.brewlet.modulepath.layer.v1+tar"
	// CDSLayerMediaType marks an optional layer carrying a single Application
	// Class-Data Sharing archive (`.jsa`) that the shim bind-mounts read-only at
	// /app/<archive>; launch then adds `-Xshare:auto -XX:SharedArchiveFile=…` to
	// speed startup. It is best-effort seed data: a build/version/classpath
	// mismatch falls back to base CDS rather than failing. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
	CDSLayerMediaType = "application/vnd.brewlet.cds.layer.v1+jsa"

	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	refNameAnnotation    = "org.opencontainers.image.ref.name"
	titleAnnotation      = "org.opencontainers.image.title"
)

// JVMConfig is the launch descriptor carried in the artifact's config blob.
//
// It carries only what the *application build* needs to launch correctly —
// never deployment concerns. The JDK feature/distribution and the launcher live
// solely in the deployment descriptor (the CRD's jvm.version/jvm.distribution/
// jvm.launcher, or the raw-Deployment pod annotations brewlet.sh/jdk and
// brewlet.sh/launcher). Likewise there is deliberately NO free-form JVM-args
// field: resource/environment tuning (heap, GC, agents, …) belongs in the
// descriptor's jvm.args. The only launch knobs here are the app-intrinsic
// correctness flags below (preview features, module-system access, system
// properties the code assumes). See https://github.com/brewlet/site/blob/main/docs/jdk-management.md and
// https://github.com/brewlet/site/blob/main/docs/resource-tuning.md.
type JVMConfig struct {
	SchemaVersion int `json:"schemaVersion"`
	// MainJar is the physical FILENAME the artifact's single primary JAR blob is
	// materialized as under /app (e.g. "orders.jar"). It is NOT the entry point in
	// classpath/module mode (mainClass/module are) — it names the file that
	// entry.classPath/entry.modulePath reference and that `-jar` launches in jar
	// mode. Optional in the JSON; the launch/bundle path defaults it to "app.jar".
	// The primary JAR always lives at /app top level; dependency layers unpack under
	// /app/lib (classpath) or /app/mods (module), never at the top level.
	MainJar string `json:"mainJar"`
	Entry   Entry  `json:"entry"`
	// EnablePreview adds --enable-preview when the bytecode was compiled with
	// preview features (the running JDK feature must match). App-intrinsic.
	EnablePreview bool `json:"enablePreview,omitempty"`
	// AddModules adds root modules not resolved by default (e.g. incubator or
	// reflectively-used modules) via --add-modules. App-intrinsic.
	AddModules []string `json:"addModules,omitempty"`
	// AddOpens opens packages for deep reflection via --add-opens; each entry is
	// a "<module>/<package>=<target>" token (e.g.
	// "java.base/java.lang=ALL-UNNAMED") required by the app or its libraries.
	AddOpens []string `json:"addOpens,omitempty"`
	// AddExports exports packages via --add-exports; same token form as AddOpens.
	AddExports []string `json:"addExports,omitempty"`
	// SystemProperties are expanded (sorted by key) into -D<key>=<value> flags
	// the application assumes at startup.
	SystemProperties map[string]string `json:"systemProperties,omitempty"`
	User             *User             `json:"user,omitempty"`
	Env              []EnvVar          `json:"env,omitempty"`
	// Arch is an OPTIONAL architecture constraint for NON-portable artifacts —
	// those bundling JNI native libraries or arch-specific dependencies (e.g.
	// netty-tcnative, RocksDB, some crypto libs) that only run on the arch(es)
	// whose natives were bundled. Each entry is a GOARCH / kubernetes.io/arch
	// token ("amd64" or "arm64"). When set, the admission webhook steers the pod
	// onto matching nodes (kubernetes.io/arch In […]) and denies with
	// NoCompatibleArch when no ready node of a required arch exists.
	//
	// Leave it UNSET for the common case: a pure-bytecode JAR is
	// architecture-neutral and runs unchanged on any provisioned arch, so an
	// empty Arch means "runs anywhere" (today's default behavior). See
	// https://github.com/brewlet/site/blob/main/docs/multi-arch.md.
	Arch []string `json:"arch,omitempty"`
	// CDS is an OPTIONAL Application Class-Data Sharing hint. When set, the
	// artifact ships a `.jsa` archive (as a cds.layer.v1+jsa layer) that the shim
	// mounts read-only at /app/<archive>, and launch adds
	// `-Xshare:auto -XX:SharedArchiveFile=/app/<archive>` to cut class-load time.
	// It is a best-effort startup accelerator, never a correctness or scheduling
	// constraint: the archive is bound to the exact JDK build + classpath layout,
	// and `-Xshare:auto` makes any mismatch fall back to base CDS instead of
	// failing. Leave it UNSET for the common case. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
	CDS *CDS `json:"cds,omitempty"`
}

// CDS carries the optional Application Class-Data Sharing archive hint (see
// JVMConfig.CDS and https://github.com/brewlet/site/blob/main/docs/appcds.md).
type CDS struct {
	// Archive is the bare filename the `.jsa` archive is materialized as under
	// /app (e.g. "app.jsa"). It must be a plain filename — no path separators,
	// no "..", no wildcard — since the archive always lives at the /app top level
	// beside the primary JAR.
	Archive string `json:"archive"`
	// Mode records how the archive was produced ("dynamic" via
	// -XX:ArchiveClassesAtExit, or "static" via -Xshare:dump). It is
	// informational only — consumption is identical either way — and may be
	// omitted. See https://github.com/brewlet/site/blob/main/docs/appcds.md §2.
	Mode string `json:"mode,omitempty"`
}

// KnownCDSModes are the recognized CDS archive-production modes for the optional
// JVMConfig.CDS hint. An empty mode is also accepted (unspecified).
var KnownCDSModes = map[string]struct{}{
	"dynamic": {},
	"static":  {},
}

// KnownArches are the architecture tokens Brewlet recognizes for the optional
// JVMConfig.Arch constraint. They match Go's GOARCH and the kubernetes.io/arch
// node-label values the admission webhook steers on.
var KnownArches = map[string]struct{}{
	"amd64": {},
	"arm64": {},
}

// IsKnownArch reports whether s is a recognized architecture token (see
// KnownArches).
func IsKnownArch(s string) bool {
	_, ok := KnownArches[s]
	return ok
}

// VanillaLauncher is the built-in OpenJDK launcher name. An empty or "java"
// launcher request means the stock `java` binary from the selected JDK, which
// every ready node provides and which needs no launcher layer.
const VanillaLauncher = "java"

// IsVanillaLauncher reports whether a launcher request names the stock OpenJDK
// `java` launcher (empty or "java"), as opposed to a custom node-installed
// launcher such as "jaz".
func IsVanillaLauncher(name string) bool {
	return name == "" || name == VanillaLauncher
}

// LauncherName resolves a launcher request to the launcher binary name,
// defaulting to the vanilla "java" launcher for an empty request.
func LauncherName(name string) string {
	if IsVanillaLauncher(name) {
		return VanillaLauncher
	}
	return name
}

type Entry struct {
	// Mode selects how the JVM launches the application:
	//   "jar"       => java -jar <mainJar>            (main class from the JAR manifest)
	//   "classpath" => java -cp <classPath> <mainClass>
	//   "module"    => java [-cp <classPath>] -p <modulePath> -m <module>[/<mainClass>]  (JPMS)
	// In "module" mode entry.classPath is optional: when set it adds a supplementary
	// class path (`-cp`) alongside the module path (`-p`) for mixed modular apps that
	// carry automatic-module or non-modular libraries on the class path. See
	// https://github.com/brewlet/site/blob/main/docs/jpms-support.md and https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md §8.
	Mode string `json:"mode"`
	// MainClass is the fully-qualified entry-point class. It is required in
	// "classpath" mode and ignored in "jar" mode, where the JAR manifest's
	// Main-Class is authoritative. In "module" mode it is optional and selects
	// the module's main class (<module>/<mainClass>); omit it when the module
	// declares its own Main-Class.
	MainClass string `json:"mainClass,omitempty"`
	// ClassPath is an optional, ordered list of /app-relative class-path entries
	// (e.g. ["app.jar", "lib/*"]). Entries are joined with the node path separator,
	// in order, and passed to `java -cp`. The "lib/*" wildcard is expanded by the
	// JVM launcher. It is used in "classpath" mode (where, when empty, launch falls
	// back to the single MainJar) and, optionally, in "module" mode to add a
	// supplementary class path next to the module path (the mixed form). See
	// https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md.
	ClassPath []string `json:"classPath,omitempty"`
	// Module is the root module name launched when Mode == "module" (the `-m`
	// argument). Required in "module" mode; forbidden in the other modes.
	Module string `json:"module,omitempty"`
	// ModulePath is an optional, ordered list of /app-relative module-path entries
	// used when Mode == "module" (e.g. ["orders.jar", "mods"]). It is the
	// module-path twin of ClassPath: entries are joined with the node path
	// separator, in order, and passed to `java -p`. When empty, module mode falls
	// back to the single MainJar (the single modular JAR case). A directory entry
	// (e.g. "mods", from a modulepath layer) contributes every JAR it contains.
	// See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
	ModulePath []string `json:"modulePath,omitempty"`
}

type User struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Validate enforces launch-config consistency: each entry mode OWNS a specific
// set of fields, and fields foreign to the selected mode are rejected rather
// than silently ignored (which otherwise surfaces as a confusing JVM error at
// runtime). It is called at publish time (PushWithLayers), at load time
// (Resolve/DecodeConfig), and at launch time (BuildJVMArgs).
func (c JVMConfig) Validate() error {
	e := c.Entry
	switch e.Mode {
	case "", "jar":
		if e.MainClass != "" {
			return fmt.Errorf("entry.mode=jar does not use entry.mainClass: `java -jar` launches the JAR manifest's Main-Class. Set entry.mode=classpath to launch a specific main class via -cp")
		}
		if len(e.ClassPath) > 0 {
			return fmt.Errorf("entry.mode=jar does not use entry.classPath. Set entry.mode=classpath for layered class-path deployment")
		}
		if e.Module != "" || len(e.ModulePath) > 0 {
			return fmt.Errorf("entry.mode=jar does not use entry.module/entry.modulePath. Set entry.mode=module to launch on the module path")
		}
	case "classpath":
		if e.MainClass == "" {
			return fmt.Errorf("entry.mode=classpath requires entry.mainClass")
		}
		if e.Module != "" || len(e.ModulePath) > 0 {
			return fmt.Errorf("entry.mode=classpath does not use entry.module/entry.modulePath. Set entry.mode=module to launch on the module path")
		}
	case "module":
		if e.Module == "" {
			return fmt.Errorf("entry.mode=module requires entry.module (the root module name for `java -m`)")
		}
		// entry.classPath IS permitted here: a modular (JPMS) app frequently needs
		// both a module path (`-p`) and a supplementary class path (`-cp`) — e.g.
		// automatic-module or non-modular libraries carried on the class path. The
		// mixed form launches `java -cp <classPath> -p <modulePath> -m <module>`.
		// See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md §8.
	default:
		return fmt.Errorf("unknown entry.mode %q (expected \"jar\", \"classpath\", or \"module\")", e.Mode)
	}
	// App-intrinsic launch knobs: reject blank tokens/keys early so a malformed
	// config fails at publish time rather than as an opaque JVM error at launch.
	for _, v := range c.AddModules {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("addModules entries must be non-empty module names")
		}
	}
	for _, v := range c.AddOpens {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("addOpens entries must be non-empty \"<module>/<package>=<target>\" tokens")
		}
	}
	for _, v := range c.AddExports {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("addExports entries must be non-empty \"<module>/<package>=<target>\" tokens")
		}
	}
	for k := range c.SystemProperties {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("systemProperties keys must be non-empty")
		}
	}
	// Arch is an optional constraint for non-portable artifacts; when present it
	// must list only recognized arch tokens (see KnownArches). An empty/unset
	// Arch means "runs anywhere" and is the common case.
	seenArch := map[string]struct{}{}
	for _, a := range c.Arch {
		if !IsKnownArch(a) {
			return fmt.Errorf("arch entry %q is not a recognized architecture (expected %q or %q)", a, "amd64", "arm64")
		}
		if _, dup := seenArch[a]; dup {
			return fmt.Errorf("arch entry %q is duplicated", a)
		}
		seenArch[a] = struct{}{}
	}
	// Dangling top-level JAR reference: a bare `<name>.jar` entry (no directory,
	// no wildcard) in classPath/modulePath can only be satisfied by the primary
	// JAR, since dependency layers unpack under /app/lib or /app/mods — never at
	// the /app top level. So when MainJar is set, any such entry must equal it;
	// otherwise it points at a file that will never exist at launch (a common
	// copy-paste slip, e.g. classPath ["app.jar"] with mainJar "orders.jar").
	if c.MainJar != "" {
		for _, e := range append(append([]string{}, c.Entry.ClassPath...), c.Entry.ModulePath...) {
			if isBareJarRef(e) && e != c.MainJar {
				return fmt.Errorf("entry references top-level JAR %q, but the primary JAR is %q: a bare *.jar entry in classPath/modulePath can only be the main JAR (dependency layers unpack under lib/ or mods/). Use %q, or place the file in a dependency layer and reference it under lib/ or mods/", e, c.MainJar, c.MainJar)
			}
		}
	}
	// CDS archive hint: when present, the archive must be a bare filename that
	// resolves to /app/<archive> (dependency layers unpack under lib/ or mods/;
	// the archive always lives at the /app top level). Reject path separators,
	// parent refs and wildcards so a malformed hint fails at publish time rather
	// than as an opaque JVM error at launch. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
	if c.CDS != nil {
		a := strings.TrimSpace(c.CDS.Archive)
		if a == "" {
			return fmt.Errorf("cds.archive must be a non-empty archive filename (e.g. \"app.jsa\")")
		}
		if a != c.CDS.Archive || strings.ContainsAny(a, "/*") || a == ".." || strings.Contains(a, string(os.PathSeparator)) {
			return fmt.Errorf("cds.archive %q must be a bare filename (no path separator, no wildcard): the archive is mounted at /app/<archive>", c.CDS.Archive)
		}
		if _, ok := KnownCDSModes[c.CDS.Mode]; c.CDS.Mode != "" && !ok {
			return fmt.Errorf("cds.mode %q is not recognized (expected \"dynamic\", \"static\", or omitted)", c.CDS.Mode)
		}
	}
	return nil
}

// JAR filename — ends in ".jar", with no path separator and no wildcard. Such an
// entry can only resolve to the primary JAR at /app top level (see Validate).
// Directory entries ("mods"), wildcards ("lib/*") and nested paths ("lib/x.jar")
// are not bare refs.
func isBareJarRef(entry string) bool {
	if !strings.HasSuffix(entry, ".jar") {
		return false
	}
	return !strings.ContainsAny(entry, "/*")
}

// DecodeConfig parses a launch-config blob with strict field checking: unknown
// fields are rejected (catching typos and fields foreign to the schema, e.g. a
// stray "modPath" instead of "modulePath") and the result is validated for
// mode/field consistency (which also rejects schema fields foreign to the
// selected entry mode).
func DecodeConfig(b []byte) (JVMConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var cfg JVMConfig
	if err := dec.Decode(&cfg); err != nil {
		return JVMConfig{}, fmt.Errorf("decode launch config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return JVMConfig{}, err
	}
	return cfg, nil
}

// ---- OCI descriptor / manifest / index ----

type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	Platform     *Platform         `json:"platform,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Platform names the OS/arch a runnable-image manifest targets. It appears on
// an image index's manifest descriptors so containerd/kubelet can select the
// entry matching the node (see image.go / docs on the runnable-image mode).
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	ArtifactType  string       `json:"artifactType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
	// Annotations carries manifest-level metadata. For a runnable OCI image
	// (image.go) it holds the Brewlet launch config JSON under
	// JVMConfigAnnotation, since a runnable image's config blob is a standard
	// OCI image config rather than the Brewlet jvm-config blob.
	Annotations map[string]string `json:"annotations,omitempty"`
}

type Index struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
}

// Store is an on-disk OCI image layout acting as our "registry" for the PoC.
type Store struct{ Root string }

func (s Store) blobsDir() string { return filepath.Join(s.Root, "blobs", "sha256") }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum)
}

func (s Store) writeBlob(b []byte) (Descriptor, error) {
	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		return Descriptor{}, err
	}
	d := digestOf(b)
	if err := os.WriteFile(filepath.Join(s.blobsDir(), d[len("sha256:"):]), b, 0o644); err != nil {
		return Descriptor{}, err
	}
	return Descriptor{Digest: d, Size: int64(len(b))}, nil
}

// ReadBlob returns the raw bytes of a blob by digest ("sha256:...").
func (s Store) ReadBlob(digest string) ([]byte, error) {
	return os.ReadFile(s.BlobPath(digest))
}

// BlobPath returns the on-disk path of a blob (used to mount the JAR directly).
func (s Store) BlobPath(digest string) string {
	return filepath.Join(s.blobsDir(), digest[len("sha256:"):])
}

// Push writes the config + JAR layer + manifest and tags it as ref in index.json.
// It is equivalent to PushWithLayers with no classpath layers.
func (s Store) Push(ref string, cfg JVMConfig, jarPath string) (Descriptor, error) {
	return s.PushWithLayers(ref, cfg, jarPath, nil)
}

// PushWithLayers writes the config, the main JAR layer, and zero or more optional
// classpath layers (tars of dependency JARs, in order), then tags the manifest as
// ref in index.json. The classpath layers are appended to Manifest.Layers after
// the JAR layer, each as its own blob so the registry and node content store dedup
// them by digest. See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md.
func (s Store) PushWithLayers(ref string, cfg JVMConfig, jarPath string, classpathTars []string) (Descriptor, error) {
	return s.PushWithTypedLayers(ref, cfg, jarPath, classpathTars, nil)
}

// PushWithTypedLayers writes the config, the main JAR layer, then optional
// classpath layers (unpacked to /app/lib, fed to `-cp`) and optional modulepath
// layers (unpacked to /app/mods, fed to `-p`), in that order, and tags the
// manifest as ref. Each tar becomes its own blob so registry/content stores dedup
// by digest. See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md and https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
func (s Store) PushWithTypedLayers(ref string, cfg JVMConfig, jarPath string, classpathTars, modulepathTars []string) (Descriptor, error) {
	return s.PushWithCDS(ref, cfg, jarPath, classpathTars, modulepathTars, "")
}

// PushWithCDS is PushWithTypedLayers with an optional Application Class-Data
// Sharing archive. When cdsArchivePath is non-empty the `.jsa` file it names is
// written as a single cds.layer.v1+jsa layer (appended after the JAR/classpath/
// modulepath layers), and the launch config MUST carry a matching cds.archive
// hint (whose basename equals the archive's basename) so the shim mounts it at
// /app/<archive> and launch adds `-Xshare:auto -XX:SharedArchiveFile=…`. Pass ""
// for the common no-CDS case. See https://github.com/brewlet/site/blob/main/docs/appcds.md.
func (s Store) PushWithCDS(ref string, cfg JVMConfig, jarPath string, classpathTars, modulepathTars []string, cdsArchivePath string) (Descriptor, error) {
	if err := cfg.Validate(); err != nil {
		return Descriptor{}, fmt.Errorf("invalid launch config: %w", err)
	}
	if err := validateCDSPairing(cfg, cdsArchivePath); err != nil {
		return Descriptor{}, err
	}
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Descriptor{}, err
	}
	cfgDesc, err := s.writeBlob(cfgBytes)
	if err != nil {
		return Descriptor{}, err
	}
	cfgDesc.MediaType = ConfigMediaType

	jarBytes, err := os.ReadFile(jarPath)
	if err != nil {
		return Descriptor{}, fmt.Errorf("read jar: %w", err)
	}
	jarDesc, err := s.writeBlob(jarBytes)
	if err != nil {
		return Descriptor{}, err
	}
	jarDesc.MediaType = JarLayerMediaType
	jarDesc.Annotations = map[string]string{titleAnnotation: cfg.MainJar}

	layers := []Descriptor{jarDesc}
	if layers, err = s.appendTarLayers(layers, classpathTars, ClasspathLayerMediaType, "classpath"); err != nil {
		return Descriptor{}, err
	}
	if layers, err = s.appendTarLayers(layers, modulepathTars, ModulepathLayerMediaType, "modulepath"); err != nil {
		return Descriptor{}, err
	}
	if cdsArchivePath != "" {
		cdsBytes, err := os.ReadFile(cdsArchivePath)
		if err != nil {
			return Descriptor{}, fmt.Errorf("read cds archive %q: %w", cdsArchivePath, err)
		}
		cdsDesc, err := s.writeBlob(cdsBytes)
		if err != nil {
			return Descriptor{}, err
		}
		cdsDesc.MediaType = CDSLayerMediaType
		cdsDesc.Annotations = map[string]string{titleAnnotation: filepath.Base(cdsArchivePath)}
		layers = append(layers, cdsDesc)
	}

	m := Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		ArtifactType:  ArtifactType,
		Config:        cfgDesc,
		Layers:        layers,
	}
	mBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Descriptor{}, err
	}
	mDesc, err := s.writeBlob(mBytes)
	if err != nil {
		return Descriptor{}, err
	}
	mDesc.MediaType = ociManifestMediaType
	mDesc.ArtifactType = ArtifactType
	mDesc.Annotations = map[string]string{refNameAnnotation: ref}

	if err := s.upsertIndex(mDesc); err != nil {
		return Descriptor{}, err
	}
	if err := s.writeLayoutMarker(); err != nil {
		return Descriptor{}, err
	}
	return mDesc, nil
}

// validateCDSPairing enforces the CDS layer <-> config invariant: an archive
// file may only be shipped alongside a matching cds.archive hint, and a config
// declaring a cds.archive must be given an archive to ship. This keeps the
// on-disk archive filename and the launch config's expected /app/<archive> in
// lockstep so the shim mount and the -XX:SharedArchiveFile path agree.
func validateCDSPairing(cfg JVMConfig, cdsArchivePath string) error {
	if cdsArchivePath == "" {
		if cfg.CDS != nil {
			return fmt.Errorf("launch config declares cds.archive %q but no CDS archive file was provided to ship", cfg.CDS.Archive)
		}
		return nil
	}
	if cfg.CDS == nil {
		return fmt.Errorf("a CDS archive file was provided but the launch config has no cds.archive hint")
	}
	if base := filepath.Base(cdsArchivePath); base != cfg.CDS.Archive {
		return fmt.Errorf("CDS archive filename %q does not match cds.archive %q (they must agree so the archive maps to /app/%s)", base, cfg.CDS.Archive, cfg.CDS.Archive)
	}
	return nil
}

func (s Store) writeLayoutMarker() error {
	return os.WriteFile(filepath.Join(s.Root, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`+"\n"), 0o644)
}

// appendTarLayers reads each tar in tars, writes it as its own blob with the
// given media type, and appends the resulting descriptors to layers. kind names
// the layer type for error messages (e.g. "classpath", "modulepath").
func (s Store) appendTarLayers(layers []Descriptor, tars []string, mediaType, kind string) ([]Descriptor, error) {
	for _, tarPath := range tars {
		tarBytes, err := os.ReadFile(tarPath)
		if err != nil {
			return nil, fmt.Errorf("read %s layer %q: %w", kind, tarPath, err)
		}
		d, err := s.writeBlob(tarBytes)
		if err != nil {
			return nil, err
		}
		d.MediaType = mediaType
		d.Annotations = map[string]string{titleAnnotation: filepath.Base(tarPath)}
		layers = append(layers, d)
	}
	return layers, nil
}

func (s Store) indexPath() string { return filepath.Join(s.Root, "index.json") }

func (s Store) upsertIndex(desc Descriptor) error {
	idx := Index{SchemaVersion: 2, MediaType: "application/vnd.oci.image.index.v1+json"}
	if b, err := os.ReadFile(s.indexPath()); err == nil {
		_ = json.Unmarshal(b, &idx)
	}
	ref := desc.Annotations[refNameAnnotation]
	out := idx.Manifests[:0]
	for _, m := range idx.Manifests {
		if m.Annotations[refNameAnnotation] != ref {
			out = append(out, m)
		}
	}
	idx.Manifests = append(out, desc)
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath(), b, 0o644)
}

// Resolve returns the manifest and JVMConfig for a tagged ref.
func (s Store) Resolve(ref string) (Manifest, JVMConfig, error) {
	var idx Index
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		return Manifest{}, JVMConfig{}, fmt.Errorf("read index: %w", err)
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return Manifest{}, JVMConfig{}, err
	}
	for _, m := range idx.Manifests {
		if m.Annotations[refNameAnnotation] == ref {
			mb, err := s.ReadBlob(m.Digest)
			if err != nil {
				return Manifest{}, JVMConfig{}, err
			}
			var man Manifest
			if err := json.Unmarshal(mb, &man); err != nil {
				return Manifest{}, JVMConfig{}, err
			}
			cb, err := s.ReadBlob(man.Config.Digest)
			if err != nil {
				return Manifest{}, JVMConfig{}, err
			}
			cfg, err := DecodeConfig(cb)
			if err != nil {
				return Manifest{}, JVMConfig{}, err
			}
			return man, cfg, nil
		}
	}
	return Manifest{}, JVMConfig{}, fmt.Errorf("ref %q not found in store %s", ref, s.Root)
}

// JarLayer returns the descriptor of the JAR payload layer.
func (m Manifest) JarLayer() (Descriptor, error) {
	for _, l := range m.Layers {
		if l.MediaType == JarLayerMediaType {
			return l, nil
		}
	}
	return Descriptor{}, fmt.Errorf("manifest has no %s layer", JarLayerMediaType)
}

// ClasspathLayers returns the descriptors of the optional classpath (dependency)
// layers, in manifest order. Empty when the artifact ships a single JAR.
func (m Manifest) ClasspathLayers() []Descriptor {
	var out []Descriptor
	for _, l := range m.Layers {
		if l.MediaType == ClasspathLayerMediaType {
			out = append(out, l)
		}
	}
	return out
}

// ModulepathLayers returns the descriptors of the optional modulepath (library
// module) layers, in manifest order. Empty unless the artifact ships a multi-JAR
// modular (JPMS) app. See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
func (m Manifest) ModulepathLayers() []Descriptor {
	var out []Descriptor
	for _, l := range m.Layers {
		if l.MediaType == ModulepathLayerMediaType {
			out = append(out, l)
		}
	}
	return out
}

// CDSLayer returns the descriptor of the optional Application Class-Data Sharing
// archive layer and true when present, or a zero descriptor and false otherwise.
// An artifact ships at most one CDS archive (the first is returned). See
// https://github.com/brewlet/site/blob/main/docs/appcds.md.
func (m Manifest) CDSLayer() (Descriptor, bool) {
	for _, l := range m.Layers {
		if l.MediaType == CDSLayerMediaType {
			return l, true
		}
	}
	return Descriptor{}, false
}
