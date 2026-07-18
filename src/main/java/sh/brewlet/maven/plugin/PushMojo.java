package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.Credential;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.util.CredentialResolver;

import java.io.IOException;
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

        JvmConfig cfg = buildConfig();
        java.io.File jar = resolveJarFile();
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

        if (runnable) {
            // Runnable image: the CDS archive is folded INTO the app layer (not a
            // separate layer), so pass only the class-path / module-path layers.
            List<ArtifactLayer> depLayers = buildArtifactLayers(cfg.getEntry().getMode());
            try {
                String indexDigest = client.pushRunnableImage(
                        tag, cfg, jar.toPath(), depLayers,
                        resolvedCdsArchive != null ? resolvedCdsArchive.toPath() : null,
                        annotations);
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

        List<ArtifactLayer> classpathLayers = new ArrayList<>(
                buildArtifactLayers(cfg.getEntry().getMode()));
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
}
