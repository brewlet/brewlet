package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;
import java.util.Map;

/**
 * OCI image manifest (schemaVersion 2) as defined in the OCI Image Spec.
 * Used with {@code artifactType: application/vnd.brewlet.app.v1+json} for
 * Brewlet OCI artifacts.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class OciManifest {

    @JsonProperty("schemaVersion")
    private int schemaVersion = 2;

    @JsonProperty("mediaType")
    private String mediaType = MediaTypes.OCI_MANIFEST_MEDIA_TYPE;

    @JsonProperty("artifactType")
    private String artifactType;

    @JsonProperty("config")
    private OciDescriptor config;

    @JsonProperty("subject")
    private OciDescriptor subject;

    @JsonProperty("layers")
    private List<OciDescriptor> layers;

    @JsonProperty("annotations")
    private Map<String, String> annotations;

    public int getSchemaVersion() { return schemaVersion; }
    public void setSchemaVersion(int schemaVersion) { this.schemaVersion = schemaVersion; }

    public String getMediaType() { return mediaType; }
    public void setMediaType(String mediaType) { this.mediaType = mediaType; }

    public String getArtifactType() { return artifactType; }
    public void setArtifactType(String artifactType) { this.artifactType = artifactType; }

    public OciDescriptor getConfig() { return config; }
    public void setConfig(OciDescriptor config) { this.config = config; }

    public OciDescriptor getSubject() { return subject; }
    public void setSubject(OciDescriptor subject) { this.subject = subject; }

    public List<OciDescriptor> getLayers() { return layers; }
    public void setLayers(List<OciDescriptor> layers) { this.layers = layers; }

    public Map<String, String> getAnnotations() { return annotations; }
    public void setAnnotations(Map<String, String> annotations) { this.annotations = annotations; }
}
