package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * OCI platform descriptor ({@code {"os":"linux","architecture":"amd64"}}) used on
 * an image index entry so a multi-arch runnable image matches whichever
 * architecture a node was provisioned for. Mirrors the Go {@code Platform} struct
 * in {@code src/internal/artifact/artifact.go}.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class Platform {

    @JsonProperty("architecture")
    private String architecture;

    @JsonProperty("os")
    private String os;

    public Platform() {}

    public Platform(String os, String architecture) {
        this.os = os;
        this.architecture = architecture;
    }

    public String getArchitecture() { return architecture; }
    public void setArchitecture(String architecture) { this.architecture = architecture; }

    public String getOs() { return os; }
    public void setOs(String os) { this.os = os; }
}
