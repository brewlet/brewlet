package artifact

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDSSEStatementVerification(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subject := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	statement := newStatement("bundle", subject, BundlePredicateType,
		BundleProvenance{SchemaVersion: 1, DependencyBundleDigest: subject})
	envelope, err := SignStatement(statement, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyStatement(envelope, &key.PublicKey, BundlePredicateType, subject); err != nil {
		t.Fatalf("VerifyStatement: %v", err)
	}
	var parsed DSSEEnvelope
	if err := json.Unmarshal(envelope, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.Payload = parsed.Payload[:len(parsed.Payload)-1] + "A"
	tampered, _ := json.Marshal(parsed)
	if _, err := VerifyStatement(tampered, &key.PublicKey, BundlePredicateType, subject); err == nil {
		t.Fatal("expected tampered envelope rejection")
	}
}

func TestBundleAndManagedAttestationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "key.pem")
	publicPath := filepath.Join(dir, "key.pub.pem")
	if err := GenerateECDSAKeyPair(privatePath, publicPath); err != nil {
		t.Fatal(err)
	}
	privateKey, err := LoadECDSAPrivateKey(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := LoadECDSAPublicKey(publicPath)
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("dependency")
	layerPath := filepath.Join(dir, "dependencies.tar")
	writeOrderedTar(t, layerPath, []tarFile{{name: "dependency.jar", content: content}})
	lock := DependencyLock{SchemaVersion: 1, Artifacts: []DependencyLockEntry{{
		GroupID: "com.example", ArtifactID: "dependency", Version: "1",
		Type: "jar", Scope: "runtime", FileName: "dependency.jar", SHA256: hexDigest(content),
	}}}
	store := Store{Root: filepath.Join(dir, "oci")}
	bundleDesc, err := store.PushDependencyBundle("platform/deps:1", DependencyBundleConfig{
		Name: "deps", Version: "1", SourceBOM: "com.example:platform-bom:1",
	}, lock, layerPath)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := store.ResolveDependencyBundle("platform/deps:1")
	if err != nil {
		t.Fatal(err)
	}
	sbomDigest, err := store.PublishBundleSupplyChain(
		bundleDesc, bundle.Config, bundle.Lock, privateKey, "platform-builder")
	if err != nil {
		t.Fatal(err)
	}
	rejectedPredicate := BundleProvenance{
		SchemaVersion: 1, DependencyBundleDigest: bundle.ManifestDigest,
		DependencyLayerDigest: bundle.Config.LayerDigest,
		DependencyLockDigest:  bundle.Config.LockDigest,
		SBOMDigest:            sbomDigest, SourceBOM: bundle.Config.SourceBOM,
		BuilderIdentity: "untrusted-builder",
	}
	rejectedEnvelope, err := SignStatement(
		newStatement("dependency-bundle", bundle.ManifestDigest,
			BundlePredicateType, rejectedPredicate), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	rejectedDescriptor, err := store.PublishReferrer(bundleDesc,
		AttestationArtifactType, DSSEEnvelopeMediaType, rejectedEnvelope,
		map[string]string{PredicateTypeAnnotation: BundlePredicateType})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishReferrer(bundleDesc,
		AttestationArtifactType, CycloneDXMediaType, []byte("malformed"),
		map[string]string{PredicateTypeAnnotation: BundlePredicateType}); err != nil {
		t.Fatal(err)
	}
	index, err := store.readIndex()
	if err != nil {
		t.Fatal(err)
	}
	for i, descriptor := range index.Manifests {
		if descriptor.Digest == rejectedDescriptor.Digest {
			index.Manifests = append([]Descriptor{descriptor},
				append(index.Manifests[:i], index.Manifests[i+1:]...)...)
			break
		}
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.indexPath(), indexJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	verified, err := store.VerifyBundleSupplyChain(bundle, publicKey, "platform-builder")
	if err != nil {
		t.Fatal(err)
	}
	if verified.SBOMDigest != sbomDigest {
		t.Fatalf("sbom digest = %s, want %s", verified.SBOMDigest, sbomDigest)
	}

	image := Descriptor{
		MediaType: OCIImageIndexMediaType,
		Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Size:      42,
	}
	if err := store.appendIndex(image); err != nil {
		t.Fatal(err)
	}
	predicate := ManagedDependencyPredicate{
		SchemaVersion: 1, FinalImageDigest: image.Digest, ThinJar: true,
		ApplicationJarDigest:   "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		DependencyBundleDigest: bundle.ManifestDigest,
		DependencyLayerDigest:  bundle.Config.LayerDigest,
		DependencyLockDigest:   bundle.Config.LockDigest,
		SBOMDigest:             sbomDigest, SourceBOM: bundle.Config.SourceBOM,
		BuilderIdentity: "platform-builder",
	}
	if _, err := store.PublishManagedAttestation(image, predicate, privateKey); err != nil {
		t.Fatal(err)
	}
	got, err := store.VerifyManagedAttestation(image, publicKey, "platform-builder")
	if err != nil {
		t.Fatal(err)
	}
	if got.SBOMDigest != sbomDigest {
		t.Fatalf("managed predicate sbom = %s", got.SBOMDigest)
	}
	if _, err := store.VerifyManagedAttestation(image, publicKey, "other-builder"); err == nil {
		t.Fatal("expected wrong identity rejection")
	}
}
