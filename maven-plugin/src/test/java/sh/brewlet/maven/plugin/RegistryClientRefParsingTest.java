package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.oci.RegistryClient;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Tests the OCI image reference parsing utilities in {@link RegistryClient}.
 */
class RegistryClientRefParsingTest {

    @Test
    void splitRef_explicitRegistry() {
        String[] parts = RegistryClient.splitRef("registry.example.com/team/orders-api:1.0.0");
        assertEquals("registry.example.com", parts[0]);
        assertEquals("team/orders-api", parts[1]);
    }

    @Test
    void splitRef_localhost() {
        String[] parts = RegistryClient.splitRef("localhost:5000/myapp:latest");
        assertEquals("localhost:5000", parts[0]);
        assertEquals("myapp", parts[1]);
    }

    @Test
    void splitRef_noExplicitRegistry() {
        // No dots/colons in the first path segment → use docker.io
        String[] parts = RegistryClient.splitRef("myimage:latest");
        assertEquals("registry-1.docker.io", parts[0]);
        // Repository must not carry the tag, or it corrupts /v2/{repository}/... URLs
        assertEquals("myimage", parts[1]);
    }

    @Test
    void extractTag_explicitTag() {
        assertEquals("1.0.0", RegistryClient.extractTag("registry.example.com/team/app:1.0.0"));
    }

    @Test
    void extractTag_latestWhenNoTag() {
        assertEquals("latest", RegistryClient.extractTag("registry.example.com/team/app"));
    }

    @Test
    void extractTag_digest() {
        String ref = "registry.example.com/team/app@sha256:abc123";
        assertEquals("sha256:abc123", RegistryClient.extractTag(ref));
    }

    @Test
    void extractTag_snapshotVersion() {
        assertEquals("1.0.0-SNAPSHOT",
                RegistryClient.extractTag("registry.example.com/team/app:1.0.0-SNAPSHOT"));
    }
}
