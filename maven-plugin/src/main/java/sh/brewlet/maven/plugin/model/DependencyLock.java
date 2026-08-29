package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;

/** Canonical, content-addressed description of a Maven runtime dependency graph. */
@JsonPropertyOrder({"schemaVersion", "artifacts"})
public class DependencyLock {
    private int schemaVersion = 1;
    private List<Entry> dependencies = new ArrayList<>();

    public int getSchemaVersion() { return schemaVersion; }
    public void setSchemaVersion(int schemaVersion) { this.schemaVersion = schemaVersion; }
    @JsonProperty("artifacts")
    public List<Entry> getDependencies() { return dependencies; }
    @JsonProperty("artifacts")
    public void setDependencies(List<Entry> dependencies) {
        this.dependencies = dependencies == null ? new ArrayList<>() : new ArrayList<>(dependencies);
        this.dependencies.sort(Comparator.comparing(Entry::coordinate));
    }

    @JsonPropertyOrder({"groupId", "artifactId", "version", "type", "classifier",
            "scope", "fileName", "sha256"})
    public record Entry(String groupId, String artifactId, String version, String type,
                        @JsonInclude(JsonInclude.Include.NON_EMPTY) String classifier, String scope,
                        @JsonProperty("fileName") String filename, String sha256) {
        public String coordinate() {
            return groupId + ":" + artifactId + ":" + type + ":"
                    + (classifier == null ? "" : classifier) + ":" + version;
        }
    }
}
