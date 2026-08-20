package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.OciDescriptor;

import java.io.File;
import java.io.IOException;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * <strong>brewlet:build</strong> — Assemble the Brewlet OCI artifact into a
 * local OCI image-layout directory ({@code target/brewlet/oci}) without pushing
 * to a registry. Useful for inspection, air-gapped flows, or integration tests
 * against a local registry.
 *
 * <p>The resulting layout matches the CLI's {@code --store} format and can be
 * read with {@code brewlet inspect} or pushed with {@code oras push}.
 *
 * <p>Example:
 * <pre>
 * mvn brewlet:build
 * ls target/brewlet/oci/blobs/sha256/
 * </pre>
 */
@Mojo(name = "build",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class BuildMojo extends AbstractBrewletMojo {

    /**
     * OCI image-layout output directory.
     * Defaults to {@code ${project.build.directory}/brewlet/oci}.
     */
    @Parameter(defaultValue = "${project.build.directory}/brewlet/oci")
    private File ociOutputDirectory;

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        if (image == null || image.isBlank()) {
            throw new MojoExecutionException(
                    "brewlet:build requires <image> to be set (used as the OCI ref name).");
        }

        JvmConfig cfg = buildConfig();
        File jar = resolveJarFile();
        File resolvedCdsArchive = applyCdsArchive(cfg);
        validateFinalConfig(cfg);
        validateCdsPairing(cfg, resolvedCdsArchive);
        List<ArtifactLayer> classpathLayers = new ArrayList<>(
                buildArtifactLayers(cfg.getEntry().getMode()));
        ArtifactLayer cdsLayer = cdsLayer(resolvedCdsArchive);
        if (cdsLayer != null) {
            classpathLayers.add(cdsLayer);
        }

        LocalStore store = new LocalStore(ociOutputDirectory.toPath());
        Map<String, String> annotations = buildAnnotations();

        OciDescriptor manifestDesc;
        try {
            manifestDesc = store.push(image, cfg, jar.toPath(), classpathLayers, annotations);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to write local OCI layout", e);
        }

        getLog().info("Brewlet: wrote OCI image-layout → " + ociOutputDirectory.getPath());
        getLog().info("  manifest: " + manifestDesc.getDigest()
                + " (" + manifestDesc.getSize() + " bytes)");
        getLog().info("  artifactType: " + manifestDesc.getArtifactType());
        getLog().info("  ref: " + image);
        if (cfg.getCds() != null) {
            getLog().info("  cds archive: " + cfg.getCds().getArchive()
                    + " (mounted /app/" + cfg.getCds().getArchive()
                    + "; -Xshare:auto, best-effort)");
        }
    }
}
