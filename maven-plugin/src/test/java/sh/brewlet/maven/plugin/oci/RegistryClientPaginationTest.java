package sh.brewlet.maven.plugin.oci;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

class RegistryClientPaginationTest {
    private static final String SUBJECT = "sha256:" + "a".repeat(64);
    private static final String TYPE = "application/vnd.brewlet.attestation.v1+json";

    private HttpServer server;

    @AfterEach
    void stopServer() {
        if (server != null) {
            server.stop(0);
        }
    }

    @Test
    void followsNativeReferrerPagination() throws Exception {
        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/v2/repo/referrers/" + SUBJECT, exchange -> {
            respond(exchange, 200, index(descriptor("b")),
                    "</previous>; rel=\"prev\", </native-page-2>; rel=\"next\"");
        });
        server.createContext("/native-page-2", exchange ->
                respond(exchange, 200, index(descriptor("c")), null));
        server.start();

        RegistryClient client = client();
        List<OciDescriptor> descriptors = client.discoverReferrers(SUBJECT, TYPE);

        assertEquals(List.of("sha256:" + "b".repeat(64), "sha256:" + "c".repeat(64)),
                descriptors.stream().map(OciDescriptor::getDigest).toList());
    }

    @Test
    void followsFallbackTagPagination() throws Exception {
        String manifest = """
                {"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
                "artifactType":"%s","subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json",
                "digest":"%s","size":42},"config":{"mediaType":"application/vnd.oci.empty.v1+json",
                "digest":"sha256:%s","size":2},"layers":[]}
                """.formatted(TYPE, SUBJECT, "0".repeat(64)).replace("\n", "");
        String tag = RegistryClient.fallbackReferrerTag(
                SUBJECT, TYPE, LocalStore.sha256Hex(manifest.getBytes(StandardCharsets.UTF_8)));

        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/v2/repo/referrers/" + SUBJECT, exchange ->
                respond(exchange, 404, "{}", null));
        server.createContext("/v2/repo/tags/list", exchange ->
                respond(exchange, 200, "{\"name\":\"repo\",\"tags\":[\"unrelated\"]}",
                        "</previous>; rel=\"prev\", </fallback-page-2>; rel=\"next\""));
        server.createContext("/fallback-page-2", exchange ->
                respond(exchange, 200, "{\"name\":\"repo\",\"tags\":[\"" + tag + "\"]}", null));
        server.createContext("/v2/repo/manifests/" + tag, exchange ->
                respond(exchange, 200, manifest, null));
        server.start();

        List<OciDescriptor> descriptors = client().discoverReferrers(SUBJECT, TYPE);

        assertEquals(1, descriptors.size());
        assertEquals(LocalStore.sha256Hex(manifest.getBytes(StandardCharsets.UTF_8)),
                descriptors.get(0).getDigest());
    }

    @Test
    void rejectsMalformedFallbackReferrersInsteadOfTreatingThemAsAbsent() throws Exception {
        String malformed = "{\"schemaVersion\":1}";
        String tag = RegistryClient.fallbackReferrerTag(
                SUBJECT, TYPE, LocalStore.sha256Hex(malformed.getBytes(StandardCharsets.UTF_8)));
        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/v2/repo/referrers/" + SUBJECT, exchange ->
                respond(exchange, 404, "{}", null));
        server.createContext("/v2/repo/tags/list", exchange ->
                respond(exchange, 200, "{\"name\":\"repo\",\"tags\":[\"" + tag + "\"]}", null));
        server.createContext("/v2/repo/manifests/" + tag, exchange ->
                respond(exchange, 200, malformed, null));
        server.start();

        assertThrows(IOException.class, () -> client().discoverReferrers(SUBJECT, TYPE));
    }

    @Test
    void rejectsMalformedNativeReferrersInsteadOfTreatingThemAsAbsent() throws Exception {
        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/v2/repo/referrers/" + SUBJECT, exchange ->
                respond(exchange, 200,
                        "{\"schemaVersion\":2,\"mediaType\":"
                                + "\"application/vnd.oci.image.index.v1+json\","
                                + "\"manifests\":[null]}", null));
        server.createContext("/v2/repo/tags/list", exchange ->
                respond(exchange, 200, "{\"name\":\"repo\",\"tags\":[]}", null));
        server.start();

        assertThrows(IOException.class, () -> client().discoverReferrers(SUBJECT, TYPE));
    }

    @Test
    void rejectsNativeResponseWithoutAnExplicitReferrerIndex() throws Exception {
        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/v2/repo/referrers/" + SUBJECT, exchange ->
                respond(exchange, 200, "{}", null));
        server.start();

        assertThrows(IOException.class, () -> client().discoverReferrers(SUBJECT, TYPE));
    }

    @Test
    void rejectsCrossOriginPagination() throws Exception {
        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/v2/repo/referrers/" + SUBJECT, exchange ->
                respond(exchange, 200, index(descriptor("b")),
                        "<http://attacker.invalid/next>; rel=\"next\""));
        server.start();

        assertThrows(IOException.class, () -> client().discoverReferrers(SUBJECT, TYPE));
    }

    private RegistryClient client() {
        return new RegistryClient("localhost:" + server.getAddress().getPort(), "repo", null);
    }

    private static String descriptor(String digestCharacter) {
        return """
                {"mediaType":"application/vnd.oci.image.manifest.v1+json",
                "digest":"sha256:%s","size":42,"artifactType":"%s"}
                """.formatted(digestCharacter.repeat(64), TYPE).replace("\n", "");
    }

    private static String index(String descriptor) {
        return "{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.index.v1+json\","
                + "\"manifests\":[" + descriptor + "]}";
    }

    private static void respond(
            HttpExchange exchange, int status, String body, String link) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        if (link != null) {
            exchange.getResponseHeaders().set("Link", link);
        }
        exchange.sendResponseHeaders(status, bytes.length);
        exchange.getResponseBody().write(bytes);
        exchange.close();
    }
}
