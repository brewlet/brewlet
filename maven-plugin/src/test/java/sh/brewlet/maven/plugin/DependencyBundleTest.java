package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.DependencyBundleConfig;
import sh.brewlet.maven.plugin.model.DependencyLock;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.model.ManagedDependencyEvidence;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciDescriptor;
import sh.brewlet.maven.plugin.oci.RunnableImageBuilder;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

class DependencyBundleTest {
    private static final ObjectMapper MAPPER = new ObjectMapper();

    @TempDir
    Path temp;

    @Test
    void bundleIsDeterministicAndRoundTripsThroughOciLayout() throws IOException {
        Path a = temp.resolve("a.jar");
        Path b = temp.resolve("b.jar");
        Files.writeString(a, "a");
        Files.writeString(b, "b");
        ArtifactLayer firstLayer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("b.jar", b, false),
                new LayerBuilder.Dep("a.jar", a, false)));
        ArtifactLayer secondLayer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("a.jar", a, false),
                new LayerBuilder.Dep("b.jar", b, false)));

        DependencyLock lock = lock("a", "b");
        DependencyBundleConfig cfg = config();
        DependencyBundle.Content first = DependencyBundle.build(cfg, lock, firstLayer);
        DependencyBundle.Content second = DependencyBundle.build(config(), lock("a", "b"), secondLayer);
        assertEquals(first.manifestDigest(), second.manifestDigest());
        assertArrayEquals(firstLayer.tar(), secondLayer.tar());
        JsonNode manifest = MAPPER.readTree(first.manifestBytes());
        JsonNode bundleLayer = manifest.get("layers").get(0);
        assertEquals(MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE,
                bundleLayer.get("mediaType").asText());
        assertEquals(MediaTypes.LAYER_ROLE_CLASSPATH,
                bundleLayer.get("annotations").get(
                        MediaTypes.LAYER_ROLE_ANNOTATION).asText());
        assertEquals(first.config().getLayerDigest(), bundleLayer.get("digest").asText());
        assertEquals(LocalStore.sha256Hex(firstLayer.tar()), first.config().getLayerDiffId());
        assertTrue(MAPPER.readTree(first.configBytes()).has("layerDiffId"));
        JsonNode lockJson = MAPPER.readTree(first.lockBytes());
        assertTrue(lockJson.has("artifacts"));
        assertTrue(lockJson.get("artifacts").get(0).has("fileName"));
        assertFalse(lockJson.get("artifacts").get(0).has("classifier"));

        LocalStore store = new LocalStore(temp.resolve("oci"));
        OciDescriptor descriptor = store.pushDependencyBundle("example/deps:1", first);
        assertEquals(MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE, descriptor.getArtifactType());

        DependencyBundle.Content loaded = DependencyBundle.loadLayout(temp.resolve("oci"));
        assertEquals(first.manifestDigest(), loaded.manifestDigest());
        assertEquals(lock.getDependencies(), loaded.lock().getDependencies());
        assertEquals(first.config().getLockDigest(), loaded.config().getLockDigest());
        assertArrayEquals(firstLayer.tar(), loaded.uncompressedLayer());
    }

    @Test
    void runnableImageReusesCompressedBundleLayerAndDiffId() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content bundle = DependencyBundle.build(
                config(), lock("a"), LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        Path jar = temp.resolve("app.jar");
        Files.writeString(jar, "thin");
        JvmConfig jvm = new JvmConfig();
        jvm.setMainJar("app.jar");
        Entry entry = new Entry("classpath", List.of("app.jar", "lib/*"));
        entry.setMainClass("com.acme.Main");
        jvm.setEntry(entry);
        String evidence = DependencyBundle.canonicalJson(new ManagedDependencyEvidence(
                true, "sha256:app", bundle.manifestDigest(),
                bundle.config().getLayerDigest(), bundle.config().getLockDigest(), "g:b:1"));

        RunnableImageBuilder.Result result =
                RunnableImageBuilder.buildWithManagedDependencyLayer(
                        jvm, jar, new RunnableImageBuilder.ManagedDependencyLayer(
                                bundle.compressedLayer(), bundle.config().getLayerDigest(),
                                bundle.config().getLayerDiffId(), "dependencies"), null,
                        java.util.Map.of(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION,
                                evidence));
        JsonNode manifest = MAPPER.readTree(result.manifests.get(0).data());
        assertEquals(evidence, manifest.get("annotations")
                .get(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION).asText());
        JsonNode reused = manifest.get("layers").get(1);
        assertEquals(bundle.config().getLayerDigest(), reused.get("digest").asText());
        String configDigest = manifest.get("config").get("digest").asText();
        RunnableImageBuilder.Blob imageConfig = result.blobs.stream()
                .filter(blob -> blob.digest().equals(configDigest))
                .findFirst().orElseThrow();
        JsonNode rootfs = MAPPER.readTree(imageConfig.data()).get("rootfs");
        assertEquals(bundle.config().getLayerDiffId(), rootfs.get("diff_ids").get(1).asText());
    }

    @Test
    void graphComparisonIncludesHashesAndCoordinates() throws IOException {
        DependencyLock changed = lock();
        changed.setDependencies(List.of(new DependencyLock.Entry(
                "g", "a", "1", "jar", null, "runtime", "a.jar", "sha256:changed")));
        IOException error = assertThrows(IOException.class,
                () -> DependencyBundle.verifyGraph(lock("a"), changed));
        assertTrue(error.getMessage().contains("does not match"));
    }

    @Test
    void graphComparisonRejectsScopeOnlyDifference() {
        DependencyLock changed = lock();
        changed.setDependencies(List.of(new DependencyLock.Entry(
                "g", "a", "1", "jar", null, "compile", "a.jar", "sha256:a")));
        assertThrows(IOException.class,
                () -> DependencyBundle.verifyGraph(lock("a"), changed));
    }

    @Test
    void bundleRejectsNonEmptyUstarPrefix() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        ArtifactLayer layer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("a.jar", dependency, false)));
        byte[] prefixedTar = layer.tar().clone();
        prefixedTar[345] = 'x';

        assertThrows(IOException.class, () -> DependencyBundle.build(
                config(), lock("a"), new ArtifactLayer(
                        layer.name(), prefixedTar, layer.mediaType())));
    }

    @Test
    void layoutLoadingRejectsTamperedBlob() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        LocalStore store = new LocalStore(temp.resolve("tampered"));
        store.pushDependencyBundle("example/deps:1", content);
        Path lockBlob = store.blobPath(content.config().getLockDigest());
        Files.writeString(lockBlob, "tampered");
        IOException error = assertThrows(IOException.class,
                () -> DependencyBundle.loadLayout(temp.resolve("tampered")));
        assertTrue(error.getMessage().contains("SHA-256 mismatch"));
    }

    @Test
    void parsingRejectsNonContractManifest() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));

        ObjectNode wrongVersion = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        wrongVersion.put("schemaVersion", 3);
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(wrongVersion), digest -> new byte[0]));

        ObjectNode missingVersion = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        missingVersion.remove("schemaVersion");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(missingVersion), digest -> new byte[0]));

        ObjectNode wrongMediaType = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        wrongMediaType.put("mediaType", "application/json");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(wrongMediaType), digest -> new byte[0]));

        ObjectNode missingMediaType = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        missingMediaType.remove("mediaType");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(missingMediaType), digest -> new byte[0]));

        ObjectNode unknownField = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        unknownField.put("unexpected", true);
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(unknownField), digest -> new byte[0]));

        ObjectNode subject = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        subject.putObject("subject")
                .put("mediaType", MediaTypes.OCI_MANIFEST_MEDIA_TYPE)
                .put("digest", "sha256:" + "a".repeat(64))
                .put("size", 1);
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(subject), digest -> new byte[0]));

        ObjectNode annotations = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        annotations.putObject("annotations").put("example", "value");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(annotations), digest -> new byte[0]));
    }

    @Test
    void parsingRejectsDuplicateCompatibleJdks() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        ObjectNode config = (ObjectNode) MAPPER.readTree(content.configBytes());
        config.putArray("compatibleJdks").add(21).add(21);
        byte[] configBytes = MAPPER.writeValueAsBytes(config);
        String configDigest = LocalStore.sha256Hex(configBytes);

        ObjectNode manifest = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        ((ObjectNode) manifest.path("config"))
                .put("digest", configDigest)
                .put("size", configBytes.length);
        byte[] manifestBytes = MAPPER.writeValueAsBytes(manifest);

        assertThrows(IOException.class, () -> DependencyBundle.parse(manifestBytes, digest -> {
            if (digest.equals(configDigest)) {
                return configBytes;
            }
            if (digest.equals(content.config().getLockDigest())) {
                return content.lockBytes();
            }
            if (digest.equals(content.config().getLayerDigest())) {
                return content.compressedLayer();
            }
            throw new IOException("unexpected digest " + digest);
        }));
    }

    @Test
    void parsingRejectsLegacyUnsignedPolicyField() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        ObjectNode config = (ObjectNode) MAPPER.readTree(content.configBytes());
        config.put("allowUnsigned", "true");
        byte[] configBytes = MAPPER.writeValueAsBytes(config);
        String configDigest = LocalStore.sha256Hex(configBytes);
        ObjectNode manifest = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        ((ObjectNode) manifest.path("config"))
                .put("digest", configDigest)
                .put("size", configBytes.length);

        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(manifest), digest -> {
                    if (digest.equals(configDigest)) {
                        return configBytes;
                    }
                    if (digest.equals(content.config().getLockDigest())) {
                        return content.lockBytes();
                    }
                    return content.compressedLayer();
                }));
    }

    @Test
    void sourceBomRequiresGav() {
        assertDoesNotThrow(() -> DependencyBundleMojo.validateSourceBom("com.acme:platform:1.2.3"));
        assertThrows(MojoExecutionException.class,
                () -> DependencyBundleMojo.validateSourceBom("com.acme:platform"));
        assertThrows(MojoExecutionException.class,
                () -> DependencyBundleMojo.validateSourceBom(null));
    }

    @Test
    void evidenceIsCanonicalAndContainsOnlyInformationalFields() throws IOException {
        String json = DependencyBundle.canonicalJson(new ManagedDependencyEvidence(
                true, "sha256:app", "sha256:bundle", "sha256:layer",
                "sha256:lock", "g:b:1"));
        JsonNode node = MAPPER.readTree(json);
        assertEquals(1, node.get("schemaVersion").asInt());
        assertTrue(node.get("thinJar").asBoolean());
        assertEquals("sha256:bundle", node.get("dependencyBundleDigest").asText());
        assertEquals("sha256:lock", node.get("dependencyLockDigest").asText());
    }

    private static DependencyBundleConfig config() {
        DependencyBundleConfig config = new DependencyBundleConfig();
        config.setName("platform");
        config.setVersion("1");
        config.setSourceBom("g:b:1");
        config.setCompatibleJdks(List.of(21, 17, 17));
        return config;
    }

    private static DependencyLock lock(String... names) {
        DependencyLock lock = new DependencyLock();
        lock.setDependencies(java.util.Arrays.stream(names)
                .map(name -> new DependencyLock.Entry(
                        "g", name, "1", "jar", null, "runtime", name + ".jar",
                        name.equals("a")
                                ? LocalStore.sha256Hex("a".getBytes()).substring(7)
                                : LocalStore.sha256Hex("b".getBytes()).substring(7)))
                .toList());
        return lock;
    }
}
