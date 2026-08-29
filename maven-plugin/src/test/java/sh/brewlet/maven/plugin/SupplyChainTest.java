package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.DependencyBundleConfig;
import sh.brewlet.maven.plugin.model.DependencyLock;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciReferrer;
import sh.brewlet.maven.plugin.supplychain.BundleProvenance;
import sh.brewlet.maven.plugin.supplychain.CanonicalJson;
import sh.brewlet.maven.plugin.supplychain.CycloneDx;
import sh.brewlet.maven.plugin.supplychain.Dsse;
import sh.brewlet.maven.plugin.supplychain.InToto;
import sh.brewlet.maven.plugin.supplychain.ManagedProvenance;
import sh.brewlet.maven.plugin.supplychain.Predicates;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.GeneralSecurityException;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.spec.ECGenParameterSpec;
import java.util.Base64;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

class SupplyChainTest {
    @TempDir Path temp;

    @Test
    void cycloneDxIsCanonicalAndContainsMavenPurlAndHash() throws Exception {
        DependencyLock lock = lock();
        byte[] first = CycloneDx.generate(lock, "platform", "1");
        byte[] second = CycloneDx.generate(lock, "platform", "1");
        assertArrayEquals(first, second);
        JsonNode bom = CanonicalJson.MAPPER.readTree(first);
        assertEquals("CycloneDX", bom.path("bomFormat").asText());
        assertEquals("1.5", bom.path("specVersion").asText());
        assertEquals("platform", bom.path("metadata").path("component").path("name").asText());
        assertEquals("pkg:maven/com.acme/library@2",
                bom.path("components").get(0).path("purl").asText());
        assertEquals("SHA-256",
                bom.path("components").get(0).path("hashes").get(0).path("alg").asText());
        ((com.fasterxml.jackson.databind.node.ObjectNode) bom.path("metadata").path("component"))
                .put("type", "library");
        assertDoesNotThrow(() -> CycloneDx.validate(
                CanonicalJson.bytes(bom), lock, "platform", "1"));
    }

    @Test
    void dsseSignsVerifiesAndRejectsTamperingAndWrongIdentity() throws Exception {
        Keys keys = keys();
        InToto.Statement statement = new InToto.Statement("subject", "sha256:" + "a".repeat(64),
                MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE,
                new Predicates.Bundle("sha256:" + "a".repeat(64), "layer", "lock",
                        "sbom", "g:a:1", "builder"));
        byte[] envelope = CanonicalJson.bytes(
                Dsse.sign(CanonicalJson.bytes(statement), keys.privatePem()));
        assertEquals("builder", Dsse.verifyStatement(envelope, keys.publicPem(),
                "sha256:" + "a".repeat(64), MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE,
                "builder").path("predicate").path("builderIdentity").asText());
        assertThrows(GeneralSecurityException.class, () -> Dsse.verifyStatement(
                envelope, keys.publicPem(), "sha256:" + "a".repeat(64),
                MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE, "other"));

        JsonNode changed = CanonicalJson.MAPPER.readTree(envelope);
        ((com.fasterxml.jackson.databind.node.ObjectNode) changed)
                .put("payload", Base64.getEncoder().encodeToString("tampered".getBytes()));
        assertThrows(GeneralSecurityException.class,
                () -> Dsse.verify(CanonicalJson.bytes(changed), keys.publicPem()));
    }

    @Test
    void ociReferrerHasSubjectEmptyConfigAndSingleLayer() throws Exception {
        OciReferrer.Content ref = OciReferrer.build("sha256:" + "1".repeat(64), 123,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE, MediaTypes.DSSE_ARTIFACT_TYPE,
                MediaTypes.DSSE_LAYER_MEDIA_TYPE, "{}".getBytes(),
                MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE);
        JsonNode manifest = CanonicalJson.MAPPER.readTree(ref.manifest());
        assertEquals("sha256:" + "1".repeat(64),
                manifest.path("subject").path("digest").asText());
        assertEquals(MediaTypes.OCI_EMPTY_CONFIG_MEDIA_TYPE,
                manifest.path("config").path("mediaType").asText());
        assertEquals(1, manifest.path("layers").size());
        assertEquals(MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE,
                manifest.path("annotations").path(
                        MediaTypes.PREDICATE_TYPE_ANNOTATION).asText());
    }

    @Test
    void bundleProvenanceValidatesEveryBinding() throws Exception {
        Keys keys = keys();
        DependencyBundle.Content bundle = bundle();
        BundleProvenance.Materials materials = BundleProvenance.create(
                bundle, keys.privatePem(), "builder", false);
        assertEquals(materials.sbomDigest(), BundleProvenance.verify(bundle,
                materials.sbom(), materials.envelope(), keys.publicPem(), "builder"));
        byte[] tampered = materials.sbom().clone();
        tampered[tampered.length - 1] ^= 1;
        assertThrows(GeneralSecurityException.class, () -> BundleProvenance.verify(
                bundle, tampered, materials.envelope(), keys.publicPem(), "builder"));

        LocalStore store = new LocalStore(temp.resolve("layout"));
        store.pushDependencyBundle("example/platform:1", bundle);
        store.pushReferrer(materials.sbomReferrer());
        store.pushReferrer(materials.provenanceReferrer());
        assertEquals(1, store.referrers(bundle.manifestDigest(),
                MediaTypes.CYCLONEDX_ARTIFACT_TYPE).size());
        assertEquals(1, store.referrers(bundle.manifestDigest(),
                MediaTypes.DSSE_ARTIFACT_TYPE).size());
    }

    @Test
    void unsignedPublicationRequiresOpsBundlePolicy() throws Exception {
        DependencyBundle.Content required = bundle();
        assertThrows(GeneralSecurityException.class,
                () -> BundleProvenance.create(required, null, null, true));

        DependencyBundleConfig config = new DependencyBundleConfig();
        config.setName("platform");
        config.setVersion("1");
        config.setSourceBom("g:b:1");
        config.setAllowUnsigned(true);
        Path jar = temp.resolve("unsigned-library.jar");
        Files.writeString(jar, "library");
        DependencyBundle.Content optional = DependencyBundle.build(config, lock(),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("library.jar", jar, false))));
        BundleProvenance.Materials materials =
                BundleProvenance.create(optional, null, null, true);
        assertNull(materials.provenanceReferrer());
        assertEquals(materials.sbomDigest(),
                BundleProvenance.validateSbom(optional, materials.sbom()));
    }

    @Test
    void finalImagePredicateBindsAllManagedInputs() throws Exception {
        Keys keys = keys();
        String image = "sha256:" + "1".repeat(64);
        OciReferrer.Content ref = ManagedProvenance.create("app", image, 42,
                "sha256:app", "sha256:bundle", "sha256:layer", "sha256:lock",
                "sha256:sbom", "g:b:1", "builder", keys.privatePem());
        JsonNode statement = Dsse.verifyStatement(ref.document(), keys.publicPem(), image,
                MediaTypes.MANAGED_DEPENDENCY_PREDICATE_TYPE, "builder");
        JsonNode predicate = statement.path("predicate");
        assertEquals(1, predicate.path("schemaVersion").asInt());
        assertEquals(image, predicate.path("finalImageDigest").asText());
        assertTrue(predicate.path("thinJar").asBoolean());
        assertEquals("sha256:sbom", predicate.path("sbomDigest").asText());
        assertEquals("sha256:bundle", predicate.path("dependencyBundleDigest").asText());
    }

    private DependencyBundle.Content bundle() throws IOException {
        Path jar = temp.resolve("library.jar");
        Files.writeString(jar, "library");
        DependencyBundleConfig config = new DependencyBundleConfig();
        config.setName("platform");
        config.setVersion("1");
        config.setSourceBom("g:b:1");
        ArtifactLayer layer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("library.jar", jar, false)));
        return DependencyBundle.build(config, lock(), layer);
    }

    private static DependencyLock lock() {
        DependencyLock lock = new DependencyLock();
        lock.setDependencies(List.of(new DependencyLock.Entry(
                "com.acme", "library", "2", "jar", null, "runtime", "library.jar",
                LocalStore.sha256Hex("library".getBytes(StandardCharsets.UTF_8)).substring(7))));
        return lock;
    }

    private Keys keys() throws Exception {
        KeyPairGenerator generator = KeyPairGenerator.getInstance("EC");
        generator.initialize(new ECGenParameterSpec("secp256r1"));
        KeyPair pair = generator.generateKeyPair();
        Path privatePem = temp.resolve("private.pem");
        Path publicPem = temp.resolve("public.pem");
        writePem(privatePem, "PRIVATE KEY", pair.getPrivate().getEncoded());
        writePem(publicPem, "PUBLIC KEY", pair.getPublic().getEncoded());
        return new Keys(privatePem, publicPem);
    }

    private static void writePem(Path path, String label, byte[] der) throws IOException {
        String encoded = Base64.getMimeEncoder(64, "\n".getBytes(StandardCharsets.US_ASCII))
                .encodeToString(der);
        Files.writeString(path, "-----BEGIN " + label + "-----\n" + encoded
                + "\n-----END " + label + "-----\n", StandardCharsets.US_ASCII);
    }

    private record Keys(Path privatePem, Path publicPem) {}
}
