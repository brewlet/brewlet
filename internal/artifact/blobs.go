package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ResolvedBlobs is an artifact after separating the JVM launch config from its
// payload layers, normalized to on-disk paths. It is independent of which store
// the blobs were read from (the Brewlet-local OCI layout or containerd's content
// store) and of the delivery format (native artifact vs runnable image): both
// the local CLI (run/bundle) and the node shim resolve down to this shape before
// assembling a sandbox.
type ResolvedBlobs struct {
	Config              JVMConfig
	JarHostPath         string   // on-disk path of the JAR payload
	ClasspathHostPaths  []string // on-disk paths of the optional classpath layer tars
	ModulepathHostPaths []string // on-disk paths of the optional modulepath layer tars
	CDSHostPath         string   // on-disk path of the optional AppCDS archive, or ""
}

// BlobSource abstracts a content-addressed blob store so format resolution
// (index following + runnable-image layer staging) can be shared by both the
// Brewlet-local OCI layout and containerd's content store. Store already
// satisfies it via its ReadBlob/BlobPath methods.
type BlobSource interface {
	// ReadBlob returns the raw bytes of a blob by digest ("sha256:…").
	ReadBlob(digest string) ([]byte, error)
	// BlobPath returns the on-disk path of a blob by digest.
	BlobPath(digest string) string
}

// ResolveBlobs resolves a tagged ref in this local OCI layout to normalized
// on-disk blobs, transparently following an image index and handling BOTH a
// native Brewlet artifact and a runnable OCI image. It is the local (run/bundle)
// counterpart of the shim's content-store resolution; the manifest tells the two
// formats apart via Manifest.IsRunnableImage.
func (s Store) ResolveBlobs(ref string) (ResolvedBlobs, error) {
	man, digest, err := s.ResolveManifestByRef(ref)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("resolve artifact: %w", err)
	}
	if man.IsRunnableImage() {
		return ResolveRunnableBlobs(s, man, digest)
	}
	return ResolveNativeBlobs(s, man)
}

// ResolveManifestFollowingIndex reads the blob at digest from src; when it is an
// OCI image index it selects the entry for the running node's architecture and
// reads that platform manifest. Returns the resolved manifest and its digest.
// Works for both native artifacts and runnable images.
func ResolveManifestFollowingIndex(src BlobSource, digest string) (Manifest, string, error) {
	raw, err := src.ReadBlob(digest)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("read manifest blob: %w", err)
	}
	if IsIndexBlob(raw) {
		var idx Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return Manifest{}, "", fmt.Errorf("parse image index %s: %w", digest, err)
		}
		sel, ok := idx.SelectPlatformManifest("")
		if !ok {
			return Manifest{}, "", fmt.Errorf("image index %s has no manifests", digest)
		}
		digest = sel.Digest
		if raw, err = src.ReadBlob(digest); err != nil {
			return Manifest{}, "", fmt.Errorf("read platform manifest %s: %w", digest, err)
		}
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return Manifest{}, "", fmt.Errorf("parse manifest %s: %w", digest, err)
	}
	return man, digest, nil
}

// ResolveNativeBlobs resolves a native Brewlet artifact (custom media-type
// layers) to on-disk blob paths, mounting each layer blob directly from src with
// no copy — the historical production/PoC path.
func ResolveNativeBlobs(src BlobSource, man Manifest) (ResolvedBlobs, error) {
	cb, err := src.ReadBlob(man.Config.Digest)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("read config blob: %w", err)
	}
	cfg, err := DecodeConfig(cb)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("parse jvm config: %w", err)
	}
	layer, err := man.JarLayer()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	jarPath := src.BlobPath(layer.Digest)
	if _, err := os.Stat(jarPath); err != nil {
		return ResolvedBlobs{}, fmt.Errorf("jar blob %s not available: %w", layer.Digest, err)
	}
	cpPaths, err := existingBlobPaths(src, man.ClasspathLayers(), "classpath")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	mpPaths, err := existingBlobPaths(src, man.ModulepathLayers(), "modulepath")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	var cdsPath string
	if l, ok := man.CDSLayer(); ok {
		cdsPath = src.BlobPath(l.Digest)
		if _, err := os.Stat(cdsPath); err != nil {
			return ResolvedBlobs{}, fmt.Errorf("cds blob %s not available: %w", l.Digest, err)
		}
	}
	return ResolvedBlobs{Config: cfg, JarHostPath: jarPath, ClasspathHostPaths: cpPaths, ModulepathHostPaths: mpPaths, CDSHostPath: cdsPath}, nil
}

// existingBlobPaths returns the on-disk path of each layer, verifying presence.
func existingBlobPaths(src BlobSource, layers []Descriptor, kind string) ([]string, error) {
	var out []string
	for _, l := range layers {
		p := src.BlobPath(l.Digest)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("%s blob %s not available: %w", kind, l.Digest, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// ResolveRunnableBlobs resolves a runnable OCI image: the launch config comes
// from the manifest's brewlet.sh/jvm-config annotation, and the standard
// tar+gzip layers are staged (gunzipped) into a per-image temp tree the sandbox
// reads from — the JAR (and optional CDS archive) as files, and the
// classpath/module layers as uncompressed tars the existing staging path
// consumes unchanged. Staging is idempotent (keyed on manifestDigest) so
// repeated resolutions of the same image reuse it.
func ResolveRunnableBlobs(src BlobSource, man Manifest, manifestDigest string) (ResolvedBlobs, error) {
	cfg, err := man.RunnableConfig()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	stageDir := runnableStageDir(manifestDigest)

	appLayer, err := man.RunnableAppLayer()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	appDir := filepath.Join(stageDir, "app")
	if err := extractGzTar(src, appLayer.Digest, appDir); err != nil {
		return ResolvedBlobs{}, fmt.Errorf("stage app layer: %w", err)
	}
	jarName := cfg.MainJar
	if jarName == "" {
		jarName = "app.jar"
	}
	jarPath := filepath.Join(appDir, jarName)
	if _, err := os.Stat(jarPath); err != nil {
		return ResolvedBlobs{}, fmt.Errorf("runnable image app layer missing jar %q: %w", jarName, err)
	}
	var cdsPath string
	if cfg.CDS != nil && cfg.CDS.Archive != "" {
		cdsPath = filepath.Join(appDir, cfg.CDS.Archive)
		if _, err := os.Stat(cdsPath); err != nil {
			return ResolvedBlobs{}, fmt.Errorf("runnable image app layer missing cds archive %q: %w", cfg.CDS.Archive, err)
		}
	}

	cpPaths, err := stageLayerTars(src, man.RunnableClasspathLayers(), stageDir, "cp")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	mpPaths, err := stageLayerTars(src, man.RunnableModulepathLayers(), stageDir, "mp")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	return ResolvedBlobs{Config: cfg, JarHostPath: jarPath, ClasspathHostPaths: cpPaths, ModulepathHostPaths: mpPaths, CDSHostPath: cdsPath}, nil
}

// runnableStageDir is the per-image staging directory a runnable image is
// gunzipped into. It is derived from the manifest digest so concurrent/repeated
// resolutions of the same image share one tree. Overridable via
// BREWLET_RUNNABLE_STAGE for tests/harnesses.
func runnableStageDir(manifestDigest string) string {
	base := os.Getenv("BREWLET_RUNNABLE_STAGE")
	if base == "" {
		base = filepath.Join(os.TempDir(), "brewlet-runnable")
	}
	_, hex, found := strings.Cut(manifestDigest, ":")
	if !found {
		hex = fmt.Sprintf("%x", sha256.Sum256([]byte(manifestDigest)))
	}
	return filepath.Join(base, hex)
}

// stageLayerTars gunzips each layer blob to an uncompressed <prefix>-<i>.tar
// under stageDir and returns those paths (which the existing classpath/modulepath
// extraction consumes unchanged).
func stageLayerTars(src BlobSource, layers []Descriptor, stageDir, prefix string) ([]string, error) {
	if len(layers) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(layers))
	for i, l := range layers {
		dst := filepath.Join(stageDir, fmt.Sprintf("%s-%d.tar", prefix, i))
		if err := gunzipToFile(src, l.Digest, dst); err != nil {
			return nil, fmt.Errorf("stage %s layer %s: %w", prefix, l.Digest, err)
		}
		out = append(out, dst)
	}
	return out, nil
}

// gunzipToFile decompresses the gzip blob at digest into dst.
func gunzipToFile(src BlobSource, digest, dst string) error {
	raw, err := src.ReadBlob(digest)
	if err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer gr.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, gr); err != nil { //nolint:gosec // trusted layer content
		return err
	}
	return nil
}

// extractGzTar gunzips the tar+gzip blob at digest and unpacks its (flat) entries
// into destDir, rejecting any entry whose path would escape destDir.
func extractGzTar(src BlobSource, digest, destDir string) error {
	raw, err := src.ReadBlob(digest)
	if err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer gr.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gr)
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
