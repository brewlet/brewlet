package sh.brewlet.maven.plugin.util;

import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.MediaTypes;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;

/**
 * Builds ordered, reproducible classpath (dependency) layers from a project's
 * resolved dependency tree. Implements the artifact side of
 * https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md:
 * instead of shipping one opaque
 * fat JAR, the dependencies are packed into their own OCI layer(s) — split
 * stable&nbsp;&rarr;&nbsp;volatile so a code-only rebuild re-pushes just the thin
 * app JAR and the heavy dependency layers dedup by digest.
 *
 * <p>Split strategy (mirrors Spring Boot's {@code layers.idx} ordering):
 * <ul>
 *   <li>{@code deps} — released third-party dependencies (rarely change)</li>
 *   <li>{@code snapshot-deps} — {@code -SNAPSHOT} / internal libs (change more often)</li>
 * </ul>
 *
 * <p>Each tar contains the dependency JARs as flat, bare-named regular files;
 * the shim unpacks them into {@code /app/lib}, reached from the launch config's
 * {@code entry.classPath} wildcard entry {@code "lib/*"}.
 */
public final class LayerBuilder {

    private LayerBuilder() {}

    /** A resolved dependency to be packed into a classpath layer. */
    public static final class Dep {
        private final String fileName;
        private final Path path;
        private final boolean snapshot;

        public Dep(String fileName, Path path, boolean snapshot) {
            this.fileName = fileName;
            this.path = path;
            this.snapshot = snapshot;
        }

        public String fileName() { return fileName; }
        public Path path() { return path; }
        public boolean snapshot() { return snapshot; }
    }

    /**
     * Builds the ordered classpath layers for {@code deps}.
     *
     * @param deps           resolved runtime dependencies (order irrelevant; the
     *                       builder sorts for reproducibility)
     * @param splitSnapshots when {@code true}, released and {@code -SNAPSHOT}
     *                       dependencies are packed into two separate layers
     *                       ({@code deps} then {@code snapshot-deps}); when
     *                       {@code false}, everything goes into a single
     *                       {@code deps} layer
     * @return ordered layers (stable first); empty when {@code deps} is empty
     */
    public static List<ArtifactLayer> build(List<Dep> deps, boolean splitSnapshots)
            throws IOException {
        List<ArtifactLayer> layers = new ArrayList<>();
        if (deps == null || deps.isEmpty()) {
            return layers;
        }

        if (!splitSnapshots) {
            layers.add(pack("deps", deps));
            return layers;
        }

        List<Dep> released = new ArrayList<>();
        List<Dep> snapshots = new ArrayList<>();
        for (Dep d : deps) {
            (d.snapshot() ? snapshots : released).add(d);
        }
        if (!released.isEmpty()) {
            layers.add(pack("deps", released));
        }
        if (!snapshots.isEmpty()) {
            layers.add(pack("snapshot-deps", snapshots));
        }
        return layers;
    }

    /** Builds the single deterministic flat classpath layer used by dependency bundles. */
    public static ArtifactLayer buildBundle(List<Dep> deps) throws IOException {
        return pack("dependencies", deps == null ? List.of() : deps);
    }

    /** Packs a set of dependencies into a single deterministic tar layer. */
    private static ArtifactLayer pack(String name, List<Dep> deps) throws IOException {
        List<Dep> sorted = new ArrayList<>(deps);
        sorted.sort(Comparator.comparing(Dep::fileName));

        TarWriter tar = new TarWriter();
        for (Dep d : sorted) {
            tar.addFile(d.fileName(), Files.readAllBytes(d.path()));
        }
        return new ArtifactLayer(name, tar.toByteArray());
    }

    /**
     * Builds a single reproducible <em>module-path</em> layer from {@code deps}
     * for a modular (JPMS) application. The dependency JARs (the app's library
     * modules, whether explicit modules or automatic modules) are packed as flat,
     * bare-named files tagged with {@link MediaTypes#MODULEPATH_LAYER_MEDIA_TYPE};
     * the shim unpacks them into {@code /app/mods}, which becomes the
     * {@code --module-path} alongside the main modular JAR.
     *
     * <p>Unlike the class path, the module path is order-independent (module
     * resolution treats it as a set), so all dependency modules go into one
     * {@code mods} layer regardless of snapshot status.
     *
     * @param deps resolved runtime dependencies (order irrelevant; sorted for
     *             reproducibility)
     * @return a single-element list with the {@code mods} layer, or an empty list
     *         when {@code deps} is empty
     */
    public static List<ArtifactLayer> buildModule(List<Dep> deps) throws IOException {
        if (deps == null || deps.isEmpty()) {
            return new ArrayList<>();
        }
        List<Dep> sorted = new ArrayList<>(deps);
        sorted.sort(Comparator.comparing(Dep::fileName));

        TarWriter tar = new TarWriter();
        for (Dep d : sorted) {
            tar.addFile(d.fileName(), Files.readAllBytes(d.path()));
        }
        List<ArtifactLayer> layers = new ArrayList<>();
        layers.add(new ArtifactLayer("mods", tar.toByteArray(),
                MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE));
        return layers;
    }
}
