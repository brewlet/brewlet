package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * A single environment variable name/value pair.
 * Maps to {@code EnvVar} in {@code src/internal/artifact/artifact.go}.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class EnvVar {

    @JsonProperty("name")
    private String name;

    @JsonProperty("value")
    private String value;

    public EnvVar() {}

    public EnvVar(String name, String value) {
        this.name = name;
        this.value = value;
    }

    public String getName() { return name; }
    public void setName(String name) { this.name = name; }

    public String getValue() { return value; }
    public void setValue(String value) { this.value = value; }
}
