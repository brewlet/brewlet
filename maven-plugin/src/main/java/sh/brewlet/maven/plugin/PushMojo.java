package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.model.ManagedDependencyEvidence;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.Credential;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.util.CredentialResolver;
import sh.brewlet.maven.plugin.util.JarInspector;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.InvalidPathException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * <strong>brewlet:push</strong> — Build the Brewlet OCI artifact and push it to
 * the registry specified by {@code <image>}. Optionally bound to the
 * {@code deploy} phase.
 *
 * <p>The push mirrors the Go CLI's {@code brewlet push} command:
 * <ol>
 *   <li>Serialize the JvmConfig → {@code application/vnd.brewlet.jvm.config.v1+json} blob.</li>
 *   <li>Push the JAR → {@code application/vnd.brewlet.jar.layer.v1+jar} layer blob.</li>
 *   <li>Build an OCI manifest with {@code artifactType: application/vnd.brewlet.app.v1+json}.</li>
 *   <li>Push the manifest and tag it.</li>
 * </ol>
 *
 * <p>Example:
 * <pre>
 * mvn clean package brewlet:push -Dbrewlet.image=registry.example.com/team/app:1.0
 * </pre>
 *
 * <p>Or bind to the deploy lifecycle:
 * <pre>{@code
 * <executions>
 *   <execution>
 *     <goals><goal>push</goal></goals>
 *   </execution>
 * </executions>
 * }</pre>
 */
@Mojo(name = "push",
      defaultPhase = LifecyclePhase.DEPLOY,
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class PushMojo extends AbstractBrewletMojo {

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        if (image == null || image.isBlank()) {
            throw new MojoExecutionException(
                    "brewlet:push requires <image> to be configured, e.g. "
                    + "<image>registry.example.com/team/app:${project.version}</image>");
        }

        java.io.File jar = resolveJarFile();
        DependencyBundle.Content managedBundle = null;
        if (dependencyBundle != null && !dependencyBundle.isBlank()) {
            if (!"image".equals(format)) {
                throw new MojoExecutionException("Managed dependency bundles require "
                        + "<format>image</format> so the approved OCI layer is reused unchanged.");
            }
            layered = true;
            entryMode = "classpath";
            try {
                List<String> embedded = JarInspector.embeddedJars(jar);
                if (!embedded.isEmpty()) {
                    throw new MojoExecutionException("Managed dependency bundle mode requires a "
                            + "thin application JAR, but found embedded JAR "
                            + embedded.get(0) + ". Disable fat-JAR repackaging.");
                }
                managedBundle = resolveDependencyBundle(dependencyBundle);
                DependencyBundle.verifyGraph(managedBundle.lock(),
                        collectRuntimeDependencyLock());
                List<Integer> compatibleJdks = managedBundle.config().getCompatibleJdks();
                if (compatibleJdks != null && !compatibleJdks.isEmpty()
                        && !compatibleJdks.contains(resolveJdkFeature())) {
                    throw new MojoExecutionException("Dependency bundle supports JDKs "
                            + compatibleJdks + " but this project requests JDK "
                            + resolveJdkFeature());
                }
            } catch (IOException e) {
                throw new MojoExecutionException("Invalid dependency bundle "
                        + dependencyBundle + ": " + e.getMessage(), e);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                throw new MojoExecutionException("Interrupted while pulling dependency bundle "
                        + dependencyBundle, e);
            }
        }
        JvmConfig cfg = buildConfig();
        if (managedBundle != null
                && (cfg.getEntry().getMainClass() == null
                || cfg.getEntry().getMainClass().isBlank())) {
            throw new MojoExecutionException("Managed dependency bundle mode requires mainClass "
                    + "for thin classpath launch. Configure <mainClass>.");
        }
        java.io.File resolvedCdsArchive = applyCdsArchive(cfg);
        validateFinalConfig(cfg);
        validateCdsPairing(cfg, resolvedCdsArchive);

        String[] parts = RegistryClient.splitRef(image);
        String registry = parts[0];
        String repository = parts[1];
        String tag = RegistryClient.extractTag(image);

        Credential credential = CredentialResolver.resolve(registry, settings);

        boolean runnable = "image".equals(format);
        if (!"artifact".equals(format) && !runnable) {
            throw new MojoExecutionException("Invalid <format> \"" + format
                    + "\": expected \"artifact\" or \"image\".");
        }

        if (dryRun) {
            getLog().info("Brewlet: dry-run mode — would push to " + image
                    + " (format=" + format + ")");
            try {
                getLog().info(MAPPER.writeValueAsString(cfg));
            } catch (IOException ignored) {}
            return;
        }

        getLog().info("Brewlet: pushing " + (runnable ? "runnable image" : "artifact")
                + " to " + image + " ...");
        getLog().info("  registry: " + registry);
        getLog().info("  repository: " + repository);
        getLog().info("  tag: " + tag);
        getLog().info("  jar: " + jar.getName() + " (" + jar.length() + " bytes)");
        if (cfg.getCds() != null) {
            getLog().info("  cds archive: " + cfg.getCds().getArchive()
                    + " (mounted /app/" + cfg.getCds().getArchive()
                    + "; -Xshare:auto, best-effort)");
        }
        if (credential != null) {
            getLog().info("  auth: credentials resolved for " + registry);
        } else {
            getLog().info("  auth: anonymous");
        }

        RegistryClient client = new RegistryClient(registry, repository, credential);
        Map<String, String> annotations = buildAnnotations();
        annotations.put("org.opencontainers.image.ref.name", image);
        List<ArtifactLayer> managedLayers = managedBundle == null
                ? null : List.of(new ArtifactLayer(
                        "dependencies", managedBundle.uncompressedLayer()));
        if (managedBundle != null) {
            try {
                ManagedDependencyEvidence evidence = new ManagedDependencyEvidence(
                        true,
                        LocalStore.sha256Hex(Files.readAllBytes(jar.toPath())),
                        managedBundle.manifestDigest(),
                        managedBundle.config().getLayerDigest(),
                        managedBundle.config().getLockDigest(),
                        managedBundle.config().getSourceBom());
                annotations.put(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION,
                        DependencyBundle.canonicalJson(evidence));
            } catch (IOException e) {
                throw new MojoExecutionException("Failed to serialize managed dependency evidence", e);
            }
        }

        if (runnable) {
            // Runnable image: the CDS archive is folded INTO the app layer (not a
            // separate layer), so pass only the class-path / module-path layers.
            try {
                String indexDigest;
                if (managedBundle != null) {
                    String sourceRepository = managedBundleSourceRepository(
                            dependencyBundle, registry);
                    indexDigest = client.pushRunnableImage(
                            tag, cfg, jar.toPath(), managedBundle,
                            resolvedCdsArchive != null ? resolvedCdsArchive.toPath() : null,
                            annotations, sourceRepository);
                } else {
                    indexDigest = client.pushRunnableImage(
                            tag, cfg, jar.toPath(),
                            buildArtifactLayers(cfg.getEntry().getMode()),
                            resolvedCdsArchive != null ? resolvedCdsArchive.toPath() : null,
                            annotations);
                }
                getLog().info("Brewlet: pushed " + image + " (runnable OCI image — kubelet-pullable)");
                getLog().info("  index: " + indexDigest);
                getLog().info("  platforms: "
                        + sh.brewlet.maven.plugin.oci.RunnableImageBuilder.targetArches(cfg));
                getLog().info("  a runtimeClassName: brewlet pod can now set image: " + image);
                getLog().info("  developer shipped ONLY the JAR; no Dockerfile, no base image.");
            } catch (IOException | InterruptedException e) {
                if (e instanceof InterruptedException) Thread.currentThread().interrupt();
                throw new MojoExecutionException("Failed to push runnable image to " + image, e);
            }
            return;
        }

        List<ArtifactLayer> classpathLayers = new ArrayList<>(managedLayers != null
                ? managedLayers : buildArtifactLayers(cfg.getEntry().getMode()));
        ArtifactLayer cdsLayer = cdsLayer(resolvedCdsArchive);
        if (cdsLayer != null) {
            classpathLayers.add(cdsLayer);
        }

        try {
            String manifestDigest = client.push(tag, cfg, jar.toPath(), classpathLayers, annotations);
            getLog().info("Brewlet: pushed " + image);
            getLog().info("  manifest: " + manifestDigest);
            getLog().info("  artifactType: " + sh.brewlet.maven.plugin.oci.MediaTypes.ARTIFACT_TYPE);
            getLog().info("  developer shipped ONLY the JAR; no Dockerfile, no base image.");
        } catch (IOException | InterruptedException e) {
            if (e instanceof InterruptedException) Thread.currentThread().interrupt();
            throw new MojoExecutionException("Failed to push artifact to " + image, e);
        }
    }

    private DependencyBundle.Content resolveDependencyBundle(String reference)
            throws IOException, InterruptedException {
        try {
            Path path = Path.of(reference);
            if (Files.isDirectory(path)) {
                return DependencyBundle.loadLayout(path);
            }
        } catch (InvalidPathException ignored) {
            // A registry reference is not required to be a valid local path.
        }
        String[] parts = RegistryClient.splitRef(reference);
        Credential bundleCredential = CredentialResolver.resolve(parts[0], settings);
        return new RegistryClient(parts[0], parts[1], bundleCredential)
                .pullDependencyBundle(RegistryClient.extractTag(reference));
    }

    private static String managedBundleSourceRepository(String reference,
                                                        String targetRegistry) {
        try {
            if (Files.isDirectory(Path.of(reference))) {
                return null;
            }
        } catch (InvalidPathException ignored) {
            // Continue parsing the registry reference.
        }
        String[] parts = RegistryClient.splitRef(reference);
        return targetRegistry.equals(parts[0]) ? parts[1] : null;
    }
}
