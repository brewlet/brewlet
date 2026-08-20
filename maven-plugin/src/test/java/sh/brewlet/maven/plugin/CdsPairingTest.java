package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertThrows;

class CdsPairingTest {

    @TempDir
    Path tempDir;

    @Test
    void configCdsWithoutArchive_fails() {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(cfg, null));
    }

    @Test
    void absentArchiveWithConfigCds_fails() {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(
                        cfg, tempDir.resolve("missing.jsa").toFile()));
    }

    @Test
    void archiveWithoutConfigCds_fails() throws IOException {
        File archive = archive("app.jsa");

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(sampleConfig(), archive));
    }

    @Test
    void archiveBasenameMismatch_fails() throws IOException {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("expected.jsa", "dynamic"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(cfg, archive("actual.jsa")));
    }

    @Test
    void archiveAndConfigMatch_ok() throws IOException {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        assertDoesNotThrow(
                () -> AbstractBrewletMojo.validateCdsPairing(cfg, archive("app.jsa")));
    }

    private File archive(String name) throws IOException {
        Path archive = tempDir.resolve(name);
        Files.writeString(archive, "jsa");
        return archive.toFile();
    }

    private static JvmConfig sampleConfig() {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));
        return cfg;
    }
}
