package artifact

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/brewlet/brewlet/pkg/attest"
)

const (
	OCIEmptyConfigMediaType = "application/vnd.oci.empty.v1+json"
	CycloneDXMediaType      = "application/vnd.cyclonedx+json"

	// The attestation media types, predicate types, and the predicate-type
	// discovery annotation are defined once in pkg/attest, the single source of
	// truth for Brewlet's signing profile. They are re-exported here so the OCI
	// store keeps a stable local API.
	AttestationArtifactType = attest.AttestationArtifactType
	DSSEEnvelopeMediaType   = attest.DSSEEnvelopeMediaType
	InTotoPayloadType       = attest.InTotoPayloadType

	BundlePredicateType  = attest.BundlePredicateType
	ManagedPredicateType = attest.ManagedPredicateType

	ReferrerSubjectAnnotation = "brewlet.sh/referrer-subject"
	PredicateTypeAnnotation   = attest.PredicateTypeAnnotation
)

type CycloneDXBOM struct {
	BOMFormat   string            `json:"bomFormat"`
	SpecVersion string            `json:"specVersion"`
	Version     int               `json:"version"`
	Metadata    CycloneDXMetadata `json:"metadata"`
	Components  []CycloneDXItem   `json:"components"`
}

type CycloneDXMetadata struct {
	Component CycloneDXComponent `json:"component"`
}

type CycloneDXComponent struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type CycloneDXItem struct {
	Type    string          `json:"type"`
	Group   string          `json:"group"`
	Name    string          `json:"name"`
	Version string          `json:"version"`
	PURL    string          `json:"purl"`
	Hashes  []CycloneDXHash `json:"hashes"`
}

type CycloneDXHash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}

// The DSSE/in-toto wire types and Brewlet predicates are defined once in
// pkg/attest. They are aliased here so existing store code and tests keep using
// the artifact.* names while sharing a single implementation.
type (
	InTotoStatement            = attest.InTotoStatement
	InTotoSubject              = attest.InTotoSubject
	DSSEEnvelope               = attest.DSSEEnvelope
	DSSESignature              = attest.DSSESignature
	BundleProvenance           = attest.BundleProvenance
	ManagedDependencyPredicate = attest.ManagedDependencyPredicate
)

type VerifiedBundleSupplyChain struct {
	SBOMDigest      string
	BuilderIdentity string
	Signed          bool
}

type Referrer struct {
	Descriptor Descriptor
	Manifest   Manifest
	Document   []byte
}

func GenerateCycloneDX(lock DependencyLock, cfg DependencyBundleConfig) ([]byte, error) {
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	items := make([]CycloneDXItem, 0, len(lock.Artifacts))
	for _, entry := range lock.Artifacts {
		purl := mavenPURL(entry)
		items = append(items, CycloneDXItem{
			Type: "library", Group: entry.GroupID, Name: entry.ArtifactID,
			Version: entry.Version, PURL: purl,
			Hashes: []CycloneDXHash{{Algorithm: "SHA-256", Content: entry.SHA256}},
		})
	}

	return json.Marshal(CycloneDXBOM{
		BOMFormat: "CycloneDX", SpecVersion: "1.5", Version: 1,
		Metadata:   CycloneDXMetadata{Component: CycloneDXComponent{Type: "library", Name: cfg.Name, Version: cfg.Version}},
		Components: items,
	})
}

func mavenPURL(entry DependencyLockEntry) string {
	escape := func(value string) string {
		return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
	}
	purl := "pkg:maven/" + escape(entry.GroupID) + "/" + escape(entry.ArtifactID) +
		"@" + escape(entry.Version)
	var qualifiers []string
	if entry.Type != "jar" {
		qualifiers = append(qualifiers, "type="+escape(entry.Type))
	}
	if entry.Classifier != "" {
		qualifiers = append(qualifiers, "classifier="+escape(entry.Classifier))
	}
	if len(qualifiers) > 0 {
		purl += "?" + strings.Join(qualifiers, "&")
	}
	return purl
}

// LoadECDSAPrivateKey reads a PKCS#8 PEM ECDSA P-256 private key. See
// attest.LoadECDSAPrivateKey.
func LoadECDSAPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	return attest.LoadECDSAPrivateKey(path)
}

// GenerateECDSAKeyPair writes a fresh ECDSA P-256 key pair. See
// attest.GenerateECDSAKeyPair.
func GenerateECDSAKeyPair(privatePath, publicPath string) error {
	return attest.GenerateECDSAKeyPair(privatePath, publicPath)
}

// LoadECDSAPublicKey reads a SubjectPublicKeyInfo PEM ECDSA P-256 public key.
// See attest.LoadECDSAPublicKey.
func LoadECDSAPublicKey(path string) (*ecdsa.PublicKey, error) {
	return attest.LoadECDSAPublicKey(path)
}

// SignStatement signs a statement into a single-signature DSSE envelope. See
// attest.SignStatement.
func SignStatement(statement InTotoStatement, key *ecdsa.PrivateKey) ([]byte, error) {
	return attest.SignStatement(statement, key)
}

// VerifyStatement validates a single-signature DSSE envelope. See
// attest.VerifyStatement.
func VerifyStatement(raw []byte, key *ecdsa.PublicKey, predicateType, subjectDigest string) (InTotoStatement, error) {
	return attest.VerifyStatement(raw, key, predicateType, subjectDigest)
}

func (s Store) PublishReferrer(subject Descriptor, artifactType, layerMediaType string, document []byte, annotations map[string]string) (Descriptor, error) {
	emptyDesc, err := s.writeBlob([]byte("{}"))
	if err != nil {
		return Descriptor{}, err
	}
	emptyDesc.MediaType = OCIEmptyConfigMediaType
	documentDesc, err := s.writeBlob(document)
	if err != nil {
		return Descriptor{}, err
	}
	documentDesc.MediaType = layerMediaType
	manifest := Manifest{
		SchemaVersion: 2, MediaType: ociManifestMediaType, ArtifactType: artifactType,
		Subject: &Descriptor{MediaType: subject.MediaType, Digest: subject.Digest, Size: subject.Size},
		Config:  emptyDesc, Layers: []Descriptor{documentDesc}, Annotations: annotations,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Descriptor{}, err
	}
	desc, err := s.writeBlob(raw)
	if err != nil {
		return Descriptor{}, err
	}
	desc.MediaType = ociManifestMediaType
	desc.ArtifactType = artifactType
	desc.Annotations = map[string]string{ReferrerSubjectAnnotation: subject.Digest}
	if predicate := annotations[PredicateTypeAnnotation]; predicate != "" {
		desc.Annotations[PredicateTypeAnnotation] = predicate
	}
	if err := s.appendIndex(desc); err != nil {
		return Descriptor{}, err
	}
	return desc, s.writeLayoutMarker()
}

func (s Store) Referrers(subjectDigest, artifactType string) ([]Referrer, error) {
	idx, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	var subject Descriptor
	for _, desc := range idx.Manifests {
		if desc.Digest == subjectDigest {
			subject = desc
			break
		}
	}
	if subject.Digest == "" {
		return nil, fmt.Errorf("referrer subject %s is not indexed", subjectDigest)
	}
	var out []Referrer
	for _, desc := range idx.Manifests {
		if artifactType != "" && desc.ArtifactType != artifactType {
			continue
		}
		referrer, err := s.readReferrer(desc, subject)
		if err != nil {
			continue
		}
		if referrer.Manifest.Subject == nil ||
			referrer.Manifest.Subject.Digest != subjectDigest {
			continue
		}
		out = append(out, referrer)
	}
	return out, nil
}

func (s Store) referrerDescriptorCount(subjectDigest, artifactType string) (int, error) {
	idx, err := s.readIndex()
	if err != nil {
		return 0, err
	}
	var subject Descriptor
	for _, desc := range idx.Manifests {
		if desc.Digest == subjectDigest {
			subject = desc
			break
		}
	}
	if subject.Digest == "" {
		return 0, fmt.Errorf("referrer subject %s is not indexed", subjectDigest)
	}
	count := 0
	for _, desc := range idx.Manifests {
		if artifactType != "" && desc.ArtifactType != artifactType {
			continue
		}
		if desc.Annotations[ReferrerSubjectAnnotation] == subjectDigest {
			count++
			continue
		}
		if _, err := s.readReferrer(desc, subject); err == nil {
			count++
		}
	}
	return count, nil
}

func (s Store) readReferrer(desc, subject Descriptor) (Referrer, error) {
	if desc.MediaType != ociManifestMediaType {
		return Referrer{}, fmt.Errorf("invalid referrer manifest descriptor media type")
	}
	raw, err := s.readVerifiedBlob(desc)
	if err != nil {
		return Referrer{}, err
	}
	var manifest Manifest
	if err := decodeStrict(raw, &manifest); err != nil {
		return Referrer{}, err
	}
	if manifest.SchemaVersion != 2 ||
		manifest.MediaType != ociManifestMediaType ||
		manifest.ArtifactType != desc.ArtifactType ||
		manifest.Subject == nil ||
		manifest.Subject.MediaType != subject.MediaType ||
		manifest.Subject.Digest != subject.Digest ||
		manifest.Subject.Size != subject.Size ||
		manifest.Config.MediaType != OCIEmptyConfigMediaType ||
		len(manifest.Layers) != 1 {
		return Referrer{}, fmt.Errorf("invalid referrer manifest contract")
	}
	empty, err := s.readVerifiedBlob(manifest.Config)
	if err != nil || !bytes.Equal(empty, []byte("{}")) {
		return Referrer{}, fmt.Errorf("invalid referrer empty config")
	}
	expectedLayerType := DSSEEnvelopeMediaType
	if manifest.ArtifactType == CycloneDXMediaType {
		expectedLayerType = CycloneDXMediaType
	}
	if manifest.Layers[0].MediaType != expectedLayerType {
		return Referrer{}, fmt.Errorf("invalid referrer document layer media type")
	}
	document, err := s.readVerifiedBlob(manifest.Layers[0])
	if err != nil {
		return Referrer{}, err
	}
	return Referrer{Descriptor: desc, Manifest: manifest, Document: document}, nil
}

func (s Store) PublishBundleSupplyChain(bundle Descriptor, cfg DependencyBundleConfig, lock DependencyLock, key *ecdsa.PrivateKey, identity string) (string, error) {
	if strings.TrimSpace(identity) == "" {
		return "", fmt.Errorf("builder identity must be non-empty")
	}
	sbom, sbomDigest, err := s.publishBundleSBOM(bundle, cfg, lock)
	if err != nil {
		return "", err
	}
	predicate := BundleProvenance{
		SchemaVersion: 1, DependencyBundleDigest: bundle.Digest,
		DependencyLayerDigest: cfg.LayerDigest, DependencyLockDigest: cfg.LockDigest,
		SBOMDigest: sbomDigest, SourceBOM: cfg.SourceBOM, BuilderIdentity: identity,
	}
	statement := newStatement("dependency-bundle", bundle.Digest, BundlePredicateType, predicate)
	envelope, err := SignStatement(statement, key)
	if err != nil {
		return "", err
	}
	_, err = s.PublishReferrer(bundle, AttestationArtifactType, DSSEEnvelopeMediaType, envelope,
		map[string]string{PredicateTypeAnnotation: BundlePredicateType})
	return digestDocument(sbom), err
}

func (s Store) PublishBundleSBOM(bundle Descriptor, cfg DependencyBundleConfig, lock DependencyLock) (string, error) {
	_, sbomDigest, err := s.publishBundleSBOM(bundle, cfg, lock)
	return sbomDigest, err
}

func (s Store) publishBundleSBOM(bundle Descriptor, cfg DependencyBundleConfig, lock DependencyLock) ([]byte, string, error) {
	sbom, err := GenerateCycloneDX(lock, cfg)
	if err != nil {
		return nil, "", err
	}
	_, err = s.PublishReferrer(bundle, CycloneDXMediaType, CycloneDXMediaType, sbom, nil)
	if err != nil {
		return nil, "", err
	}
	return sbom, digestDocument(sbom), nil
}

func (s Store) VerifyBundleSupplyChain(bundle ResolvedDependencyBundle, key *ecdsa.PublicKey, identity string) (VerifiedBundleSupplyChain, error) {
	sbomCount, err := s.referrerDescriptorCount(bundle.ManifestDigest, CycloneDXMediaType)
	if err != nil {
		return VerifiedBundleSupplyChain{}, err
	}
	sboms, err := s.Referrers(bundle.ManifestDigest, CycloneDXMediaType)
	if err != nil {
		return VerifiedBundleSupplyChain{}, err
	}
	if sbomCount != 1 || len(sboms) != 1 {
		return VerifiedBundleSupplyChain{}, fmt.Errorf(
			"bundle requires exactly one valid CycloneDX SBOM referrer, discovered %d descriptor(s) and %d valid document(s)",
			sbomCount, len(sboms))
	}
	if err := validateCycloneDX(sboms[0].Document, bundle.Lock, bundle.Config); err != nil {
		return VerifiedBundleSupplyChain{}, err
	}
	sbomDigest := digestDocument(sboms[0].Document)

	attestations, err := s.Referrers(bundle.ManifestDigest, AttestationArtifactType)
	if err != nil {
		return VerifiedBundleSupplyChain{}, err
	}
	if len(attestations) == 0 {
		count, err := s.referrerDescriptorCount(bundle.ManifestDigest, AttestationArtifactType)
		if err != nil {
			return VerifiedBundleSupplyChain{}, err
		}
		if count != 0 {
			return VerifiedBundleSupplyChain{}, fmt.Errorf("bundle provenance is present but all %d referrer manifest(s) are invalid", count)
		}
		return VerifiedBundleSupplyChain{SBOMDigest: sbomDigest}, nil
	}
	if key == nil || strings.TrimSpace(identity) == "" {
		return VerifiedBundleSupplyChain{}, fmt.Errorf("bundle provenance is present; trusted public key and signer identity are required")
	}
	var verificationErr error
	for _, attestation := range attestations {
		if attestation.Manifest.Annotations[PredicateTypeAnnotation] != BundlePredicateType {
			continue
		}
		statement, err := VerifyStatement(attestation.Document, key, BundlePredicateType, bundle.ManifestDigest)
		if err != nil {
			verificationErr = err
			continue
		}
		raw, err := json.Marshal(statement.Predicate)
		if err != nil {
			return VerifiedBundleSupplyChain{}, err
		}
		var predicate BundleProvenance
		if err := decodeStrict(raw, &predicate); err != nil {
			verificationErr = err
			continue
		}
		if predicate.SchemaVersion != 1 || predicate.BuilderIdentity != identity ||
			predicate.DependencyBundleDigest != bundle.ManifestDigest ||
			predicate.DependencyLayerDigest != bundle.Config.LayerDigest ||
			predicate.DependencyLockDigest != bundle.Config.LockDigest ||
			predicate.SBOMDigest != sbomDigest ||
			predicate.SourceBOM != bundle.Config.SourceBOM {
			verificationErr = fmt.Errorf("bundle provenance bindings or signer identity do not match")
			continue
		}
		return VerifiedBundleSupplyChain{
			SBOMDigest: predicate.SBOMDigest, BuilderIdentity: identity, Signed: true,
		}, nil
	}
	if verificationErr != nil {
		return VerifiedBundleSupplyChain{}, fmt.Errorf("no trusted bundle provenance attestation: %w", verificationErr)
	}
	return VerifiedBundleSupplyChain{}, fmt.Errorf("bundle provenance is present but no trusted attestation satisfies the contract")
}

func validateCycloneDX(raw []byte, lock DependencyLock, cfg DependencyBundleConfig) error {
	var sbom CycloneDXBOM
	if err := json.Unmarshal(raw, &sbom); err != nil {
		return fmt.Errorf("decode CycloneDX SBOM: %w", err)
	}
	if sbom.BOMFormat != "CycloneDX" || sbom.SpecVersion != "1.5" || sbom.Version != 1 ||
		sbom.Metadata.Component.Name != cfg.Name || sbom.Metadata.Component.Version != cfg.Version ||
		len(sbom.Components) != len(lock.Artifacts) {
		return fmt.Errorf("bundle SBOM metadata or component count does not match the dependency bundle")
	}
	expected := make(map[string]DependencyLockEntry, len(lock.Artifacts))
	for _, entry := range lock.Artifacts {
		expected[mavenPURL(entry)] = entry
	}
	for _, component := range sbom.Components {
		entry, ok := expected[component.PURL]
		if !ok || component.Group != entry.GroupID || component.Name != entry.ArtifactID ||
			component.Version != entry.Version || len(component.Hashes) != 1 ||
			component.Hashes[0].Algorithm != "SHA-256" ||
			component.Hashes[0].Content != entry.SHA256 {
			return fmt.Errorf("bundle SBOM component %s:%s:%s does not match the dependency lock",
				component.Group, component.Name, component.Version)
		}
		delete(expected, component.PURL)
	}

	if len(expected) != 0 {
		return fmt.Errorf("bundle SBOM is missing dependency-lock components")
	}
	return nil
}

func digestDocument(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s Store) PublishManagedAttestation(image Descriptor, predicate ManagedDependencyPredicate, key *ecdsa.PrivateKey) (Descriptor, error) {
	if err := predicate.Validate(image.Digest); err != nil {
		return Descriptor{}, err
	}
	statement := newStatement("application-image", image.Digest, ManagedPredicateType, predicate)
	envelope, err := SignStatement(statement, key)
	if err != nil {
		return Descriptor{}, err
	}
	return s.PublishReferrer(image, AttestationArtifactType, DSSEEnvelopeMediaType, envelope,
		map[string]string{PredicateTypeAnnotation: ManagedPredicateType})
}

func (s Store) VerifyManagedAttestation(image Descriptor, key *ecdsa.PublicKey, identity string) (ManagedDependencyPredicate, error) {
	attestations, err := s.Referrers(image.Digest, AttestationArtifactType)
	if err != nil {
		return ManagedDependencyPredicate{}, err
	}
	var verificationErr error
	for _, attestation := range attestations {
		if attestation.Manifest.Annotations[PredicateTypeAnnotation] != ManagedPredicateType {
			continue
		}
		statement, err := VerifyStatement(attestation.Document, key, ManagedPredicateType, image.Digest)
		if err != nil {
			verificationErr = err
			continue
		}
		raw, _ := json.Marshal(statement.Predicate)
		var predicate ManagedDependencyPredicate
		if err := decodeStrict(raw, &predicate); err != nil {
			verificationErr = err
			continue
		}
		if err := predicate.Validate(image.Digest); err != nil {
			verificationErr = err
			continue
		}
		if predicate.BuilderIdentity != identity {
			verificationErr = fmt.Errorf("managed dependency attestation bindings or identity do not match")
			continue
		}
		return predicate, nil
	}
	if verificationErr != nil {
		return ManagedDependencyPredicate{}, fmt.Errorf("no trusted managed dependency attestation: %w", verificationErr)
	}
	return ManagedDependencyPredicate{}, fmt.Errorf("signed managed dependency attestation is missing")
}

// newStatement builds an in-toto Statement v1 for a single subject digest. See
// attest.NewStatement.
func newStatement(name, digest, predicateType string, predicate any) InTotoStatement {
	return attest.NewStatement(name, digest, predicateType, predicate)
}
