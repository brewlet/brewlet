package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.util.TarWriter;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.zip.GZIPOutputStream;

/**
 * Assembles a <strong>runnable OCI image</strong> — a STANDARD, kubelet-pullable
 * image carrying a Java application — entirely in memory, with no network access.
 * {@link RegistryClient#pushRunnableImage} pushes the result; unit tests exercise
 * this builder directly. This is the Java twin of the Go writer in
 * {@code src/internal/artifact/image.go}; the two MUST stay behaviourally in sync.
 *
 * <p>Why this exists: the native Brewlet artifact uses custom layer media types
 * ({@code +jar}, {@code .classpath+tar}, …) that containerd's CRI differ cannot
 * unpack, so a pod naming it as its {@code image:} fails to pull. A runnable image
 * packages the exact same JAR (+ optional dependency/module/CDS payload) as a
 * standard {@code tar+gzip} image so containerd/kubelet pull + unpack it normally;
 * the shim reads the launch contract from the manifest's
 * {@code brewlet.sh/jvm-config} annotation. See
 * https://github.com/brewlet/site/blob/main/docs/runnable-image.md.
 *
 * <p>Layout produced:
 * <ul>
 *   <li>one <em>app</em> layer: a flat tar of the primary JAR (named
 *       {@code cfg.mainJar}) plus the optional CDS {@code .jsa};</li>
 *   <li>the class-path / module-path dependency layers, gzip-compressed and
 *       role-tagged;</li>
 *   <li>a real OCI image config per architecture (with {@code rootfs.diff_ids}
 *       over the <em>uncompressed</em> tars);</li>
 *   <li>an OCI image manifest per architecture carrying the launch config in the
 *       {@code brewlet.sh/jvm-config} annotation;</li>
 *   <li>a multi-arch OCI image index tying the per-arch manifests together.</li>
 * </ul>
 */
public final class RunnableImageBuilder {

    private RunnableImageBuilder() {}

    /** Platforms a portable (pure-bytecode) JAR is published for. */
    private static final List<String> DEFAULT_ARCHES = List.of("amd64", "arm64");

    /** A content-addressable blob (layer, config, manifest, or index). */
    public record Blob(String digest, byte[] data, String mediaType) {}

    /** The fully-assembled image: what to push and in what role. */
    public static final class Result {
        /** Layers + per-arch image configs — pushed via the blob upload flow. */
        public final List<Blob> blobs = new ArrayList<>();
        /** Per-arch image manifests — pushed to the manifests endpoint by digest. */
        public final List<Blob> manifests = new ArrayList<>();
        /** The tagged multi-arch image index. */
        public byte[] indexBytes;
        public String indexDigest;
        /** The architectures this image was published for. */
        public List<String> arches;
    }

    /**
     * The architecture set to publish for: the JAR's declared native arches
     * (non-portable) sorted, or the portable default ({@code amd64}+{@code arm64}).
     */
    public static List<String> targetArches(JvmConfig cfg) {
        if (cfg.getArch() != null && !cfg.getArch().isEmpty()) {
            List<String> out = new ArrayList<>(cfg.getArch());
            Collections.sort(out);
            return out;
        }
        return DEFAULT_ARCHES;
    }

    private record Layer(OciDescriptor desc, String diffId) {}

    /**
     * Assembles the runnable image.
     *
     * @param cfg              the launch config (carried in the jvm-config annotation)
     * @param jarPath          the primary application JAR
     * @param depLayers        class-path / module-path dependency layers (may be
     *                         empty); each is mapped to a Brewlet role by its
     *                         {@link ArtifactLayer#mediaType()}. Must NOT contain a
     *                         CDS layer — pass the CDS archive via {@code cdsArchive}.
     * @param cdsArchive       optional AppCDS {@code .jsa} folded into the app layer, or null
     * @param indexAnnotations optional annotations for the image index (provenance), or null
     */
    public static Result build(JvmConfig cfg, Path jarPath, List<ArtifactLayer> depLayers,
                               Path cdsArchive, Map<String, String> indexAnnotations)
            throws IOException {
        ObjectMapper pretty = new ObjectMapper().enable(SerializationFeature.INDENT_OUTPUT);
        ObjectMapper compact = new ObjectMapper();

        Result result = new Result();
        List<Layer> layers = new ArrayList<>();

        // Layer 0 (role=app): flat tar of the primary JAR (+ optional CDS archive).
        String mainJar = (cfg.getMainJar() != null && !cfg.getMainJar().isBlank())
                ? cfg.getMainJar()
                : jarPath.getFileName().toString();
        TarWriter appTar = new TarWriter();
        appTar.addFile(mainJar, Files.readAllBytes(jarPath));
        if (cdsArchive != null) {
            String name = cdsArchive.getFileName().toString();
            if (cfg.getCds() != null && cfg.getCds().getArchive() != null
                    && !cfg.getCds().getArchive().isBlank()) {
                name = cfg.getCds().getArchive();
            }
            appTar.addFile(name, Files.readAllBytes(cdsArchive));
        }
        layers.add(buildLayer(appTar.toByteArray(), MediaTypes.LAYER_ROLE_APP, mainJar, result.blobs));

        // Dependency layers: the SAME flat-JAR tars a native artifact ships, just
        // gzip-compressed and role-tagged so the shim stages them unchanged.
        if (depLayers != null) {
            for (ArtifactLayer l : depLayers) {
                String role = MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE.equals(l.mediaType())
                        ? MediaTypes.LAYER_ROLE_MODULEPATH
                        : MediaTypes.LAYER_ROLE_CLASSPATH;
                layers.add(buildLayer(l.tar(), role, l.name(), result.blobs));
            }
        }

        String jvmConfigJson = compact.writeValueAsString(cfg);

        List<String> diffIds = new ArrayList<>(layers.size());
        List<OciDescriptor> layerDescs = new ArrayList<>(layers.size());
        for (Layer l : layers) {
            diffIds.add(l.diffId());
            layerDescs.add(l.desc());
        }

        result.arches = targetArches(cfg);
        List<OciDescriptor> indexManifests = new ArrayList<>(result.arches.size());
        for (String arch : result.arches) {
            // Per-arch image config blob (differs only in architecture).
            OciImageConfig imgCfg = new OciImageConfig(arch,
                    new OciImageConfig.RunConfig(Map.of("sh.brewlet.runnable", "true")),
                    new OciImageConfig.RootFs(diffIds));
            byte[] cfgBytes = pretty.writeValueAsBytes(imgCfg);
            String cfgDigest = sha256(cfgBytes);
            result.blobs.add(new Blob(cfgDigest, cfgBytes, MediaTypes.OCI_IMAGE_CONFIG_MEDIA_TYPE));
            OciDescriptor cfgDesc = new OciDescriptor(
                    MediaTypes.OCI_IMAGE_CONFIG_MEDIA_TYPE, cfgDigest, cfgBytes.length);

            // Per-arch image manifest carrying the launch config annotation.
            OciManifest man = new OciManifest();
            man.setConfig(cfgDesc);
            man.setLayers(layerDescs);
            man.setAnnotations(Map.of(MediaTypes.JVM_CONFIG_ANNOTATION, jvmConfigJson));
            byte[] manBytes = pretty.writeValueAsBytes(man);
            String manDigest = sha256(manBytes);
            result.manifests.add(new Blob(manDigest, manBytes, MediaTypes.OCI_MANIFEST_MEDIA_TYPE));

            OciDescriptor manDesc = new OciDescriptor(
                    MediaTypes.OCI_MANIFEST_MEDIA_TYPE, manDigest, manBytes.length);
            manDesc.setPlatform(new Platform("linux", arch));
            indexManifests.add(manDesc);
        }

        OciIndex idx = new OciIndex();
        idx.setManifests(indexManifests);
        if (indexAnnotations != null && !indexAnnotations.isEmpty()) {
            idx.setAnnotations(new LinkedHashMap<>(indexAnnotations));
        }
        result.indexBytes = pretty.writeValueAsBytes(idx);
        result.indexDigest = sha256(result.indexBytes);
        return result;
    }

    /**
     * gzip-compresses {@code uncompressedTar}, records the compressed blob, and
     * returns a descriptor whose digest is over the compressed bytes plus the
     * uncompressed diffID the image config's {@code rootfs.diff_ids} needs.
     */
    private static Layer buildLayer(byte[] uncompressedTar, String role, String title,
                                    List<Blob> outBlobs) throws IOException {
        String diffId = sha256(uncompressedTar);
        byte[] gz = gzip(uncompressedTar);
        String digest = sha256(gz);
        OciDescriptor desc = new OciDescriptor(MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE, digest, gz.length);
        Map<String, String> ann = new LinkedHashMap<>();
        ann.put(MediaTypes.LAYER_ROLE_ANNOTATION, role);
        if (title != null && !title.isBlank()) {
            ann.put(MediaTypes.ANNOTATION_TITLE, title);
        }
        desc.setAnnotations(ann);
        outBlobs.add(new Blob(digest, gz, MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE));
        return new Layer(desc, diffId);
    }

    private static byte[] gzip(byte[] data) throws IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        try (GZIPOutputStream gz = new GZIPOutputStream(bos)) {
            gz.write(data);
        }
        return bos.toByteArray();
    }

    private static String sha256(byte[] data) {
        return LocalStore.sha256Hex(data);
    }
}
