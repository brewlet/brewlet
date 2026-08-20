package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciDescriptor;
import sh.brewlet.maven.plugin.oci.RunnableImageBuilder;
import sh.brewlet.maven.plugin.util.TarWriter;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.zip.GZIPInputStream;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Verifies {@link RunnableImageBuilder} assembles a STANDARD, kubelet-pullable
 * OCI image. This is the Java twin of the Go
 * {@code TestPushRunnableImageIsKubeletPullable} in
 * {@code src/internal/artifact/image_test.go}; the two MUST assert the same
 * contract so a JAR pushed by the Maven plugin behaves exactly like one pushed
 * by the CLI.
 */
class RunnableImageBuilderTest {

    @TempDir
    Path tmp;

    private static final ObjectMapper MAPPER = new ObjectMapper();

    /** Reads a tar+gzip layer blob into an entry name -> content map. */
    private static Map<String, String> gunzipTar(byte[] gz) throws IOException {
        Map<String, String> out = new HashMap<>();
        try (GZIPInputStream gr = new GZIPInputStream(new ByteArrayInputStream(gz))) {
            byte[] raw = gr.readAllBytes();
            // Minimal USTAR reader mirroring TarWriter's 512-byte block layout.
            int pos = 0;
            while (pos + 512 <= raw.length) {
                // A zero name byte marks the end-of-archive padding blocks.
                if (raw[pos] == 0) {
                    break;
                }
                int nameEnd = pos;
                while (nameEnd < pos + 100 && raw[nameEnd] != 0) {
                    nameEnd++;
                }
                String name = new String(raw, pos, nameEnd - pos);
                long size = parseOctal(raw, pos + 124, 12);
                int dataStart = pos + 512;
                String content = new String(raw, dataStart, (int) size);
                out.put(name, content);
                long blocks = (size + 511) / 512;
                pos = dataStart + (int) (blocks * 512);
            }
        }
        return out;
    }

    private static long parseOctal(byte[] b, int off, int len) {
        long v = 0;
        for (int i = off; i < off + len; i++) {
            int c = b[i] & 0xff;
            if (c == 0 || c == ' ') {
                continue;
            }
            v = (v << 3) + (c - '0');
        }
        return v;
    }

    private byte[] depTar(Map<String, String> files) throws IOException {
        TarWriter w = new TarWriter();
        for (Map.Entry<String, String> e : files.entrySet()) {
            w.addFile(e.getKey(), e.getValue().getBytes());
        }
        return w.toByteArray();
    }

    @Test
    void buildsMultiArchKubeletPullableImage() throws IOException {
        Path jar = tmp.resolve("orders.jar");
        Files.write(jar, "PK\u0003\u0004 orders-fat-jar".getBytes());

        JvmConfig cfg = new JvmConfig();
        cfg.setSchemaVersion(1);
        cfg.setMainJar("orders.jar");
        Entry entry = new Entry("classpath", List.of("orders.jar", "lib/*"));
        entry.setMainClass("com.acme.Main");
        cfg.setEntry(entry);
        cfg.setSystemProperties(Map.of("spring.aot.enabled", "true"));

        List<ArtifactLayer> deps = List.of(new ArtifactLayer("deps",
                depTar(Map.of("spring-core.jar", "aaa", "jackson.jar", "bbb")),
                MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE));

        RunnableImageBuilder.Result r = RunnableImageBuilder.build(cfg, jar, deps, null, null);

        // Portable (pure-bytecode) JAR -> multi-arch index of amd64 + arm64.
        assertEquals(List.of("amd64", "arm64"), r.arches);
        assertEquals(2, r.manifests.size());

        LocalStore store = new LocalStore(tmp.resolve("oci"));
        OciDescriptor localIndex = store.pushRunnableImage("demo/orders:1.0.0", r);
        assertEquals(r.indexDigest, localIndex.getDigest());
        JsonNode layoutIndex = MAPPER.readTree(tmp.resolve("oci/index.json").toFile());
        assertEquals("demo/orders:1.0.0",
                layoutIndex.get("manifests").get(0).get("annotations")
                        .get(MediaTypes.ANNOTATION_REF_NAME).asText());

        JsonNode idx = MAPPER.readTree(r.indexBytes);
        assertEquals(MediaTypes.OCI_INDEX_MEDIA_TYPE, idx.get("mediaType").asText());
        JsonNode idxManifests = idx.get("manifests");
        assertEquals(2, idxManifests.size());
        Set<String> arches = new LinkedHashSet<>();
        for (JsonNode m : idxManifests) {
            assertEquals("linux", m.get("platform").get("os").asText());
            arches.add(m.get("platform").get("architecture").asText());
        }
        assertEquals(Set.of("amd64", "arm64"), arches);

        // Resolve one per-arch manifest and verify the runnable-image contract.
        JsonNode man = MAPPER.readTree(r.manifests.get(0).data());
        // A runnable image is a real image: standard OCI config, no artifactType.
        assertTrue(man.get("artifactType") == null || man.get("artifactType").isNull());
        assertEquals(MediaTypes.OCI_IMAGE_CONFIG_MEDIA_TYPE,
                man.get("config").get("mediaType").asText());

        // The launch config round-trips from the jvm-config annotation.
        String jvmJson = man.get("annotations").get(MediaTypes.JVM_CONFIG_ANNOTATION).asText();
        JvmConfig got = MAPPER.readValue(jvmJson, JvmConfig.class);
        assertEquals("orders.jar", got.getMainJar());
        assertEquals("com.acme.Main", got.getEntry().getMainClass());
        assertEquals("true", got.getSystemProperties().get("spring.aot.enabled"));

        // Every layer is a STANDARD tar+gzip layer with a Brewlet role annotation.
        JsonNode layers = man.get("layers");
        assertEquals(2, layers.size());
        for (JsonNode l : layers) {
            assertEquals(MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE, l.get("mediaType").asText());
            assertNotNull(l.get("annotations").get(MediaTypes.LAYER_ROLE_ANNOTATION));
        }
        assertEquals(MediaTypes.LAYER_ROLE_APP,
                layers.get(0).get("annotations").get(MediaTypes.LAYER_ROLE_ANNOTATION).asText());
        assertEquals(MediaTypes.LAYER_ROLE_CLASSPATH,
                layers.get(1).get("annotations").get(MediaTypes.LAYER_ROLE_ANNOTATION).asText());

        // The image config's rootfs.diff_ids MUST equal the sha256 of each layer's
        // UNCOMPRESSED tar, or containerd rejects the image on unpack.
        String cfgDigest = man.get("config").get("digest").asText();
        byte[] cfgBytes = blobByDigest(r, cfgDigest);
        JsonNode imgCfg = MAPPER.readTree(cfgBytes);
        assertEquals("/brewlet", imgCfg.get("config").get("Entrypoint").get(0).asText());
        JsonNode diffIds = imgCfg.get("rootfs").get("diff_ids");
        assertEquals(layers.size(), diffIds.size());
        for (int i = 0; i < layers.size(); i++) {
            byte[] gz = blobByDigest(r, layers.get(i).get("digest").asText());
            byte[] raw;
            try (GZIPInputStream gr = new GZIPInputStream(new ByteArrayInputStream(gz))) {
                raw = gr.readAllBytes();
            }
            assertEquals(LocalStore.sha256Hex(raw), diffIds.get(i).asText(),
                    "layer[" + i + "] diff_id must be the uncompressed tar digest");
        }

        // The app layer carries the JAR flat under its MainJar name; the classpath
        // layer carries the dependency JARs.
        byte[] appGz = blobByDigest(r, layers.get(0).get("digest").asText());
        Map<String, String> appFiles = gunzipTar(appGz);
        assertEquals("PK\u0003\u0004 orders-fat-jar", appFiles.get("orders.jar"));

        byte[] cpGz = blobByDigest(r, layers.get(1).get("digest").asText());
        Map<String, String> cpFiles = gunzipTar(cpGz);
        assertEquals("aaa", cpFiles.get("spring-core.jar"));
        assertEquals("bbb", cpFiles.get("jackson.jar"));
    }

    @Test
    void foldsCdsArchiveIntoAppLayer() throws IOException {
        Path jar = tmp.resolve("app.jar");
        Files.write(jar, "PK\u0003\u0004 app".getBytes());
        Path cds = tmp.resolve("app.jsa");
        Files.write(cds, "cds-archive-bytes".getBytes());

        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));

        RunnableImageBuilder.Result r = RunnableImageBuilder.build(cfg, jar, null, cds, null);

        // No dependency layers -> a single (app) layer that also carries the CDS.
        JsonNode man = MAPPER.readTree(r.manifests.get(0).data());
        JsonNode layers = man.get("layers");
        assertEquals(1, layers.size());
        byte[] appGz = blobByDigest(r, layers.get(0).get("digest").asText());
        Map<String, String> files = gunzipTar(appGz);
        assertEquals("PK\u0003\u0004 app", files.get("app.jar"));
        assertEquals("cds-archive-bytes", files.get("app.jsa"));
    }

    @Test
    void targetArchesDefaultsToPortablePair() {
        JvmConfig cfg = new JvmConfig();
        assertEquals(List.of("amd64", "arm64"), RunnableImageBuilder.targetArches(cfg));
    }

    @Test
    void targetArchesHonorsExplicitSortedArch() {
        JvmConfig cfg = new JvmConfig();
        cfg.setArch(new ArrayList<>(List.of("arm64")));
        assertEquals(List.of("arm64"), RunnableImageBuilder.targetArches(cfg));

        JvmConfig cfg2 = new JvmConfig();
        cfg2.setArch(new ArrayList<>(List.of("s390x", "amd64")));
        assertEquals(List.of("amd64", "s390x"), RunnableImageBuilder.targetArches(cfg2));
    }

    @Test
    void modulepathLayerIsRoleTagged() throws IOException {
        Path jar = tmp.resolve("mod.jar");
        Files.write(jar, "PK\u0003\u0004 mod".getBytes());

        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("mod.jar");
        cfg.setEntry(new Entry("module"));

        List<ArtifactLayer> mods = List.of(new ArtifactLayer("mods",
                depTar(Map.of("greeter.jar", "ggg")),
                MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE));

        RunnableImageBuilder.Result r = RunnableImageBuilder.build(cfg, jar, mods, null, null);

        JsonNode layers = MAPPER.readTree(r.manifests.get(0).data()).get("layers");
        assertEquals(MediaTypes.LAYER_ROLE_MODULEPATH,
                layers.get(1).get("annotations").get(MediaTypes.LAYER_ROLE_ANNOTATION).asText());
    }

    /** Finds a pushed blob (layer or config) by its digest. */
    private static byte[] blobByDigest(RunnableImageBuilder.Result r, String digest) {
        for (RunnableImageBuilder.Blob b : r.blobs) {
            if (b.digest().equals(digest)) {
                return b.data();
            }
        }
        throw new AssertionError("no blob with digest " + digest);
    }
}
