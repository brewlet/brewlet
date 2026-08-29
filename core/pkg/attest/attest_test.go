package attest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

const testSubject = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func validManagedPredicate() ManagedDependencyPredicate {
	d := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	return ManagedDependencyPredicate{
		SchemaVersion:          1,
		FinalImageDigest:       testSubject,
		ThinJar:                true,
		ApplicationJarDigest:   d,
		DependencyBundleDigest: d,
		DependencyLayerDigest:  d,
		DependencyLockDigest:   d,
		SBOMDigest:             d,
		SourceBOM:              "com.example:approved-bom:2026.08",
		BuilderIdentity:        "https://ci.example.com/application-publisher",
	}
}

func signedManaged(t *testing.T, key *ecdsa.PrivateKey, p ManagedDependencyPredicate, subject string) []byte {
	t.Helper()
	env, err := SignStatement(NewStatement("application-image", subject, ManagedPredicateType, p), key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return env
}

func TestSignVerifyRoundTrip(t *testing.T) {
	key := testKey(t)
	env := signedManaged(t, key, validManagedPredicate(), testSubject)
	if _, err := VerifyStatement(env, &key.PublicKey, ManagedPredicateType, testSubject); err != nil {
		t.Fatalf("round trip verify: %v", err)
	}
}

func TestVerifyManagedAttestationSuccess(t *testing.T) {
	key := testKey(t)
	env := signedManaged(t, key, validManagedPredicate(), testSubject)
	pred, err := VerifyManagedAttestation(env, &key.PublicKey, "https://ci.example.com/application-publisher", testSubject)
	if err != nil {
		t.Fatalf("verify managed: %v", err)
	}
	if pred.FinalImageDigest != testSubject {
		t.Fatalf("unexpected predicate subject %q", pred.FinalImageDigest)
	}
}

// TestVerifyManagedAttestationFailClosed asserts every tampering or mismatch
// mode is rejected: wrong key, wrong identity, wrong subject, tampered payload,
// unsigned/empty evidence, and a non-thin-JAR predicate.
func TestVerifyManagedAttestationFailClosed(t *testing.T) {
	key := testKey(t)
	other := testKey(t)
	identity := "https://ci.example.com/application-publisher"

	t.Run("wrong key", func(t *testing.T) {
		env := signedManaged(t, key, validManagedPredicate(), testSubject)
		if _, err := VerifyManagedAttestation(env, &other.PublicKey, identity, testSubject); err == nil {
			t.Fatal("expected wrong-key rejection")
		}
	})

	t.Run("wrong identity", func(t *testing.T) {
		env := signedManaged(t, key, validManagedPredicate(), testSubject)
		if _, err := VerifyManagedAttestation(env, &key.PublicKey, "https://evil.example.com/other", testSubject); err == nil {
			t.Fatal("expected wrong-identity rejection")
		}
	})

	t.Run("wrong subject", func(t *testing.T) {
		env := signedManaged(t, key, validManagedPredicate(), testSubject)
		wrong := "sha256:9999999999999999999999999999999999999999999999999999999999999999"
		if _, err := VerifyManagedAttestation(env, &key.PublicKey, identity, wrong); err == nil {
			t.Fatal("expected wrong-subject rejection")
		}
	})

	t.Run("subject not bound in predicate", func(t *testing.T) {
		// Statement subject matches the requested digest, but the predicate's
		// finalImageDigest binds a different image.
		p := validManagedPredicate()
		p.FinalImageDigest = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
		env := signedManaged(t, key, p, testSubject)
		if _, err := VerifyManagedAttestation(env, &key.PublicKey, identity, testSubject); err == nil {
			t.Fatal("expected predicate-subject-binding rejection")
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		env := signedManaged(t, key, validManagedPredicate(), testSubject)
		var e DSSEEnvelope
		if err := json.Unmarshal(env, &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		e.Payload = e.Payload[:len(e.Payload)-1] + flipLast(e.Payload)
		tampered, _ := json.Marshal(e)
		if _, err := VerifyManagedAttestation(tampered, &key.PublicKey, identity, testSubject); err == nil {
			t.Fatal("expected tampered-payload rejection")
		}
	})

	t.Run("empty evidence", func(t *testing.T) {
		if _, err := VerifyManagedAttestation(nil, &key.PublicKey, identity, testSubject); err == nil {
			t.Fatal("expected empty-evidence rejection")
		}
	})

	t.Run("nil key", func(t *testing.T) {
		env := signedManaged(t, key, validManagedPredicate(), testSubject)
		if _, err := VerifyManagedAttestation(env, nil, identity, testSubject); err == nil {
			t.Fatal("expected nil-key rejection")
		}
	})

	t.Run("empty identity", func(t *testing.T) {
		env := signedManaged(t, key, validManagedPredicate(), testSubject)
		if _, err := VerifyManagedAttestation(env, &key.PublicKey, "", testSubject); err == nil {
			t.Fatal("expected empty-identity rejection")
		}
	})

	t.Run("not thin jar", func(t *testing.T) {
		p := validManagedPredicate()
		p.ThinJar = false
		env := signedManaged(t, key, p, testSubject)
		if _, err := VerifyManagedAttestation(env, &key.PublicKey, identity, testSubject); err == nil {
			t.Fatal("expected non-thin-jar rejection")
		}
	})

	t.Run("wrong predicate type", func(t *testing.T) {
		// A bundle predicate signed under the same subject must not satisfy a
		// managed-attestation request.
		env, err := SignStatement(NewStatement("dependency-bundle", testSubject, BundlePredicateType,
			BundleProvenance{SchemaVersion: 1, DependencyBundleDigest: testSubject}), key)
		if err != nil {
			t.Fatalf("sign bundle: %v", err)
		}
		if _, err := VerifyManagedAttestation(env, &key.PublicKey, identity, testSubject); err == nil {
			t.Fatal("expected wrong-predicate-type rejection")
		}
	})
}

// flipLast returns a base64 character different from the last character of s so
// the decoded payload changes without breaking base64 decoding.
func flipLast(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	last := s[len(s)-1]
	if last == 'A' {
		return "B"
	}
	if strings.IndexByte(alphabet, last) < 0 {
		return "A"
	}
	return "A"
}

func TestParseECDSAPublicKeyRejectsNonPEM(t *testing.T) {
	if _, err := ParseECDSAPublicKey([]byte("not a pem")); err == nil {
		t.Fatal("expected non-PEM rejection")
	}
}

func TestPublicKeyIDStable(t *testing.T) {
	key := testKey(t)
	id, err := PublicKeyID(&key.PublicKey)
	if err != nil {
		t.Fatalf("keyid: %v", err)
	}
	if !strings.HasPrefix(id, "sha256:") || len(id) != len("sha256:")+64 {
		t.Fatalf("unexpected key id %q", id)
	}
	// The key ID is embedded verbatim in the DSSE signature.
	env := signedManaged(t, key, validManagedPredicate(), testSubject)
	var e DSSEEnvelope
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Signatures[0].KeyID != id {
		t.Fatalf("key id mismatch: envelope=%q computed=%q", e.Signatures[0].KeyID, id)
	}
	if _, err := base64.StdEncoding.DecodeString(e.Signatures[0].Sig); err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
}
