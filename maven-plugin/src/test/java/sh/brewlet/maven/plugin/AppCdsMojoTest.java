package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.File;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class AppCdsMojoTest {

    @Test
    void parseJavaFeatureVersion_currentFormats() {
        assertEquals(21, AppCdsMojo.parseJavaFeatureVersion("21.0.4"));
        assertEquals(25, AppCdsMojo.parseJavaFeatureVersion(
                "openjdk version \"25.0.1\" 2026-10-21\nOpenJDK Runtime Environment"));
    }

    @Test
    void parseJavaFeatureVersion_legacyJava8() {
        assertEquals(8, AppCdsMojo.parseJavaFeatureVersion("java version \"1.8.0_402\""));
    }

    @Test
    void parseJavaFeatureVersion_rejectsUnknown() {
        assertThrows(IllegalArgumentException.class,
                () -> AppCdsMojo.parseJavaFeatureVersion("not-a-version"));
    }

    @Test
    void appIntrinsicJvmArgs_mirrorLaunchConfigOrder() {
        JvmConfig cfg = new JvmConfig();
        cfg.setEntry(new Entry("jar"));
        cfg.setMainJar("app.jar");
        cfg.setEnablePreview(true);
        cfg.setAddModules(List.of("jdk.incubator.vector", "java.sql"));
        cfg.setAddOpens(List.of("java.base/java.lang=ALL-UNNAMED"));
        cfg.setAddExports(List.of("java.base/sun.nio.ch=ALL-UNNAMED"));
        cfg.setSystemProperties(Map.of("b", "2", "a", "1"));

        assertEquals(List.of(
                "--enable-preview",
                "--add-modules=jdk.incubator.vector,java.sql",
                "--add-opens", "java.base/java.lang=ALL-UNNAMED",
                "--add-exports", "java.base/sun.nio.ch=ALL-UNNAMED",
                "-Da=1",
                "-Db=2"), AppCdsMojo.appIntrinsicJvmArgs(cfg));
    }

    @Test
    void buildTrainingCommand_addsArchiveJarAndTrainingArgs() {
        JvmConfig cfg = new JvmConfig();
        cfg.setEntry(new Entry("jar"));
        cfg.setMainJar("app.jar");

        File java = new File("jdk/bin/java");
        File archive = new File("target/brewlet/app.jsa");
        List<String> command = AppCdsMojo.buildTrainingCommand(
                java, cfg, archive, "app.jar", List.of("--warmup"));

        assertEquals(java.getAbsolutePath(), command.get(0));
        assertTrue(command.contains("-XX:ArchiveClassesAtExit=" + archive.getAbsolutePath()));
        assertEquals(List.of("-jar", "app.jar", "--warmup"),
                command.subList(command.size() - 3, command.size()));
    }

    @Test
    void normalizeMode_defaultsAndCaseInsensitive() throws Exception {
        assertEquals("exit", AppCdsMojo.normalizeMode(null));
        assertEquals("exit", AppCdsMojo.normalizeMode(""));
        assertEquals("exit", AppCdsMojo.normalizeMode("  EXIT "));
        assertEquals("signal", AppCdsMojo.normalizeMode("Signal"));
    }

    @Test
    void normalizeMode_rejectsUnknown() {
        assertThrows(MojoExecutionException.class, () -> AppCdsMojo.normalizeMode("kill"));
    }

    @Test
    void validateReadiness_requiresASignalInSignalMode() {
        assertThrows(MojoExecutionException.class,
                () -> AppCdsMojo.validateReadiness(null, null, 0));
    }

    @Test
    void validateReadiness_acceptsAnySingleSignal() throws Exception {
        AppCdsMojo.validateReadiness("Started .* in .* seconds", null, 0);
        AppCdsMojo.validateReadiness(null, "http://localhost:8080/health", 0);
        AppCdsMojo.validateReadiness(null, null, 10);
    }

    @Test
    void launchSelector_jarMode() {
        JvmConfig cfg = new JvmConfig();
        cfg.setEntry(new Entry("jar"));
        cfg.setMainJar("app.jar");
        assertEquals(List.of("-jar", "app.jar"), AppCdsMojo.launchSelector(cfg, "app.jar"));
    }

    @Test
    void launchSelector_classpathMode() {
        Entry e = new Entry("classpath");
        e.setClassPath(List.of("app.jar", "lib/*"));
        e.setMainClass("com.acme.Main");
        JvmConfig cfg = new JvmConfig();
        cfg.setEntry(e);
        cfg.setMainJar("app.jar");
        List<String> sel = AppCdsMojo.launchSelector(cfg, "app.jar");
        assertEquals("-cp", sel.get(0));
        assertEquals("app.jar" + File.pathSeparator + "lib/*", sel.get(1));
        assertEquals("com.acme.Main", sel.get(2));
    }

    @Test
    void launchSelector_moduleMode() {
        Entry e = new Entry("module");
        e.setModule("com.acme.app");
        e.setMainClass("com.acme.app.Main");
        e.setModulePath(List.of("app.jar", "mods"));
        JvmConfig cfg = new JvmConfig();
        cfg.setEntry(e);
        cfg.setMainJar("app.jar");
        assertEquals(List.of(
                "-p", "app.jar" + File.pathSeparator + "mods",
                "-m", "com.acme.app/com.acme.app.Main"),
                AppCdsMojo.launchSelector(cfg, "app.jar"));
    }

    @Test
    void launchSelector_moduleModeNoMainClass() {
        Entry e = new Entry("module");
        e.setModule("com.acme.app");
        e.setModulePath(List.of("app.jar"));
        JvmConfig cfg = new JvmConfig();
        cfg.setEntry(e);
        cfg.setMainJar("app.jar");
        assertEquals(List.of("-p", "app.jar", "-m", "com.acme.app"),
                AppCdsMojo.launchSelector(cfg, "app.jar"));
    }

    @Test
    void referencesDir_detectsStagingDirs() {
        assertTrue(AppCdsMojo.referencesDir(List.of("app.jar", "lib/*"), "lib"));
        assertTrue(AppCdsMojo.referencesDir(List.of("app.jar", "mods"), "mods"));
        assertTrue(AppCdsMojo.referencesDir(List.of("lib/foo.jar"), "lib"));
        assertFalse(AppCdsMojo.referencesDir(List.of("app.jar"), "lib"));
        assertFalse(AppCdsMojo.referencesDir(null, "lib"));
    }
}
