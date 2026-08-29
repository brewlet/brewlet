package sh.brewlet.maven.plugin.supplychain;

import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciReferrer;

import java.io.IOException;
import java.nio.file.Path;
import java.security.GeneralSecurityException;

/** Creates the signed managed-dependency statement for a final image index. */
public final class ManagedProvenance {
    private ManagedProvenance() {}

    public static OciReferrer.Content create(String subjectName, String finalImageDigest,
                                             long finalImageSize, String applicationJarDigest,
                                             String dependencyBundleDigest,
                                             String dependencyLayerDigest,
                                             String dependencyLockDigest, String sbomDigest,
                                             String sourceBom, String builderIdentity,
                                             Path signingKey)
            throws IOException, GeneralSecurityException {
        if (signingKey == null || builderIdentity == null || builderIdentity.isBlank()) {
            throw new GeneralSecurityException(
                    "signingKey and signerIdentity are required for managed images");
        }
        Predicates.Managed predicate = new Predicates.Managed(finalImageDigest, true,
                applicationJarDigest, dependencyBundleDigest, dependencyLayerDigest,
                dependencyLockDigest, sbomDigest, sourceBom, builderIdentity);
        InToto.Statement statement = new InToto.Statement(subjectName, finalImageDigest,
                MediaTypes.MANAGED_DEPENDENCY_PREDICATE_TYPE, predicate);
        byte[] envelope = CanonicalJson.bytes(
                Dsse.sign(CanonicalJson.bytes(statement), signingKey));
        return OciReferrer.build(finalImageDigest, finalImageSize,
                MediaTypes.OCI_INDEX_MEDIA_TYPE, MediaTypes.DSSE_ARTIFACT_TYPE,
                MediaTypes.DSSE_LAYER_MEDIA_TYPE, envelope,
                MediaTypes.MANAGED_DEPENDENCY_PREDICATE_TYPE);
    }
}
