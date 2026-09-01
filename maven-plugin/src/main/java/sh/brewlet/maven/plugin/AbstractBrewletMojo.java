package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.execution.MavenSession;
import org.apache.maven.plugin.AbstractMojo;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.project.MavenProject;
import org.apache.maven.settings.Settings;
import sh.brewlet.maven.plugin.model.*;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.util.JarInspector;
import sh.brewlet.maven.plugin.util.JdkVersionResolver;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.time.Instant;
import java.util.*;

/**
 * Base class for all Brewlet plugin goals, providing the shared configuration
 * surface and config-inference logic that maps POM metadata to a
 * {@link JvmConfig} launch descriptor.
 */
public abstract class AbstractBrewletMojo extends AbstractMojo {

    protected static final ObjectMapper MAPPER = new ObjectMapper()
            .enable(SerializationFeature.INDENT_OUTPUT);

    // -----------------------------------------------------------------------
    // Injected Maven components
    // -----------------------------------------------------------------------

    @Parameter(defaultValue = "${project}", readonly = true, required = true)
    protected MavenProject project;

    @Parameter(defaultValue = "${session}", readonly = true, required = true)
    protected MavenSession session;

    @Parameter(defaultValue = "${settings}", readonly = true, required = true)
    protected Settings settings;

    // -----------------------------------------------------------------------
    // Plugin configuration parameters
    // -----------------------------------------------------------------------

    /**
     * OCI image reference to push to, e.g.
     * {@code registry.example.com/team/orders-api:${project.version}}.
     * Required for {@code brewlet:push}; optional for {@code brewlet:config}.
     */
    @Parameter(property = "brewlet.image")
    protected String image;

    /**
     * Path to the JAR file to push. Defaults to the project's primary artifact.
     */
    @Parameter(property = "brewlet.jarFile")
    protected File jarFile;

    /**
     * Optional prebuilt AppCDS {@code .jsa} archive to ship as an OCI layer.
     * The archive is mounted read-only at {@code /app/<name>} and launch uses
     * {@code -Xshare:auto -XX:SharedArchiveFile=/app/<name>} as a best-effort
     * accelerator. Mirrors the CLI's {@code --appcds-archive}; see
     * https://github.com/brewlet/brewlet/blob/main/docs/appcds.md.
     */
    @Parameter(property = "brewlet.cdsArchive")
    protected File cdsArchive;

    /**
     * Fully-qualified main class name. Inferred from {@code Main-Class} in the
     * JAR manifest when not set; enables {@code entry.mode=classpath}. In
     * {@code entry.mode=module} it optionally selects {@code <module>/<mainClass>}.
     */
    @Parameter(property = "brewlet.mainClass")
    protected String mainClass;

    /**
     * Entry mode: {@code "jar"}, {@code "classpath"}, or {@code "module"} (JPMS).
     * Inferred from the JAR when not set (a root {@code module-info.class} selects
     * {@code module}; otherwise the manifest's {@code Main-Class} selects
     * {@code jar}, else {@code classpath}).
     */
    @Parameter(property = "brewlet.entryMode")
    protected String entryMode;

    /**
     * Required JDK feature (major) version for the deployment descriptor's
     * {@code spec.jvm.version}. Inferred from the project's release/target when
     * not set. This is a scheduling request written to the CRD/Deployment — not
     * the artifact config — so a single source of truth selects the JDK.
     */
    @Parameter(property = "brewlet.jdkFeature")
    protected Integer jdkFeature;

    /**
     * Optional JDK distribution (e.g. {@code temurin}, {@code microsoft}) for the
     * deployment descriptor's {@code spec.jvm.distribution}. With {@code jdkFeature}
     * it selects an exact {@code <distribution>-<feature>} node JDK; omitted, any
     * distribution of the requested feature is acceptable and each node picks the
     * lexically-first installed one (no built-in vendor preference).
     * Written to the CRD/Deployment, not the artifact config.
     */
    @Parameter(property = "brewlet.jdkDistribution")
    protected String jdkDistribution;

    /**
     * Optional custom launcher (e.g. {@code jaz} for node auto-tuning) for the
     * deployment descriptor's {@code spec.jvm.launcher}. When omitted, the
     * vanilla OpenJDK {@code java} launcher is used. Written to the CRD/Deployment,
     * not the artifact config.
     */
    @Parameter(property = "brewlet.launcher")
    protected String launcher;

    /**
     * App-intrinsic launch knobs baked into the artifact. These are correctness
     * flags the *code* requires everywhere it runs — NOT resource/environment
     * tuning (heap, GC, agents), which belongs in the deployment descriptor's
     * {@code jvm.args}. Brewlet injects none of its own.
     */
    @Parameter(property = "brewlet.enablePreview")
    protected Boolean enablePreview;

    /** Root modules added via {@code --add-modules} (incubator/reflective modules). */
    @Parameter
    protected List<String> addModules;

    /** {@code --add-opens} tokens ("&lt;module&gt;/&lt;package&gt;=&lt;target&gt;") for deep reflection. */
    @Parameter
    protected List<String> addOpens;

    /** {@code --add-exports} tokens ("&lt;module&gt;/&lt;package&gt;=&lt;target&gt;"). */
    @Parameter
    protected List<String> addExports;

    /** System properties expanded into {@code -Dkey=value} the app assumes at startup. */
    @Parameter
    protected Map<String, String> systemProperties;

    /**
     * Optional UNIX user/group override for the sandbox process.
     */
    @Parameter
    protected User user;

    /**
     * Environment variables to set in the container.
     */
    @Parameter
    protected List<EnvVar> env;

    /**
     * Optional architecture constraint for a NON-portable artifact — one bundling
     * JNI native libraries or arch-specific dependencies (e.g. netty-tcnative,
     * RocksDB) that only run on the arch(es) whose natives were bundled. Each
     * entry is a GOARCH / {@code kubernetes.io/arch} token ({@code amd64} or
     * {@code arm64}). When set, it OVERRIDES native-library auto-detection and is
     * folded into the artifact's launch config; the operator then steers
     * scheduling onto matching nodes. Leave it unset for the common case: a
     * pure-bytecode JAR is architecture-neutral and runs on any provisioned arch.
     */
    @Parameter(property = "brewlet.arch")
    protected List<String> arch;

    /**
     * Whether to scan the JAR for bundled native libraries and, when found,
     * default the {@code arch} constraint from the architectures they target
     * (defaults to {@code true}). An explicit {@code <arch>} always wins; set this
     * to {@code false} to publish arch-neutral without scanning.
     */
    @Parameter(property = "brewlet.detectNativeArch", defaultValue = "true")
    protected boolean detectNativeArch;

    /**
     * Skip all Brewlet goals when {@code true}.
     */
    @Parameter(property = "brewlet.skip", defaultValue = "false")
    protected boolean skip;

    /**
     * Enable <strong>layered deployment</strong>: instead of shipping a single
     * opaque fat JAR, the project's resolved runtime dependencies are packed into
     * their own reproducible OCI layer(s) alongside a thin application JAR.
     *
     * <p>The layer kind follows the entry mode:
     * <ul>
     *   <li><b>class-path apps</b> (non-modular JAR, or {@code <entryMode>classpath</entryMode>}):
     *       {@code classpath.layer.v1+tar} layer(s) unpacked to {@code /app/lib};
     *       forces {@code entry.mode=classpath} and sets
     *       {@code entry.classPath=[mainJar, "lib/*"]} so the shim launches
     *       {@code java -cp /app/app.jar:/app/lib/* <mainClass>}. See
     *       https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md.</li>
     *   <li><b>modular (JPMS) apps</b> (a JAR with a root {@code module-info.class},
     *       or {@code <entryMode>module</entryMode>}): a single
     *       {@code modulepath.layer.v1+tar} (named {@code mods}) unpacked to
     *       {@code /app/mods} and sets {@code entry.modulePath=[mainJar, "mods"]}
     *       so the shim launches {@code java -p /app/app.jar:/app/mods -m <module>}.
     *       See https://github.com/brewlet/brewlet/blob/main/docs/jpms-support.md.</li>
     * </ul>
     *
     * <p>Unchanged dependency layers dedup by digest across rebuilds and apps, so
     * a code-only change re-pushes only the small app JAR. Backward compatible:
     * defaults to {@code false} (single fat-JAR layer).
     */
    @Parameter(property = "brewlet.layered", defaultValue = "false")
    protected boolean layered;

    /**
     * When {@link #layered} is enabled, pack released and {@code -SNAPSHOT}
     * dependencies into two separate layers ({@code deps} then
     * {@code snapshot-deps}), mirroring Spring Boot's {@code layers.idx}
     * stable&nbsp;&rarr;&nbsp;volatile ordering for finer-grained dedup. When
     * {@code false}, all dependencies go into a single {@code deps} layer.
     */
    @Parameter(property = "brewlet.splitSnapshotLayers", defaultValue = "true")
    protected boolean splitSnapshotLayers;

    /**
     * Dry-run mode: generate and display the config but do not push anything.
     */
    @Parameter(property = "brewlet.dryRun", defaultValue = "false")
    protected boolean dryRun;

    /**
     * Delivery format for {@code brewlet:push}:
     * <ul>
     *   <li>{@code "image"} (default) — a standard, kubelet-pullable OCI image
     *       (real image config, {@code tar+gzip} layers, the launch config in the
     *       {@code brewlet.sh/jvm-config} manifest annotation, multi-arch index).
     *       A {@code runtimeClassName: brewlet} pod can set {@code image: <ref>}
     *       and containerd/kubelet pull + unpack it as SpinKube does for a
     *       Spin-compatible Wasm application.</li>
     *   <li>{@code "artifact"} — a native Brewlet OCI artifact with custom media
     *       types. Registry-native and deployment-agnostic, but containerd cannot
     *       unpack it, so it must be delivered to nodes out of band (not by
     *       kubelet).</li>
     * </ul>
     * See https://github.com/brewlet/brewlet/blob/main/docs/runnable-image.md.
     */
    @Parameter(property = "brewlet.format", defaultValue = "image")
    protected String format;

    /**
     * Managed dependency bundle reference for {@code brewlet:push}. This may be
     * an OCI registry reference or a path to a local OCI image layout.
     */
    @Parameter(property = "brewlet.dependencyBundle")
    protected String dependencyBundle;

    /** PKCS#8 PEM ECDSA P-256 private key used for supply-chain attestations. */
    @Parameter(property = "brewlet.signingKey")
    protected File signingKey;

    /** SubjectPublicKeyInfo PEM public key trusted for managed dependency bundles. */
    @Parameter(property = "brewlet.trustedPublicKey")
    protected File trustedPublicKey;

    /** Expected and asserted builder identity in signed predicates. */
    @Parameter(property = "brewlet.signerIdentity")
    protected String signerIdentity;

    /** Expected identity in signed dependency-bundle provenance. */
    @Parameter(property = "brewlet.trustedSignerIdentity")
    protected String trustedSignerIdentity;

    /** Identity asserted by the final application-image publisher. */
    @Parameter(property = "brewlet.builderIdentity")
    protected String builderIdentity;

    /**
     * Output directory for generated Brewlet files
     * ({@code jvm-config.json}, OCI layout, manifests).
     */
    @Parameter(defaultValue = "${project.build.directory}/brewlet")
    protected File outputDirectory;

    // -----------------------------------------------------------------------
    // Config-inference helpers
    // -----------------------------------------------------------------------

    /**
     * Resolves the effective JAR file: the configured {@link #jarFile} if set,
     * otherwise the project's primary artifact file.
     */
    protected File resolveJarFile() throws MojoExecutionException {
        if (jarFile != null) {
            if (!jarFile.exists()) {
                throw new MojoExecutionException(
                        "Configured jarFile does not exist: " + jarFile.getAbsolutePath()
                        + "  — run 'mvn package' first.");
            }
            return jarFile;
        }
        File artifact = project.getArtifact().getFile();
        if (artifact == null || !artifact.exists()) {
            throw new MojoExecutionException(
                    "Project artifact not found. Run 'mvn package' before this goal.");
        }
        return artifact;
    }

    /**
     * Builds a fully-inferred {@link JvmConfig} from POM metadata, JAR manifest,
     * and user-supplied configuration.
     */
    protected JvmConfig buildConfig() throws MojoExecutionException {
        File jar = resolveJarFile();

        // Inspect the JAR manifest. Main-Class is what makes `java -jar` valid;
        // Start-Class (Spring Boot) gives the effective application main class.
        String manifestMainClass = null;
        String manifestEffectiveClass = null;
        String jarModuleName = null;
        String jarModuleMainClass = null;
        try {
            manifestMainClass = JarInspector.mainClass(jar);
            manifestEffectiveClass = JarInspector.effectiveMainClass(jar);
            jarModuleName = JarInspector.moduleName(jar);
            jarModuleMainClass = JarInspector.moduleMainClass(jar);
        } catch (IOException e) {
            getLog().warn("Could not inspect JAR: " + e.getMessage());
        }
        boolean modularJar = jarModuleName != null;

        // Explicit <mainClass> wins; otherwise fall back to the manifest's
        // effective class.
        String resolvedMainClass = (mainClass != null) ? mainClass : manifestEffectiveClass;

        // Reject conflicting user configuration up front (dev-time) with
        // actionable messages, rather than silently ignoring fields at deploy time.
        if (entryMode != null && !"jar".equals(entryMode)
                && !"classpath".equals(entryMode) && !"module".equals(entryMode)) {
            throw new MojoExecutionException("Invalid <entryMode> \"" + entryMode
                    + "\": expected \"jar\", \"classpath\", or \"module\".");
        }
        if ("jar".equals(entryMode) && layered) {
            throw new MojoExecutionException("<entryMode>jar</entryMode> conflicts with "
                    + "<layered>true</layered>: layered deployment ships dependencies as "
                    + "their own layers, so `java -jar` (which reads only the single JAR's "
                    + "manifest class path) cannot see them. Use <entryMode>classpath</entryMode> "
                    + "or <entryMode>module</entryMode>, or drop <entryMode>.");
        }
        if ("jar".equals(entryMode) && mainClass != null) {
            throw new MojoExecutionException("<mainClass> is set but <entryMode>jar</entryMode> "
                    + "ignores it (`java -jar` uses the manifest Main-Class). Use "
                    + "<entryMode>classpath</entryMode> to launch <mainClass> via -cp.");
        }
        if ("module".equals(entryMode) && !modularJar) {
            throw new MojoExecutionException("<entryMode>module</entryMode> requires a modular "
                    + "JAR (a root module-info.class), but " + jar.getName() + " has none. "
                    + "Build a modular JAR or drop <entryMode>module</entryMode>.");
        }

        // Entry mode is driven by the JAR's shape, NOT by a user-supplied
        // <mainClass>: an explicit module (module-info.class) launches on the
        // module path; otherwise `java -jar` is only valid when the manifest
        // actually declares Main-Class.
        String resolvedEntryMode = entryMode;
        if (resolvedEntryMode == null) {
            if (modularJar) {
                resolvedEntryMode = "module";
            } else {
                resolvedEntryMode = (manifestMainClass != null) ? "jar" : "classpath";
            }
        }

        // Layered deployment ships dependencies as their own OCI layer(s) instead
        // of one fat JAR. The launch mode still follows the JAR's shape: a modular
        // JAR stays on the module path (deps -> /app/mods), while a non-modular JAR
        // is inherently class-path mode (deps -> /app/lib, launched via
        // `java -cp app.jar:lib/* MainClass`) regardless of the manifest Main-Class.
        if (layered && !"module".equals(resolvedEntryMode)) {
            resolvedEntryMode = "classpath";
        }

        // In classpath mode the artifact is launched via `java -cp ... MainClass`, so a
        // main class is mandatory. Fail early with an actionable message rather than
        // emitting an unrunnable config.
        if ("classpath".equals(resolvedEntryMode) && resolvedMainClass == null) {
            throw new MojoExecutionException(
                    "entry.mode=classpath requires a main class, but the JAR manifest has "
                    + "no Main-Class/Start-Class and no <mainClass> was configured. "
                    + "Set <mainClass> in the plugin configuration.");
        }

        JvmConfig cfg = new JvmConfig();
        cfg.setSchemaVersion(1);
        cfg.setMainJar(jar.getName());

        // In layered class-path mode, launch from the thin app JAR plus the
        // dependency layers unpacked to /app/lib via the JVM's `lib/*` wildcard.
        // (Module mode sets entry.modulePath instead — see below.)
        Entry resolvedEntry = new Entry(resolvedEntryMode);
        if (layered && "classpath".equals(resolvedEntryMode)) {
            resolvedEntry.setClassPath(List.of(jar.getName(), "lib/*"));
        }

        // mainClass lives on the entry and is only meaningful in classpath mode;
        // in jar mode `java -jar` uses the manifest's Main-Class. (resolvedMainClass
        // is guaranteed non-null here for classpath mode.)
        if ("classpath".equals(resolvedEntryMode)) {
            resolvedEntry.setMainClass(resolvedMainClass);
        }

        // In module mode the artifact launches via `java -p … -m <module>[/<mainClass>]`.
        // The module name comes from the JAR's descriptor; an explicit <mainClass>
        // (or the module's declared main class) is optional and only set when known.
        if ("module".equals(resolvedEntryMode)) {
            resolvedEntry.setModule(jarModuleName);
            String moduleMain = (mainClass != null) ? mainClass : jarModuleMainClass;
            if (moduleMain != null) {
                resolvedEntry.setMainClass(moduleMain);
            }
            // When dependencies are shipped as a module layer (layered mode with
            // runtime deps), the module path is the main JAR plus the /app/mods
            // directory that layer unpacks to. A single self-contained modular JAR
            // needs no explicit module path (the JAR alone is the module path).
            if (layered && !collectRuntimeDeps().isEmpty()) {
                resolvedEntry.setModulePath(List.of(jar.getName(), "mods"));
            }
        }
        cfg.setEntry(resolvedEntry);

        cfg.setEnablePreview(Boolean.TRUE.equals(enablePreview) ? Boolean.TRUE : null);
        cfg.setAddModules(addModules != null && !addModules.isEmpty() ? addModules : null);
        cfg.setAddOpens(addOpens != null && !addOpens.isEmpty() ? addOpens : null);
        cfg.setAddExports(addExports != null && !addExports.isEmpty() ? addExports : null);
        cfg.setSystemProperties(systemProperties != null && !systemProperties.isEmpty() ? systemProperties : null);
        cfg.setUser(user);
        cfg.setEnv(env != null && !env.isEmpty() ? env : null);

        // Optional arch constraint (non-portable artifacts): an explicit <arch>
        // wins; otherwise scan the JAR for bundled natives and default the
        // constraint, unless auto-detection is disabled. See
        // https://github.com/brewlet/brewlet/blob/main/docs/multi-arch.md.
        if (arch != null && !arch.isEmpty()) {
            cfg.setArch(arch);
        } else if (detectNativeArch) {
            try {
                JarInspector.NativeArchScan scan = JarInspector.scanNativeArch(jar);
                if (!scan.arches().isEmpty()) {
                    cfg.setArch(scan.arches());
                    getLog().info("Detected bundled native libraries -> arch constraint "
                            + scan.arches() + " (override with <arch>, disable with "
                            + "<detectNativeArch>false</detectNativeArch>).");
                } else if (!scan.unrecognized().isEmpty()) {
                    getLog().warn("Found " + scan.nativeLibs() + " bundled native library(ies) but "
                            + "could not infer architecture; publishing arch-neutral. Set <arch> "
                            + "explicitly if this JAR is not portable (e.g. "
                            + scan.unrecognized().get(0) + ").");
                }
            } catch (IOException e) {
                getLog().warn("Could not scan JAR for native libraries: " + e.getMessage());
            }
        }

        // Backstop: enforce the same mode/field invariants as the Go launch core
        // so a bad config can never be published.
        try {
            cfg.validate();
        } catch (IllegalStateException e) {
            throw new MojoExecutionException("Generated launch config is invalid: " + e.getMessage(), e);
        }

        return cfg;
    }

    /**
     * Resolves the required JDK feature (major) version for the deployment
     * descriptor's {@code spec.jvm.version}: an explicit {@code <jdkFeature>}
     * wins, otherwise it is inferred from the project's release/target via
     * {@link JdkVersionResolver}. This feeds the CRD/Deployment, not the
     * artifact config.
     */
    protected int resolveJdkFeature() {
        return (jdkFeature != null && jdkFeature > 0)
                ? jdkFeature
                : JdkVersionResolver.resolve(project);
    }

    /**
     * Resolves the launcher name for the deployment descriptor's
     * {@code spec.jvm.launcher}, defaulting to the vanilla {@code java} launcher.
     */
    protected String resolveLauncher() {
        return (launcher == null || launcher.isBlank()) ? "java" : launcher;
    }

    /**
     * Resolves the optional JDK distribution for the deployment descriptor's
     * {@code spec.jvm.distribution}, or {@code null} when unset (any distribution
     * of the requested feature is acceptable).
     */
    protected String resolveJdkDistribution() {
        return (jdkDistribution == null || jdkDistribution.isBlank())
                ? null
                : jdkDistribution.trim();
    }

    /**
     * Collects the project's resolved runtime dependencies (the JARs a running
     * container actually needs) as {@link LayerBuilder.Dep}s. Only
     * compile/runtime-scoped artifacts added to the class path are included;
     * {@code test}, {@code provided}, and {@code system} scopes are excluded.
     */
    protected List<LayerBuilder.Dep> collectRuntimeDeps() {
        List<LayerBuilder.Dep> deps = new ArrayList<>();
        Set<Artifact> artifacts = project.getArtifacts();
        for (Artifact a : artifacts) {
            if (a.getArtifactHandler() != null && !a.getArtifactHandler().isAddedToClasspath()) {
                continue;
            }
            String scope = a.getScope();
            if (Artifact.SCOPE_TEST.equals(scope)
                    || Artifact.SCOPE_PROVIDED.equals(scope)
                    || Artifact.SCOPE_SYSTEM.equals(scope)) {
                continue;
            }
            File file = a.getFile();
            if (file == null || !file.exists()) {
                getLog().warn("Skipping unresolved dependency (run 'mvn package' first): "
                        + a.getId());
                continue;
            }
            deps.add(new LayerBuilder.Dep(file.getName(), file.toPath(), a.isSnapshot()));
        }
        return deps;
    }

    /** Builds the canonical lock for the current resolved Maven runtime graph. */
    protected DependencyLock collectRuntimeDependencyLock() throws MojoExecutionException {
        List<DependencyLock.Entry> entries = new ArrayList<>();
        Set<String> filenames = new HashSet<>();
        for (Artifact artifact : project.getArtifacts()) {
            if (artifact.getArtifactHandler() != null
                    && !artifact.getArtifactHandler().isAddedToClasspath()) {
                continue;
            }
            String scope = artifact.getScope();
            if (Artifact.SCOPE_TEST.equals(scope) || Artifact.SCOPE_PROVIDED.equals(scope)
                    || Artifact.SCOPE_SYSTEM.equals(scope)) {
                continue;
            }
            File file = artifact.getFile();
            if (file == null || !file.isFile()) {
                throw new MojoExecutionException("Runtime dependency is unresolved: "
                        + artifact.getId() + ". Run Maven dependency resolution/package first.");
            }
            if (!filenames.add(file.getName())) {
                throw new MojoExecutionException("Dependency bundle cannot use a flat classpath: "
                        + "duplicate filename " + file.getName());
            }
            try {
                entries.add(new DependencyLock.Entry(
                        artifact.getGroupId(), artifact.getArtifactId(), artifact.getVersion(),
                        artifact.getType(), blankToNull(artifact.getClassifier()), scope,
                        file.getName(), LocalStore.sha256Hex(Files.readAllBytes(file.toPath()))
                                .substring("sha256:".length())));
            } catch (IOException e) {
                throw new MojoExecutionException("Failed to hash dependency " + artifact.getId(), e);
            }
        }
        DependencyLock lock = new DependencyLock();
        lock.setDependencies(entries);
        return lock;
    }

    private static String blankToNull(String value) {
        return value == null || value.isBlank() ? null : value;
    }

    /**
     * Builds the ordered dependency layers for the artifact from the project's
     * resolved runtime dependency tree ({@link MavenProject#getArtifacts()}).
     * Returns an empty list when {@link #layered} is disabled or the project has
     * no runtime dependencies.
     *
     * <p>The layer kind follows {@code resolvedEntryMode}: {@code module} mode
     * produces a single {@code modulepath.layer.v1+tar} (the app's library
     * modules, unpacked to {@code /app/mods} and fed to {@code --module-path});
     * any other mode produces {@code classpath.layer.v1+tar} layer(s) (unpacked
     * to {@code /app/lib}).
     */
    protected List<ArtifactLayer> buildArtifactLayers(String resolvedEntryMode)
            throws MojoExecutionException {
        if (!layered) {
            return List.of();
        }

        List<LayerBuilder.Dep> deps = collectRuntimeDeps();
        if (deps.isEmpty()) {
            getLog().warn("brewlet.layered=true but the project has no runtime "
                    + "dependencies; shipping the JAR layer only.");
            return List.of();
        }

        boolean module = "module".equals(resolvedEntryMode);
        try {
            List<ArtifactLayer> layers = module
                    ? LayerBuilder.buildModule(deps)
                    : LayerBuilder.build(deps, splitSnapshotLayers);
            getLog().info("Brewlet: layered deployment — " + deps.size()
                    + " dependency JAR(s) in " + layers.size() + " "
                    + (module ? "module-path" : "class-path") + " layer(s):");
            for (ArtifactLayer layer : layers) {
                getLog().info("  " + layer.name() + ": " + layer.tar().length + " bytes");
            }
            return layers;
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to build "
                    + (module ? "module-path" : "class-path") + " layers", e);
        }
    }

    /**
     * Applies the optional {@code brewlet.cdsArchive} parameter to the launch
     * config. When a prebuilt archive is provided, the {@code cds} hint defaults
     * from the archive basename unless already set, matching the Go CLI.
     */
    protected File applyCdsArchive(JvmConfig cfg) throws MojoExecutionException {
        if (cdsArchive == null) {
            return null;
        }
        if (!cdsArchive.exists()) {
            throw new MojoExecutionException(
                    "Configured cdsArchive does not exist: " + cdsArchive.getAbsolutePath());
        }
        if (!cdsArchive.isFile()) {
            throw new MojoExecutionException(
                    "Configured cdsArchive is not a file: " + cdsArchive.getAbsolutePath());
        }
        String base = cdsArchive.getName();
        if (cfg.getCds() == null) {
            cfg.setCds(new JvmConfig.Cds(base, "dynamic"));
        } else if (cfg.getCds().getArchive() == null || cfg.getCds().getArchive().isBlank()) {
            cfg.getCds().setArchive(base);
        }
        return cdsArchive;
    }

    /**
     * Builds the optional AppCDS archive layer. The layer is appended after any
     * classpath/modulepath layers and mounted by the shim at {@code /app/<name>}.
     */
    protected ArtifactLayer cdsLayer(File resolvedArchive) throws MojoExecutionException {
        if (resolvedArchive == null) {
            return null;
        }
        try {
            return new ArtifactLayer(resolvedArchive.getName(),
                    Files.readAllBytes(resolvedArchive.toPath()),
                    MediaTypes.CDS_LAYER_MEDIA_TYPE);
        } catch (IOException e) {
            throw new MojoExecutionException(
                    "Failed to read CDS archive " + resolvedArchive.getAbsolutePath(), e);
        }
    }

    /**
     * Enforces the CDS layer ↔ config invariant: a shipped archive must have a
     * matching {@code cds.archive} hint and vice versa, so the layer filename and
     * {@code -XX:SharedArchiveFile=/app/<archive>} agree.
     */
    static void validateCdsPairing(JvmConfig cfg, File cdsArchive) throws MojoExecutionException {
        boolean hasArchive = cdsArchive != null && cdsArchive.exists();
        if (!hasArchive) {
            if (cfg.getCds() != null) {
                throw new MojoExecutionException("launch config declares cds.archive \""
                        + cfg.getCds().getArchive()
                        + "\" but no CDS archive file was provided to ship");
            }
            return;
        }
        if (cfg.getCds() == null) {
            throw new MojoExecutionException(
                    "a CDS archive file was provided but the launch config has no cds.archive hint");
        }
        String base = cdsArchive.getName();
        if (!base.equals(cfg.getCds().getArchive())) {
            throw new MojoExecutionException("CDS archive filename \"" + base
                    + "\" does not match cds.archive \"" + cfg.getCds().getArchive()
                    + "\" (they must agree so the archive maps to /app/"
                    + cfg.getCds().getArchive() + ")");
        }
    }

    /** Re-runs launch-config validation after goals mutate optional fields. */
    protected void validateFinalConfig(JvmConfig cfg) throws MojoExecutionException {
        try {
            cfg.validate();
        } catch (IllegalStateException e) {
            throw new MojoExecutionException("Generated launch config is invalid: " + e.getMessage(), e);
        }
    }

    /**
     * Builds the OCI manifest annotations containing standard provenance
     * information (source, revision, version, created).
     */
    protected Map<String, String> buildAnnotations() {
        Map<String, String> ann = new LinkedHashMap<>();
        if (project.getScm() != null && project.getScm().getUrl() != null) {
            ann.put(MediaTypes.ANNOTATION_SOURCE, project.getScm().getUrl());
        }
        ann.put(MediaTypes.ANNOTATION_VERSION, project.getVersion());
        ann.put(MediaTypes.ANNOTATION_CREATED, Instant.now().toString());
        return ann;
    }

    @Override
    public final void execute() throws MojoExecutionException, MojoFailureException {
        if (skip) {
            getLog().info("Brewlet: skipping (brewlet.skip=true)");
            return;
        }
        doExecute();
    }

    protected abstract void doExecute() throws MojoExecutionException, MojoFailureException;
}
