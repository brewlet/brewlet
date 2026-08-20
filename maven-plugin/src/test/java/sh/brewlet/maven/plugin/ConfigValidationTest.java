package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertThrows;

/**
 * Mirrors the Go {@code TestValidate} in {@code src/internal/artifact/artifact_test.go}:
 * each entry mode owns its fields, and foreign fields are rejected rather than
 * silently ignored.
 */
class ConfigValidationTest {

    private static JvmConfig withEntry(Entry entry) {
        return withEntry("app.jar", entry);
    }

    private static JvmConfig withEntry(String mainJar, Entry entry) {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar(mainJar);
        cfg.setEntry(entry);
        return cfg;
    }

    @Test
    void jarMode_valid() {
        assertDoesNotThrow(() -> withEntry(new Entry("jar")).validate());
    }

    @Test
    void emptyMode_treatedAsJar() {
        assertDoesNotThrow(() -> withEntry(new Entry()).validate());
    }

    @Test
    void classpathMode_withMainClass_valid() {
        Entry e = new Entry("classpath");
        e.setMainClass("com.acme.Main");
        assertDoesNotThrow(() -> withEntry(e).validate());
    }

    @Test
    void classpathMode_withoutMainClass_fails() {
        assertThrows(IllegalStateException.class, () -> withEntry(new Entry("classpath")).validate());
    }

    @Test
    void jarMode_withMainClass_fails() {
        Entry e = new Entry("jar");
        e.setMainClass("com.acme.Main");
        assertThrows(IllegalStateException.class, () -> withEntry(e).validate());
    }

    @Test
    void jarMode_withClassPath_fails() {
        Entry e = new Entry("jar", List.of("app.jar", "lib/*"));
        assertThrows(IllegalStateException.class, () -> withEntry(e).validate());
    }

    @Test
    void moduleMode_withModule_valid() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        assertDoesNotThrow(() -> withEntry(e).validate());
    }

    @Test
    void moduleMode_withMainClass_valid() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        e.setMainClass("com.acme.orders.Main");
        assertDoesNotThrow(() -> withEntry(e).validate());
    }

    @Test
    void moduleMode_withModulePath_valid() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        e.setModulePath(List.of("orders.jar", "mods"));
        assertDoesNotThrow(() -> withEntry("orders.jar", e).validate());
    }

    @Test
    void moduleMode_withoutModule_fails() {
        assertThrows(IllegalStateException.class, () -> withEntry(new Entry("module")).validate());
    }

    @Test
    void moduleMode_withClassPath_succeeds() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        e.setClassPath(List.of("lib/*"));
        assertDoesNotThrow(() -> withEntry(e).validate());
    }

    @Test
    void moduleMode_withModulePathAndClassPath_succeeds() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        e.setModulePath(List.of("orders.jar", "mods"));
        e.setClassPath(List.of("lib/*"));
        assertDoesNotThrow(() -> withEntry("orders.jar", e).validate());
    }

    @Test
    void jarMode_withModule_fails() {
        Entry e = new Entry("jar");
        e.setModule("com.acme.orders");
        assertThrows(IllegalStateException.class, () -> withEntry(e).validate());
    }

    @Test
    void classpathMode_withModule_fails() {
        Entry e = new Entry("classpath");
        e.setMainClass("com.acme.Main");
        e.setModule("com.acme.orders");
        assertThrows(IllegalStateException.class, () -> withEntry(e).validate());
    }

    @Test
    void unknownMode_fails() {
        assertThrows(IllegalStateException.class, () -> withEntry(new Entry("bogus")).validate());
    }

    @Test
    void moduleMode_mismatchedTopLevelJar_fails() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        e.setModulePath(List.of("app.jar", "mods"));
        assertThrows(IllegalStateException.class, () -> withEntry("orders.jar", e).validate());
    }

    @Test
    void classpathMode_mismatchedTopLevelJar_fails() {
        Entry e = new Entry("classpath");
        e.setMainClass("com.acme.Main");
        e.setClassPath(List.of("app.jar", "lib/*"));
        assertThrows(IllegalStateException.class, () -> withEntry("orders.jar", e).validate());
    }

    @Test
    void mixedMode_mismatchedTopLevelClassPathJar_fails() {
        Entry e = new Entry("module");
        e.setModule("com.acme.orders");
        e.setModulePath(List.of("orders.jar", "mods"));
        e.setClassPath(List.of("legacy.jar"));
        assertThrows(IllegalStateException.class, () -> withEntry("orders.jar", e).validate());
    }

    @Test
    void classpathMode_nestedJarUnderLib_ok() {
        Entry e = new Entry("classpath");
        e.setMainClass("com.acme.Main");
        e.setClassPath(List.of("orders.jar", "lib/legacy.jar"));
        assertDoesNotThrow(() -> withEntry("orders.jar", e).validate());
    }

    @Test
    void arch_unset_ok() {
        assertDoesNotThrow(() -> withEntry(new Entry("jar")).validate());
    }

    @Test
    void arch_recognizedTokens_ok() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setArch(List.of("amd64", "arm64"));
        assertDoesNotThrow(cfg::validate);
    }

    @Test
    void arch_unknownToken_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setArch(List.of("riscv64"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void arch_duplicateToken_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setArch(List.of("amd64", "amd64"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void cds_unset_ok() {
        assertDoesNotThrow(() -> withEntry(new Entry("jar")).validate());
    }

    @Test
    void cds_dynamicAndStaticModes_ok() {
        JvmConfig dynamic = withEntry(new Entry("jar"));
        dynamic.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));
        assertDoesNotThrow(dynamic::validate);

        JvmConfig staticMode = withEntry(new Entry("jar"));
        staticMode.setCds(new JvmConfig.Cds("app.jsa", "static"));
        assertDoesNotThrow(staticMode::validate);
    }

    @Test
    void cds_emptyArchive_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("", "dynamic"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void cds_archiveWithSlash_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("dir/app.jsa", "dynamic"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void cds_archiveWithBackslash_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("dir\\app.jsa", "dynamic"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void cds_archiveWithParentReference_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("..app.jsa", "dynamic"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void cds_archiveWithWildcard_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("*.jsa", "dynamic"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }

    @Test
    void cds_badMode_fails() {
        JvmConfig cfg = withEntry(new Entry("jar"));
        cfg.setCds(new JvmConfig.Cds("app.jsa", "generated"));
        assertThrows(IllegalStateException.class, cfg::validate);
    }
}
