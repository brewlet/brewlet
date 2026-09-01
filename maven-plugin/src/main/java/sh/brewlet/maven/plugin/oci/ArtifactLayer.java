package sh.brewlet.maven.plugin.oci;

/**
 * An ordered dependency layer appended to a Brewlet OCI artifact after the main
 * JAR layer. Each layer is a reproducible tar of JAR files whose OCI media type
 * determines how the shim unpacks it on the node:
 *
 * <ul>
 *   <li>{@link MediaTypes#CLASSPATH_LAYER_MEDIA_TYPE} &rarr; unpacked under
 *       {@code /app/lib} for a layered <em>class-path</em> deployment
 *       ({@code java -cp app.jar:lib/* MainClass}); see
 *       https://github.com/brewlet/brewlet/blob/main/docs/layered-classpath-deployment.md.</li>
 *   <li>{@link MediaTypes#MODULEPATH_LAYER_MEDIA_TYPE} &rarr; unpacked under
 *       {@code /app/mods} and fed to {@code --module-path} for a modular (JPMS)
 *       app ({@code java -p app.jar:mods -m module}); see
 *       https://github.com/brewlet/brewlet/blob/main/docs/jpms-support.md.</li>
 *   <li>{@link MediaTypes#CDS_LAYER_MEDIA_TYPE} &rarr; mounted as a single
 *       AppCDS archive at {@code /app/<name>} for
 *       {@code -Xshare:auto -XX:SharedArchiveFile}; see
 *       https://github.com/brewlet/brewlet/blob/main/docs/appcds.md.</li>
 * </ul>
 *
 * @param name      a human-readable layer name (e.g. {@code "deps"},
 *                  {@code "snapshot-deps"}, or {@code "mods"}) recorded as the
 *                  layer's {@code org.opencontainers.image.title} annotation
 * @param tar       the tar bytes for this layer
 * @param mediaType the OCI media type of this layer
 */
public record ArtifactLayer(String name, byte[] tar, String mediaType) {

    /**
     * Convenience constructor for a class-path (dependency) layer, the historical
     * default; equivalent to passing {@link MediaTypes#CLASSPATH_LAYER_MEDIA_TYPE}.
     */
    public ArtifactLayer(String name, byte[] tar) {
        this(name, tar, MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE);
    }
}
