package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

public final class Predicates {
    private Predicates() {}

    @JsonPropertyOrder({"schemaVersion", "dependencyBundleDigest",
            "dependencyLayerDigest", "dependencyLockDigest", "sbomDigest",
            "sourceBom", "builderIdentity"})
    public record Bundle(int schemaVersion, String dependencyBundleDigest,
                         String dependencyLayerDigest, String dependencyLockDigest,
                         String sbomDigest, String sourceBom, String builderIdentity) {
        public Bundle(String bundleDigest, String layerDigest, String lockDigest,
                      String sbomDigest, String sourceBom, String builderIdentity) {
            this(1, bundleDigest, layerDigest, lockDigest, sbomDigest, sourceBom,
                    builderIdentity);
        }
    }

    @JsonPropertyOrder({"schemaVersion", "finalImageDigest", "thinJar", "applicationJarDigest",
            "dependencyBundleDigest", "dependencyLayerDigest", "dependencyLockDigest",
            "sbomDigest", "sourceBom", "builderIdentity"})
    public record Managed(int schemaVersion, String finalImageDigest, boolean thinJar,
                          String applicationJarDigest,
                          String dependencyBundleDigest, String dependencyLayerDigest,
                          String dependencyLockDigest, String sbomDigest, String sourceBom,
                          String builderIdentity) {
        public Managed(String finalImageDigest, boolean thinJar, String applicationJarDigest,
                       String dependencyBundleDigest, String dependencyLayerDigest,
                       String dependencyLockDigest, String sbomDigest, String sourceBom,
                       String builderIdentity) {
            this(1, finalImageDigest, thinJar, applicationJarDigest,
                    dependencyBundleDigest, dependencyLayerDigest, dependencyLockDigest,
                    sbomDigest, sourceBom, builderIdentity);
        }
    }
}
