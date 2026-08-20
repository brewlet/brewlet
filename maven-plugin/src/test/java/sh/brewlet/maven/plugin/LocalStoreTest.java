package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciDescriptor;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Tests the local OCI image-layout store: verifies that blobs, index.json, and
 * oci-layout are written correctly, matching the same structure produced by the
 * Go store in {@code src/internal/artifact/artifact.go}.
 */
class LocalStoreTest {

    @TempDir
    Path tempDir;

    @Test
    void push_writesOciLayoutMarker() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        store.push("demo/hello:1.0.0", cfg, jar, null);

        Path ociLayout = tempDir.resolve("oci").resolve("oci-layout");
        assertTrue(Files.exists(ociLayout), "oci-layout file must exist");
        String content = Files.readString(ociLayout);
        assertTrue(content.contains("1.0.0"), "oci-layout must contain imageLayoutVersion 1.0.0");
    }

    @Test
    void push_writesIndexJson() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        store.push("demo/hello:1.0.0", cfg, jar, null);

        Path indexFile = tempDir.resolve("oci").resolve("index.json");
        assertTrue(Files.exists(indexFile), "index.json must exist");
        String content = Files.readString(indexFile);
        assertTrue(content.contains("demo/hello:1.0.0"), "index.json must contain the ref");
    }

    @Test
    void push_writesBlobsUnderSha256Directory() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        store.push("demo/hello:1.0.0", cfg, jar, null);

        Path blobsDir = tempDir.resolve("oci").resolve("blobs").resolve("sha256");
        assertTrue(Files.exists(blobsDir), "blobs/sha256 directory must exist");
        long blobCount = Files.list(blobsDir).count();
        // Expect at least 3 blobs: config, JAR layer, manifest
        assertTrue(blobCount >= 3, "Expected at least 3 blobs, found " + blobCount);
    }

    @Test
    void push_manifestDescriptorHasCorrectMediaTypes() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        OciDescriptor desc = store.push("demo/hello:1.0.0", cfg, jar, null);

        assertEquals(MediaTypes.OCI_MANIFEST_MEDIA_TYPE, desc.getMediaType());
        assertEquals(MediaTypes.ARTIFACT_TYPE, desc.getArtifactType());
    }

    @Test
    void push_upsertReplacesExistingRefInIndex() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        store.push("demo/hello:1.0.0", cfg, jar, null);
        store.push("demo/hello:1.0.0", cfg, jar, null); // push same ref again

        String indexContent = Files.readString(tempDir.resolve("oci").resolve("index.json"));
        // Count occurrences of the ref — should be exactly 1
        long count = indexContent.chars()
                .filter(c -> c == '{')
                .count();
        // The index manifests array should have exactly 1 entry for the ref
        assertTrue(count >= 1);

        // Verify the ref appears only once
        int firstIdx = indexContent.indexOf("demo/hello:1.0.0");
        int lastIdx = indexContent.lastIndexOf("demo/hello:1.0.0");
        assertEquals(firstIdx, lastIdx, "Ref should appear exactly once in index.json");
    }

    @Test
    void sha256Hex_producesCorrectFormat() {
        String digest = LocalStore.sha256Hex("hello world".getBytes());
        assertTrue(digest.startsWith("sha256:"), "Digest must start with 'sha256:'");
        assertEquals(64 + 7, digest.length(), "sha256: prefix (7) + 64 hex chars");
    }

    @Test
    void push_withArtifactLayers_writesMultiLayerManifest() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        List<sh.brewlet.maven.plugin.oci.ArtifactLayer> layers = List.of(
                new sh.brewlet.maven.plugin.oci.ArtifactLayer("deps", "deps-tar".getBytes()),
                new sh.brewlet.maven.plugin.oci.ArtifactLayer("snapshot-deps", "snap-tar".getBytes()));

        OciDescriptor manifestDesc =
                store.push("demo/hello:1.0.0", cfg, jar, layers, null);

        // Read the manifest blob back and assert its layers.
        Path manifestBlob = store.blobPath(manifestDesc.getDigest());
        String manifest = Files.readString(manifestBlob);

        // One JAR layer + two classpath layers = three layer descriptors.
        long jarLayers = countOccurrences(manifest, MediaTypes.JAR_LAYER_MEDIA_TYPE);
        long cpLayers = countOccurrences(manifest, MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE);
        assertEquals(1, jarLayers, "exactly one jar layer");
        assertEquals(2, cpLayers, "two classpath layers");
        assertTrue(manifest.contains("deps"), "layer titles recorded");
    }

    @Test
    void push_withModuleLayer_tagsModulepathMediaType() throws IOException {
        LocalStore store = new LocalStore(tempDir.resolve("oci"));
        JvmConfig cfg = sampleConfig();
        Path jar = createFakeJar();

        List<sh.brewlet.maven.plugin.oci.ArtifactLayer> layers = List.of(
                new sh.brewlet.maven.plugin.oci.ArtifactLayer("mods", "mods-tar".getBytes(),
                        MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE));

        OciDescriptor manifestDesc =
                store.push("demo/orders:1.0.0", cfg, jar, layers, null);

        Path manifestBlob = store.blobPath(manifestDesc.getDigest());
        String manifest = Files.readString(manifestBlob);

        // The module layer must be tagged as a modulepath layer, not a classpath one.
        assertEquals(1, countOccurrences(manifest, MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE),
                "one modulepath layer");
        assertEquals(0, countOccurrences(manifest, MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE),
                "no classpath layer for a modular app");
        assertTrue(manifest.contains("mods"), "module layer title recorded");
    }

    private static long countOccurrences(String haystack, String needle) {
        long count = 0;
        int idx = 0;
        while ((idx = haystack.indexOf(needle, idx)) != -1) {
            count++;
            idx += needle.length();
        }
        return count;
    }

    // -----------------------------------------------------------------------

    private static JvmConfig sampleConfig() {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));
        cfg.setAddOpens(List.of("java.base/java.lang=ALL-UNNAMED"));
        return cfg;
    }

    private Path createFakeJar() throws IOException {
        Path jar = tempDir.resolve("app.jar");
        Files.write(jar, "fake jar content".getBytes());
        return jar;
    }
}
