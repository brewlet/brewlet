package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brewlet/brewlet/pkg/attest"

	"github.com/ratify-project/ratify/pkg/common"
	"github.com/ratify-project/ratify/pkg/ocispecs"
	"github.com/ratify-project/ratify/pkg/referrerstore"
	"github.com/ratify-project/ratify/pkg/verifier"
	"github.com/ratify-project/ratify/pkg/verifier/plugin/skel"
)

const (
	pluginName    = "brewlet-managed-dependencies"
	pluginVersion = "1.0.0"
	verifierType  = "brewlet-managed-dependencies"
)

// PluginConfig is the plugin-specific configuration carried in the Ratify
// verifier config block (the JSON object under "config" in the plugin input).
// Ratify's reserved keys (name, type, artifactTypes, version, source,
// nestedReferences) are ignored here.
type PluginConfig struct {
	// TrustedPublicKey is an inline SubjectPublicKeyInfo PEM ECDSA P-256 key.
	// Provide exactly one of TrustedPublicKey or TrustedPublicKeyPath.
	TrustedPublicKey string `json:"trustedPublicKey"`
	// TrustedPublicKeyPath is a filesystem path to the SubjectPublicKeyInfo PEM
	// ECDSA P-256 key (e.g. a mounted Secret).
	TrustedPublicKeyPath string `json:"trustedPublicKeyPath"`
	// ExpectedBuilderIdentity is the application-builder identity that MUST
	// appear, verbatim, in the signed managed-dependency predicate.
	ExpectedBuilderIdentity string `json:"expectedBuilderIdentity"`
}

// stdinEnvelope is the subset of Ratify's PluginInputConfig this plugin reads.
// The verifier config block is nested under "config"; skel passes the subject,
// reference descriptor, and store separately to VerifyReference.
type stdinEnvelope struct {
	Config json.RawMessage `json:"config"`
}

// VerifyReference is the Ratify external-verifier entrypoint. It never returns a
// non-nil error: every negative outcome is reported as a VerifierResult with
// IsSuccess=false so admission fails closed with a clear reason rather than being
// recorded as a plugin fault.
func VerifyReference(args *skel.CmdArgs, subjectReference common.Reference, referenceDescriptor ocispecs.ReferenceDescriptor, store referrerstore.ReferrerStore) (*verifier.VerifierResult, error) {
	cfg, err := parseConfig(args.StdinData)
	if err != nil {
		return deny(fmt.Sprintf("plugin configuration error: %v", err)), nil
	}
	key, err := cfg.trustedKey()
	if err != nil {
		return deny(fmt.Sprintf("trusted public key error: %v", err)), nil
	}
	subjectDigest := subjectReference.Digest.String()
	return verifyManaged(context.Background(), store, subjectReference, subjectDigest, referenceDescriptor, key, cfg.ExpectedBuilderIdentity), nil
}

func parseConfig(stdin []byte) (PluginConfig, error) {
	var env stdinEnvelope
	if err := json.Unmarshal(stdin, &env); err != nil {
		return PluginConfig{}, fmt.Errorf("decode plugin input: %w", err)
	}
	var cfg PluginConfig
	if len(env.Config) > 0 {
		if err := json.Unmarshal(env.Config, &cfg); err != nil {
			return PluginConfig{}, fmt.Errorf("decode verifier config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.ExpectedBuilderIdentity) == "" {
		return PluginConfig{}, fmt.Errorf("expectedBuilderIdentity must be set")
	}
	inline := strings.TrimSpace(cfg.TrustedPublicKey) != ""
	path := strings.TrimSpace(cfg.TrustedPublicKeyPath) != ""
	if inline == path {
		return PluginConfig{}, fmt.Errorf("exactly one of trustedPublicKey or trustedPublicKeyPath must be set")
	}
	return cfg, nil
}

func (c PluginConfig) trustedKey() (*ecdsa.PublicKey, error) {
	if strings.TrimSpace(c.TrustedPublicKey) != "" {
		return attest.ParseECDSAPublicKey([]byte(c.TrustedPublicKey))
	}
	return attest.LoadECDSAPublicKey(c.TrustedPublicKeyPath)
}

// verifyManaged performs the fail-closed verification against a single Brewlet
// attestation referrer. It is separated from VerifyReference so it can be unit
// tested with a fake ReferrerStore.
func verifyManaged(
	ctx context.Context,
	store referrerstore.ReferrerStore,
	subjectReference common.Reference,
	subjectDigest string,
	referenceDescriptor ocispecs.ReferenceDescriptor,
	key *ecdsa.PublicKey,
	expectedIdentity string,
) *verifier.VerifierResult {
	if !strings.HasPrefix(subjectDigest, "sha256:") || len(subjectDigest) != len("sha256:")+64 {
		return deny(fmt.Sprintf("subject is not resolved to a sha256 digest: %q", subjectDigest))
	}
	if referenceDescriptor.ArtifactType != attest.AttestationArtifactType {
		return deny(fmt.Sprintf("referrer artifactType %q is not a Brewlet attestation", referenceDescriptor.ArtifactType))
	}

	manifest, err := store.GetReferenceManifest(ctx, subjectReference, referenceDescriptor)
	if err != nil {
		return deny(fmt.Sprintf("fetch referrer manifest: %v", err))
	}
	// Contract: a Brewlet attestation referrer carries exactly one DSSE envelope
	// layer. The predicate-type annotation is only a discovery hint; the trust
	// decision rests entirely on the signature verified below.
	if len(manifest.Blobs) != 1 || manifest.Blobs[0].MediaType != attest.DSSEEnvelopeMediaType {
		return deny("referrer manifest does not carry exactly one DSSE envelope layer")
	}

	envelope, err := store.GetBlobContent(ctx, subjectReference, manifest.Blobs[0].Digest)
	if err != nil {
		return deny(fmt.Sprintf("fetch DSSE envelope blob: %v", err))
	}

	predicate, err := attest.VerifyManagedAttestation(envelope, key, expectedIdentity, subjectDigest)
	if err != nil {
		return deny(fmt.Sprintf("managed-dependency attestation rejected: %v", err))
	}
	return allow(predicate)
}

func deny(reason string) *verifier.VerifierResult {
	return &verifier.VerifierResult{
		IsSuccess:    false,
		Name:         pluginName,
		VerifierName: pluginName,
		Type:         verifierType,
		VerifierType: verifierType,
		Message:      "Brewlet managed-dependency attestation verification failed",
		ErrorReason:  reason,
		Remediation:  "Publish the final image with a valid managed-dependency attestation signed by the trusted key and expected builder identity, and ensure the registry exposes the OCI 1.1 Referrers API.",
	}
}

func allow(predicate attest.ManagedDependencyPredicate) *verifier.VerifierResult {
	return &verifier.VerifierResult{
		IsSuccess:    true,
		Name:         pluginName,
		VerifierName: pluginName,
		Type:         verifierType,
		VerifierType: verifierType,
		Message:      "Brewlet managed-dependency attestation verified",
		Extensions: map[string]any{
			"builderIdentity":        predicate.BuilderIdentity,
			"sourceBom":              predicate.SourceBOM,
			"dependencyBundleDigest": predicate.DependencyBundleDigest,
			"finalImageDigest":       predicate.FinalImageDigest,
		},
	}
}
