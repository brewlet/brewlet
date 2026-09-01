package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * Writes a Brewlet OCI artifact to a local OCI image-layout directory
 * ({@code target/brewlet/oci}). The layout is compatible with the Go
 * store used by the PoC CLI ({@code src/internal/artifact/artifact.go}).
 *
 * <p>Layout on disk:
 * <pre>
 * oci/
 *   oci-layout
 *   index.json
 *   blobs/sha256/&lt;hex&gt;
 * </pre>
 */
public class LocalStore {

    private static final ObjectMapper MAPPER = new ObjectMapper()
            .enable(SerializationFeature.INDENT_OUTPUT);

    private final Path root;

    public LocalStore(Path root) {
        this.root = root;
    }

    public Path getRoot() { return root; }

    /** Returns the blobs/sha256 directory, creating it if needed. */
    private Path blobsDir() throws IOException {
        Path dir = root.resolve("blobs").resolve("sha256");
        Files.createDirectories(dir);
        return dir;
    }

    /**
     * Writes {@code content} to the blob store and returns its descriptor
     * (digest + size; mediaType is set by the caller).
     */
    public OciDescriptor writeBlob(byte[] content) throws IOException {
        String digest = sha256Hex(content);
        String hexPart = digest.substring("sha256:".length());
        Path blobFile = blobsDir().resolve(hexPart);
        if (!Files.exists(blobFile)) {
            Files.write(blobFile, content);
        }
        return new OciDescriptor(null, digest, content.length);
    }

    /**
     * Assembles and writes the full Brewlet OCI artifact:
     * <ol>
     *   <li>Serializes {@code cfg} as the config blob.</li>
     *   <li>Writes the JAR bytes as the layer blob.</li>
     *   <li>Builds the OCI manifest and writes it.</li>
     *   <li>Updates {@code index.json}.</li>
     * </ol>
     *
     * @param ref     OCI reference ({@code name:tag} or full registry ref)
     * @param cfg     launch descriptor
     * @param jarPath path to the JAR file
     * @param extraAnnotations extra OCI annotations for the manifest (may be null)
     * @return descriptor of the written manifest
     */
    public OciDescriptor push(String ref, JvmConfig cfg, Path jarPath,
                              Map<String, String> extraAnnotations) throws IOException {
        return push(ref, cfg, jarPath, List.of(), extraAnnotations);
    }

    /**
     * Assembles and writes the full Brewlet OCI artifact, optionally with one or
     * more classpath (dependency) layers appended after the main JAR layer for
     * layered class-path deployment. See
     * https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md.
     *
     * @param ref              OCI reference ({@code name:tag} or full registry ref)
     * @param cfg              launch descriptor
     * @param jarPath          path to the (thin) application JAR file
     * @param classpathLayers  ordered dependency layers (may be empty)
     * @param extraAnnotations extra OCI annotations for the manifest (may be null)
     * @return descriptor of the written manifest
     */
    public OciDescriptor push(String ref, JvmConfig cfg, Path jarPath,
                              List<ArtifactLayer> classpathLayers,
                              Map<String, String> extraAnnotations) throws IOException {
        // 1. Config blob
        byte[] cfgBytes = MAPPER.writeValueAsBytes(cfg);
        OciDescriptor cfgDesc = writeBlob(cfgBytes);
        cfgDesc.setMediaType(MediaTypes.CONFIG_MEDIA_TYPE);

        // 2. JAR layer blob
        byte[] jarBytes = Files.readAllBytes(jarPath);
        OciDescriptor jarDesc = writeBlob(jarBytes);
        jarDesc.setMediaType(MediaTypes.JAR_LAYER_MEDIA_TYPE);
        jarDesc.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, cfg.getMainJar()));

        // 2b. Classpath (dependency) layers — stable → volatile, each its own blob.
        List<OciDescriptor> layers = new ArrayList<>();
        layers.add(jarDesc);
        if (classpathLayers != null) {
            for (ArtifactLayer layer : classpathLayers) {
                OciDescriptor desc = writeBlob(layer.tar());
                desc.setMediaType(layer.mediaType());
                desc.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, layer.name()));
                layers.add(desc);
            }
        }

        // 3. OCI Manifest
        OciManifest manifest = new OciManifest();
        manifest.setArtifactType(MediaTypes.ARTIFACT_TYPE);
        manifest.setConfig(cfgDesc);
        manifest.setLayers(layers);
        if (extraAnnotations != null && !extraAnnotations.isEmpty()) {
            manifest.setAnnotations(extraAnnotations);
        }

        byte[] manifestBytes = MAPPER.writeValueAsBytes(manifest);
        OciDescriptor manifestDesc = writeBlob(manifestBytes);
        manifestDesc.setMediaType(MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
        manifestDesc.setArtifactType(MediaTypes.ARTIFACT_TYPE);
        manifestDesc.setAnnotations(Map.of(MediaTypes.ANNOTATION_REF_NAME, ref));

        // 4. index.json
        upsertIndex(manifestDesc);

        // 5. oci-layout marker
        Files.createDirectories(root);
        Files.writeString(root.resolve("oci-layout"),
                "{\"imageLayoutVersion\":\"1.0.0\"}\n");

        return manifestDesc;
    }

    /** Writes a preassembled runnable image as a tagged OCI image-layout entry. */
    public OciDescriptor pushRunnableImage(String ref, RunnableImageBuilder.Result image)
            throws IOException {
        for (RunnableImageBuilder.Blob blob : image.blobs) {
            writeBlob(blob.data());
        }
        for (RunnableImageBuilder.Blob manifest : image.manifests) {
            writeBlob(manifest.data());
        }

        OciDescriptor indexDesc = writeBlob(image.indexBytes);
        indexDesc.setMediaType(MediaTypes.OCI_INDEX_MEDIA_TYPE);
        indexDesc.setAnnotations(Map.of(MediaTypes.ANNOTATION_REF_NAME, ref));
        upsertIndex(indexDesc);

        Files.createDirectories(root);
        Files.writeString(root.resolve("oci-layout"),
                "{\"imageLayoutVersion\":\"1.0.0\"}\n");
        return indexDesc;
    }

    /** Writes a managed dependency bundle as an OCI image-layout entry. */
    public OciDescriptor pushDependencyBundle(String ref, DependencyBundle.Content bundle)
            throws IOException {
        writeBlob(bundle.configBytes());
        writeBlob(bundle.lockBytes());
        writeBlob(bundle.compressedLayer());
        OciDescriptor manifestDesc = writeBlob(bundle.manifestBytes());
        manifestDesc.setMediaType(MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
        manifestDesc.setArtifactType(MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE);
        manifestDesc.setAnnotations(Map.of(MediaTypes.ANNOTATION_REF_NAME, ref));
        upsertIndex(manifestDesc);
        Files.createDirectories(root);
        Files.writeString(root.resolve("oci-layout"),
                "{\"imageLayoutVersion\":\"1.0.0\"}\n");
        return manifestDesc;
    }

    /** Writes a referrer and indexes it by immutable subject digest. */
    public OciDescriptor pushReferrer(OciReferrer.Content referrer) throws IOException {
        writeBlob(OciReferrer.emptyConfig());
        writeBlob(referrer.document());
        OciDescriptor manifestDesc = writeBlob(referrer.manifest());
        manifestDesc.setMediaType(MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
        manifestDesc.setArtifactType(referrer.model().getArtifactType());
        Map<String, String> annotations = new java.util.HashMap<>();
        annotations.put(MediaTypes.REFERRER_SUBJECT_ANNOTATION,
                referrer.model().getSubject().getDigest());
        if (referrer.model().getAnnotations() != null) {
            annotations.putAll(referrer.model().getAnnotations());
        }
        manifestDesc.setAnnotations(annotations);
        upsertIndex(manifestDesc);
        Files.createDirectories(root);
        Files.writeString(root.resolve("oci-layout"),
                "{\"imageLayoutVersion\":\"1.0.0\"}\n");
        return manifestDesc;
    }

    /** Returns referrer descriptors for a subject and artifact type. */
    public List<OciDescriptor> referrers(String subjectDigest, String artifactType)
            throws IOException {
        OciIndex index = readIndex();
        if (index.getManifests() == null) {
            return List.of();
        }
        List<OciDescriptor> matches = new ArrayList<>();
        for (OciDescriptor descriptor : index.getManifests()) {
            if (descriptor == null || !artifactType.equals(descriptor.getArtifactType())) {
                continue;
            }
            if (descriptor.getAnnotations() != null
                    && subjectDigest.equals(descriptor.getAnnotations().get(
                    MediaTypes.REFERRER_SUBJECT_ANNOTATION))) {
                matches.add(descriptor);
                continue;
            }
            try {
                byte[] manifestBytes = Files.readAllBytes(blobPath(descriptor.getDigest()));
                OciManifest manifest = OciReferrer.parseManifest(manifestBytes);
                if (manifest != null && manifest.getSubject() != null
                        && subjectDigest.equals(manifest.getSubject().getDigest())) {
                    matches.add(descriptor);
                }
            } catch (IOException | RuntimeException ignored) {
                // A malformed descriptor without a matching discovery hint is unrelated.
            }
        }
        return matches;
    }

    /** Reads and validates the sole document layer of a local referrer. */
    public byte[] readReferrerDocument(OciDescriptor descriptor) throws IOException {
        if (descriptor == null || !isDigest(descriptor.getDigest())) {
            throw new IOException("Referrer manifest descriptor has an invalid digest");
        }
        byte[] manifestBytes = Files.readAllBytes(blobPath(descriptor.getDigest()));
        if (!MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(descriptor.getMediaType())
                || manifestBytes.length != descriptor.getSize()
                || !sha256Hex(manifestBytes).equals(descriptor.getDigest())) {
            throw new IOException("Referrer manifest media type, digest, or size mismatch");
        }
        OciManifest manifest = OciReferrer.parseManifest(manifestBytes);
        String subjectDigest = manifest == null || manifest.getSubject() == null
                ? null : manifest.getSubject().getDigest();
        OciDescriptor subject = readIndex().getManifests().stream()
                .filter(candidate -> candidate != null
                        && java.util.Objects.equals(candidate.getDigest(), subjectDigest))
                .findFirst()
                .orElseThrow(() -> new IOException(
                        "Referrer subject is not indexed in the OCI layout"));
        validateReferrerManifest(manifest, descriptor, subject);
        if (manifest.getLayers() == null || manifest.getLayers().size() != 1) {
            throw new IOException("Referrer must contain exactly one layer");
        }
        OciDescriptor layer = manifest.getLayers().get(0);
        if (layer == null || !isDigest(layer.getDigest())) {
            throw new IOException("Referrer document layer has an invalid digest");
        }
        String expectedLayerType = MediaTypes.CYCLONEDX_ARTIFACT_TYPE.equals(
                manifest.getArtifactType()) ? MediaTypes.CYCLONEDX_LAYER_MEDIA_TYPE
                : MediaTypes.DSSE_LAYER_MEDIA_TYPE;
        if (!expectedLayerType.equals(layer.getMediaType())) {
            throw new IOException("Referrer document layer media type mismatch");
        }
        byte[] document = Files.readAllBytes(blobPath(layer.getDigest()));
        if (!sha256Hex(document).equals(layer.getDigest()) || document.length != layer.getSize()) {
            throw new IOException("Referrer document digest or size mismatch");
        }
        return document;
    }

    private static void validateReferrerManifest(OciManifest manifest, OciDescriptor descriptor,
                                                  OciDescriptor expectedSubject) throws IOException {
        if (manifest == null
                || manifest.getSchemaVersion() != 2
                || !MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(manifest.getMediaType())
                || !java.util.Objects.equals(
                        descriptor.getArtifactType(), manifest.getArtifactType())
                || manifest.getSubject() == null
                || !java.util.Objects.equals(
                        expectedSubject.getMediaType(), manifest.getSubject().getMediaType())
                || !java.util.Objects.equals(
                        expectedSubject.getDigest(), manifest.getSubject().getDigest())
                || expectedSubject.getSize() != manifest.getSubject().getSize()) {
            throw new IOException("Referrer manifest contract or subject mismatch");
        }
        OciDescriptor config = manifest.getConfig();
        if (config == null
                || !MediaTypes.OCI_EMPTY_CONFIG_MEDIA_TYPE.equals(config.getMediaType())
                || !sha256Hex(OciReferrer.emptyConfig()).equals(config.getDigest())
                || config.getSize() != OciReferrer.emptyConfig().length) {
            throw new IOException("Referrer must use the OCI empty config");
        }
    }

    private static boolean isDigest(String value) {
        return value != null && value.matches("sha256:[0-9a-f]{64}");
    }

    private void upsertIndex(OciDescriptor newEntry) throws IOException {
        OciIndex idx = readIndex();
        List<OciDescriptor> manifests = idx.getManifests() == null
                ? new ArrayList<>()
                : new ArrayList<>(idx.getManifests());

        String ref = newEntry.getAnnotations() != null
                ? newEntry.getAnnotations().get(MediaTypes.ANNOTATION_REF_NAME)
                : null;

        // Repeated publication of identical content is idempotent. A named
        // artifact also replaces the previous descriptor for that name.
        manifests.removeIf(m -> newEntry.getDigest().equals(m.getDigest())
                || (ref != null && m.getAnnotations() != null
                && ref.equals(m.getAnnotations().get(MediaTypes.ANNOTATION_REF_NAME))));
        manifests.add(newEntry);
        idx.setManifests(manifests);

        Files.createDirectories(root);
        Files.writeString(root.resolve("index.json"), MAPPER.writeValueAsString(idx));
    }

    private OciIndex readIndex() throws IOException {
        Path indexFile = root.resolve("index.json");
        if (Files.exists(indexFile)) {
            return MAPPER.readValue(indexFile.toFile(), OciIndex.class);
        }
        OciIndex idx = new OciIndex();
        idx.setSchemaVersion(2);
        idx.setMediaType(MediaTypes.OCI_INDEX_MEDIA_TYPE);
        return idx;
    }

    /**
     * Returns the absolute path to a blob file given its digest
     * ({@code "sha256:<hex>"}).
     */
    public Path blobPath(String digest) {
        return root.resolve("blobs").resolve("sha256")
                   .resolve(digest.substring("sha256:".length()));
    }

    public static String sha256Hex(byte[] data) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            byte[] hash = md.digest(data);
            StringBuilder sb = new StringBuilder("sha256:");
            for (byte b : hash) {
                sb.append(String.format("%02x", b));
            }
            return sb.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 not available", e);
        }
    }
}
