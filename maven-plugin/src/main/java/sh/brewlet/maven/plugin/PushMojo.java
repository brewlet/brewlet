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
import sh.brewlet.maven.plugin.supplychain.BundleProvenance;
import sh.brewlet.maven.plugin.supplychain.ManagedProvenance;
import sh.brewlet.maven.plugin.oci.OciDescriptor;
import sh.brewlet.maven.plugin.oci.OciReferrer;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.InvalidPathException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.security.GeneralSecurityException;

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
        VerifiedBundle verifiedBundle = null;
        DependencyBundle.Content managedBundle = null;
        String expectedBundleSigner = valueOrFallback(trustedSignerIdentity, signerIdentity);
        String applicationBuilder = signingKey == null
                ? builderIdentity : valueOrFallback(builderIdentity, signerIdentity);
        if (dependencyBundle != null && !dependencyBundle.isBlank()) {
            if ((signingKey == null) !=
                    (applicationBuilder == null || applicationBuilder.isBlank())) {
                throw new MojoExecutionException(
                        "signingKey and builderIdentity must be configured together");
            }
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
                verifiedBundle = resolveDependencyBundle(
                        dependencyBundle, expectedBundleSigner);
                managedBundle = verifiedBundle.bundle();
                DependencyBundle.verifyGraph(managedBundle.lock(),
                        collectRuntimeDependencyLock());
                List<Integer> compatibleJdks = managedBundle.config().getCompatibleJdks();
                if (compatibleJdks != null && !compatibleJdks.isEmpty()
                        && !compatibleJdks.contains(resolveJdkFeature())) {
                    throw new MojoExecutionException("Dependency bundle supports JDKs "
                            + compatibleJdks + " but this project requests JDK "
                            + resolveJdkFeature());
                }
            } catch (IOException | GeneralSecurityException e) {
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
                        1, true,
                        LocalStore.sha256Hex(Files.readAllBytes(jar.toPath())),
                        managedBundle.manifestDigest(),
                        managedBundle.config().getLayerDigest(),
                        managedBundle.config().getLockDigest(),
                        verifiedBundle.sbomDigest(),
                        managedBundle.config().getSourceBom(),
                        signingKey == null ? null : applicationBuilder);
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
                if (managedBundle != null && signingKey != null) {
                    byte[] indexBytes = client.pullManifest(indexDigest);
                    OciReferrer.Content attestation = ManagedProvenance.create(
                            image, indexDigest, indexBytes.length,
                            LocalStore.sha256Hex(Files.readAllBytes(jar.toPath())),
                            managedBundle.manifestDigest(),
                            managedBundle.config().getLayerDigest(),
                            managedBundle.config().getLockDigest(),
                            verifiedBundle.sbomDigest(), managedBundle.config().getSourceBom(),
                            applicationBuilder, signingKey.toPath());
                    client.pushReferrer(attestation);
                    getLog().info("  managed dependency attestation: "
                            + attestation.manifestDigest());
                }
                getLog().info("Brewlet: pushed " + image + " (runnable OCI image — kubelet-pullable)");
                getLog().info("  index: " + indexDigest);
                getLog().info("  platforms: "
                        + sh.brewlet.maven.plugin.oci.RunnableImageBuilder.targetArches(cfg));
                getLog().info("  a runtimeClassName: brewlet pod can now set image: " + image);
                getLog().info("  developer shipped ONLY the JAR; no Dockerfile, no base image.");
            } catch (IOException | InterruptedException | GeneralSecurityException e) {
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

    private VerifiedBundle resolveDependencyBundle(String reference, String expectedBundleSigner)
            throws IOException, InterruptedException, GeneralSecurityException {
        try {
            Path path = Path.of(reference);
            if (Files.isDirectory(path)) {
                DependencyBundle.Content bundle = DependencyBundle.loadLayout(path);
                LocalStore store = new LocalStore(path);
                List<OciDescriptor> sbomRefs = store.referrers(bundle.manifestDigest(),
                        MediaTypes.CYCLONEDX_ARTIFACT_TYPE);
                List<OciDescriptor> provenanceRefs = store.referrers(bundle.manifestDigest(),
                        MediaTypes.DSSE_ARTIFACT_TYPE);
                return verifyBundleReferrers(bundle, sbomRefs, provenanceRefs,
                        store::readReferrerDocument, expectedBundleSigner);
            }
        } catch (InvalidPathException ignored) {
            // A registry reference is not required to be a valid local path.
        }
        String[] parts = RegistryClient.splitRef(reference);
        Credential bundleCredential = CredentialResolver.resolve(parts[0], settings);
        RegistryClient client = new RegistryClient(parts[0], parts[1], bundleCredential);
        DependencyBundle.Content bundle =
                client.pullDependencyBundle(RegistryClient.extractTag(reference));
        List<OciDescriptor> sbomRefs = client.discoverReferrers(bundle.manifestDigest(),
                MediaTypes.CYCLONEDX_ARTIFACT_TYPE);
        List<OciDescriptor> provenanceRefs = client.discoverReferrers(bundle.manifestDigest(),
                MediaTypes.DSSE_ARTIFACT_TYPE);
        return verifyBundleReferrers(bundle, sbomRefs, provenanceRefs,
                descriptor -> client.pullReferrerDocument(
                        descriptor, bundle.manifestDigest(),
                        bundle.manifestBytes().length), expectedBundleSigner);
    }

    private VerifiedBundle verifyBundleReferrers(DependencyBundle.Content bundle,
                                                  List<OciDescriptor> sbomRefs,
                                                  List<OciDescriptor> provenanceRefs,
                                                  ReferrerReader reader,
                                                  String expectedBundleSigner)
            throws IOException, InterruptedException, GeneralSecurityException {
        if (sbomRefs.size() != 1) {
            throw new GeneralSecurityException(
                    "Managed bundle requires exactly one SBOM referrer");
        }
        byte[] sbom = reader.read(sbomRefs.get(0));
        if (provenanceRefs.isEmpty()) {
            return new VerifiedBundle(bundle, BundleProvenance.validateSbom(bundle, sbom));
        }
        if (trustedPublicKey == null || expectedBundleSigner == null
                || expectedBundleSigner.isBlank()) {
            throw new GeneralSecurityException("Bundle provenance is present; "
                    + "trustedPublicKey and trustedSignerIdentity are required");
        }
        GeneralSecurityException rejection = null;
        for (OciDescriptor provenanceRef : provenanceRefs) {
            try {
                byte[] envelope = reader.read(provenanceRef);
                String digest = BundleProvenance.verify(bundle, sbom, envelope,
                        trustedPublicKey.toPath(), expectedBundleSigner);
                return new VerifiedBundle(bundle, digest);
            } catch (GeneralSecurityException | IOException e) {
                rejection = new GeneralSecurityException(
                        "Rejected dependency-bundle provenance referrer "
                                + provenanceRef.getDigest(), e);
            }
        }
        throw new GeneralSecurityException(
                "No dependency-bundle provenance referrer was signed by the trusted signer",
                rejection);
    }

    private record VerifiedBundle(DependencyBundle.Content bundle, String sbomDigest) {}

    @FunctionalInterface
    private interface ReferrerReader {
        byte[] read(OciDescriptor descriptor) throws IOException, InterruptedException;
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

    private static String valueOrFallback(String value, String fallback) {
        return value == null || value.isBlank() ? fallback : value;
    }
}
