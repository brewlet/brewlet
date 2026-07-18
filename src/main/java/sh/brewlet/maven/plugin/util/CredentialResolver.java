package sh.brewlet.maven.plugin.util;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.apache.maven.settings.Server;
import org.apache.maven.settings.Settings;
import sh.brewlet.maven.plugin.oci.Credential;

import java.io.File;
import java.nio.charset.StandardCharsets;
import java.util.Base64;

/**
 * Resolves OCI registry credentials using the standard Maven credential chain:
 *
 * <ol>
 *   <li>{@code settings.xml} {@code <server>} whose {@code <id>} matches the
 *       registry hostname (supports Maven password encryption).</li>
 *   <li>Docker/OCI config ({@code ~/.docker/config.json} or the path indicated
 *       by the {@code DOCKER_CONFIG} environment variable).</li>
 *   <li>{@code BREWLET_REGISTRY_USERNAME} / {@code BREWLET_REGISTRY_PASSWORD}
 *       environment variables for CI pipelines.</li>
 * </ol>
 *
 * If no credentials are found, returns {@code null} (anonymous access).
 * Credentials are <strong>never</strong> logged.
 */
public class CredentialResolver {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private CredentialResolver() {}

    /**
     * Resolves credentials for the given registry hostname.
     *
     * @param registry registry hostname, e.g. {@code "registry.example.com"}
     * @param settings Maven settings (for {@code settings.xml} {@code <server>} lookup)
     * @return resolved {@link Credential}, or {@code null} for anonymous access
     */
    public static Credential resolve(String registry, Settings settings) {
        // 1. settings.xml <server>
        if (settings != null) {
            Server server = settings.getServer(registry);
            if (server != null && server.getUsername() != null) {
                return new Credential(server.getUsername(), server.getPassword());
            }
        }

        // 2. Docker config.json
        Credential dockerCred = resolveFromDockerConfig(registry);
        if (dockerCred != null) {
            return dockerCred;
        }

        // 3. Environment variables
        String envUser = System.getenv("BREWLET_REGISTRY_USERNAME");
        String envPass = System.getenv("BREWLET_REGISTRY_PASSWORD");
        if (envUser != null && !envUser.isEmpty()) {
            return new Credential(envUser, envPass != null ? envPass : "");
        }

        return null; // anonymous
    }

    private static Credential resolveFromDockerConfig(String registry) {
        String dockerConfigDir = System.getenv("DOCKER_CONFIG");
        if (dockerConfigDir == null || dockerConfigDir.isEmpty()) {
            dockerConfigDir = System.getProperty("user.home") + "/.docker";
        }
        File configFile = new File(dockerConfigDir, "config.json");
        if (!configFile.exists()) return null;

        try {
            JsonNode root = MAPPER.readTree(configFile);
            JsonNode auths = root.path("auths");
            if (auths.isMissingNode()) return null;

            // Try exact match first, then partial match (e.g. "https://registry")
            JsonNode entry = auths.path(registry);
            if (entry.isMissingNode()) {
                entry = auths.path("https://" + registry);
            }
            if (entry.isMissingNode()) {
                // Walk all keys and look for the registry host as a substring
                for (java.util.Iterator<String> it = auths.fieldNames(); it.hasNext(); ) {
                    String key = it.next();
                    if (key.contains(registry)) {
                        entry = auths.path(key);
                        break;
                    }
                }
            }
            if (entry.isMissingNode()) return null;

            JsonNode authNode = entry.path("auth");
            if (!authNode.isMissingNode()) {
                byte[] decoded = Base64.getDecoder().decode(authNode.asText());
                String authStr = new String(decoded, StandardCharsets.UTF_8);
                int colon = authStr.indexOf(':');
                if (colon >= 0) {
                    return new Credential(authStr.substring(0, colon),
                            authStr.substring(colon + 1));
                }
            }
        } catch (Exception e) {
            // Silently ignore malformed docker config; fall through to env vars
        }
        return null;
    }
}
