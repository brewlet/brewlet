package sh.brewlet.maven.plugin.oci;

/**
 * OCI and Brewlet media-type constants. These MUST stay in sync with the Go
 * constants in {@code src/internal/artifact/artifact.go}:
 *
 * <pre>
 * const (
 *     ArtifactType      = "application/vnd.brewlet.app.v1+json"
 *     ConfigMediaType   = "application/vnd.brewlet.jvm.config.v1+json"
 *     JarLayerMediaType = "application/vnd.brewlet.jar.layer.v1+jar"
 *     ClasspathLayerMediaType = "application/vnd.brewlet.classpath.layer.v1+tar"
 *     ModulepathLayerMediaType = "application/vnd.brewlet.modulepath.layer.v1+tar"
 *     CDSLayerMediaType = "application/vnd.brewlet.cds.layer.v1+jsa"
 * )
 * </pre>
 */
public final class MediaTypes {

    private MediaTypes() {}

    /** OCI artifact type for the Brewlet application artifact. */
    public static final String ARTIFACT_TYPE = "application/vnd.brewlet.app.v1+json";

    /** Media type for the serialized {@code JvmConfig} config blob. */
    public static final String CONFIG_MEDIA_TYPE = "application/vnd.brewlet.jvm.config.v1+json";

    /** Media type for the JAR payload layer. */
    public static final String JAR_LAYER_MEDIA_TYPE = "application/vnd.brewlet.jar.layer.v1+jar";

    /**
     * Media type for an optional classpath (dependency) layer: a tar of extra
     * JARs the shim unpacks under {@code /app/lib} for layered class-path
     * deployment. See
     * https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md.
     */
    public static final String CLASSPATH_LAYER_MEDIA_TYPE =
            "application/vnd.brewlet.classpath.layer.v1+tar";

    /**
     * Media type for an optional modulepath (library-module) layer: a tar of
     * library module JARs the shim unpacks under {@code /app/mods} for modular
     * (JPMS) deployment; the module-path twin of {@link #CLASSPATH_LAYER_MEDIA_TYPE}.
     * See https://github.com/brewlet/site/blob/main/docs/jpms-support.md.
     */
    public static final String MODULEPATH_LAYER_MEDIA_TYPE =
            "application/vnd.brewlet.modulepath.layer.v1+tar";

    /**
     * Media type for an optional AppCDS archive layer: a single {@code .jsa}
     * file mounted at {@code /app/<archive>} and consumed with
     * {@code -Xshare:auto -XX:SharedArchiveFile=...}. See
     * https://github.com/brewlet/site/blob/main/docs/appcds.md.
     */
    public static final String CDS_LAYER_MEDIA_TYPE =
            "application/vnd.brewlet.cds.layer.v1+jsa";

    /** Standard OCI image manifest v1 media type. */
    public static final String OCI_MANIFEST_MEDIA_TYPE =
            "application/vnd.oci.image.manifest.v1+json";

    /** Standard OCI image index v1 media type. */
    public static final String OCI_INDEX_MEDIA_TYPE =
            "application/vnd.oci.image.index.v1+json";

    // -----------------------------------------------------------------------
    // Runnable-image (kubelet-pullable) delivery mode. These MUST stay in sync
    // with the Go constants in src/internal/artifact/image.go. Unlike the native
    // artifact above, a runnable image uses STANDARD OCI media types so
    // containerd/kubelet pull + unpack it with no special config; the Brewlet
    // launch contract rides in the JVM_CONFIG_ANNOTATION manifest annotation and
    // each layer's Brewlet role rides in the LAYER_ROLE_ANNOTATION. See
    // https://github.com/brewlet/site/blob/main/docs/runnable-image.md and
    // https://github.com/brewlet/brewlet/blob/main/specs/SPECIFICATION.md#44-oci-artifact-format.
    // -----------------------------------------------------------------------

    /** Standard OCI image config v1 media type (the runnable-image config blob). */
    public static final String OCI_IMAGE_CONFIG_MEDIA_TYPE =
            "application/vnd.oci.image.config.v1+json";

    /** Standard OCI {@code tar+gzip} layer media type (all runnable-image layers). */
    public static final String OCI_LAYER_GZIP_MEDIA_TYPE =
            "application/vnd.oci.image.layer.v1.tar+gzip";

    /**
     * Manifest annotation carrying the Brewlet launch config JSON on a runnable
     * image. A runnable image's config blob is a standard OCI image config (so
     * containerd unpacks the image), so the launch contract a native artifact
     * keeps in its config blob rides here instead.
     */
    public static final String JVM_CONFIG_ANNOTATION = "brewlet.sh/jvm-config";

    /**
     * Layer annotation tagging each runnable-image layer with its Brewlet role,
     * because every layer now shares the standard {@code tar+gzip} media type and
     * can no longer be told apart by media type alone.
     */
    public static final String LAYER_ROLE_ANNOTATION = "brewlet.sh/layer";

    /** {@link #LAYER_ROLE_ANNOTATION} value: tar of the primary JAR (+ optional CDS archive), flat. */
    public static final String LAYER_ROLE_APP = "app";
    /** {@link #LAYER_ROLE_ANNOTATION} value: tar of dependency JARs, unpacked to {@code /app/lib}. */
    public static final String LAYER_ROLE_CLASSPATH = "classpath";
    /** {@link #LAYER_ROLE_ANNOTATION} value: tar of library module JARs, unpacked to {@code /app/mods}. */
    public static final String LAYER_ROLE_MODULEPATH = "modulepath";

    /** OCI annotation key for the human-readable tag/ref name. */
    public static final String ANNOTATION_REF_NAME = "org.opencontainers.image.ref.name";

    /** OCI annotation key for the image title (used on the JAR layer). */
    public static final String ANNOTATION_TITLE = "org.opencontainers.image.title";

    /** OCI annotations for build provenance. */
    public static final String ANNOTATION_SOURCE   = "org.opencontainers.image.source";
    public static final String ANNOTATION_REVISION = "org.opencontainers.image.revision";
    public static final String ANNOTATION_VERSION  = "org.opencontainers.image.version";
    public static final String ANNOTATION_CREATED  = "org.opencontainers.image.created";

    /** Brewlet framework detection label key. */
    public static final String LABEL_FRAMEWORK = "org.brewlet.framework";
}
