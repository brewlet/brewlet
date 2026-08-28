package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brewlet/brewlet/internal/artifact"
)

// artifactBlobs is the OCI artifact after separating the JVM launch config from
// the payload layers — independent of *where* those blobs were read from and of
// the delivery format. It is the shared normalized shape defined in the artifact
// package; the shim keeps this alias for readability at call sites.
type artifactBlobs = artifact.ResolvedBlobs

// contentStoreSource reads blobs from containerd's on-disk content store. It
// satisfies artifact.BlobSource so the shared format-resolution logic in the
// artifact package works against it unchanged.
type contentStoreSource struct{ root string }

func (c contentStoreSource) ReadBlob(digest string) ([]byte, error) {
	return os.ReadFile(c.BlobPath(digest))
}
func (c contentStoreSource) BlobPath(digest string) string { return contentBlobPath(c.root, digest) }

// loadArtifactBlobs reads the artifact's config + payload layers from the
// configured blob source. Two backends are supported:
//
//   - "layout"     — a Brewlet-local OCI image layout (the PoC's stand-in for a
//     registry; addressed by tag/ref). Selected when StoreRoot is set.
//   - "containerd" — containerd's own on-disk content store, addressed by the
//     artifact manifest digest. This is the production path once containerd has
//     pulled the OCI artifact into its content store.
//
// The backend is taken from ic.Backend, or inferred: a StoreRoot implies the
// layout backend, otherwise the containerd content store is used. Each backend
// handles BOTH a native Brewlet artifact (custom media types) and a runnable OCI
// image (standard tar+gzip layers pulled by kubelet); the manifest tells them
// apart via Manifest.IsRunnableImage. The actual format resolution lives in the
// artifact package so the local CLI (run/bundle) and the shim share one path.
func loadArtifactBlobs(ic imageConfig) (artifactBlobs, error) {
	backend := ic.Backend
	if backend == "" {
		if ic.StoreRoot != "" {
			backend = "layout"
		} else {
			backend = "containerd"
		}
	}
	switch backend {
	case "layout":
		return artifact.Store{Root: ic.StoreRoot}.ResolveBlobs(ic.Ref)
	case "containerd":
		root := ic.ContentRoot
		if root == "" {
			root = defaultContentRoot
		}
		return contentStoreBlobs(root, ic.ManifestDigest)
	default:
		return artifactBlobs{}, fmt.Errorf("unknown artifact store backend %q", backend)
	}
}

// defaultContentRoot is containerd's default content store location.
const defaultContentRoot = "/var/lib/containerd/io.containerd.content.v1.content"

// contentStoreBlobs resolves the artifact from containerd's content store given
// the manifest digest (which may be an image index — the running node's platform
// manifest is selected). It handles both a native Brewlet artifact and a
// kubelet-pulled runnable image. No copy of the native JAR blob is made — it is
// mounted from the content store directly; a runnable image's gzip layers are
// staged (gunzipped) once per image into a temp tree the sandbox reads from.
func contentStoreBlobs(contentRoot, manifestDigest string) (artifactBlobs, error) {
	if manifestDigest == "" {
		return artifactBlobs{}, fmt.Errorf("containerd backend requires the artifact manifest digest (annotation %q)", annArtifactDigest)
	}
	src := contentStoreSource{root: contentRoot}
	man, digest, err := artifact.ResolveManifestFollowingIndex(src, manifestDigest)
	if err != nil {
		return artifactBlobs{}, err
	}
	if man.IsRunnableImage() {
		return artifact.ResolveRunnableBlobs(src, man, digest)
	}
	if man.ArtifactType != "" && man.ArtifactType != artifact.ArtifactType {
		return artifactBlobs{}, fmt.Errorf("not a Brewlet OCI artifact (artifactType=%q)", man.ArtifactType)
	}
	return artifact.ResolveNativeBlobs(src, man)
}

// contentBlobPath maps a "sha256:<hex>" digest to its on-disk blob path in an
// OCI-style content store: <root>/blobs/<algo>/<hex>.
func contentBlobPath(root, digest string) string {
	algo, hex, found := strings.Cut(digest, ":")
	if !found {
		algo, hex = "sha256", digest
	}
	return filepath.Join(root, "blobs", algo, hex)
}
