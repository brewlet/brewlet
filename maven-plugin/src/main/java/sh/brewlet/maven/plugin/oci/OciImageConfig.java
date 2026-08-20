package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;
import java.util.Map;

/**
 * The minimal standard OCI image config blob
 * ({@code application/vnd.oci.image.config.v1+json}) a runnable image needs so
 * containerd recognizes it as an image and unpacks its layers. The placeholder
 * entrypoint satisfies CRI implementations that reject an empty command; the
 * Brewlet shim replaces it with the launch contract from the manifest's
 * {@code brewlet.sh/jvm-config} annotation. Mirrors the Go {@code ociImageConfig} in
 * {@code src/internal/artifact/image.go}.
 *
 * <p>The critical field is {@code rootfs.diff_ids}: these MUST be the sha256 of
 * each layer's <em>uncompressed</em> tar (not the compressed blob digest), or
 * containerd rejects the image on unpack.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class OciImageConfig {

    @JsonProperty("architecture")
    private String architecture;

    @JsonProperty("os")
    private String os = "linux";

    @JsonProperty("config")
    private RunConfig config;

    @JsonProperty("rootfs")
    private RootFs rootfs;

    public OciImageConfig() {}

    public OciImageConfig(String architecture, RunConfig config, RootFs rootfs) {
        this.architecture = architecture;
        this.config = config;
        this.rootfs = rootfs;
    }

    public String getArchitecture() { return architecture; }
    public void setArchitecture(String architecture) { this.architecture = architecture; }

    public String getOs() { return os; }
    public void setOs(String os) { this.os = os; }

    public RunConfig getConfig() { return config; }
    public void setConfig(RunConfig config) { this.config = config; }

    public RootFs getRootfs() { return rootfs; }
    public void setRootfs(RootFs rootfs) { this.rootfs = rootfs; }

    /** The container run config; Brewlet replaces the placeholder entrypoint. */
    @JsonInclude(JsonInclude.Include.NON_NULL)
    public static class RunConfig {
        @JsonProperty("Entrypoint")
        private List<String> entrypoint = List.of("/brewlet");

        @JsonProperty("Labels")
        private Map<String, String> labels;

        public RunConfig() {}

        public RunConfig(Map<String, String> labels) { this.labels = labels; }

        public List<String> getEntrypoint() { return entrypoint; }
        public void setEntrypoint(List<String> entrypoint) { this.entrypoint = entrypoint; }

        public Map<String, String> getLabels() { return labels; }
        public void setLabels(Map<String, String> labels) { this.labels = labels; }
    }

    /** The rootfs section carrying the uncompressed-layer diff ids. */
    @JsonInclude(JsonInclude.Include.NON_NULL)
    public static class RootFs {
        @JsonProperty("type")
        private String type = "layers";

        @JsonProperty("diff_ids")
        private List<String> diffIds;

        public RootFs() {}

        public RootFs(List<String> diffIds) { this.diffIds = diffIds; }

        public String getType() { return type; }
        public void setType(String type) { this.type = type; }

        public List<String> getDiffIds() { return diffIds; }
        public void setDiffIds(List<String> diffIds) { this.diffIds = diffIds; }
    }
}
