package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.Base64;
import java.util.List;
import java.util.Map;
import java.util.logging.Logger;

/**
 * Minimal OCI Distribution Spec v1.1 client that pushes a Brewlet OCI artifact
 * to any OCI-compliant registry. Implemented using the JDK's built-in
 * {@link HttpClient} — no external binary required (Option A from the design).
 *
 * <p>Push flow:
 * <ol>
 *   <li>Serialize JvmConfig → config blob, push via single-chunk monolithic upload.</li>
 *   <li>Push the JAR bytes as the layer blob.</li>
 *   <li>Build and PUT the OCI manifest.</li>
 * </ol>
 *
 * <p>Authentication:
 * <ol>
 *   <li>Try request unauthenticated; if 401, parse {@code WWW-Authenticate} header.</li>
 *   <li>If ****** exchange credentials for a token at the realm URL.</li>
 *   <li>If Basic challenge, use Basic auth directly.</li>
 * </ol>
 */
public class RegistryClient {

    private static final Logger LOG = Logger.getLogger(RegistryClient.class.getName());
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final String registry;
    private final String repository;
    private final Credential credential;
    private final HttpClient httpClient;

    /** Resolved bearer token (cached after first auth). */
    private volatile String bearerToken;

    public RegistryClient(String registry, String repository, Credential credential) {
        this.registry = registry;
        this.repository = repository;
        this.credential = credential;
        this.httpClient = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(30))
                .build();
    }

    /**
     * Pushes the Brewlet OCI artifact to the registry.
     *
     * @param reference  OCI tag or digest reference, e.g. {@code "1.0.0"}
     * @param cfg        the JVM launch descriptor
     * @param jarPath    path to the JAR file
     * @param extraAnnotations OCI manifest annotations (may be null)
     * @return the manifest digest
     */
    public String push(String reference, JvmConfig cfg, Path jarPath,
                       Map<String, String> extraAnnotations) throws IOException, InterruptedException {
        return push(reference, cfg, jarPath, List.of(), extraAnnotations);
    }

    /**
     * Pushes the Brewlet OCI artifact to the registry, optionally with one or
     * more classpath (dependency) layers appended after the main JAR layer for
     * layered class-path deployment. Each layer is uploaded as its own blob so
     * unchanged dependency layers dedup by digest across rebuilds and apps.
     * See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md.
     *
     * @param reference        OCI tag or digest reference, e.g. {@code "1.0.0"}
     * @param cfg              the JVM launch descriptor
     * @param jarPath          path to the (thin) application JAR file
     * @param classpathLayers  ordered dependency layers (may be empty)
     * @param extraAnnotations OCI manifest annotations (may be null)
     * @return the manifest digest
     */
    public String push(String reference, JvmConfig cfg, Path jarPath,
                       List<ArtifactLayer> classpathLayers,
                       Map<String, String> extraAnnotations) throws IOException, InterruptedException {
        // 1. Config blob
        byte[] cfgBytes = new ObjectMapper()
                .writerWithDefaultPrettyPrinter()
                .writeValueAsBytes(cfg);
        String cfgDigest = LocalStore.sha256Hex(cfgBytes);
        if (!blobExists(cfgDigest)) {
            pushBlob(cfgDigest, cfgBytes);
        }

        OciDescriptor cfgDesc = new OciDescriptor(MediaTypes.CONFIG_MEDIA_TYPE, cfgDigest, cfgBytes.length);

        // 2. JAR layer blob
        byte[] jarBytes = Files.readAllBytes(jarPath);
        String jarDigest = LocalStore.sha256Hex(jarBytes);
        if (!blobExists(jarDigest)) {
            LOG.info(String.format("Pushing JAR layer (%,d bytes) ...", jarBytes.length));
            pushBlob(jarDigest, jarBytes);
        } else {
            LOG.info("JAR layer already exists in registry (skipping upload).");
        }

        OciDescriptor jarDesc = new OciDescriptor(MediaTypes.JAR_LAYER_MEDIA_TYPE, jarDigest, jarBytes.length);
        jarDesc.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, cfg.getMainJar()));

        // 2b. Classpath (dependency) layers — pushed as their own blobs.
        java.util.List<OciDescriptor> layers = new java.util.ArrayList<>();
        layers.add(jarDesc);
        if (classpathLayers != null) {
            for (ArtifactLayer layer : classpathLayers) {
                byte[] tarBytes = layer.tar();
                String tarDigest = LocalStore.sha256Hex(tarBytes);
                if (!blobExists(tarDigest)) {
                    LOG.info(String.format("Pushing %s layer (%,d bytes) ...",
                            layer.name(), tarBytes.length));
                    pushBlob(tarDigest, tarBytes);
                } else {
                    LOG.info(String.format(
                            "%s layer already exists in registry (skipping upload).",
                            layer.name()));
                }
                OciDescriptor desc = new OciDescriptor(
                        layer.mediaType(), tarDigest, tarBytes.length);
                desc.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, layer.name()));
                layers.add(desc);
            }
        }

        // 3. Build and push manifest
        OciManifest manifest = new OciManifest();
        manifest.setArtifactType(MediaTypes.ARTIFACT_TYPE);
        manifest.setConfig(cfgDesc);
        manifest.setLayers(layers);
        if (extraAnnotations != null && !extraAnnotations.isEmpty()) {
            manifest.setAnnotations(extraAnnotations);
        }

        byte[] manifestBytes = new ObjectMapper()
                .writerWithDefaultPrettyPrinter()
                .writeValueAsBytes(manifest);
        String manifestDigest = LocalStore.sha256Hex(manifestBytes);

        pushManifest(reference, manifestBytes);
        LOG.info(String.format("Pushed manifest: %s", manifestDigest));
        return manifestDigest;
    }

    /**
     * Pushes a <strong>runnable OCI image</strong> (kubelet-pullable) built by
     * {@link RunnableImageBuilder} and tags it {@code reference}. Unlike
     * {@link #push}, which writes a native Brewlet artifact with custom media
     * types, every layer here is a standard {@code tar+gzip} blob and the tagged
     * object is a multi-arch OCI image index, so containerd/kubelet pull and
     * unpack it with no special configuration. The launch contract rides in each
     * platform manifest's {@code brewlet.sh/jvm-config} annotation. See
     * https://github.com/brewlet/site/blob/main/docs/runnable-image.md.
     *
     * @param reference        OCI tag reference, e.g. {@code "1.0.0"}
     * @param cfg              the JVM launch descriptor
     * @param jarPath          path to the primary application JAR
     * @param depLayers        class-path / module-path dependency layers (may be
     *                         empty); must NOT include a CDS layer
     * @param cdsArchive       optional AppCDS {@code .jsa} folded into the app layer, or null
     * @param extraAnnotations OCI image-index annotations (provenance), may be null
     * @return the image-index digest
     */
    public String pushRunnableImage(String reference, JvmConfig cfg, Path jarPath,
                                    List<ArtifactLayer> depLayers, Path cdsArchive,
                                    Map<String, String> extraAnnotations)
            throws IOException, InterruptedException {
        RunnableImageBuilder.Result image;
        try {
            image = RunnableImageBuilder.build(cfg, jarPath, depLayers, cdsArchive, extraAnnotations);
        } catch (IOException e) {
            throw new IOException("Failed to assemble runnable image: " + e.getMessage(), e);
        }

        // 1. Content-addressable blobs (layers + per-arch image configs).
        for (RunnableImageBuilder.Blob b : image.blobs) {
            if (!blobExists(b.digest())) {
                LOG.info(String.format("Pushing %s (%,d bytes) ...", b.mediaType(), b.data().length));
                pushBlob(b.digest(), b.data());
            } else {
                LOG.info(String.format("%s blob already exists in registry (skipping upload).", b.mediaType()));
            }
        }

        // 2. Per-arch image manifests, addressed by digest.
        for (RunnableImageBuilder.Blob m : image.manifests) {
            pushManifest(m.digest(), m.data(), m.mediaType());
        }

        // 3. The multi-arch image index, tagged with the reference.
        pushManifest(reference, image.indexBytes, MediaTypes.OCI_INDEX_MEDIA_TYPE);
        LOG.info(String.format("Pushed runnable image index: %s (platforms %s)",
                image.indexDigest, image.arches));
        return image.indexDigest;
    }

    // -----------------------------------------------------------------------
    // OCI Distribution Spec API helpers
    // -----------------------------------------------------------------------

    /** Returns {@code true} if the blob already exists in the registry. */
    boolean blobExists(String digest) throws IOException, InterruptedException {
        URI uri = registryUri("/v2/" + repository + "/blobs/" + digest);
        HttpRequest req = authedRequest(HttpRequest.newBuilder(uri).method("HEAD", HttpRequest.BodyPublishers.noBody()));
        HttpResponse<Void> resp = httpClient.send(req, HttpResponse.BodyHandlers.discarding());
        if (resp.statusCode() == 401) {
            negotiate(resp);
            req = authedRequest(HttpRequest.newBuilder(uri).method("HEAD", HttpRequest.BodyPublishers.noBody()));
            resp = httpClient.send(req, HttpResponse.BodyHandlers.discarding());
        }
        return resp.statusCode() == 200;
    }

    /**
     * Pushes a blob using the monolithic upload flow:
     * POST /v2/{name}/blobs/uploads/ → then PUT with full content + digest.
     */
    void pushBlob(String digest, byte[] content) throws IOException, InterruptedException {
        // Initiate upload
        URI initiateUri = registryUri("/v2/" + repository + "/blobs/uploads/");
        HttpRequest initReq = authedRequest(
                HttpRequest.newBuilder(initiateUri)
                        .POST(HttpRequest.BodyPublishers.noBody())
                        .header("Content-Length", "0"));
        HttpResponse<String> initResp = httpClient.send(initReq, HttpResponse.BodyHandlers.ofString());
        if (initResp.statusCode() == 401) {
            negotiate(initResp);
            initResp = httpClient.send(
                    authedRequest(HttpRequest.newBuilder(initiateUri)
                            .POST(HttpRequest.BodyPublishers.noBody())
                            .header("Content-Length", "0")),
                    HttpResponse.BodyHandlers.ofString());
        }
        if (initResp.statusCode() != 202) {
            throw new IOException("Failed to initiate blob upload: HTTP " + initResp.statusCode()
                    + "\n" + initResp.body());
        }

        // Resolve upload URL from Location header
        String location = initResp.headers().firstValue("Location")
                .orElseThrow(() -> new IOException("No Location header in upload initiation response"));
        URI uploadUri = resolveLocation(location, digest);

        // PUT the blob
        HttpRequest putReq = authedRequest(
                HttpRequest.newBuilder(uploadUri)
                        .PUT(HttpRequest.BodyPublishers.ofByteArray(content))
                        .header("Content-Type", "application/octet-stream")
                        .header("Content-Length", String.valueOf(content.length)));
        HttpResponse<String> putResp = httpClient.send(putReq, HttpResponse.BodyHandlers.ofString());
        if (putResp.statusCode() != 201) {
            throw new IOException("Failed to push blob " + digest + ": HTTP " + putResp.statusCode()
                    + "\n" + putResp.body());
        }
    }

    /** Pushes the OCI manifest for the given reference (tag or digest). */
    void pushManifest(String reference, byte[] manifestBytes)
            throws IOException, InterruptedException {
        pushManifest(reference, manifestBytes, MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
    }

    /**
     * Pushes an OCI manifest or image index for the given reference (tag or
     * digest) with an explicit {@code Content-Type} — a runnable image pushes its
     * per-arch manifests with the image-manifest type and the tagged index with
     * the image-index type.
     */
    void pushManifest(String reference, byte[] manifestBytes, String contentType)
            throws IOException, InterruptedException {
        URI uri = registryUri("/v2/" + repository + "/manifests/" + reference);
        HttpRequest req = authedRequest(
                HttpRequest.newBuilder(uri)
                        .PUT(HttpRequest.BodyPublishers.ofByteArray(manifestBytes))
                        .header("Content-Type", contentType)
                        .header("Content-Length", String.valueOf(manifestBytes.length)));
        HttpResponse<String> resp = httpClient.send(req, HttpResponse.BodyHandlers.ofString());
        if (resp.statusCode() == 401) {
            negotiate(resp);
            resp = httpClient.send(
                    authedRequest(HttpRequest.newBuilder(uri)
                            .PUT(HttpRequest.BodyPublishers.ofByteArray(manifestBytes))
                            .header("Content-Type", contentType)
                            .header("Content-Length", String.valueOf(manifestBytes.length))),
                    HttpResponse.BodyHandlers.ofString());
        }
        if (resp.statusCode() != 201) {
            throw new IOException("Failed to push manifest: HTTP " + resp.statusCode()
                    + "\n" + resp.body());
        }
    }

    // -----------------------------------------------------------------------
    // Authentication
    // -----------------------------------------------------------------------

    /** Applies auth headers to the given request builder and builds it. */
    private HttpRequest authedRequest(HttpRequest.Builder builder) {
        if (bearerToken != null) {
            builder.header("Authorization", "Bearer " + bearerToken);
        } else if (credential != null && credential.getUsername() != null) {
            String basic = Base64.getEncoder().encodeToString(
                    (credential.getUsername() + ":" + credential.getPassword())
                            .getBytes(StandardCharsets.UTF_8));
            builder.header("Authorization", "Basic " + basic);
        }
        return builder.build();
    }

    /**
     * Parses the {@code WWW-Authenticate} header from a 401 response and
     * exchanges credentials for a ****** if needed.
     */
    private void negotiate(HttpResponse<?> resp401) throws IOException, InterruptedException {
        String wwwAuth = resp401.headers().firstValue("WWW-Authenticate").orElse("");
        if (wwwAuth.startsWith("Bearer ")) {
            String realm = extractChallenge(wwwAuth, "realm");
            String service = extractChallenge(wwwAuth, "service");
            String scope = extractChallenge(wwwAuth, "scope");

            StringBuilder tokenUrl = new StringBuilder(realm)
                    .append("?service=").append(URLEncoder.encode(service, StandardCharsets.UTF_8))
                    .append("&scope=").append(URLEncoder.encode(scope, StandardCharsets.UTF_8));

            HttpRequest.Builder tokenReqBuilder = HttpRequest.newBuilder(URI.create(tokenUrl.toString()))
                    .GET();
            if (credential != null && credential.getUsername() != null) {
                String basic = Base64.getEncoder().encodeToString(
                        (credential.getUsername() + ":" + credential.getPassword())
                                .getBytes(StandardCharsets.UTF_8));
                tokenReqBuilder.header("Authorization", "Basic " + basic);
            }
            HttpResponse<String> tokenResp = httpClient.send(tokenReqBuilder.build(),
                    HttpResponse.BodyHandlers.ofString());
            if (tokenResp.statusCode() != 200) {
                throw new IOException("Token exchange failed: HTTP " + tokenResp.statusCode());
            }
            JsonNode json = MAPPER.readTree(tokenResp.body());
            bearerToken = json.has("token") ? json.get("token").asText()
                    : json.get("access_token").asText();
        }
        // For "Basic" challenges, credentials are applied directly in authedRequest()
    }

    /** Extracts a named parameter value from a {@code Bearer} challenge string. */
    private static String extractChallenge(String challenge, String key) {
        int idx = challenge.indexOf(key + "=\"");
        if (idx < 0) return "";
        int start = idx + key.length() + 2;
        int end = challenge.indexOf('"', start);
        return end < 0 ? "" : challenge.substring(start, end);
    }

    // -----------------------------------------------------------------------
    // URI helpers
    // -----------------------------------------------------------------------

    /**
     * Registries on {@code localhost}/{@code 127.*} are treated as insecure
     * (plain HTTP); everything else uses HTTPS.
     */
    private String scheme() {
        return registry.startsWith("localhost") || registry.startsWith("127.")
                ? "http" : "https";
    }

    private URI registryUri(String path) {
        return URI.create(scheme() + "://" + registry + path);
    }

    /**
     * Appends the {@code ?digest=} query parameter to the upload Location URL,
     * handling whether the URL already has query parameters.
     */
    private URI resolveLocation(String location, String digest) {
        String encodedDigest = URLEncoder.encode(digest, StandardCharsets.UTF_8);
        if (!location.startsWith("http")) {
            // Relative URL — make it absolute, honoring the registry's scheme
            // (HTTP for insecure localhost registries) so the follow-up PUT
            // targets the correct endpoint.
            location = scheme() + "://" + registry + location;
        }
        String separator = location.contains("?") ? "&" : "?";
        return URI.create(location + separator + "digest=" + encodedDigest);
    }

    // -----------------------------------------------------------------------
    // Public helpers
    // -----------------------------------------------------------------------

    /**
     * Parses the registry hostname and repository from a full OCI image reference.
     * Supports {@code registry/name:tag} and {@code registry/name@digest} formats.
     *
     * @return two-element array: {@code [registry, repository]}
     */
    public static String[] splitRef(String imageRef) {
        // Remove tag or digest suffix first
        String withoutTag = imageRef.replaceFirst("[:@][^/]*$", "");
        int firstSlash = withoutTag.indexOf('/');
        if (firstSlash < 0 || (!withoutTag.substring(0, firstSlash).contains(".")
                && !withoutTag.substring(0, firstSlash).contains(":"))) {
            // No explicit registry — default to docker.io. Use the tag/digest-free
            // repository so it never leaks into /v2/{repository}/... URLs.
            return new String[]{"registry-1.docker.io", withoutTag};
        }
        return new String[]{withoutTag.substring(0, firstSlash),
                withoutTag.substring(firstSlash + 1)};
    }

    /**
     * Extracts the tag or digest from a full OCI image reference.
     * Returns {@code "latest"} if no tag/digest is present.
     */
    public static String extractTag(String imageRef) {
        // Check for digest first
        int atIdx = imageRef.lastIndexOf('@');
        if (atIdx >= 0) return imageRef.substring(atIdx + 1);
        int colonIdx = imageRef.lastIndexOf(':');
        if (colonIdx >= 0 && !imageRef.substring(colonIdx).contains("/")) {
            return imageRef.substring(colonIdx + 1);
        }
        return "latest";
    }
}
