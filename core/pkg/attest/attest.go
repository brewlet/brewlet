// Package attest is the single source of truth for Brewlet's DSSE/in-toto
// signing and verification of managed-dependency evidence. It contains only the
// wire types, the ECDSA P-256 signing profile, and the predicate-binding checks
// defined by the Brewlet specification (§4.5). It has no OCI-store, registry, or
// Maven dependencies so that out-of-tree consumers — for example an admission
// verifier that receives an already-fetched DSSE envelope — can reuse the exact
// verification logic instead of reimplementing divergent crypto.
//
// The in-tree OCI store in internal/artifact aliases and delegates to this
// package; there is exactly one implementation of the signing profile.
package attest

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
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	// DSSEEnvelopeMediaType is the OCI layer media type carrying a signed DSSE
	// envelope in a Brewlet attestation referrer.
	DSSEEnvelopeMediaType = "application/vnd.dsse.envelope.v1+json"
	// InTotoPayloadType is the DSSE payloadType for the in-toto Statement.
	InTotoPayloadType = "application/vnd.in-toto+json"
	// AttestationArtifactType is the OCI artifactType of a Brewlet attestation
	// referrer manifest (both bundle and managed-image provenance).
	AttestationArtifactType = "application/vnd.brewlet.attestation.v1+json"

	// BundlePredicateType is the in-toto predicate type for a dependency-bundle
	// provenance attestation.
	BundlePredicateType = "https://brewlet.sh/attestations/dependency-bundle/v1"
	// ManagedPredicateType is the in-toto predicate type for a final-image
	// managed-dependency attestation.
	ManagedPredicateType = "https://brewlet.sh/attestations/managed-dependencies/v1"

	// PredicateTypeAnnotation is the referrer-manifest discovery hint recording
	// the predicate type. It is never accepted as trust evidence.
	PredicateTypeAnnotation = "brewlet.sh/predicate-type"

	statementType = "https://in-toto.io/Statement/v1"
)

// InTotoStatement is an in-toto Statement v1.
type InTotoStatement struct {
	Type          string          `json:"_type"`
	Subject       []InTotoSubject `json:"subject"`
	PredicateType string          `json:"predicateType"`
	Predicate     any             `json:"predicate"`
}

// InTotoSubject binds a statement to a single subject digest.
type InTotoSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// DSSEEnvelope is a Dead Simple Signing Envelope.
type DSSEEnvelope struct {
	PayloadType string          `json:"payloadType"`
	Payload     string          `json:"payload"`
	Signatures  []DSSESignature `json:"signatures"`
}

// DSSESignature is a single DSSE signature and its key identifier.
type DSSESignature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// BundleProvenance is the dependency-bundle predicate.
type BundleProvenance struct {
	SchemaVersion          int    `json:"schemaVersion"`
	DependencyBundleDigest string `json:"dependencyBundleDigest"`
	DependencyLayerDigest  string `json:"dependencyLayerDigest"`
	DependencyLockDigest   string `json:"dependencyLockDigest"`
	SBOMDigest             string `json:"sbomDigest"`
	SourceBOM              string `json:"sourceBom"`
	BuilderIdentity        string `json:"builderIdentity"`
}

// ManagedDependencyPredicate is the final-image managed-dependency predicate.
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

// LoadECDSAPrivateKey reads a PKCS#8 PEM ECDSA P-256 private key.
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

// GenerateECDSAKeyPair writes a fresh PKCS#8 private and SubjectPublicKeyInfo
// public PEM key pair to the given paths.
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

// ParseECDSAPublicKey parses a SubjectPublicKeyInfo PEM ECDSA P-256 public key
// from memory. It is the byte-oriented counterpart to LoadECDSAPublicKey for
// callers that hold the key material directly (e.g. a mounted Secret).
func ParseECDSAPublicKey(raw []byte) (*ecdsa.PublicKey, error) {
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

// LoadECDSAPublicKey reads a SubjectPublicKeyInfo PEM ECDSA P-256 public key.
func LoadECDSAPublicKey(path string) (*ecdsa.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trusted public key: %w", err)
	}
	return ParseECDSAPublicKey(raw)
}

// NewStatement builds an in-toto Statement v1 for a single subject digest.
func NewStatement(name, digest, predicateType string, predicate any) InTotoStatement {
	return InTotoStatement{
		Type: statementType,
		Subject: []InTotoSubject{{Name: name, Digest: map[string]string{
			"sha256": strings.TrimPrefix(digest, "sha256:"),
		}}},
		PredicateType: predicateType, Predicate: predicate,
	}
}

// SignStatement marshals and signs a statement into a single-signature DSSE
// envelope using ECDSA P-256 over the DSSE pre-authentication encoding.
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
	keyID, err := PublicKeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return json.Marshal(DSSEEnvelope{
		PayloadType: InTotoPayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []DSSESignature{{KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(signature)}},
	})
}

// VerifyStatement validates a single-signature DSSE envelope against the trusted
// public key and returns the decoded statement only when the signature, key ID,
// statement type, predicate type, and single-subject digest all match. It fails
// closed on any deviation.
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
	keyID, err := PublicKeyID(key)
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
	if statement.Type != statementType || statement.PredicateType != predicateType {
		return InTotoStatement{}, fmt.Errorf("unexpected in-toto statement or predicate type")
	}
	if len(statement.Subject) != 1 || "sha256:"+statement.Subject[0].Digest["sha256"] != subjectDigest {
		return InTotoStatement{}, fmt.Errorf("in-toto subject digest mismatch")
	}
	return statement, nil
}

// VerifyManagedAttestation verifies a final-image managed-dependency DSSE
// envelope end to end: the signature and key ID against the trusted public key,
// the managed predicate type, the subject binding to the final image digest,
// every predicate digest binding, and the expected application-builder identity.
// It returns the verified predicate, or a fail-closed error for missing,
// malformed, wrong-key, wrong-identity, or wrong-subject evidence.
func VerifyManagedAttestation(envelope []byte, key *ecdsa.PublicKey, identity, subjectDigest string) (ManagedDependencyPredicate, error) {
	if key == nil {
		return ManagedDependencyPredicate{}, fmt.Errorf("trusted public key is required")
	}
	if strings.TrimSpace(identity) == "" {
		return ManagedDependencyPredicate{}, fmt.Errorf("expected builder identity is required")
	}
	statement, err := VerifyStatement(envelope, key, ManagedPredicateType, subjectDigest)
	if err != nil {
		return ManagedDependencyPredicate{}, err
	}
	raw, err := json.Marshal(statement.Predicate)
	if err != nil {
		return ManagedDependencyPredicate{}, err
	}
	var predicate ManagedDependencyPredicate
	if err := decodeStrict(raw, &predicate); err != nil {
		return ManagedDependencyPredicate{}, fmt.Errorf("decode managed predicate: %w", err)
	}
	if err := predicate.Validate(subjectDigest); err != nil {
		return ManagedDependencyPredicate{}, err
	}
	if predicate.BuilderIdentity != identity {
		return ManagedDependencyPredicate{}, fmt.Errorf("managed dependency attestation identity does not match the expected builder identity")
	}
	return predicate, nil
}

// Validate checks that a managed-dependency predicate is internally well formed
// and bound to the given subject digest.
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

// PublicKeyID is the Brewlet key identifier: "sha256:" followed by the lowercase
// SHA-256 of the SubjectPublicKeyInfo DER encoding.
func PublicKeyID(key *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
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
