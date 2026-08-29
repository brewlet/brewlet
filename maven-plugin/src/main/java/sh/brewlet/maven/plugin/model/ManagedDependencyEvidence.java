package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;

/** Canonical unsigned evidence attached to images assembled from a dependency bundle. */
@JsonInclude(JsonInclude.Include.NON_NULL)
@JsonPropertyOrder({"schemaVersion", "thinJar", "applicationJarDigest",
        "dependencyBundleDigest", "dependencyLayerDigest",
        "dependencyLockDigest", "sbomDigest", "sourceBom", "builderIdentity"})
public record ManagedDependencyEvidence(
        int schemaVersion,
        boolean thinJar,
        String applicationJarDigest,
        String dependencyBundleDigest,
        String dependencyLayerDigest,
        String dependencyLockDigest,
        String sbomDigest,
        String sourceBom,
        String builderIdentity) {

    public ManagedDependencyEvidence(boolean thinJar, String applicationJarDigest,
                                     String bundleDigest, String dependencyLayerDigest,
                                     String lockDigest, String sourceBom) {
        this(1, thinJar, applicationJarDigest,
                bundleDigest, dependencyLayerDigest, lockDigest, null, sourceBom, null);
    }
}
