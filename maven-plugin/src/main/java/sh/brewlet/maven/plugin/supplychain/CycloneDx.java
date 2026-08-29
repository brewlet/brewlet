package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.databind.JsonNode;
import sh.brewlet.maven.plugin.model.DependencyLock;

import java.io.IOException;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Deterministic CycloneDX 1.5 JSON generation for a dependency lock. */
public final class CycloneDx {
    private CycloneDx() {}

    public static byte[] generate(DependencyLock lock, String name, String version) throws IOException {
        Map<String, Object> metadataComponent = component("application", null, name, version, null);
        List<Map<String, Object>> components = lock.getDependencies().stream()
                .map(entry -> component("library", entry.groupId(), entry.artifactId(),
                        entry.version(), entry))
                .toList();
        Map<String, Object> bom = new LinkedHashMap<>();
        bom.put("bomFormat", "CycloneDX");
        bom.put("specVersion", "1.5");
        bom.put("version", 1);
        bom.put("metadata", Map.of("component", metadataComponent));
        bom.put("components", components);
        return CanonicalJson.bytes(bom);
    }

    public static void validate(byte[] document, DependencyLock lock, String name, String version)
            throws GeneralSecurityException {
        JsonNode bom;
        try {
            bom = CanonicalJson.MAPPER.readTree(document);
        } catch (IOException e) {
            throw new GeneralSecurityException("Bundle SBOM is not valid CycloneDX JSON", e);
        }
        JsonNode metadata = bom.path("metadata").path("component");
        JsonNode components = bom.path("components");
        if (!"CycloneDX".equals(bom.path("bomFormat").asText())
                || !"1.5".equals(bom.path("specVersion").asText())
                || bom.path("version").asInt() != 1
                || !name.equals(metadata.path("name").asText())
                || !version.equals(metadata.path("version").asText())
                || !components.isArray()
                || components.size() != lock.getDependencies().size()) {
            throw new GeneralSecurityException(
                    "Bundle SBOM metadata or component count does not match dependency lock");
        }
        Map<String, DependencyLock.Entry> expected = new HashMap<>();
        for (DependencyLock.Entry entry : lock.getDependencies()) {
            expected.put(purl(entry), entry);
        }
        for (JsonNode component : components) {
            String componentKey = component.path("purl").asText();
            DependencyLock.Entry entry = expected.remove(componentKey);
            JsonNode hashes = component.path("hashes");
            if (entry == null
                    || !entry.groupId().equals(component.path("group").asText())
                    || !entry.artifactId().equals(component.path("name").asText())
                    || !entry.version().equals(component.path("version").asText())
                    || !hashes.isArray() || hashes.size() != 1
                    || !"SHA-256".equals(hashes.get(0).path("alg").asText())
                    || !entry.sha256().equals(hashes.get(0).path("content").asText())) {
                throw new GeneralSecurityException(
                        "Bundle SBOM component " + componentKey
                                + " does not match dependency lock");
            }
        }
        if (!expected.isEmpty()) {
            throw new GeneralSecurityException(
                    "Bundle SBOM is missing dependency-lock components");
        }
    }

    private static Map<String, Object> component(String type, String group, String name,
                                                  String version, DependencyLock.Entry entry) {
        Map<String, Object> component = new LinkedHashMap<>();
        component.put("type", type);
        if (group != null) component.put("group", group);
        component.put("name", name);
        component.put("version", version);
        if (entry != null) {
            component.put("purl", purl(entry));
            component.put("hashes", List.of(Map.of("alg", "SHA-256", "content", entry.sha256())));
        }
        return component;
    }

    private static String purl(DependencyLock.Entry entry) {
        String value = "pkg:maven/" + encode(entry.groupId()) + "/" + encode(entry.artifactId())
                + "@" + encode(entry.version());
        StringBuilder query = new StringBuilder();
        if (!"jar".equals(entry.type())) query.append("type=").append(encode(entry.type()));
        if (entry.classifier() != null && !entry.classifier().isBlank()) {
            if (!query.isEmpty()) query.append('&');
            query.append("classifier=").append(encode(entry.classifier()));
        }
        return query.isEmpty() ? value : value + "?" + query;
    }

    private static String encode(String value) {
        return URLEncoder.encode(value, StandardCharsets.UTF_8).replace("+", "%20");
    }
}
