package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	DependencyBundleArtifactType = "application/vnd.brewlet.dependencies.v1+json"
	DependencyBundleConfigType   = "application/vnd.brewlet.dependencies.config.v1+json"
	DependencyLockMediaType      = "application/vnd.brewlet.dependencies.lock.v1+json"

	ManagedDependencyAnnotation = "brewlet.sh/managed-dependency-evidence"
)

// DependencyBundleConfig describes an immutable, Ops-managed dependency bundle.
// The layer fields are derived while publishing and bind the config to the exact
// compressed OCI layer and its uncompressed diff ID.
type DependencyBundleConfig struct {
	SchemaVersion  int    `json:"schemaVersion"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	SourceBOM      string `json:"sourceBom"`
	LockDigest     string `json:"lockDigest"`
	LayerDigest    string `json:"layerDigest"`
	LayerDiffID    string `json:"layerDiffId"`
	CompatibleJDKs []int  `json:"compatibleJdks,omitempty"`
}

// DependencyLock is the canonical inventory used to compare an application's
// resolved Maven runtime graph with the approved bundle.
type DependencyLock struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Artifacts     []DependencyLockEntry `json:"artifacts"`
}

type DependencyLockEntry struct {
	GroupID    string `json:"groupId"`
	ArtifactID string `json:"artifactId"`
	Version    string `json:"version"`
	Type       string `json:"type,omitempty"`
	Classifier string `json:"classifier,omitempty"`
	Scope      string `json:"scope,omitempty"`
	FileName   string `json:"fileName"`
	SHA256     string `json:"sha256"`
}

// ManagedDependencyEvidence is unsigned canonical evidence suitable for binding
// into an in-toto/Sigstore attestation. The current local OCI writer records it
// on the final image manifest; a trusted publisher signs the same predicate.
type ManagedDependencyEvidence struct {
	SchemaVersion          int    `json:"schemaVersion"`
	ThinJar                bool   `json:"thinJar"`
	ApplicationJarDigest   string `json:"applicationJarDigest"`
	DependencyBundleDigest string `json:"dependencyBundleDigest"`
	DependencyLayerDigest  string `json:"dependencyLayerDigest"`
	DependencyLockDigest   string `json:"dependencyLockDigest"`
	SourceBOM              string `json:"sourceBom"`
}

type ResolvedDependencyBundle struct {
	ManifestDigest string
	Config         DependencyBundleConfig
	Lock           DependencyLock
	Layer          Descriptor
}

func (c DependencyBundleConfig) Validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("dependency bundle schemaVersion must be 1, got %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("dependency bundle name must be non-empty")
	}
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("dependency bundle version must be non-empty")
	}
	if err := validateMavenCoordinate(c.SourceBOM); err != nil {
		return fmt.Errorf("sourceBom: %w", err)
	}
	for field, value := range map[string]string{
		"lockDigest":  c.LockDigest,
		"layerDigest": c.LayerDigest,
		"layerDiffId": c.LayerDiffID,
	} {
		if !isSHA256Digest(value) {
			return fmt.Errorf("%s must be a sha256 digest, got %q", field, value)
		}
	}
	seen := map[int]struct{}{}
	for _, feature := range c.CompatibleJDKs {
		if feature <= 0 {
			return fmt.Errorf("compatibleJdks entries must be positive, got %d", feature)
		}
		if _, ok := seen[feature]; ok {
			return fmt.Errorf("compatibleJdks entry %d is duplicated", feature)
		}
		seen[feature] = struct{}{}
	}
	return nil
}

func (l DependencyLock) Validate() error {
	if l.SchemaVersion != 1 {
		return fmt.Errorf("dependency lock schemaVersion must be 1, got %d", l.SchemaVersion)
	}
	if len(l.Artifacts) == 0 {
		return fmt.Errorf("dependency lock must contain at least one artifact")
	}
	seenCoordinates := map[string]struct{}{}
	seenFiles := map[string]struct{}{}
	for i, entry := range l.Artifacts {
		if strings.TrimSpace(entry.GroupID) == "" || strings.TrimSpace(entry.ArtifactID) == "" || strings.TrimSpace(entry.Version) == "" {
			return fmt.Errorf("dependency lock artifact %d requires groupId, artifactId, and version", i)
		}
		if strings.TrimSpace(entry.Type) == "" || strings.TrimSpace(entry.Scope) == "" {
			return fmt.Errorf("dependency lock artifact %d requires type and scope", i)
		}
		if entry.FileName == "" || filepath.Base(entry.FileName) != entry.FileName || !strings.HasSuffix(strings.ToLower(entry.FileName), ".jar") {
			return fmt.Errorf("dependency lock artifact %d fileName %q must be a flat JAR filename", i, entry.FileName)
		}
		if len(entry.SHA256) != 64 || !isLowerHex(entry.SHA256) {
			return fmt.Errorf("dependency lock artifact %d sha256 must be 64 lowercase hexadecimal characters", i)
		}
		coordinate := strings.Join([]string{entry.GroupID, entry.ArtifactID, entry.Type, entry.Classifier, entry.Version}, ":")
		if _, ok := seenCoordinates[coordinate]; ok {
			return fmt.Errorf("dependency lock coordinate %q is duplicated", coordinate)
		}
		seenCoordinates[coordinate] = struct{}{}
		if _, ok := seenFiles[entry.FileName]; ok {
			return fmt.Errorf("dependency lock fileName %q is duplicated", entry.FileName)
		}
		seenFiles[entry.FileName] = struct{}{}
	}
	return nil
}

func DecodeDependencyBundleConfig(raw []byte) (DependencyBundleConfig, error) {
	var cfg DependencyBundleConfig
	if err := decodeStrict(raw, &cfg); err != nil {
		return DependencyBundleConfig{}, fmt.Errorf("decode dependency bundle config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return DependencyBundleConfig{}, err
	}
	return cfg, nil
}

func DecodeDependencyLock(raw []byte) (DependencyLock, error) {
	var lock DependencyLock
	if err := decodeStrict(raw, &lock); err != nil {
		return DependencyLock{}, fmt.Errorf("decode dependency lock: %w", err)
	}
	sort.Slice(lock.Artifacts, func(i, j int) bool {
		return lockCoordinate(lock.Artifacts[i]) < lockCoordinate(lock.Artifacts[j])
	})
	if err := lock.Validate(); err != nil {
		return DependencyLock{}, err
	}
	return lock, nil
}

func decodeStrict(raw []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("document must contain exactly one JSON value")
	}
	return nil
}

// PushDependencyBundle publishes one deterministic classpath tar and its lock as
// an OCI artifact. The classpath layer is stored in standard gzip form so the
// exact descriptor can be reused by a runnable application image.
func (s Store) PushDependencyBundle(ref string, cfg DependencyBundleConfig, lock DependencyLock, classpathTar string) (Descriptor, error) {
	sort.Slice(lock.Artifacts, func(i, j int) bool {
		return lockCoordinate(lock.Artifacts[i]) < lockCoordinate(lock.Artifacts[j])
	})
	if err := lock.Validate(); err != nil {
		return Descriptor{}, err
	}
	tarBytes, err := os.ReadFile(classpathTar)
	if err != nil {
		return Descriptor{}, fmt.Errorf("read dependency classpath layer: %w", err)
	}
	if err := validateDependencyTar(tarBytes, lock); err != nil {
		return Descriptor{}, err
	}

	lockBytes, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return Descriptor{}, err
	}
	lockDesc, err := s.writeBlob(lockBytes)
	if err != nil {
		return Descriptor{}, err
	}
	lockDesc.MediaType = DependencyLockMediaType
	lockDesc.Annotations = map[string]string{titleAnnotation: "dependency-lock.json"}

	layer := runnableLayer{
		DiffID: digestOf(tarBytes),
	}
	layer.Desc, err = s.writeBlob(gzipBytes(tarBytes))
	if err != nil {
		return Descriptor{}, err
	}
	layer.Desc.MediaType = OCILayerGzipMediaType
	layer.Desc.Annotations = map[string]string{
		LayerRoleAnnotation: LayerRoleClasspath,
		titleAnnotation:     filepath.Base(classpathTar),
	}

	cfg.SchemaVersion = 1
	cfg.LockDigest = lockDesc.Digest
	cfg.LayerDigest = layer.Desc.Digest
	cfg.LayerDiffID = layer.DiffID
	sort.Ints(cfg.CompatibleJDKs)
	if err := cfg.Validate(); err != nil {
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
	cfgDesc.MediaType = DependencyBundleConfigType

	manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		ArtifactType:  DependencyBundleArtifactType,
		Config:        cfgDesc,
		Layers:        []Descriptor{layer.Desc, lockDesc},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Descriptor{}, err
	}
	desc, err := s.writeBlob(raw)
	if err != nil {
		return Descriptor{}, err
	}
	desc.MediaType = ociManifestMediaType
	desc.ArtifactType = DependencyBundleArtifactType
	desc.Annotations = map[string]string{refNameAnnotation: ref}
	if err := s.upsertIndex(desc); err != nil {
		return Descriptor{}, err
	}
	if err := s.writeLayoutMarker(); err != nil {
		return Descriptor{}, err
	}
	return desc, nil
}

func (s Store) ResolveDependencyBundle(ref string) (ResolvedDependencyBundle, error) {
	manifest, digest, err := s.ResolveManifestByRef(ref)
	if err != nil {
		return ResolvedDependencyBundle{}, err
	}
	if manifest.ArtifactType != DependencyBundleArtifactType {
		return ResolvedDependencyBundle{}, fmt.Errorf("ref %q is not a Brewlet dependency bundle (artifactType=%q)", ref, manifest.ArtifactType)
	}
	if manifest.Config.MediaType != DependencyBundleConfigType {
		return ResolvedDependencyBundle{}, fmt.Errorf("dependency bundle config mediaType=%q, want %q", manifest.Config.MediaType, DependencyBundleConfigType)
	}
	cfgRaw, err := s.readVerifiedBlob(manifest.Config)
	if err != nil {
		return ResolvedDependencyBundle{}, err
	}
	cfg, err := DecodeDependencyBundleConfig(cfgRaw)
	if err != nil {
		return ResolvedDependencyBundle{}, err
	}
	var layers, locks []Descriptor
	for _, layer := range manifest.Layers {
		switch {
		case layer.MediaType == OCILayerGzipMediaType && layer.Annotations[LayerRoleAnnotation] == LayerRoleClasspath:
			layers = append(layers, layer)
		case layer.MediaType == DependencyLockMediaType:
			locks = append(locks, layer)
		}
	}
	if len(layers) != 1 || len(locks) != 1 {
		return ResolvedDependencyBundle{}, fmt.Errorf("dependency bundle requires exactly one classpath layer and one dependency lock (got %d and %d)", len(layers), len(locks))
	}
	if cfg.LayerDigest != layers[0].Digest || cfg.LockDigest != locks[0].Digest {
		return ResolvedDependencyBundle{}, fmt.Errorf("dependency bundle config digest binding does not match manifest layers")
	}
	lockRaw, err := s.readVerifiedBlob(locks[0])
	if err != nil {
		return ResolvedDependencyBundle{}, err
	}
	lock, err := DecodeDependencyLock(lockRaw)
	if err != nil {
		return ResolvedDependencyBundle{}, err
	}
	layerRaw, err := s.readVerifiedBlob(layers[0])
	if err != nil {
		return ResolvedDependencyBundle{}, err
	}

	reader, err := gzip.NewReader(bytes.NewReader(layerRaw))
	if err != nil {
		return ResolvedDependencyBundle{}, fmt.Errorf("open dependency classpath layer: %w", err)
	}
	layerTar, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return ResolvedDependencyBundle{}, fmt.Errorf("read dependency classpath layer: %w", readErr)
	}
	if closeErr != nil {
		return ResolvedDependencyBundle{}, fmt.Errorf("close dependency classpath layer: %w", closeErr)
	}
	if got := digestOf(layerTar); got != cfg.LayerDiffID {
		return ResolvedDependencyBundle{}, fmt.Errorf("dependency classpath layer diff ID mismatch: got %s, config requires %s", got, cfg.LayerDiffID)
	}
	if err := validateDependencyTar(layerTar, lock); err != nil {
		return ResolvedDependencyBundle{}, err
	}
	return ResolvedDependencyBundle{ManifestDigest: digest, Config: cfg, Lock: lock, Layer: layers[0]}, nil
}

func (s Store) readVerifiedBlob(desc Descriptor) ([]byte, error) {
	raw, err := s.ReadBlob(desc.Digest)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != desc.Size {
		return nil, fmt.Errorf("blob %s size mismatch: got %d, descriptor requires %d", desc.Digest, len(raw), desc.Size)
	}
	if got := digestOf(raw); got != desc.Digest {
		return nil, fmt.Errorf("blob digest mismatch: got %s, descriptor requires %s", got, desc.Digest)
	}
	return raw, nil
}

func VerifyDependencyLock(expected, actual DependencyLock) error {
	sort.Slice(expected.Artifacts, func(i, j int) bool {
		return lockCoordinate(expected.Artifacts[i]) < lockCoordinate(expected.Artifacts[j])
	})
	sort.Slice(actual.Artifacts, func(i, j int) bool {
		return lockCoordinate(actual.Artifacts[i]) < lockCoordinate(actual.Artifacts[j])
	})
	if !reflect.DeepEqual(expected, actual) {
		return fmt.Errorf("application dependency lock does not exactly match the managed bundle lock")
	}
	return nil
}

func (b ResolvedDependencyBundle) Evidence(jarPath string) (ManagedDependencyEvidence, error) {
	raw, err := os.ReadFile(jarPath)
	if err != nil {
		return ManagedDependencyEvidence{}, fmt.Errorf("read application JAR: %w", err)
	}
	return ManagedDependencyEvidence{
		SchemaVersion:          1,
		ThinJar:                true,
		ApplicationJarDigest:   digestOf(raw),
		DependencyBundleDigest: b.ManifestDigest,
		DependencyLayerDigest:  b.Layer.Digest,
		DependencyLockDigest:   b.Config.LockDigest,
		SourceBOM:              b.Config.SourceBOM,
	}, nil
}

func (m Manifest) ManagedDependencyEvidence() (ManagedDependencyEvidence, bool, error) {
	raw := m.Annotations[ManagedDependencyAnnotation]
	if raw == "" {
		return ManagedDependencyEvidence{}, false, nil
	}
	var evidence ManagedDependencyEvidence
	if err := decodeStrict([]byte(raw), &evidence); err != nil {
		return ManagedDependencyEvidence{}, true, fmt.Errorf("decode managed dependency evidence: %w", err)
	}
	if evidence.SchemaVersion != 1 || !evidence.ThinJar {
		return ManagedDependencyEvidence{}, true, fmt.Errorf("invalid managed dependency evidence schema or thinJar verdict")
	}
	return evidence, true, nil
}

func validateDependencyTar(raw []byte, lock DependencyLock) error {
	expected := make(map[string]string, len(lock.Artifacts))
	for _, entry := range lock.Artifacts {
		expected[entry.FileName] = entry.SHA256
	}
	seen := map[string]struct{}{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read dependency classpath tar: %w", err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != header.Name || !strings.HasSuffix(strings.ToLower(header.Name), ".jar") {
			return fmt.Errorf("dependency classpath layer entry %q must be a flat regular JAR", header.Name)
		}
		want, ok := expected[header.Name]
		if !ok {
			return fmt.Errorf("dependency classpath layer contains %q, which is absent from the lock", header.Name)
		}
		if _, ok := seen[header.Name]; ok {
			return fmt.Errorf("dependency classpath layer contains duplicate entry %q", header.Name)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("read dependency %q: %w", header.Name, err)
		}
		got := strings.TrimPrefix(digestOf(content), "sha256:")
		if got != want {
			return fmt.Errorf("dependency %q checksum mismatch: got %s, lock requires %s", header.Name, got, want)
		}
		seen[header.Name] = struct{}{}
	}
	if len(seen) != len(expected) {
		var missing []string
		for name := range expected {
			if _, ok := seen[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("dependency classpath layer is missing locked artifacts: %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateMavenCoordinate(value string) error {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return fmt.Errorf("must use groupId:artifactId:version syntax")
	}
	for i, part := range parts {
		if strings.TrimSpace(part) == "" || part != strings.TrimSpace(part) {
			return fmt.Errorf("coordinate segment %d must be non-empty and trimmed", i+1)
		}
	}
	return nil
}

func isSHA256Digest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) == len("sha256:")+64 && isLowerHex(strings.TrimPrefix(value, "sha256:"))
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func lockCoordinate(entry DependencyLockEntry) string {
	return strings.Join([]string{entry.GroupID, entry.ArtifactID, entry.Type, entry.Classifier, entry.Version, entry.FileName, entry.SHA256}, ":")
}
