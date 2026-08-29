package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"testing"

	"github.com/brewlet/brewlet/pkg/attest"

	"github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ratify-project/ratify/pkg/common"
	"github.com/ratify-project/ratify/pkg/ocispecs"
	"github.com/ratify-project/ratify/pkg/referrerstore"
	rconfig "github.com/ratify-project/ratify/pkg/referrerstore/config"
)

const subjectDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fakeStore is a minimal in-memory referrerstore.ReferrerStore that serves one
// referrer manifest and its DSSE blob, so verifyManaged can be exercised against
// the real Ratify store interface without a registry.
type fakeStore struct {
	manifest    ocispecs.ReferenceManifest
	blobs       map[digest.Digest][]byte
	manifestErr error
	blobErr     error
}

func (f *fakeStore) Name() string { return "fake" }

func (f *fakeStore) ListReferrers(context.Context, common.Reference, []string, string, *ocispecs.SubjectDescriptor) (referrerstore.ListReferrersResult, error) {
	return referrerstore.ListReferrersResult{}, nil
}

func (f *fakeStore) GetBlobContent(_ context.Context, _ common.Reference, d digest.Digest) ([]byte, error) {
	if f.blobErr != nil {
		return nil, f.blobErr
	}
	b, ok := f.blobs[d]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", d)
	}
	return b, nil
}

func (f *fakeStore) GetReferenceManifest(context.Context, common.Reference, ocispecs.ReferenceDescriptor) (ocispecs.ReferenceManifest, error) {
	if f.manifestErr != nil {
		return ocispecs.ReferenceManifest{}, f.manifestErr
	}
	return f.manifest, nil
}

func (f *fakeStore) GetConfig() *rconfig.StoreConfig { return &rconfig.StoreConfig{} }

func (f *fakeStore) GetSubjectDescriptor(context.Context, common.Reference) (*ocispecs.SubjectDescriptor, error) {
	return &ocispecs.SubjectDescriptor{}, nil
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

func pubPEM(t *testing.T, k *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func validPredicate() attest.ManagedDependencyPredicate {
	d := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	return attest.ManagedDependencyPredicate{
		SchemaVersion: 1, FinalImageDigest: subjectDigest, ThinJar: true,
		ApplicationJarDigest: d, DependencyBundleDigest: d, DependencyLayerDigest: d,
		DependencyLockDigest: d, SBOMDigest: d,
		SourceBOM: "com.example:approved-bom:2026.08", BuilderIdentity: identity,
	}
}

const identity = "https://ci.example.com/application-publisher"

// storeFor builds a fakeStore whose single referrer carries envelope.
func storeFor(envelope []byte) *fakeStore {
	blobDigest := digest.FromBytes(envelope)
	return &fakeStore{
		manifest: ocispecs.ReferenceManifest{
			ArtifactType: attest.AttestationArtifactType,
			Blobs: []oci.Descriptor{{
				MediaType: attest.DSSEEnvelopeMediaType,
				Digest:    blobDigest,
				Size:      int64(len(envelope)),
			}},
			Annotations: map[string]string{attest.PredicateTypeAnnotation: attest.ManagedPredicateType},
		},
		blobs: map[digest.Digest][]byte{blobDigest: envelope},
	}
}

func managedRefDesc() ocispecs.ReferenceDescriptor {
	return ocispecs.ReferenceDescriptor{ArtifactType: attest.AttestationArtifactType}
}

func sign(t *testing.T, key *ecdsa.PrivateKey, p attest.ManagedDependencyPredicate, subj string) []byte {
	t.Helper()
	env, err := attest.SignStatement(attest.NewStatement("application-image", subj, attest.ManagedPredicateType, p), key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return env
}

func TestVerifyManagedAllows(t *testing.T) {
	key := testKey(t)
	store := storeFor(sign(t, key, validPredicate(), subjectDigest))
	res := verifyManaged(context.Background(), store, common.Reference{Digest: digest.Digest(subjectDigest)}, subjectDigest, managedRefDesc(), &key.PublicKey, identity)
	if !res.IsSuccess {
		t.Fatalf("expected success, got: %s", res.ErrorReason)
	}
	if res.Name != pluginName || res.VerifierName != pluginName {
		t.Fatalf("result identity fields not set: %+v", res)
	}
}

func TestVerifyManagedFailClosed(t *testing.T) {
	key := testKey(t)
	other := testKey(t)

	cases := []struct {
		name  string
		store *fakeStore
		key   *ecdsa.PublicKey
		id    string
		subj  string
		desc  ocispecs.ReferenceDescriptor
	}{
		{
			name:  "wrong key",
			store: storeFor(sign(t, key, validPredicate(), subjectDigest)),
			key:   &other.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name:  "wrong identity",
			store: storeFor(sign(t, key, validPredicate(), subjectDigest)),
			key:   &key.PublicKey, id: "https://evil.example.com/x", subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name:  "wrong subject",
			store: storeFor(sign(t, key, validPredicate(), subjectDigest)),
			key:   &key.PublicKey, id: identity,
			subj: "sha256:9999999999999999999999999999999999999999999999999999999999999999", desc: managedRefDesc(),
		},
		{
			name: "predicate binds different image",
			store: func() *fakeStore {
				p := validPredicate()
				p.FinalImageDigest = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
				return storeFor(sign(t, key, p, subjectDigest))
			}(),
			key: &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name:  "wrong artifact type",
			store: storeFor(sign(t, key, validPredicate(), subjectDigest)),
			key:   &key.PublicKey, id: identity, subj: subjectDigest,
			desc: ocispecs.ReferenceDescriptor{ArtifactType: "application/vnd.oci.image.manifest.v1+json"},
		},
		{
			name:  "non-digest subject",
			store: storeFor(sign(t, key, validPredicate(), subjectDigest)),
			key:   &key.PublicKey, id: identity, subj: "latest", desc: managedRefDesc(),
		},
		{
			name: "wrong layer media type",
			store: func() *fakeStore {
				s := storeFor(sign(t, key, validPredicate(), subjectDigest))
				s.manifest.Blobs[0].MediaType = "application/octet-stream"
				return s
			}(),
			key: &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name: "two layers",
			store: func() *fakeStore {
				s := storeFor(sign(t, key, validPredicate(), subjectDigest))
				s.manifest.Blobs = append(s.manifest.Blobs, s.manifest.Blobs[0])
				return s
			}(),
			key: &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name: "malformed envelope",
			store: func() *fakeStore {
				s := storeFor([]byte("{not json"))
				return s
			}(),
			key: &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name:  "manifest fetch error",
			store: &fakeStore{manifestErr: fmt.Errorf("registry unreachable")},
			key:   &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name: "blob fetch error",
			store: func() *fakeStore {
				s := storeFor(sign(t, key, validPredicate(), subjectDigest))
				s.blobErr = fmt.Errorf("blob unreachable")
				return s
			}(),
			key: &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
		{
			name: "bundle predicate not accepted",
			store: func() *fakeStore {
				env, _ := attest.SignStatement(attest.NewStatement("dependency-bundle", subjectDigest, attest.BundlePredicateType,
					attest.BundleProvenance{SchemaVersion: 1, DependencyBundleDigest: subjectDigest}), key)
				return storeFor(env)
			}(),
			key: &key.PublicKey, id: identity, subj: subjectDigest, desc: managedRefDesc(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := verifyManaged(context.Background(), tc.store,
				common.Reference{Digest: digest.Digest(tc.subj)}, tc.subj, tc.desc, tc.key, tc.id)
			if res.IsSuccess {
				t.Fatalf("expected fail-closed denial for %q, got success", tc.name)
			}
			if res.ErrorReason == "" {
				t.Fatalf("denial for %q must carry an error reason", tc.name)
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	key := testKey(t)
	pub := pubPEM(t, key)

	good := mustStdin(t, map[string]any{
		"config": map[string]any{
			"name": "brewlet-managed-dependencies", "type": "brewlet-managed-dependencies",
			"artifactTypes": attest.AttestationArtifactType, "version": "1.0.0",
			"trustedPublicKey": pub, "expectedBuilderIdentity": identity,
		},
	})
	cfg, err := parseConfig(good)
	if err != nil {
		t.Fatalf("parse good config: %v", err)
	}
	if _, err := cfg.trustedKey(); err != nil {
		t.Fatalf("resolve key: %v", err)
	}

	bad := []map[string]any{
		{"config": map[string]any{"trustedPublicKey": pub}},                                                                    // missing identity
		{"config": map[string]any{"expectedBuilderIdentity": identity}},                                                        // no key
		{"config": map[string]any{"expectedBuilderIdentity": identity, "trustedPublicKey": pub, "trustedPublicKeyPath": "/x"}}, // both keys
	}
	for i, b := range bad {
		if _, err := parseConfig(mustStdin(t, b)); err == nil {
			t.Fatalf("bad config %d should be rejected", i)
		}
	}
}

func mustStdin(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal stdin: %v", err)
	}
	return b
}
