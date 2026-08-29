// Runnable OCI image mode.
//
// The Brewlet application artifact (artifact.go) uses custom layer media types
// (`+jar`, `.classpath+tar`, …). Those are perfect for a registry-native,
// deployment-agnostic payload, but containerd's CRI PullImage UNPACKS every
// layer into a snapshot before the shim's Create() runs, and its differ only
// understands `tar`, `tar+gzip` and `tar+zstd`. A pod that names a custom
// artifact as its `image:` therefore fails to pull (ImagePullBackOff) and never
// reaches the shim — so the artifact can only be delivered to a node out of band
// (e.g. `ctr images import`), not by kubelet.
//
// A *runnable OCI image* is the SpinKube-style counterpart that closes that gap:
// the exact same JAR + optional dependency/module layers, but packaged as a
// STANDARD, kubelet-pullable OCI image — real `application/vnd.oci.image.config`
// config, standard `tar+gzip` layers, and (when the JAR is pure bytecode) a
// multi-arch image index so any provisioned node matches. containerd pulls and
// unpacks it with no special configuration; the shim then reads the launch
// contract from the manifest's `brewlet.sh/jvm-config` annotation and runs it on
// the node-resident JDK exactly as for a native artifact. This is what lets a
// `runtimeClassName: brewlet` pod set `image: <ref>` and Just Work, like a
// SpinKube does for a Spin-compatible Wasm application. See
// https://github.com/brewlet/brewlet/tree/main/specs §4 and
// https://github.com/brewlet/site/blob/main/docs/runnable-image.md.
package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// marshalIndent is json.MarshalIndent with Brewlet's canonical 2-space layout,
// matching the native-artifact writer so blobs are byte-stable across writers.
func marshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// encodeConfigCompact renders a launch config as a compact JSON string suitable
// for the JVMConfigAnnotation. It round-trips through RunnableConfig/DecodeConfig.
func encodeConfigCompact(cfg JVMConfig) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Standard OCI media types that make a Brewlet image kubelet-pullable.
const (
	OCIImageConfigMediaType = "application/vnd.oci.image.config.v1+json"
	OCIImageIndexMediaType  = "application/vnd.oci.image.index.v1+json"
	OCILayerGzipMediaType   = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Manifest/layer annotations specific to a runnable image.
const (
	// JVMConfigAnnotation carries the Brewlet launch config JSON on a runnable
	// image manifest. A runnable image's config blob is a standard OCI image
	// config (so containerd will unpack the image), so the launch contract that
	// a native artifact keeps in its config blob rides here instead.
	JVMConfigAnnotation = "brewlet.sh/jvm-config"
	// LayerRoleAnnotation tags each runnable-image layer with its Brewlet role,
	// because every layer now shares the standard tar+gzip media type and can no
	// longer be told apart by media type alone.
	LayerRoleAnnotation = "brewlet.sh/layer"
)

// Runnable-image layer roles (values of LayerRoleAnnotation).
const (
	LayerRoleApp        = "app"        // tar of the primary JAR (+ optional CDS archive), flat
	LayerRoleClasspath  = "classpath"  // tar of dependency JARs, unpacked to /app/lib
	LayerRoleModulepath = "modulepath" // tar of library module JARs, unpacked to /app/mods
)

// defaultRunnableArches is the platform set a portable (pure-bytecode) JAR is
// published for, so a runnable image matches any arch Brewlet provisions. A
// non-portable JAR (JVMConfig.Arch set) narrows this to the arches whose native
// libraries it bundles.
var defaultRunnableArches = []string{"amd64", "arm64"}

// ociImageConfig is the minimal standard OCI image config blob a runnable image
// needs so containerd recognizes it as an image and unpacks its layers. The
// placeholder entrypoint satisfies CRI implementations that reject an empty
// command; the Brewlet shim replaces it with the node-JDK launch contract from
// the manifest's JVMConfigAnnotation.
type ociImageConfig struct {
	Architecture string       `json:"architecture"`
	OS           string       `json:"os"`
	Config       ociRunConfig `json:"config"`
	RootFS       ociRootFS    `json:"rootfs"`
}

type ociRunConfig struct {
	Env        []string          `json:"Env,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

type ociRootFS struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

// runnableLayer is a fully-materialized runnable-image layer: its compressed
// blob descriptor plus the uncompressed (diffID) digest the image config needs.
type runnableLayer struct {
	Desc   Descriptor // compressed tar+gzip blob descriptor (already written)
	DiffID string     // sha256 of the UNCOMPRESSED tar (rootfs.diff_ids entry)
}

// RunnableImageOptions carries optional publication-time composition metadata.
// A managed dependency layer is already gzip-compressed and content-addressed,
// so the final image reuses its descriptor without repacking the bytes.
type RunnableImageOptions struct {
	ManagedDependency *ResolvedDependencyBundle
	ManagedEvidence   *ManagedDependencyEvidence
}

// PushRunnableImage publishes cfg + jarPath (and any classpath/module/CDS
// layers) as a STANDARD, kubelet-pullable OCI image and tags it as ref. Unlike
// PushWithCDS (which writes a native Brewlet artifact with custom media types),
// every layer here is a `tar+gzip` blob and the top-level object is an OCI image
// index spanning the target architectures, so containerd/kubelet pull and unpack
// it with no special configuration. The returned descriptor is the tagged index.
//
// The launch contract (cfg) is carried verbatim on each platform manifest under
// the brewlet.sh/jvm-config annotation; the shim reads it from there instead of
// a Brewlet config blob. Pass "" for cdsArchivePath when the app ships no CDS
// archive. See https://github.com/brewlet/site/blob/main/docs/runnable-image.md.
func (s Store) PushRunnableImage(ref string, cfg JVMConfig, jarPath string, classpathTars, modulepathTars []string, cdsArchivePath string) (Descriptor, error) {
	return s.PushRunnableImageWithOptions(ref, cfg, jarPath, classpathTars, modulepathTars, cdsArchivePath, RunnableImageOptions{})
}

func (s Store) PushRunnableImageWithOptions(ref string, cfg JVMConfig, jarPath string, classpathTars, modulepathTars []string, cdsArchivePath string, opts RunnableImageOptions) (Descriptor, error) {
	if err := cfg.Validate(); err != nil {
		return Descriptor{}, fmt.Errorf("invalid launch config: %w", err)
	}
	if err := validateCDSPairing(cfg, cdsArchivePath); err != nil {
		return Descriptor{}, err
	}

	// Layer 0 (role=app): a flat tar of the primary JAR (materialized as
	// cfg.MainJar) plus the optional CDS archive. The shim untars it to recover
	// the JAR (and .jsa) as files, exactly like the native artifact's JAR blob.
	appFiles, err := appLayerFiles(cfg, jarPath, cdsArchivePath)
	if err != nil {
		return Descriptor{}, err
	}
	appLayer, err := s.writeRunnableLayer(appFiles, LayerRoleApp, mainJarName(cfg, jarPath))
	if err != nil {
		return Descriptor{}, err
	}
	layers := []runnableLayer{appLayer}
	if opts.ManagedDependency != nil {
		if len(classpathTars) > 0 || len(modulepathTars) > 0 {
			return Descriptor{}, fmt.Errorf("managed dependency bundle and local classpath/modulepath layers are mutually exclusive")
		}
		if cfg.Entry.Mode != "classpath" {
			return Descriptor{}, fmt.Errorf("managed dependency bundle requires entry.mode=classpath")
		}
		required := map[string]bool{cfg.MainJar: false, "lib/*": false}
		for _, entry := range cfg.Entry.ClassPath {
			if _, ok := required[entry]; ok {
				required[entry] = true
			}
		}
		if !required[cfg.MainJar] || !required["lib/*"] {
			return Descriptor{}, fmt.Errorf("managed dependency bundle requires classPath to include %q and %q", cfg.MainJar, "lib/*")
		}
	}

	// Classpath/module layers are the SAME flat-JAR tars a native artifact ships,
	// just gzip-compressed and role-tagged, so the shim's existing
	// StageClasspathLayers/StageModulepathLayers stage them unchanged.
	for _, kind := range []struct {
		tars []string
		role string
	}{{classpathTars, LayerRoleClasspath}, {modulepathTars, LayerRoleModulepath}} {
		for _, tarPath := range kind.tars {
			raw, err := os.ReadFile(tarPath)
			if err != nil {
				return Descriptor{}, fmt.Errorf("read %s layer %q: %w", kind.role, tarPath, err)
			}
			l, err := s.writeRunnableLayerFromTar(raw, kind.role, filepath.Base(tarPath))
			if err != nil {
				return Descriptor{}, err
			}
			layers = append(layers, l)
		}
	}
	manifestAnnotations := map[string]string{}
	if opts.ManagedDependency != nil {
		bundle := opts.ManagedDependency
		if bundle.Layer.MediaType != OCILayerGzipMediaType || bundle.Layer.Annotations[LayerRoleAnnotation] != LayerRoleClasspath {
			return Descriptor{}, fmt.Errorf("managed dependency bundle has an invalid runnable classpath layer")
		}
		if _, err := s.ReadBlob(bundle.Layer.Digest); err != nil {
			return Descriptor{}, fmt.Errorf("managed dependency layer %s is unavailable in the OCI store: %w", bundle.Layer.Digest, err)
		}
		layers = append(layers, runnableLayer{Desc: bundle.Layer, DiffID: bundle.Config.LayerDiffID})
		var evidence ManagedDependencyEvidence
		if opts.ManagedEvidence != nil {
			evidence = *opts.ManagedEvidence
		} else {
			var err error
			evidence, err = bundle.Evidence(jarPath)
			if err != nil {
				return Descriptor{}, err
			}
		}
		raw, err := json.Marshal(evidence)
		if err != nil {
			return Descriptor{}, err
		}
		manifestAnnotations[ManagedDependencyAnnotation] = string(raw)
	}

	cfgJSON, err := encodeConfigCompact(cfg)
	if err != nil {
		return Descriptor{}, err
	}

	arches := targetArches(cfg)
	indexManifests := make([]Descriptor, 0, len(arches))
	for _, arch := range arches {
		manDesc, err := s.writeRunnableManifest(arch, cfgJSON, layers, manifestAnnotations)
		if err != nil {
			return Descriptor{}, err
		}
		manDesc.Platform = &Platform{OS: "linux", Architecture: arch}
		indexManifests = append(indexManifests, manDesc)
	}

	idx := Index{
		SchemaVersion: 2,
		MediaType:     OCIImageIndexMediaType,
		Manifests:     indexManifests,
		Annotations:   manifestAnnotations,
	}
	idxBytes, err := marshalIndent(idx)
	if err != nil {
		return Descriptor{}, err
	}
	idxDesc, err := s.writeBlob(idxBytes)
	if err != nil {
		return Descriptor{}, err
	}
	idxDesc.MediaType = OCIImageIndexMediaType
	idxDesc.Annotations = map[string]string{refNameAnnotation: ref}

	if err := s.upsertIndex(idxDesc); err != nil {
		return Descriptor{}, err
	}
	if err := s.writeLayoutMarker(); err != nil {
		return Descriptor{}, err
	}
	return idxDesc, nil
}

// writeRunnableManifest writes one platform's image config blob and image
// manifest blob (sharing the given layers) and returns the manifest descriptor.
func (s Store) writeRunnableManifest(arch, jvmConfigJSON string, layers []runnableLayer, extraAnnotations map[string]string) (Descriptor, error) {
	diffIDs := make([]string, 0, len(layers))
	mLayers := make([]Descriptor, 0, len(layers))
	for _, l := range layers {
		diffIDs = append(diffIDs, l.DiffID)
		mLayers = append(mLayers, l.Desc)
	}
	imgCfg := ociImageConfig{
		Architecture: arch,
		OS:           "linux",
		Config: ociRunConfig{
			Entrypoint: []string{"/brewlet"},
			Labels:     map[string]string{"sh.brewlet.runnable": "true"},
		},
		RootFS: ociRootFS{Type: "layers", DiffIDs: diffIDs},
	}
	cfgBytes, err := marshalIndent(imgCfg)
	if err != nil {
		return Descriptor{}, err
	}
	cfgDesc, err := s.writeBlob(cfgBytes)
	if err != nil {
		return Descriptor{}, err
	}
	cfgDesc.MediaType = OCIImageConfigMediaType

	annotations := map[string]string{JVMConfigAnnotation: jvmConfigJSON}
	for key, value := range extraAnnotations {
		annotations[key] = value
	}
	man := Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config:        cfgDesc,
		Layers:        mLayers,
		Annotations:   annotations,
	}
	mBytes, err := marshalIndent(man)
	if err != nil {
		return Descriptor{}, err
	}
	mDesc, err := s.writeBlob(mBytes)
	if err != nil {
		return Descriptor{}, err
	}
	mDesc.MediaType = ociManifestMediaType
	return mDesc, nil
}

// writeRunnableLayer builds a flat tar from files (name -> content), gzips it,
// writes it as a blob, and records both the compressed descriptor and the
// uncompressed diffID.
func (s Store) writeRunnableLayer(files []tarEntry, role, title string) (runnableLayer, error) {
	tarBytes, err := buildTar(files)
	if err != nil {
		return runnableLayer{}, err
	}
	return s.writeRunnableLayerFromTar(tarBytes, role, title)
}

// writeRunnableLayerFromTar gzips an already-built (uncompressed) tar, writes it
// as a role-tagged tar+gzip blob, and records the compressed descriptor plus the
// uncompressed diffID.
func (s Store) writeRunnableLayerFromTar(tarBytes []byte, role, title string) (runnableLayer, error) {
	diffID := digestOf(tarBytes)
	gz := gzipBytes(tarBytes)
	desc, err := s.writeBlob(gz)
	if err != nil {
		return runnableLayer{}, err
	}
	desc.MediaType = OCILayerGzipMediaType
	desc.Annotations = map[string]string{LayerRoleAnnotation: role}
	if title != "" {
		desc.Annotations[titleAnnotation] = title
	}
	return runnableLayer{Desc: desc, DiffID: diffID}, nil
}

// ---- runnable-image accessors on Manifest ----

// IsRunnableImage reports whether m is a runnable OCI image (a standard image
// carrying the Brewlet launch config annotation) rather than a native artifact.
func (m Manifest) IsRunnableImage() bool {
	if m.Annotations[JVMConfigAnnotation] == "" {
		return false
	}
	return m.Config.MediaType == OCIImageConfigMediaType || m.Config.MediaType == ""
}

// RunnableConfig decodes the launch config a runnable image carries in its
// JVMConfigAnnotation.
func (m Manifest) RunnableConfig() (JVMConfig, error) {
	raw := m.Annotations[JVMConfigAnnotation]
	if raw == "" {
		return JVMConfig{}, fmt.Errorf("manifest carries no %s annotation", JVMConfigAnnotation)
	}
	return DecodeConfig([]byte(raw))
}

// layersByRole returns the runnable-image layers whose LayerRoleAnnotation
// equals role, in manifest order.
func (m Manifest) layersByRole(role string) []Descriptor {
	var out []Descriptor
	for _, l := range m.Layers {
		if l.Annotations[LayerRoleAnnotation] == role {
			out = append(out, l)
		}
	}
	return out
}

// RunnableAppLayer returns the descriptor of a runnable image's app layer (the
// tar carrying the primary JAR and optional CDS archive).
func (m Manifest) RunnableAppLayer() (Descriptor, error) {
	ls := m.layersByRole(LayerRoleApp)
	if len(ls) == 0 {
		return Descriptor{}, fmt.Errorf("runnable image has no %s=%s layer", LayerRoleAnnotation, LayerRoleApp)
	}
	return ls[0], nil
}

// RunnableClasspathLayers / RunnableModulepathLayers return the descriptors of a
// runnable image's dependency and module layers, in manifest order.
func (m Manifest) RunnableClasspathLayers() []Descriptor  { return m.layersByRole(LayerRoleClasspath) }
func (m Manifest) RunnableModulepathLayers() []Descriptor { return m.layersByRole(LayerRoleModulepath) }

// ---- helpers ----

type tarEntry struct {
	name    string
	content []byte
}

// appLayerFiles gathers the flat files the app layer tar carries: the primary
// JAR (named cfg.MainJar / the JAR basename) and, when shipped, the CDS archive.
func appLayerFiles(cfg JVMConfig, jarPath, cdsArchivePath string) ([]tarEntry, error) {
	jarBytes, err := os.ReadFile(jarPath)
	if err != nil {
		return nil, fmt.Errorf("read jar: %w", err)
	}
	files := []tarEntry{{name: mainJarName(cfg, jarPath), content: jarBytes}}
	if cdsArchivePath != "" {
		cdsBytes, err := os.ReadFile(cdsArchivePath)
		if err != nil {
			return nil, fmt.Errorf("read cds archive %q: %w", cdsArchivePath, err)
		}
		name := filepath.Base(cdsArchivePath)
		if cfg.CDS != nil && cfg.CDS.Archive != "" {
			name = cfg.CDS.Archive
		}
		files = append(files, tarEntry{name: name, content: cdsBytes})
	}
	return files, nil
}

// mainJarName is the filename the primary JAR is materialized as under /app: the
// launch config's MainJar when set, else the JAR's basename.
func mainJarName(cfg JVMConfig, jarPath string) string {
	if cfg.MainJar != "" {
		return cfg.MainJar
	}
	return filepath.Base(jarPath)
}

// targetArches is the architecture set to publish a runnable image for: the
// JAR's declared native arches (non-portable) or the portable default.
func targetArches(cfg JVMConfig) []string {
	if len(cfg.Arch) > 0 {
		out := append([]string(nil), cfg.Arch...)
		sort.Strings(out)
		return out
	}
	return defaultRunnableArches
}

// RunnableArches reports the platform architectures PushRunnableImage publishes
// for a given launch config (its declared native arches, or the portable
// default when arch-neutral). Exposed for CLI reporting.
func RunnableArches(cfg JVMConfig) []string { return targetArches(cfg) }

// buildTar writes files into an uncompressed tar and returns its bytes.
func buildTar(files []tarEntry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.name,
			Mode:     0o644,
			Size:     int64(len(f.content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gzipBytes returns b gzip-compressed.
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(b)
	_ = gw.Close()
	return buf.Bytes()
}

// SelectPlatformManifest returns the digest of the index entry matching arch
// (defaulting to the running node's GOARCH), for an image index. It lets a
// consumer resolve a runnable image's platform manifest from the tagged index.
func (idx Index) SelectPlatformManifest(arch string) (Descriptor, bool) {
	if arch == "" {
		arch = runtime.GOARCH
	}
	for _, m := range idx.Manifests {
		if m.Platform != nil && m.Platform.Architecture == arch {
			return m, true
		}
	}
	// Fall back to the first entry (e.g. a single-arch index).
	if len(idx.Manifests) > 0 {
		return idx.Manifests[0], true
	}
	return Descriptor{}, false
}

// ResolveManifestByRef reads the manifest a ref tags, transparently following an
// image index to the entry matching the running node's architecture. It returns
// the resolved (platform) manifest and its digest. Unlike Resolve it does NOT
// decode a Brewlet config blob, so it works for both native artifacts and
// runnable images — the caller inspects Manifest.IsRunnableImage to branch.
func (s Store) ResolveManifestByRef(ref string) (Manifest, string, error) {
	var idx Index
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		return Manifest{}, "", fmt.Errorf("read index: %w", err)
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return Manifest{}, "", err
	}
	for _, top := range idx.Manifests {
		if top.Annotations[refNameAnnotation] != ref {
			continue
		}
		return s.readManifestFollowingIndex(top)
	}
	return Manifest{}, "", fmt.Errorf("ref %q not found in store %s", ref, s.Root)
}

// readManifestFollowingIndex reads the blob at digest; if it is an image index
// it selects the entry for the running node's arch and reads that manifest.
func (s Store) readManifestFollowingIndex(desc Descriptor) (Manifest, string, error) {
	raw, err := s.readVerifiedBlob(desc)
	if err != nil {
		return Manifest{}, "", err
	}
	digest := desc.Digest
	if IsIndexBlob(raw) {
		var idx Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return Manifest{}, "", err
		}
		sel, ok := idx.SelectPlatformManifest("")
		if !ok {
			return Manifest{}, "", fmt.Errorf("image index %s has no manifests", digest)
		}
		digest = sel.Digest
		if raw, err = s.readVerifiedBlob(sel); err != nil {
			return Manifest{}, "", err
		}
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return Manifest{}, "", err
	}
	return man, digest, nil
}

// IsIndexBlob reports whether raw is an OCI image index (a manifest list) rather
// than a single manifest, by peeking at its mediaType / manifests field.
func IsIndexBlob(raw []byte) bool {
	var probe struct {
		MediaType string       `json:"mediaType"`
		Manifests []Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.MediaType == OCIImageIndexMediaType || len(probe.Manifests) > 0
}
