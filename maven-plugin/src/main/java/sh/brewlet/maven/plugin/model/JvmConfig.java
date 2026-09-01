package sh.brewlet.maven.plugin.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * Launch descriptor for a Brewlet JVM artifact. Maps 1-to-1 with the Go
 * {@code JVMConfig} struct in {@code src/internal/artifact/artifact.go} so
 * that the JSON produced by this plugin is byte-compatible with the CLI.
 *
 * <p>The JDK feature/distribution and launcher are deliberately NOT part of
 * this config: they are set once, in the deployment descriptor (the CRD's
 * {@code jvm.version}/{@code jvm.launcher}, or the raw-Deployment pod
 * annotations {@code brewlet.sh/jdk} and {@code brewlet.sh/launcher}). See
 * https://github.com/brewlet/brewlet/blob/main/docs/jdk-management.md.
 *
 * <p>Media type of the serialized blob:
 * {@code application/vnd.brewlet.jvm.config.v1+json}
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class JvmConfig {

    @JsonProperty("schemaVersion")
    private int schemaVersion = 1;

    @JsonProperty("mainJar")
    private String mainJar;

    @JsonProperty("entry")
    private Entry entry;

    @JsonProperty("enablePreview")
    private Boolean enablePreview;

    @JsonProperty("addModules")
    private List<String> addModules;

    @JsonProperty("addOpens")
    private List<String> addOpens;

    @JsonProperty("addExports")
    private List<String> addExports;

    @JsonProperty("systemProperties")
    private Map<String, String> systemProperties;

    @JsonProperty("user")
    private User user;

    @JsonProperty("env")
    private List<EnvVar> env;

    @JsonProperty("arch")
    private List<String> arch;

    @JsonProperty("cds")
    private Cds cds;

    /** Recognized architecture tokens for the optional {@code arch} constraint,
     * mirroring {@code KnownArches} in the Go artifact package. */
    public static final java.util.Set<String> KNOWN_ARCHES = java.util.Set.of("amd64", "arm64");

    /** Recognized CDS archive-production modes for the optional {@code cds} hint. */
    public static final java.util.Set<String> KNOWN_CDS_MODES = java.util.Set.of("dynamic", "static");

    public int getSchemaVersion() { return schemaVersion; }
    public void setSchemaVersion(int schemaVersion) { this.schemaVersion = schemaVersion; }

    public String getMainJar() { return mainJar; }
    public void setMainJar(String mainJar) { this.mainJar = mainJar; }

    public Entry getEntry() { return entry; }
    public void setEntry(Entry entry) { this.entry = entry; }

    public Boolean getEnablePreview() { return enablePreview; }
    public void setEnablePreview(Boolean enablePreview) { this.enablePreview = enablePreview; }

    public List<String> getAddModules() { return addModules; }
    public void setAddModules(List<String> addModules) { this.addModules = addModules; }

    public List<String> getAddOpens() { return addOpens; }
    public void setAddOpens(List<String> addOpens) { this.addOpens = addOpens; }

    public List<String> getAddExports() { return addExports; }
    public void setAddExports(List<String> addExports) { this.addExports = addExports; }

    public Map<String, String> getSystemProperties() { return systemProperties; }
    public void setSystemProperties(Map<String, String> systemProperties) { this.systemProperties = systemProperties; }

    public User getUser() { return user; }
    public void setUser(User user) { this.user = user; }

    public List<EnvVar> getEnv() { return env; }
    public void setEnv(List<EnvVar> env) { this.env = env; }

    public List<String> getArch() { return arch; }
    public void setArch(List<String> arch) { this.arch = arch; }

    public Cds getCds() { return cds; }
    public void setCds(Cds cds) { this.cds = cds; }

    /**
     * Enforces launch-config consistency, mirroring the Go
     * {@code JVMConfig.Validate()} in {@code src/internal/artifact/artifact.go}:
     * each entry mode OWNS a specific set of fields, and fields foreign to the
     * selected mode are rejected rather than silently ignored (which otherwise
     * surfaces as a confusing JVM error at deploy/run time).
     *
     * @throws IllegalStateException if the entry mode and its fields are inconsistent
     */
    public void validate() {
        Entry e = (entry != null) ? entry : new Entry();
        String mode = (e.getMode() == null || e.getMode().isEmpty()) ? "jar" : e.getMode();
        boolean hasClassPath = e.getClassPath() != null && !e.getClassPath().isEmpty();
        boolean hasModule = e.getModule() != null && !e.getModule().isEmpty();
        boolean hasModulePath = e.getModulePath() != null && !e.getModulePath().isEmpty();
        switch (mode) {
            case "jar":
                if (e.getMainClass() != null) {
                    throw new IllegalStateException("entry.mode=jar does not use entry.mainClass: "
                            + "`java -jar` launches the JAR manifest's Main-Class. Set "
                            + "entry.mode=classpath to launch a specific main class via -cp.");
                }
                if (hasClassPath) {
                    throw new IllegalStateException("entry.mode=jar does not use entry.classPath. "
                            + "Set entry.mode=classpath for layered class-path deployment.");
                }
                if (hasModule || hasModulePath) {
                    throw new IllegalStateException("entry.mode=jar does not use entry.module/"
                            + "entry.modulePath. Set entry.mode=module to launch on the module path.");
                }
                break;
            case "classpath":
                if (e.getMainClass() == null) {
                    throw new IllegalStateException("entry.mode=classpath requires entry.mainClass.");
                }
                if (hasModule || hasModulePath) {
                    throw new IllegalStateException("entry.mode=classpath does not use entry.module/"
                            + "entry.modulePath. Set entry.mode=module to launch on the module path.");
                }
                break;
            case "module":
                if (!hasModule) {
                    throw new IllegalStateException("entry.mode=module requires entry.module "
                            + "(the root module name for `java -m`).");
                }
                // entry.classPath IS permitted here: a modular (JPMS) app may need
                // both a module path (`-p`) and a supplementary class path (`-cp`)
                // for automatic-module or non-modular libraries. Mirrors the Go
                // launch core. See
                // https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md#8-shim-resolution-algorithm.
                break;
            default:
                throw new IllegalStateException("unknown entry.mode \"" + mode
                        + "\" (expected \"jar\", \"classpath\", or \"module\").");
        }
        // Dangling top-level JAR reference: a bare `<name>.jar` entry (no directory,
        // no wildcard) in classPath/modulePath can only be satisfied by the primary
        // JAR, since dependency layers unpack under /app/lib or /app/mods — never at
        // the /app top level. When mainJar is set, any such entry must equal it.
        // Mirrors JVMConfig.Validate() in src/internal/artifact/artifact.go.
        if (mainJar != null && !mainJar.isEmpty()) {
            List<String> refs = new ArrayList<>();
            if (hasClassPath) {
                refs.addAll(e.getClassPath());
            }
            if (hasModulePath) {
                refs.addAll(e.getModulePath());
            }
            for (String ref : refs) {
                if (isBareJarRef(ref) && !ref.equals(mainJar)) {
                    throw new IllegalStateException("entry references top-level JAR \"" + ref
                            + "\", but the primary JAR is \"" + mainJar + "\": a bare *.jar entry "
                            + "in classPath/modulePath can only be the main JAR (dependency layers "
                            + "unpack under lib/ or mods/). Use \"" + mainJar + "\", or place the "
                            + "file in a dependency layer and reference it under lib/ or mods/.");
                }
            }
        }
        // Optional arch constraint (non-portable artifacts): only recognized,
        // non-duplicate tokens. Mirrors JVMConfig.Validate() in the Go core.
        if (arch != null) {
            java.util.Set<String> seen = new java.util.HashSet<>();
            for (String a : arch) {
                if (!KNOWN_ARCHES.contains(a)) {
                    throw new IllegalStateException("arch entry \"" + a
                            + "\" is not a recognized architecture (expected \"amd64\" or \"arm64\").");
                }
                if (!seen.add(a)) {
                    throw new IllegalStateException("arch entry \"" + a + "\" is duplicated.");
                }
            }
        }
        // Optional AppCDS archive hint: a best-effort startup accelerator only.
        // The archive is mounted at /app/<archive> and consumed with -Xshare:auto;
        // it is bound to the exact JDK build + classpath layout. See
        // https://github.com/brewlet/brewlet/blob/main/docs/appcds.md.
        if (cds != null) {
            String archive = cds.getArchive();
            String trimmed = archive == null ? "" : archive.trim();
            if (trimmed.isEmpty()) {
                throw new IllegalStateException(
                        "cds.archive must be a non-empty archive filename (e.g. \"app.jsa\").");
            }
            if (!trimmed.equals(archive) || archive.indexOf('/') >= 0 || archive.indexOf('\\') >= 0
                    || archive.contains("..") || archive.indexOf('*') >= 0) {
                throw new IllegalStateException("cds.archive \"" + archive
                        + "\" must be a bare filename (no path separator, no parent reference, "
                        + "no wildcard): the archive is mounted at /app/<archive>.");
            }
            String cdsMode = cds.getMode();
            if (cdsMode != null && !cdsMode.isEmpty() && !KNOWN_CDS_MODES.contains(cdsMode)) {
                throw new IllegalStateException("cds.mode \"" + cdsMode
                        + "\" is not recognized (expected \"dynamic\", \"static\", or omitted).");
            }
        }
    }

    /**
     * Whether a classPath/modulePath entry is a plain top-level JAR filename — ends
     * in {@code .jar}, with no path separator and no wildcard. Such an entry can
     * only resolve to the primary JAR at the {@code /app} top level. Mirrors
     * {@code isBareJarRef} in the Go launch core.
     */
    private static boolean isBareJarRef(String ref) {
        return ref != null && ref.endsWith(".jar")
                && ref.indexOf('/') < 0 && ref.indexOf('*') < 0;
    }

    /**
     * Optional Application Class-Data Sharing hint. When set, the artifact ships a
     * {@code .jsa} archive as a CDS layer, the shim mounts it read-only at
     * {@code /app/<archive>}, and launch adds
     * {@code -Xshare:auto -XX:SharedArchiveFile=/app/<archive>} to cut class-load
     * time. It is best-effort seed data: archives are bound to the exact JDK build
     * and classpath layout, and {@code -Xshare:auto} falls back to base CDS on
     * mismatch. See https://github.com/brewlet/brewlet/blob/main/docs/appcds.md.
     */
    @JsonInclude(JsonInclude.Include.NON_NULL)
    public static class Cds {
        @JsonProperty("archive")
        private String archive;

        @JsonProperty("mode")
        private String mode;

        public Cds() {}

        public Cds(String archive, String mode) {
            this.archive = archive;
            this.mode = mode;
        }

        public String getArchive() { return archive; }
        public void setArchive(String archive) { this.archive = archive; }

        public String getMode() { return mode; }
        public void setMode(String mode) { this.mode = mode; }
    }
}
