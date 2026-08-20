package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.Map;

/**
 * OCI content descriptor (§4 of the OCI Image Layout spec).
 * Used for config blobs, layers, and manifests.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class OciDescriptor {

    @JsonProperty("mediaType")
    private String mediaType;

    @JsonProperty("artifactType")
    private String artifactType;

    @JsonProperty("digest")
    private String digest;

    @JsonProperty("size")
    private long size;

    @JsonProperty("annotations")
    private Map<String, String> annotations;

    @JsonProperty("platform")
    private Platform platform;

    public OciDescriptor() {}

    public OciDescriptor(String mediaType, String digest, long size) {
        this.mediaType = mediaType;
        this.digest = digest;
        this.size = size;
    }

    public String getMediaType() { return mediaType; }
    public void setMediaType(String mediaType) { this.mediaType = mediaType; }

    public String getArtifactType() { return artifactType; }
    public void setArtifactType(String artifactType) { this.artifactType = artifactType; }

    public String getDigest() { return digest; }
    public void setDigest(String digest) { this.digest = digest; }

    public long getSize() { return size; }
    public void setSize(long size) { this.size = size; }

    public Map<String, String> getAnnotations() { return annotations; }
    public void setAnnotations(Map<String, String> annotations) { this.annotations = annotations; }

    public Platform getPlatform() { return platform; }
    public void setPlatform(Platform platform) { this.platform = platform; }
}
