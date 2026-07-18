package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.model.*;

import java.io.IOException;
import java.io.InputStream;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Validates that the Java {@link JvmConfig} serializes to JSON that is
 * byte-compatible with the Go {@code JVMConfig} schema. This is the
 * "conformance" test described in the design: any shared fixture that both
 * the Go CLI and the Maven plugin validate against.
 */
class ModelSerializationTest {

    private static final ObjectMapper MAPPER = new ObjectMapper()
            .enable(SerializationFeature.INDENT_OUTPUT);

    /** Fixture path (on the classpath). */
    private static final String FIXTURE = "/fixtures/sample-config.json";

    @Test
    void roundTrip_fixtureMatchesExpectedShape() throws IOException {
        // Load the reference fixture (shared with the Go conformance tests)
        InputStream is = getClass().getResourceAsStream(FIXTURE);
        assertNotNull(is, "Fixture not found: " + FIXTURE);
        JsonNode expected = MAPPER.readTree(is);

        // Deserialize into the Java model
        JvmConfig cfg = MAPPER.treeToValue(expected, JvmConfig.class);

        // Re-serialize and compare
        JsonNode actual = MAPPER.valueToTree(cfg);
        assertEquals(expected, actual,
                "Re-serialized JvmConfig must match the fixture exactly");
    }

    @Test
    void jvmConfig_schemaVersionIsOne() throws IOException {
        JvmConfig cfg = roundTrip(buildSampleConfig());
        assertEquals(1, cfg.getSchemaVersion());
    }

    @Test
    void jvmConfig_entryModeIsPreserved() throws IOException {
        JvmConfig cfg = roundTrip(buildSampleConfig());
        assertEquals("jar", cfg.getEntry().getMode());
    }

    @Test
    void jvmConfig_nullFieldsAreOmitted() throws IOException {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));

        String json = MAPPER.writeValueAsString(cfg);
        JsonNode node = MAPPER.readTree(json);

        // Fields that are null should not appear in the JSON
        assertFalse(node.has("enablePreview"), "enablePreview should be omitted when unset");
        assertFalse(node.has("addModules"), "addModules should be omitted when null");
        assertFalse(node.has("addOpens"), "addOpens should be omitted when null");
        assertFalse(node.has("addExports"), "addExports should be omitted when null");
        assertFalse(node.has("systemProperties"), "systemProperties should be omitted when null");
        assertFalse(node.has("user"), "user should be omitted when null");
        assertFalse(node.has("env"), "env should be omitted when null");
        assertFalse(node.has("arch"), "arch should be omitted when null");
        assertFalse(node.has("cds"), "cds should be omitted when null");
    }

    @Test
    void jvmConfig_archSerializedWhenSet() throws IOException {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));
        cfg.setArch(java.util.List.of("amd64"));

        JsonNode node = MAPPER.readTree(MAPPER.writeValueAsString(cfg));
        assertTrue(node.has("arch"), "arch should be serialized when set");
        assertEquals("amd64", node.get("arch").get(0).asText());
    }

    @Test
    void jvmConfig_cdsRoundTripsWhenSet() throws IOException {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        JsonNode node = MAPPER.readTree(MAPPER.writeValueAsString(cfg));
        assertTrue(node.has("cds"), "cds should be serialized when set");
        assertEquals("app.jsa", node.get("cds").get("archive").asText());
        assertEquals("dynamic", node.get("cds").get("mode").asText());

        JvmConfig back = MAPPER.readValue(MAPPER.writeValueAsString(cfg), JvmConfig.class);
        assertNotNull(back.getCds());
        assertEquals("app.jsa", back.getCds().getArchive());
        assertEquals("dynamic", back.getCds().getMode());
    }

    @Test
    void jvmConfig_jdkAndLauncherAreNotArtifactFields() throws IOException {
        // JDK and launcher live in the deployment descriptor, never the artifact.
        JvmConfig cfg = roundTrip(buildSampleConfig());
        String json = MAPPER.writeValueAsString(cfg);
        JsonNode node = MAPPER.readTree(json);
        assertFalse(node.has("jdk"), "jdk must not be an artifact-config field");
        assertFalse(node.has("launcher"), "launcher must not be an artifact-config field");
        assertFalse(node.has("labels"), "labels must not be an artifact-config field");
        assertFalse(node.has("ports"), "ports must not be an artifact-config field");
    }

    @Test
    void mediaTypes_matchGoConstants() {
        // These must stay in sync with src/internal/artifact/artifact.go
        assertEquals("application/vnd.brewlet.app.v1+json",
                sh.brewlet.maven.plugin.oci.MediaTypes.ARTIFACT_TYPE);
        assertEquals("application/vnd.brewlet.jvm.config.v1+json",
                sh.brewlet.maven.plugin.oci.MediaTypes.CONFIG_MEDIA_TYPE);
        assertEquals("application/vnd.brewlet.jar.layer.v1+jar",
                sh.brewlet.maven.plugin.oci.MediaTypes.JAR_LAYER_MEDIA_TYPE);
        assertEquals("application/vnd.brewlet.classpath.layer.v1+tar",
                sh.brewlet.maven.plugin.oci.MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE);
        assertEquals("application/vnd.brewlet.modulepath.layer.v1+tar",
                sh.brewlet.maven.plugin.oci.MediaTypes.MODULEPATH_LAYER_MEDIA_TYPE);
        assertEquals("application/vnd.brewlet.cds.layer.v1+jsa",
                sh.brewlet.maven.plugin.oci.MediaTypes.CDS_LAYER_MEDIA_TYPE);
    }

    @Test
    void entry_classPathOmittedWhenNull() throws IOException {
        JsonNode node = MAPPER.readTree(MAPPER.writeValueAsString(new Entry("jar")));
        assertFalse(node.has("classPath"), "classPath should be omitted when null");
        assertFalse(node.has("mainClass"), "mainClass should be omitted when null");
    }

    @Test
    void entry_classPathRoundTrips() throws IOException {
        Entry entry = new Entry("classpath", List.of("app.jar", "lib/*"));
        entry.setMainClass("com.acme.orders.Main");
        String json = MAPPER.writeValueAsString(entry);
        Entry back = MAPPER.readValue(json, Entry.class);
        assertEquals("classpath", back.getMode());
        assertEquals("com.acme.orders.Main", back.getMainClass());
        assertEquals(List.of("app.jar", "lib/*"), back.getClassPath());
    }

    @Test
    void entry_moduleFieldsOmittedWhenNull() throws IOException {
        JsonNode node = MAPPER.readTree(MAPPER.writeValueAsString(new Entry("jar")));
        assertFalse(node.has("module"), "module should be omitted when null");
        assertFalse(node.has("modulePath"), "modulePath should be omitted when null");
    }

    @Test
    void entry_moduleRoundTrips() throws IOException {
        Entry entry = new Entry("module");
        entry.setModule("com.acme.orders");
        entry.setModulePath(List.of("orders.jar", "mods"));
        String json = MAPPER.writeValueAsString(entry);
        Entry back = MAPPER.readValue(json, Entry.class);
        assertEquals("module", back.getMode());
        assertEquals("com.acme.orders", back.getModule());
        assertEquals(List.of("orders.jar", "mods"), back.getModulePath());
        assertNull(back.getClassPath());
    }

    // -----------------------------------------------------------------------

    private static JvmConfig buildSampleConfig() {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("orders-api.jar");
        cfg.setEntry(new Entry("jar"));
        cfg.setEnablePreview(true);
        cfg.setAddOpens(List.of("java.base/java.lang=ALL-UNNAMED"));
        cfg.setSystemProperties(Map.of("file.encoding", "UTF-8"));
        return cfg;
    }

    private static JvmConfig roundTrip(JvmConfig cfg) throws IOException {
        String json = MAPPER.writeValueAsString(cfg);
        return MAPPER.readValue(json, JvmConfig.class);
    }
}
