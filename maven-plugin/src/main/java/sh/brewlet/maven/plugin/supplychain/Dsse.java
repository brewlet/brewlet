package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;
import com.fasterxml.jackson.databind.JsonNode;
import sh.brewlet.maven.plugin.oci.LocalStore;

import java.io.IOException;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.GeneralSecurityException;
import java.security.KeyFactory;
import java.security.PrivateKey;
import java.security.PublicKey;
import java.security.Signature;
import java.security.interfaces.ECPrivateKey;
import java.security.spec.ECFieldFp;
import java.security.spec.ECParameterSpec;
import java.security.spec.ECPoint;
import java.security.spec.ECPublicKeySpec;
import java.security.spec.PKCS8EncodedKeySpec;
import java.security.spec.X509EncodedKeySpec;
import java.util.Base64;
import java.util.List;

/** DSSE signing and verification with standard Java P-256 ECDSA keys. */
public final class Dsse {
    public static final String PAYLOAD_TYPE = "application/vnd.in-toto+json";

    @JsonPropertyOrder({"payloadType", "payload", "signatures"})
    public record Envelope(String payloadType, String payload, List<SignatureEntry> signatures) {}
    @JsonPropertyOrder({"keyid", "sig"})
    public record SignatureEntry(String keyid, String sig) {}

    private Dsse() {}

    public static Envelope sign(byte[] payload, Path privateKeyPem)
            throws IOException, GeneralSecurityException {
        PrivateKey privateKey = readPrivateKey(privateKeyPem);
        PublicKey publicKey = derivePublicKey(privateKey);
        Signature signer = Signature.getInstance("SHA256withECDSA");
        signer.initSign(privateKey);
        signer.update(pae(PAYLOAD_TYPE, payload));
        String keyId = LocalStore.sha256Hex(publicKey.getEncoded());
        return new Envelope(PAYLOAD_TYPE, Base64.getEncoder().encodeToString(payload),
                List.of(new SignatureEntry(keyId,
                        Base64.getEncoder().encodeToString(signer.sign()))));
    }

    public static byte[] verify(byte[] envelopeBytes, Path publicKeyPem)
            throws IOException, GeneralSecurityException {
        Envelope envelope = CanonicalJson.MAPPER.readValue(envelopeBytes, Envelope.class);
        if (!PAYLOAD_TYPE.equals(envelope.payloadType()) || envelope.payload() == null
                || envelope.signatures() == null
                || envelope.signatures().size() != 1) {
            throw new GeneralSecurityException("Invalid DSSE envelope");
        }
        PublicKey key = readPublicKey(publicKeyPem);
        SignatureEntry entry = envelope.signatures().get(0);
        String expectedKeyId = LocalStore.sha256Hex(key.getEncoded());
        if (!expectedKeyId.equals(entry.keyid())) {
            throw new GeneralSecurityException("DSSE keyid does not match trusted public key");
        }
        byte[] payload;
        byte[] signature;
        try {
            payload = Base64.getDecoder().decode(envelope.payload());
            signature = Base64.getDecoder().decode(entry.sig());
        } catch (IllegalArgumentException e) {
            throw new GeneralSecurityException("Invalid DSSE base64", e);
        }
        Signature verifier = Signature.getInstance("SHA256withECDSA");
        verifier.initVerify(key);
        verifier.update(pae(envelope.payloadType(), payload));
        if (!verifier.verify(signature)) {
            throw new GeneralSecurityException("Invalid DSSE signature");
        }
        return payload;
    }

    public static JsonNode verifyStatement(byte[] envelopeBytes, Path publicKeyPem,
                                           String subjectDigest, String predicateType,
                                           String signerIdentity)
            throws IOException, GeneralSecurityException {
        JsonNode statement = CanonicalJson.MAPPER.readTree(verify(envelopeBytes, publicKeyPem));
        if (!InToto.STATEMENT_TYPE.equals(statement.path("_type").asText())
                || !predicateType.equals(statement.path("predicateType").asText())) {
            throw new GeneralSecurityException("Unexpected in-toto statement or predicate type");
        }
        JsonNode subjects = statement.path("subject");
        if (!subjects.isArray() || subjects.size() != 1
                || !stripSha256(subjectDigest).equals(
                        subjects.get(0).path("digest").path("sha256").asText())) {
            throw new GeneralSecurityException("Attestation subject digest mismatch");
        }
        if (!signerIdentity.equals(statement.path("predicate").path("builderIdentity").asText())) {
            throw new GeneralSecurityException("Attestation signer identity mismatch");
        }
        return statement;
    }

    public static byte[] pae(String payloadType, byte[] payload) {
        byte[] prefix = ("DSSEv1 " + payloadType.getBytes(StandardCharsets.UTF_8).length + " "
                + payloadType + " " + payload.length + " ").getBytes(StandardCharsets.UTF_8);
        byte[] result = new byte[prefix.length + payload.length];
        System.arraycopy(prefix, 0, result, 0, prefix.length);
        System.arraycopy(payload, 0, result, prefix.length, payload.length);
        return result;
    }

    private static String stripSha256(String digest) {
        return digest != null && digest.startsWith("sha256:") ? digest.substring(7) : digest;
    }

    private static PrivateKey readPrivateKey(Path path) throws IOException, GeneralSecurityException {
        byte[] der = pem(path, "PRIVATE KEY");
        PrivateKey key = KeyFactory.getInstance("EC").generatePrivate(new PKCS8EncodedKeySpec(der));
        if (!(key instanceof ECPrivateKey ec) || !isP256(ec.getParams())) {
            throw new GeneralSecurityException("Signing key must be ECDSA P-256");
        }
        return key;
    }

    private static PublicKey readPublicKey(Path path) throws IOException, GeneralSecurityException {
        PublicKey key = KeyFactory.getInstance("EC")
                .generatePublic(new X509EncodedKeySpec(pem(path, "PUBLIC KEY")));
        if (!(key instanceof java.security.interfaces.ECPublicKey ec)
                || !isP256(ec.getParams())) {
            throw new GeneralSecurityException("Trusted key must be ECDSA P-256");
        }
        return key;
    }

    private static byte[] pem(Path path, String label) throws IOException {
        String value = Files.readString(path, StandardCharsets.US_ASCII);
        String begin = "-----BEGIN " + label + "-----";
        String end = "-----END " + label + "-----";
        int from = value.indexOf(begin);
        int to = value.indexOf(end);
        if (from < 0 || to < from) {
            throw new IOException("PEM does not contain " + label + ": " + path);
        }
        try {
            return Base64.getMimeDecoder().decode(value.substring(from + begin.length(), to));
        } catch (IllegalArgumentException e) {
            throw new IOException("Invalid PEM encoding: " + path, e);
        }
    }

    private static PublicKey derivePublicKey(PrivateKey key) throws GeneralSecurityException {
        ECPrivateKey privateKey = (ECPrivateKey) key;
        ECParameterSpec params = privateKey.getParams();
        ECPoint point = multiply(params.getGenerator(), privateKey.getS(),
                ((ECFieldFp) params.getCurve().getField()).getP(),
                params.getCurve().getA());
        return KeyFactory.getInstance("EC").generatePublic(new ECPublicKeySpec(point, params));
    }

    private static boolean isP256(ECParameterSpec params) {
        return params.getCurve().getField().getFieldSize() == 256
                && params.getOrder().equals(new BigInteger(
                "FFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551", 16));
    }

    private static ECPoint multiply(ECPoint point, BigInteger scalar, BigInteger prime,
                                    BigInteger a) throws GeneralSecurityException {
        ECPoint result = null;
        ECPoint addend = point;
        for (int bit = 0; bit < scalar.bitLength(); bit++) {
            if (scalar.testBit(bit)) result = add(result, addend, prime, a);
            addend = add(addend, addend, prime, a);
        }
        if (result == null) throw new GeneralSecurityException("Invalid EC private key");
        return result;
    }

    private static ECPoint add(ECPoint p, ECPoint q, BigInteger mod, BigInteger a) {
        if (p == null) return q;
        if (q == null) return p;
        BigInteger px = p.getAffineX(), py = p.getAffineY();
        BigInteger qx = q.getAffineX(), qy = q.getAffineY();
        if (px.equals(qx) && py.add(qy).mod(mod).signum() == 0) return null;
        BigInteger slope = p.equals(q)
                ? px.multiply(px).multiply(BigInteger.valueOf(3)).add(a)
                    .multiply(py.multiply(BigInteger.TWO).modInverse(mod)).mod(mod)
                : qy.subtract(py).multiply(qx.subtract(px).mod(mod).modInverse(mod)).mod(mod);
        BigInteger x = slope.multiply(slope).subtract(px).subtract(qx).mod(mod);
        BigInteger y = slope.multiply(px.subtract(x)).subtract(py).mod(mod);
        return new ECPoint(x, y);
    }
}
