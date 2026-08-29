package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;

import java.util.List;

/** Versioned config blob for an OCI dependency bundle. */
@JsonInclude(JsonInclude.Include.NON_NULL)
@JsonPropertyOrder({"schemaVersion", "name", "version", "sourceBom", "lockDigest",
        "layerDigest", "layerDiffId", "compatibleJdks", "allowUnsigned"})
public class DependencyBundleConfig {
    private int schemaVersion = 1;
    private String name;
    private String version;
    private String sourceBom;
    private String lockDigest;
    private String layerDigest;
    private String layerDiffId;
    private List<Integer> compatibleJdks;
    private Boolean allowUnsigned;

    public int getSchemaVersion() { return schemaVersion; }
    public void setSchemaVersion(int schemaVersion) { this.schemaVersion = schemaVersion; }
    public String getName() { return name; }
    public void setName(String name) { this.name = name; }
    public String getVersion() { return version; }
    public void setVersion(String version) { this.version = version; }
    public String getSourceBom() { return sourceBom; }
    public void setSourceBom(String sourceBom) { this.sourceBom = sourceBom; }
    public String getLockDigest() { return lockDigest; }
    public void setLockDigest(String lockDigest) { this.lockDigest = lockDigest; }
    public String getLayerDigest() { return layerDigest; }
    public void setLayerDigest(String layerDigest) { this.layerDigest = layerDigest; }
    public String getLayerDiffId() { return layerDiffId; }
    public void setLayerDiffId(String layerDiffId) { this.layerDiffId = layerDiffId; }
    public List<Integer> getCompatibleJdks() { return compatibleJdks; }
    public void setCompatibleJdks(List<Integer> compatibleJdks) {
        this.compatibleJdks = compatibleJdks;
    }
    public void normalizeCompatibleJdks() {
        if (compatibleJdks != null) {
            compatibleJdks = compatibleJdks.stream().sorted().distinct().toList();
        }
    }
    public Boolean getAllowUnsigned() { return allowUnsigned; }
    public void setAllowUnsigned(Boolean allowUnsigned) { this.allowUnsigned = allowUnsigned; }
    public boolean allowsUnsigned() { return Boolean.TRUE.equals(allowUnsigned); }
}
