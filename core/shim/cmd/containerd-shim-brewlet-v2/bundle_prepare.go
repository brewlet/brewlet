// Brewlet-specific bundle assembly shared by the harness (`prepare-bundle`) and
// the production Linux shim's Create() hook. This is the *novel* part of the
// shim: turning an OCI artifact + resource limits into an OCI runtime bundle a
// node-resident JDK can execute. It is deliberately portable (no containerd or
// Linux-only deps) so it builds and is testable on any host.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/brewlet/brewlet/internal/artifact"
	kcruntime "github.com/brewlet/brewlet/internal/runtime"
)

// Annotations the CRI/kubelet path carries the OCI artifact identity on. We
// accept a Brewlet-native ref key first, then fall back to the standard CRI
// image annotation so a raw `runtimeClassName: brewlet` pod works unchanged.
// The manifest digest lets the shim read the artifact straight from
// containerd's content store.
const (
	annArtifactRef    = "brewlet.sh/artifact-ref"
	annArtifactDigest = "brewlet.sh/artifact-digest"
	annCRIImage       = "io.kubernetes.cri.image-name"
	// annRequestedJDK / annRequestedLauncher carry the deployment descriptor's
	// JDK and launcher request onto the pod (set by the operator from
	// jvm.version/jvm.launcher, or by the user on a raw Deployment). They are
	// propagated onto the OCI runtime spec like annArtifactRef, and are the
	// single source of truth for which JDK/launcher the workload runs on — the
	// artifact's launch config no longer carries them.
	annRequestedJDK      = "brewlet.sh/jdk"
	annRequestedLauncher = "brewlet.sh/launcher"
	// annCDSRegenerate opts the workload into node-side AppCDS regeneration
	// (https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §4.3). Value "true" (set by the operator from
	// spec.jvm.cds.regenerate, or by the user on a raw Deployment) tells the shim
	// to maintain a per-(artifact, JDK-build) archive cache with
	// -XX:+AutoCreateSharedArchive rather than consume a shipped archive. It is a
	// deployment/fleet decision, so it lives on the pod, not in the artifact.
	annCDSRegenerate   = "brewlet.sh/cds-regenerate"
	jdkHomeMetadata    = ".brewlet-java-home"
	jdkActiveInventory = ".brewlet-active"
)

// imageConfig is the subset of CRI image metadata the kubelet hands the shim:
// where the pulled artifact's config + JAR layer live in the content store, and
// the container resource limits from the pod spec.
type imageConfig struct {
	StoreRoot        string `json:"storeRoot"`        // OCI content store / layout root
	Ref              string `json:"ref"`              // image ref (the OCI artifact)
	JDKRootsDir      string `json:"jdkRootsDir"`      // e.g. /opt/brewlet/jdks
	LauncherRootsDir string `json:"launcherRootsDir"` // e.g. /opt/brewlet/launchers
	CPULimit         string `json:"cpuLimit"`         // from container.resources.limits.cpu
	MemoryLimit      string `json:"memoryLimit"`      // from container.resources.limits.memory

	// JDKRequest / LauncherName are the JDK and launcher the *deployment
	// descriptor* asked for, carried on the pod as the brewlet.sh/jdk and
	// brewlet.sh/launcher annotations and propagated onto the OCI runtime spec
	// (like brewlet.sh/artifact-ref). They are NOT read from the artifact's
	// launch config: the descriptor is the single source of truth for which JDK
	// feature/distribution and launcher a workload runs on. JDKRequest is a
	// "<dist>-<feature>" token (e.g. "temurin-21") or a bare feature ("21");
	// empty means the node default. LauncherName is "" or "java" for the vanilla
	// OpenJDK launcher.
	JDKRequest   string `json:"jdkRequest,omitempty"`
	LauncherName string `json:"launcherName,omitempty"`

	// CDSRegenerate mirrors the brewlet.sh/cds-regenerate pod annotation (set by
	// the operator from spec.jvm.cds.regenerate): opt into node-side AppCDS
	// regeneration. Like JDKRequest/LauncherName it is a deployment decision
	// carried on the pod, not read from the artifact. See https://github.com/brewlet/brewlet/blob/main/docs/appcds.md §4.3.
	CDSRegenerate bool `json:"cdsRegenerate,omitempty"`

	// Content-store resolution (production path). When Backend is "containerd"
	// the artifact's manifest is read from containerd's own content store by
	// digest, rather than a Brewlet-local OCI layout.
	Backend        string `json:"backend,omitempty"`        // "" (infer) | "layout" | "containerd"
	ContentRoot    string `json:"contentRoot,omitempty"`    // containerd content root
	ManifestDigest string `json:"manifestDigest,omitempty"` // "sha256:…" of the artifact manifest
}

// resolvedArtifact is everything the shim needs after disassembling a Brewlet
// OCI artifact against the node's installed JDK/launcher roots.
type resolvedArtifact struct {
	Config              artifact.JVMConfig
	JarHostPath         string   // on-disk path of the JAR payload blob
	ClasspathHostPaths  []string // on-disk paths of the optional classpath layer tars
	ModulepathHostPaths []string // on-disk paths of the optional modulepath layer tars
	CDSHostPath         string   // on-disk path of the optional AppCDS archive blob, or ""
	JDKRoot             string   // selected node-resident userland root (e.g. /opt/brewlet/jdks/temurin-21)
	JDKHome             string   // JDK or jlink runtime within JDKRoot; mounted at /opt/jdk
	LauncherRoot        string   // node-installed launcher layer, or "" for vanilla `java`
	LauncherName        string   // launcher the descriptor requested ("" or "java" for vanilla)
	Format              string   // native artifact or runnable image
}

// resolveArtifact performs the shared §6.1 steps 1-2b: read the artifact,
// separate the JVM config from the JAR layer (from whichever blob source the
// deployment uses), and select the node-resident JDK (and optional custom
// launcher) the *deployment descriptor* requested. Failures map to
// NoCompatibleJDK / NoCompatibleLauncher pod events in the real shim.
func resolveArtifact(ic imageConfig) (resolvedArtifact, error) {
	blobs, err := loadArtifactBlobs(ic)
	if err != nil {
		return resolvedArtifact{}, err
	}
	jdkRoot, err := selectJDK(ic.JDKRootsDir, ic.JDKRequest)
	if err != nil {
		return resolvedArtifact{}, err
	}
	jdkHome, err := resolveJDKHome(jdkRoot)
	if err != nil {
		return resolvedArtifact{}, err
	}
	launcherRoot, err := selectLauncher(ic.LauncherRootsDir, ic.LauncherName)
	if err != nil {
		return resolvedArtifact{}, err
	}
	return resolvedArtifact{
		Config:              blobs.Config,
		JarHostPath:         blobs.JarHostPath,
		ClasspathHostPaths:  blobs.ClasspathHostPaths,
		ModulepathHostPaths: blobs.ModulepathHostPaths,
		CDSHostPath:         blobs.CDSHostPath,
		JDKRoot:             jdkRoot,
		JDKHome:             jdkHome,
		LauncherRoot:        launcherRoot,
		LauncherName:        ic.LauncherName,
		Format:              blobs.Format,
	}, nil
}

// prepareBundle is the heart of Create(): turn an artifact + limits into an OCI
// runtime bundle that runc can execute. This mirrors §6.1 of the spec and is the
// entrypoint the e2e harness drives directly.
func prepareBundle(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: prepare-bundle <imageConfig.json> <bundleDir>")
	}
	cfgBytes, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var ic imageConfig
	if err := json.Unmarshal(cfgBytes, &ic); err != nil {
		return err
	}
	bundleDir := args[1]

	ra, err := resolveArtifact(ic)
	if err != nil {
		return err
	}

	res := kcruntime.Resources{CPULimit: ic.CPULimit, MemoryLimit: ic.MemoryLimit}
	regen := kcruntime.CDSRegenOptions{
		Regenerate:  ic.CDSRegenerate,
		ArtifactKey: firstNonEmptyStr(ic.ManifestDigest, ic.Ref),
		CacheDir:    os.Getenv("BREWLET_CDS_CACHE"),
		MetricsDir:  os.Getenv("BREWLET_METRICS_DIR"),
	}
	if err := kcruntime.GenerateBundleWithRegen(ra.Config, ra.JDKHome, ra.LauncherRoot, ra.LauncherName, ra.JarHostPath, ra.ClasspathHostPaths, ra.ModulepathHostPaths, ra.CDSHostPath, bundleDir, res, nil, regen); err != nil {
		return fmt.Errorf("generate bundle: %w", err)
	}

	fmt.Printf("shim: prepared bundle %s (jdk=%s", bundleDir, ra.JDKRoot)
	if !artifact.IsVanillaLauncher(ra.LauncherName) {
		fmt.Printf(", launcher=%s", artifact.LauncherName(ra.LauncherName))
	}
	fmt.Printf(")\n")
	fmt.Printf("shim: containerd Start() would now exec: runc run -b %s <container-id>\n", bundleDir)
	return nil
}

// selectJDK finds the node-installed JDK for the descriptor's request. The
// request is a "<dist>-<feature>" token (e.g. "temurin-21", matched exactly), a
// bare feature ("21"), or empty (feature 21). It is supplied by the deployment
// descriptor (brewlet.sh/jdk annotation), not the artifact config.
//
// For a bare feature (no distribution) there is NO built-in vendor preference:
// the node picks the lexically-first installed distribution that provides the
// feature, so selection is deterministic across nodes with the same inventory.
// To pin a distribution, set spec.jvm.distribution (or a "<dist>-<feature>"
// annotation).
func selectJDK(rootsDir, request string) (string, error) {
	dist, feature := parseJDKRequest(request)
	active, err := activeJDKs(rootsDir)
	if err != nil {
		return "", err
	}

	// Explicit distribution: exact match only — never silently fall back to a
	// different vendor's build.
	if dist != "" {
		name := fmt.Sprintf("%s-%d", dist, feature)
		if active != nil && !active[name] {
			return "", fmt.Errorf("NoCompatibleJDK: JDK %s is not active under %s", name, rootsDir)
		}
		root := filepath.Join(rootsDir, name)
		if _, err := resolveJDKHome(root); err == nil {
			return root, nil
		}
		return "", fmt.Errorf("NoCompatibleJDK: no JDK %s-%d under %s", dist, feature, rootsDir)
	}

	// Bare feature: choose the lexically-first installed distribution for this
	// feature. os.ReadDir returns entries already sorted by name; sort again to
	// keep the guarantee explicit and independent of that detail.
	suffix := fmt.Sprintf("-%d", feature)
	entries, err := os.ReadDir(rootsDir)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: cannot read JDK roots %s: %w", rootsDir, err)
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if active != nil && !active[name] {
			continue
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		if _, err := resolveJDKHome(filepath.Join(rootsDir, name)); err == nil {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	if len(matches) > 0 {
		return filepath.Join(rootsDir, matches[0]), nil
	}
	return "", fmt.Errorf("NoCompatibleJDK: no JDK feature %d under %s", feature, rootsDir)
}

func activeJDKs(rootsDir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(rootsDir, jdkActiveInventory))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("NoCompatibleJDK: read active inventory under %s: %w", rootsDir, err)
	}
	active := make(map[string]bool)
	for _, name := range strings.Fields(string(data)) {
		active[name] = true
	}
	return active, nil
}

func resolveJDKHome(root string) (string, error) {
	home := root
	data, err := os.ReadFile(filepath.Join(root, jdkHomeMetadata))
	switch {
	case err == nil:
		inRoot := strings.TrimSpace(string(data))
		if inRoot == "" || !filepath.IsAbs(inRoot) || filepath.Clean(inRoot) != inRoot || inRoot == string(filepath.Separator) {
			return "", fmt.Errorf("NoCompatibleJDK: invalid %s in %s", jdkHomeMetadata, root)
		}
		home = filepath.Join(root, strings.TrimPrefix(inRoot, string(filepath.Separator)))
	case !os.IsNotExist(err):
		return "", fmt.Errorf("NoCompatibleJDK: read %s in %s: %w", jdkHomeMetadata, root, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: resolve root %s: %w", root, err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: resolve Java home %s: %w", home, err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedHome)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("NoCompatibleJDK: Java home %s resolves outside %s", home, root)
	}
	if _, err := os.Stat(filepath.Join(resolvedHome, "bin", "java")); err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: java missing from %s: %w", resolvedHome, err)
	}
	return resolvedHome, nil
}

// parseJDKRequest splits a brewlet.sh/jdk request into distribution and feature.
// Accepted forms: "<dist>-<feature>" (e.g. "temurin-21"), a bare feature ("21"),
// or "" (no distribution, feature 21). An unparseable feature defaults to 21.
// An empty distribution means "any installed distribution of this feature"
// (selectJDK picks the lexically-first one).
func parseJDKRequest(request string) (dist string, feature int) {
	feature = 21
	request = strings.TrimSpace(request)
	if request == "" {
		return "", feature
	}
	if i := strings.LastIndex(request, "-"); i > 0 && i < len(request)-1 {
		if n, err := strconv.Atoi(request[i+1:]); err == nil && n > 0 {
			return request[:i], n
		}
	}
	if n, err := strconv.Atoi(request); err == nil && n > 0 {
		return "", n
	}
	return "", feature
}

// firstNonEmptyStr returns the first non-empty argument, or "".
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// selectLauncher resolves the node-installed launcher layer for a custom
// launcher named by the deployment descriptor (brewlet.sh/launcher). The vanilla
// `java` launcher needs no layer (returns ""). A missing layer surfaces as
// NoCompatibleLauncher, mirroring selectJDK/NoCompatibleJDK.
func selectLauncher(rootsDir, launcherName string) (string, error) {
	if artifact.IsVanillaLauncher(launcherName) {
		return "", nil
	}
	name := artifact.LauncherName(launcherName)
	if rootsDir == "" {
		return "", fmt.Errorf("NoCompatibleLauncher: launcher %q requested but no launcher roots on node", name)
	}
	root := filepath.Join(rootsDir, name)
	if _, err := os.Stat(filepath.Join(root, "bin", name)); err != nil {
		return "", fmt.Errorf("NoCompatibleLauncher: launcher %q not installed under %s", name, rootsDir)
	}
	return root, nil
}
