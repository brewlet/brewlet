package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;
import java.util.Map;

/**
 * OCI image index ({@code index.json}) for the local OCI image-layout store.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class OciIndex {

    @JsonProperty("schemaVersion")
    private int schemaVersion = 2;

    @JsonProperty("mediaType")
    private String mediaType = MediaTypes.OCI_INDEX_MEDIA_TYPE;

    @JsonProperty("manifests")
    private List<OciDescriptor> manifests;

    @JsonProperty("annotations")
    private Map<String, String> annotations;

    public int getSchemaVersion() { return schemaVersion; }
    public void setSchemaVersion(int schemaVersion) { this.schemaVersion = schemaVersion; }

    public String getMediaType() { return mediaType; }
    public void setMediaType(String mediaType) { this.mediaType = mediaType; }

    public List<OciDescriptor> getManifests() { return manifests; }
    public void setManifests(List<OciDescriptor> manifests) { this.manifests = manifests; }

    public Map<String, String> getAnnotations() { return annotations; }
    public void setAnnotations(Map<String, String> annotations) { this.annotations = annotations; }
}
