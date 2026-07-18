package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;

/**
 * Entry point mode: {@code "jar"} → {@code java -jar} (main class from the JAR
 * manifest); {@code "classpath"} → {@code java -cp <classPath> <mainClass>};
 * {@code "module"} → {@code java [-cp <classPath>] -p <modulePath> -m <module>[/<mainClass>]}
 * (JPMS, optionally with a supplementary class path for the mixed form —
 * see https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md#8-shim-resolution-algorithm).
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class Entry {

    @JsonProperty("mode")
    private String mode;

    /**
     * Fully-qualified entry-point class. Required when {@code mode == "classpath"};
     * ignored in {@code "jar"} mode, where the manifest's {@code Main-Class} is used.
     * Optional in {@code "module"} mode, where it selects {@code <module>/<mainClass>}.
     * Must stay in sync with the Go {@code Entry.MainClass} field.
     */
    @JsonProperty("mainClass")
    private String mainClass;

    /**
     * Optional, ordered {@code /app}-relative class-path entries (e.g.
     * {@code ["app.jar", "lib/*"]}) joined and passed to {@code java -cp}. Used in
     * {@code "classpath"} mode and, optionally, in {@code "module"} mode to add a
     * supplementary class path next to the module path (the mixed form). Omitted
     * when null/empty. Must stay in sync with the Go {@code Entry.ClassPath} field.
     * See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md.
     */
    @JsonProperty("classPath")
    private List<String> classPath;

    /**
     * Root module name launched when {@code mode == "module"} (the {@code -m}
     * argument). Required in {@code "module"} mode; forbidden otherwise. Must stay
     * in sync with the Go {@code Entry.Module} field. See
     * https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
     */
    @JsonProperty("module")
    private String module;

    /**
     * Optional, ordered {@code /app}-relative module-path entries used when
     * {@code mode == "module"} (e.g. {@code ["orders.jar", "mods"]}); the
     * module-path twin of {@code classPath}. Omitted when null/empty. Must stay in
     * sync with the Go {@code Entry.ModulePath} field. See
     * https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
     */
    @JsonProperty("modulePath")
    private List<String> modulePath;

    public Entry() {}

    public Entry(String mode) {
        this.mode = mode;
    }

    public Entry(String mode, List<String> classPath) {
        this.mode = mode;
        this.classPath = classPath;
    }

    public String getMode() { return mode; }
    public void setMode(String mode) { this.mode = mode; }

    public String getMainClass() { return mainClass; }
    public void setMainClass(String mainClass) { this.mainClass = mainClass; }

    public List<String> getClassPath() { return classPath; }
    public void setClassPath(List<String> classPath) { this.classPath = classPath; }

    public String getModule() { return module; }
    public void setModule(String module) { this.module = module; }

    public List<String> getModulePath() { return modulePath; }
    public void setModulePath(List<String> modulePath) { this.modulePath = modulePath; }
}
