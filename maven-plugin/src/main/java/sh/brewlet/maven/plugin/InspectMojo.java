package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;

import java.io.IOException;
import java.util.List;

/**
 * <strong>brewlet:inspect</strong> — Print the fully-resolved launch config and
 * OCI artifact descriptor that <em>would</em> be pushed, without actually
 * pushing anything (dry-run equivalent).
 *
 * <p>Useful for verifying inference (JDK version, main class, entry mode)
 * before committing to {@code brewlet:push}.
 *
 * <p>Example:
 * <pre>
 * mvn brewlet:inspect
 * </pre>
 */
@Mojo(name = "inspect",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class InspectMojo extends AbstractBrewletMojo {

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        JvmConfig cfg = buildConfig();
        java.io.File jar = resolveJarFile();
        boolean runnable = "image".equals(format);

        getLog().info("== Brewlet inspect ==");
        getLog().info("  image: " + (image != null ? image : "(not set)"));
        getLog().info("  jar: " + jar.getAbsolutePath() + " (" + jar.length() + " bytes)");
        getLog().info("  format: " + format
                + (runnable ? " (standard, kubelet-pullable OCI image)"
                            : " (native Brewlet OCI artifact, custom media types)"));
        if (runnable) {
            getLog().info("  kind: runnable OCI image");
            getLog().info("  configMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.OCI_IMAGE_CONFIG_MEDIA_TYPE);
            getLog().info("  layerMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE);
            getLog().info("  launchConfigAnnotation: " + sh.brewlet.maven.plugin.oci.MediaTypes.JVM_CONFIG_ANNOTATION);
            getLog().info("  platforms: "
                    + sh.brewlet.maven.plugin.oci.RunnableImageBuilder.targetArches(cfg));
        } else {
            getLog().info("  artifactType: " + sh.brewlet.maven.plugin.oci.MediaTypes.ARTIFACT_TYPE);
            getLog().info("  configMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.CONFIG_MEDIA_TYPE);
            getLog().info("  layerMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.JAR_LAYER_MEDIA_TYPE);
        }

        if (layered) {
            String mode = cfg.getEntry().getMode();
            List<ArtifactLayer> layers = buildArtifactLayers(mode);
            String kind = "module".equals(mode) ? "module-path" : "class-path";
            getLog().info("  layered: true (" + layers.size() + " " + kind + " layer(s))");
            for (ArtifactLayer layer : layers) {
                getLog().info("    - " + layer.name() + ": " + layer.mediaType()
                        + " (" + layer.tar().length + " bytes)");
            }
        }

        try {
            getLog().info("\n== jvm-config.json ==\n" + MAPPER.writeValueAsString(cfg));
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to serialize config", e);
        }
    }
}
