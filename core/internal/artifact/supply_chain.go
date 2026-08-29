package artifact

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	OCIEmptyConfigMediaType = "application/vnd.oci.empty.v1+json"
	CycloneDXMediaType      = "application/vnd.cyclonedx+json"
	AttestationArtifactType = "application/vnd.brewlet.attestation.v1+json"
	DSSEEnvelopeMediaType   = "application/vnd.dsse.envelope.v1+json"
	InTotoPayloadType       = "application/vnd.in-toto+json"

	BundlePredicateType  = "https://brewlet.sh/attestations/dependency-bundle/v1"
	ManagedPredicateType = "https://brewlet.sh/attestations/managed-dependencies/v1"

	ReferrerSubjectAnnotation = "brewlet.sh/referrer-subject"
	PredicateTypeAnnotation   = "brewlet.sh/predicate-type"
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

type InTotoStatement struct {
	Type          string          `json:"_type"`
	Subject       []InTotoSubject `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     any             `json:"predicate"`
}

type InTotoSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type DSSEEnvelope struct {
	PayloadType string          `json:"payloadType"`
	Payload     string          `json:"payload"`
	Signatures  []DSSESignature `json:"signatures"`
}

type DSSESignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

type BundleProvenance struct {
	SchemaVersion          int    `json:"schemaVersion"`
	DependencyBundleDigest string `json:"dependencyBundleDigest"`
	DependencyLayerDigest  string `json:"dependencyLayerDigest"`
	DependencyLockDigest   string `json:"dependencyLockDigest"`
	SBOMDigest             string `json:"sbomDigest"`
	SourceBOM              string `json:"sourceBom"`
	BuilderIdentity        string `json:"builderIdentity"`
}

type ManagedDependencyPredicate struct {
	SchemaVersion          int    `json:"schemaVersion"`
	FinalImageDigest       string `json:"finalImageDigest"`
	ThinJar                bool   `json:"thinJar"`
	ApplicationJarDigest   string `json:"applicationJarDigest"`
	DependencyBundleDigest string `json:"dependencyBundleDigest"`
	DependencyLayerDigest  string `json:"dependencyLayerDigest"`
	DependencyLockDigest   string `json:"dependencyLockDigest"`
	SBOMDigest             string `json:"sbomDigest"`
	SourceBOM              string `json:"sourceBom"`
	BuilderIdentity        string `json:"builderIdentity"`
}

type VerifiedBundleSupplyChain struct {
	SBOMDigest      string
	BuilderIdentity string
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

func LoadECDSAPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("signing key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 signing key: %w", err)
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok || ecdsaKey.Curve != elliptic.P256() {
		return nil, fmt.Errorf("signing key must be ECDSA P-256")
	}
	return ecdsaKey, nil
}

func GenerateECDSAKeyPair(privatePath, publicPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{
		Type: "PRIVATE KEY", Bytes: privateDER,
	}), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{
		Type: "PUBLIC KEY", Bytes: publicDER,
	}), 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}

func LoadECDSAPublicKey(path string) (*ecdsa.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trusted public key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("trusted public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse trusted public key: %w", err)
	}
	ecdsaKey, ok := key.(*ecdsa.PublicKey)
	if !ok || ecdsaKey.Curve != elliptic.P256() {
		return nil, fmt.Errorf("trusted public key must be ECDSA P-256")
	}
	return ecdsaKey, nil
}

func SignStatement(statement InTotoStatement, key *ecdsa.PrivateKey) ([]byte, error) {
	payload, err := json.Marshal(statement)
	if err != nil {
		return nil, err
	}
	pae := dssePAE(InTotoPayloadType, payload)
	sum := sha256.Sum256(pae)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return nil, err
	}
	keyID, err := publicKeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return json.Marshal(DSSEEnvelope{
		PayloadType: InTotoPayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []DSSESignature{{KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(signature)}},
	})
}

func VerifyStatement(raw []byte, key *ecdsa.PublicKey, predicateType, subjectDigest string) (InTotoStatement, error) {
	var envelope DSSEEnvelope
	if err := decodeStrict(raw, &envelope); err != nil {
		return InTotoStatement{}, fmt.Errorf("decode DSSE envelope: %w", err)
	}
	if envelope.PayloadType != InTotoPayloadType || len(envelope.Signatures) != 1 {
		return InTotoStatement{}, fmt.Errorf("unsupported DSSE payload type or signature count")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return InTotoStatement{}, fmt.Errorf("decode DSSE payload: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signatures[0].Sig)
	if err != nil {
		return InTotoStatement{}, fmt.Errorf("decode DSSE signature: %w", err)
	}
	keyID, err := publicKeyID(key)
	if err != nil {
		return InTotoStatement{}, err
	}
	if envelope.Signatures[0].KeyID != keyID {
		return InTotoStatement{}, fmt.Errorf("DSSE key ID is not trusted")
	}
	sum := sha256.Sum256(dssePAE(envelope.PayloadType, payload))
	if !ecdsa.VerifyASN1(key, sum[:], signature) {
		return InTotoStatement{}, fmt.Errorf("DSSE signature verification failed")
	}
	var statement InTotoStatement
	if err := json.Unmarshal(payload, &statement); err != nil {
		return InTotoStatement{}, fmt.Errorf("decode in-toto statement: %w", err)
	}
	if statement.Type != "https://in-toto.io/Statement/v1" || statement.PredicateType != predicateType {
		return InTotoStatement{}, fmt.Errorf("unexpected in-toto statement or predicate type")
	}
	if len(statement.Subject) != 1 || "sha256:"+statement.Subject[0].Digest["sha256"] != subjectDigest {
		return InTotoStatement{}, fmt.Errorf("in-toto subject digest mismatch")
	}
	return statement, nil
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
		if desc.Annotations[ReferrerSubjectAnnotation] != subjectDigest ||
			(artifactType != "" && desc.ArtifactType != artifactType) {
			continue
		}
		referrer, err := s.readReferrer(desc, subject)
		if err != nil {
			continue
		}
		out = append(out, referrer)
	}
	return out, nil
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
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Referrer{}, err
	}
	if manifest.MediaType != ociManifestMediaType ||
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
	sbom, err := GenerateCycloneDX(lock, cfg)
	if err != nil {
		return "", err
	}
	_, err = s.PublishReferrer(bundle, CycloneDXMediaType, CycloneDXMediaType, sbom, nil)
	if err != nil {
		return "", err
	}
	predicate := BundleProvenance{
		SchemaVersion: 1, DependencyBundleDigest: bundle.Digest,
		DependencyLayerDigest: cfg.LayerDigest, DependencyLockDigest: cfg.LockDigest,
		SBOMDigest: digestDocument(sbom), SourceBOM: cfg.SourceBOM, BuilderIdentity: identity,
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

func (s Store) VerifyBundleSupplyChain(bundle ResolvedDependencyBundle, key *ecdsa.PublicKey, identity string) (VerifiedBundleSupplyChain, error) {
	sboms, err := s.Referrers(bundle.ManifestDigest, CycloneDXMediaType)
	if err != nil {
		return VerifiedBundleSupplyChain{}, err
	}
	if len(sboms) != 1 {
		return VerifiedBundleSupplyChain{}, fmt.Errorf("bundle requires exactly one CycloneDX SBOM referrer, got %d", len(sboms))
	}
	if err := validateCycloneDX(sboms[0].Document, bundle.Lock, bundle.Config); err != nil {
		return VerifiedBundleSupplyChain{}, err
	}

	attestations, err := s.Referrers(bundle.ManifestDigest, AttestationArtifactType)
	if err != nil {
		return VerifiedBundleSupplyChain{}, err
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
			predicate.SBOMDigest != digestDocument(sboms[0].Document) ||
			predicate.SourceBOM != bundle.Config.SourceBOM {
			verificationErr = fmt.Errorf("bundle provenance bindings or signer identity do not match")
			continue
		}
		return VerifiedBundleSupplyChain{SBOMDigest: predicate.SBOMDigest, BuilderIdentity: identity}, nil
	}
	if verificationErr != nil {
		return VerifiedBundleSupplyChain{}, fmt.Errorf("no trusted bundle provenance attestation: %w", verificationErr)
	}
	return VerifiedBundleSupplyChain{}, fmt.Errorf("signed bundle provenance attestation is missing")
}

func validateCycloneDX(raw []byte, lock DependencyLock, cfg DependencyBundleConfig) error {
	var sbom CycloneDXBOM
	if err := decodeStrict(raw, &sbom); err != nil {
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

func (p ManagedDependencyPredicate) Validate(subjectDigest string) error {
	if p.SchemaVersion != 1 || !p.ThinJar {
		return fmt.Errorf("managed dependency predicate requires schemaVersion=1 and thinJar=true")
	}
	if p.FinalImageDigest != subjectDigest {
		return fmt.Errorf("managed predicate final image digest does not match subject")
	}
	for field, digest := range map[string]string{
		"applicationJarDigest":   p.ApplicationJarDigest,
		"dependencyBundleDigest": p.DependencyBundleDigest,
		"dependencyLayerDigest":  p.DependencyLayerDigest,
		"dependencyLockDigest":   p.DependencyLockDigest,
		"sbomDigest":             p.SBOMDigest,
	} {
		if !isSHA256Digest(digest) {
			return fmt.Errorf("managed predicate %s is not a sha256 digest", field)
		}
	}
	if err := validateMavenCoordinate(p.SourceBOM); err != nil {
		return fmt.Errorf("managed predicate sourceBom: %w", err)
	}
	if strings.TrimSpace(p.BuilderIdentity) == "" {
		return fmt.Errorf("managed predicate builderIdentity must be non-empty")
	}
	return nil
}

func newStatement(name, digest, predicateType string, predicate any) InTotoStatement {
	return InTotoStatement{
		Type: "https://in-toto.io/Statement/v1",
		Subject: []InTotoSubject{{Name: name, Digest: map[string]string{
			"sha256": strings.TrimPrefix(digest, "sha256:"),
		}}},
		PredicateType: predicateType, Predicate: predicate,
	}
}

func dssePAE(payloadType string, payload []byte) []byte {
	return bytes.Join([][]byte{
		[]byte("DSSEv1"),
		[]byte(strconv.Itoa(len(payloadType))),
		[]byte(payloadType),
		[]byte(strconv.Itoa(len(payload))),
		payload,
	}, []byte(" "))
}

func publicKeyID(key *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
