package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * Container port advertisement. Maps to {@code Port} in
 * {@code src/internal/artifact/artifact.go}.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class Port {

    @JsonProperty("name")
    private String name;

    @JsonProperty("containerPort")
    private int containerPort;

    @JsonProperty("protocol")
    private String protocol;

    public Port() {}

    public Port(String name, int containerPort, String protocol) {
        this.name = name;
        this.containerPort = containerPort;
        this.protocol = protocol;
    }

    public String getName() { return name; }
    public void setName(String name) { this.name = name; }

    public int getContainerPort() { return containerPort; }
    public void setContainerPort(int containerPort) { this.containerPort = containerPort; }

    public String getProtocol() { return protocol; }
    public void setProtocol(String protocol) { this.protocol = protocol; }
}
