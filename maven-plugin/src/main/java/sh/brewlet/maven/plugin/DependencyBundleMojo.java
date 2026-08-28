package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.DependencyBundleConfig;
import sh.brewlet.maven.plugin.model.DependencyLock;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.Credential;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.util.CredentialResolver;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.File;
import java.io.IOException;
import java.util.List;

/** Creates and publishes the resolved runtime dependency closure as an OCI bundle. */
@Mojo(name = "dependency-bundle", defaultPhase = LifecyclePhase.PACKAGE,
        requiresProject = true, requiresDependencyResolution = ResolutionScope.RUNTIME,
        threadSafe = true)
public class DependencyBundleMojo extends AbstractBrewletMojo {
    @Parameter(property = "brewlet.dependencyBundleImage")
    private String dependencyBundleImage;

    @Parameter(property = "brewlet.sourceBom")
    private String sourceBom;

    @Parameter
    private List<Integer> compatibleJdks;

    @Parameter(defaultValue = "${project.build.directory}/brewlet/dependency-bundle-oci")
    private File dependencyBundleOutputDirectory;

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        String ref = dependencyBundleImage;
        if (ref == null || ref.isBlank()) {
            ref = image;
        }
        if (ref == null || ref.isBlank()) {
            throw new MojoExecutionException("brewlet:dependency-bundle requires "
                    + "<dependencyBundleImage> (or <image>) with an OCI reference.");
        }
        validateSourceBom(sourceBom);

        DependencyLock lock = collectRuntimeDependencyLock();
        ArtifactLayer layer;
        try {
            layer = LayerBuilder.buildBundle(collectRuntimeDeps());
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to build dependency classpath layer", e);
        }
        DependencyBundleConfig config = new DependencyBundleConfig();
        config.setName(project.getArtifactId());
        config.setVersion(project.getVersion());
        config.setSourceBom(sourceBom);
        config.setCompatibleJdks(compatibleJdks);

        DependencyBundle.Content bundle;
        try {
            bundle = DependencyBundle.build(config, lock, layer);
            new LocalStore(dependencyBundleOutputDirectory.toPath())
                    .pushDependencyBundle(ref, bundle);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to write dependency bundle OCI layout", e);
        }
        getLog().info("Brewlet: wrote dependency bundle OCI layout -> "
                + dependencyBundleOutputDirectory);
        getLog().info("  manifest: " + bundle.manifestDigest());
        getLog().info("  lock: " + bundle.config().getLockDigest());
        getLog().info("  dependencies: " + lock.getDependencies().size());

        if (dryRun) {
            getLog().info("Brewlet: dry-run mode — bundle was not published to " + ref);
            return;
        }
        String[] parts = RegistryClient.splitRef(ref);
        Credential credential = CredentialResolver.resolve(parts[0], settings);
        try {
            String digest = new RegistryClient(parts[0], parts[1], credential)
                    .pushDependencyBundle(RegistryClient.extractTag(ref), bundle);
            getLog().info("Brewlet: published dependency bundle " + ref + " (" + digest + ")");
        } catch (IOException | InterruptedException e) {
            if (e instanceof InterruptedException) {
                Thread.currentThread().interrupt();
            }
            throw new MojoExecutionException("Failed to publish dependency bundle " + ref, e);
        }
    }

    static void validateSourceBom(String value) throws MojoExecutionException {
        if (value == null || value.isBlank()) {
            throw new MojoExecutionException("brewlet:dependency-bundle requires "
                    + "<sourceBom> in G:A:V syntax.");
        }
        if (!value.matches("[^:\\s]+:[^:\\s]+:[^:\\s]+")) {
            throw new MojoExecutionException("<sourceBom> must use G:A:V syntax, got: " + value);
        }
    }
}
