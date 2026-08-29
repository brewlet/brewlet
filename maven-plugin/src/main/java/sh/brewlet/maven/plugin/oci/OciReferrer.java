package sh.brewlet.maven.plugin.oci;

import sh.brewlet.maven.plugin.supplychain.CanonicalJson;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.MapperFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.json.JsonMapper;

import java.io.IOException;
import java.util.List;
import java.util.Map;

/** Builds a single-layer OCI 1.1 referrer manifest. */
public final class OciReferrer {
    private static final ObjectMapper STRICT = JsonMapper.builder()
            .disable(MapperFeature.ALLOW_COERCION_OF_SCALARS)
            .build();
    private static final byte[] EMPTY_CONFIG = "{}".getBytes(java.nio.charset.StandardCharsets.UTF_8);

    private OciReferrer() {}

    public record Content(byte[] document, byte[] manifest, String manifestDigest,
                          OciManifest model) {}

    public static Content build(String subjectDigest, long subjectSize, String subjectMediaType,
                                String artifactType, String layerMediaType, byte[] document,
                                String predicateType) throws IOException {
        OciDescriptor config = descriptor(MediaTypes.OCI_EMPTY_CONFIG_MEDIA_TYPE, EMPTY_CONFIG);
        OciDescriptor layer = descriptor(layerMediaType, document);
        OciDescriptor subject = new OciDescriptor(subjectMediaType, subjectDigest, subjectSize);
        OciManifest manifest = new OciManifest();
        manifest.setArtifactType(artifactType);
        manifest.setConfig(config);
        manifest.setSubject(subject);
        manifest.setLayers(List.of(layer));
        if (predicateType != null) {
            manifest.setAnnotations(Map.of(MediaTypes.PREDICATE_TYPE_ANNOTATION, predicateType));
        }
        byte[] bytes = CanonicalJson.bytes(manifest);
        return new Content(document, bytes, LocalStore.sha256Hex(bytes), manifest);
    }

    public static byte[] emptyConfig() {
        return EMPTY_CONFIG.clone();
    }

    static OciManifest parseManifest(byte[] bytes) throws IOException {
        JsonNode value = STRICT.readTree(bytes);
        for (String field : List.of(
                "schemaVersion", "mediaType", "artifactType", "subject", "config", "layers")) {
            if (!value.hasNonNull(field)) {
                throw new IOException("Referrer manifest is missing required field " + field);
            }
        }
        return STRICT.treeToValue(value, OciManifest.class);
    }

    private static OciDescriptor descriptor(String mediaType, byte[] bytes) {
        return new OciDescriptor(mediaType, LocalStore.sha256Hex(bytes), bytes.length);
    }
}
