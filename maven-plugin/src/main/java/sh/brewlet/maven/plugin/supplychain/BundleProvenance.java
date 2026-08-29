package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.databind.JsonNode;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciReferrer;

import java.io.IOException;
import java.nio.file.Path;
import java.security.GeneralSecurityException;

/** Creates and validates the SBOM and signed provenance attached to a bundle. */
public final class BundleProvenance {
    private BundleProvenance() {}

    public record Materials(byte[] sbom, String sbomDigest,
                            OciReferrer.Content sbomReferrer,
                            byte[] envelope, OciReferrer.Content provenanceReferrer) {}

    public static Materials create(DependencyBundle.Content bundle, Path signingKey,
                                   String signerIdentity)
            throws IOException, GeneralSecurityException {
        byte[] sbom = CycloneDx.generate(bundle.lock(), bundle.config().getName(),
                bundle.config().getVersion());
        String sbomDigest = LocalStore.sha256Hex(sbom);
        OciReferrer.Content sbomReferrer = OciReferrer.build(
                bundle.manifestDigest(), bundle.manifestBytes().length,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE, MediaTypes.CYCLONEDX_ARTIFACT_TYPE,
                MediaTypes.CYCLONEDX_LAYER_MEDIA_TYPE, sbom, null);
        if (signingKey == null) {
            return new Materials(sbom, sbomDigest, sbomReferrer, null, null);
        }
        requireIdentity(signerIdentity);
        Predicates.Bundle predicate = new Predicates.Bundle(
                bundle.manifestDigest(), bundle.config().getLayerDigest(),
                bundle.config().getLockDigest(), sbomDigest,
                bundle.config().getSourceBom(), signerIdentity);
        InToto.Statement statement = new InToto.Statement(bundle.config().getName(),
                bundle.manifestDigest(), MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE, predicate);
        byte[] envelope = CanonicalJson.bytes(Dsse.sign(CanonicalJson.bytes(statement), signingKey));
        OciReferrer.Content provenanceReferrer = OciReferrer.build(
                bundle.manifestDigest(), bundle.manifestBytes().length,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE, MediaTypes.DSSE_ARTIFACT_TYPE,
                MediaTypes.DSSE_LAYER_MEDIA_TYPE, envelope,
                MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE);
        return new Materials(sbom, sbomDigest, sbomReferrer, envelope, provenanceReferrer);
    }

    public static String verify(DependencyBundle.Content bundle, byte[] sbom, byte[] envelope,
                                Path trustedPublicKey, String signerIdentity)
            throws IOException, GeneralSecurityException {
        requireIdentity(signerIdentity);
        if (trustedPublicKey == null) {
            throw new GeneralSecurityException("trustedPublicKey is required");
        }
        CycloneDx.validate(sbom, bundle.lock(), bundle.config().getName(),
                bundle.config().getVersion());
        String sbomDigest = LocalStore.sha256Hex(sbom);
        JsonNode statement = Dsse.verifyStatement(envelope, trustedPublicKey,
                bundle.manifestDigest(), MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE,
                signerIdentity);
        JsonNode predicate = statement.path("predicate");
        if (predicate.path("schemaVersion").asInt() != 1) {
            throw new GeneralSecurityException("Unsupported bundle provenance schemaVersion");
        }
        require(predicate, "dependencyBundleDigest", bundle.manifestDigest());
        require(predicate, "dependencyLayerDigest", bundle.config().getLayerDigest());
        require(predicate, "dependencyLockDigest", bundle.config().getLockDigest());
        require(predicate, "sbomDigest", sbomDigest);
        require(predicate, "sourceBom", bundle.config().getSourceBom());
        return sbomDigest;
    }

    public static String validateSbom(DependencyBundle.Content bundle, byte[] sbom)
            throws IOException, GeneralSecurityException {
        CycloneDx.validate(sbom, bundle.lock(), bundle.config().getName(),
                bundle.config().getVersion());
        return LocalStore.sha256Hex(sbom);
    }

    private static void require(JsonNode predicate, String field, String expected)
            throws GeneralSecurityException {
        if (!expected.equals(predicate.path(field).asText())) {
            throw new GeneralSecurityException("Bundle provenance " + field + " mismatch");
        }
    }

    private static void requireIdentity(String identity) throws GeneralSecurityException {
        if (identity == null || identity.isBlank()) {
            throw new GeneralSecurityException("signerIdentity is required");
        }
    }
}
