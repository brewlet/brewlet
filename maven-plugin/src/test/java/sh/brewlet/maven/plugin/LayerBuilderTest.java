package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Verifies {@link LayerBuilder} splits the dependency tree into ordered,
 * reproducible classpath layers.
 */
class LayerBuilderTest {

    @TempDir
    Path tmp;

    private LayerBuilder.Dep dep(String name, boolean snapshot) throws IOException {
        Path p = tmp.resolve(name);
        Files.write(p, ("jar-bytes-of-" + name).getBytes());
        return new LayerBuilder.Dep(name, p, snapshot);
    }

    @Test
    void emptyDepsProduceNoLayers() throws IOException {
        assertTrue(LayerBuilder.build(List.of(), true).isEmpty());
        assertTrue(LayerBuilder.build(null, true).isEmpty());
    }

    @Test
    void singleLayerWhenSplitDisabled() throws IOException {
        List<ArtifactLayer> layers = LayerBuilder.build(
                List.of(dep("a-1.0.jar", false), dep("b-2.0-SNAPSHOT.jar", true)),
                false);

        assertEquals(1, layers.size());
        assertEquals("deps", layers.get(0).name());
    }

    @Test
    void splitsReleasedAndSnapshotDepsInStableOrder() throws IOException {
        List<ArtifactLayer> layers = LayerBuilder.build(
                List.of(
                        dep("b-2.0-SNAPSHOT.jar", true),
                        dep("a-1.0.jar", false),
                        dep("c-3.0-SNAPSHOT.jar", true)),
                true);

        assertEquals(2, layers.size());
        // Stable (released) layer must come first, volatile snapshots second.
        assertEquals("deps", layers.get(0).name());
        assertEquals("snapshot-deps", layers.get(1).name());
    }

    @Test
    void onlySnapshotsProduceSingleSnapshotLayer() throws IOException {
        List<ArtifactLayer> layers = LayerBuilder.build(
                List.of(dep("only-1.0-SNAPSHOT.jar", true)), true);

        assertEquals(1, layers.size());
        assertEquals("snapshot-deps", layers.get(0).name());
    }

    @Test
    void layerTarIsReproducibleRegardlessOfInputOrder() throws IOException {
        LayerBuilder.Dep a = dep("a-1.0.jar", false);
        LayerBuilder.Dep b = dep("b-1.0.jar", false);

        byte[] first = LayerBuilder.build(List.of(a, b), false).get(0).tar();
        byte[] second = LayerBuilder.build(List.of(b, a), false).get(0).tar();

        assertArrayEquals(first, second,
                "the same dependency set must yield an identical layer tar (stable digest)");
    }

    @Test
    void moduleBuildEmptyDepsProduceNoLayers() throws IOException {
        assertTrue(LayerBuilder.buildModule(List.of()).isEmpty());
        assertTrue(LayerBuilder.buildModule(null).isEmpty());
    }

    @Test
    void moduleBuildPacksAllDepsIntoSingleModsLayer() throws IOException {
        List<ArtifactLayer> layers = LayerBuilder.buildModule(
                List.of(
                        dep("greeter-1.0.jar", false),
                        dep("audit-2.0-SNAPSHOT.jar", true)));

        // The module path is a set, so released and snapshot modules share one layer.
        assertEquals(1, layers.size());
        assertEquals("mods", layers.get(0).name());
        assertEquals(MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE, layers.get(0).mediaType());
    }

    @Test
    void moduleLayerTarIsReproducibleRegardlessOfInputOrder() throws IOException {
        LayerBuilder.Dep a = dep("greeter-1.0.jar", false);
        LayerBuilder.Dep b = dep("audit-1.0.jar", false);

        byte[] first = LayerBuilder.buildModule(List.of(a, b)).get(0).tar();
        byte[] second = LayerBuilder.buildModule(List.of(b, a)).get(0).tar();

        assertArrayEquals(first, second,
                "the same module set must yield an identical layer tar (stable digest)");
    }

    @Test
    void classpathLayerDefaultsToClasspathMediaType() throws IOException {
        List<ArtifactLayer> layers = LayerBuilder.build(List.of(dep("a-1.0.jar", false)), false);
        assertEquals(MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE, layers.get(0).mediaType());
    }
}
