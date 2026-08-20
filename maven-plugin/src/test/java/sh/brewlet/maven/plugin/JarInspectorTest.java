package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.util.JarInspector;

import javax.tools.JavaCompiler;
import javax.tools.ToolProvider;
import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.jar.Attributes;
import java.util.jar.JarOutputStream;
import java.util.jar.Manifest;
import java.util.zip.ZipEntry;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Tests JAR manifest inspection logic used by {@code brewlet:config} to infer
 * the entry mode and main class without requiring a real JAR on the classpath.
 */
class JarInspectorTest {

    @TempDir
    Path tempDir;

    @Test
    void mainClass_extractedFromManifest() throws IOException {
        File jar = createJar("com.example.MyApp", null);
        assertEquals("com.example.MyApp", JarInspector.mainClass(jar));
    }

    @Test
    void mainClass_nullWhenAbsent() throws IOException {
        File jar = createJar(null, null);
        assertNull(JarInspector.mainClass(jar));
    }

    @Test
    void effectiveMainClass_prefersStartClass() throws IOException {
        // Spring Boot fat JARs set Start-Class to the real app main class
        File jar = createJar("org.springframework.boot.loader.JarLauncher", "com.example.App");
        assertEquals("com.example.App", JarInspector.effectiveMainClass(jar));
    }

    @Test
    void effectiveMainClass_fallsBackToMainClass() throws IOException {
        File jar = createJar("com.example.Plain", null);
        assertEquals("com.example.Plain", JarInspector.effectiveMainClass(jar));
    }

    @Test
    void entryMode_jarWhenMainClassPresent() throws IOException {
        File jar = createJar("com.example.MyApp", null);
        assertEquals("jar", JarInspector.entryMode(jar));
    }

    @Test
    void entryMode_classpathWhenNoMainClass() throws IOException {
        File jar = createJar(null, null);
        assertEquals("classpath", JarInspector.entryMode(jar));
    }

    @Test
    void isModular_falseForPlainJar() throws IOException {
        File jar = createJar("com.example.MyApp", null);
        assertFalse(JarInspector.isModular(jar));
    }

    @Test
    void isModular_trueForModularJar() throws IOException {
        File jar = createModularJar();
        assertTrue(JarInspector.isModular(jar));
    }

    @Test
    void moduleName_extractedFromDescriptor() throws IOException {
        File jar = createModularJar();
        assertEquals("com.acme.orders", JarInspector.moduleName(jar));
    }

    @Test
    void moduleMainClass_extractedFromDescriptor() throws IOException {
        File jar = createModularJar();
        assertEquals("com.acme.orders.Main", JarInspector.moduleMainClass(jar));
    }

    @Test
    void entryMode_moduleForModularJar() throws IOException {
        File jar = createModularJar();
        assertEquals("module", JarInspector.entryMode(jar));
    }

    @Test
    void scanNativeArch_pureBytecodeJarYieldsNoArch() throws IOException {
        File jar = createJarWithEntries("com/acme/App.class", "META-INF/MANIFEST.MF");
        JarInspector.NativeArchScan scan = JarInspector.scanNativeArch(jar);
        assertTrue(scan.arches().isEmpty());
        assertEquals(0, scan.nativeLibs());
    }

    @Test
    void scanNativeArch_linuxX8664Amd64() throws IOException {
        File jar = createJarWithEntries("META-INF/native/libnetty_tcnative_linux_x86_64.so");
        JarInspector.NativeArchScan scan = JarInspector.scanNativeArch(jar);
        assertEquals(java.util.List.of("amd64"), scan.arches());
        assertEquals(1, scan.nativeLibs());
    }

    @Test
    void scanNativeArch_aarch64Arm64() throws IOException {
        File jar = createJarWithEntries("com/sun/jna/linux-aarch64/libjnidispatch.so");
        JarInspector.NativeArchScan scan = JarInspector.scanNativeArch(jar);
        assertEquals(java.util.List.of("arm64"), scan.arches());
    }

    @Test
    void scanNativeArch_bothArches() throws IOException {
        File jar = createJarWithEntries(
                "native/linux-x86_64/libz.so",
                "native/linux-aarch64/libz.so",
                "native/win32-x86-64/z.dll");
        JarInspector.NativeArchScan scan = JarInspector.scanNativeArch(jar);
        assertEquals(java.util.List.of("amd64", "arm64"), scan.arches());
        assertEquals(3, scan.nativeLibs());
    }

    @Test
    void scanNativeArch_nativeWithNoInferableArchIsUnrecognized() throws IOException {
        File jar = createJarWithEntries("lib/libfoo.so");
        JarInspector.NativeArchScan scan = JarInspector.scanNativeArch(jar);
        assertTrue(scan.arches().isEmpty());
        assertEquals(1, scan.nativeLibs());
        assertEquals(1, scan.unrecognized().size());
    }

    // -----------------------------------------------------------------------
    // Helpers
    // -----------------------------------------------------------------------

    private File createJar(String mainClass, String startClass) throws IOException {
        Manifest mf = new Manifest();
        mf.getMainAttributes().put(Attributes.Name.MANIFEST_VERSION, "1.0");
        if (mainClass != null) {
            mf.getMainAttributes().put(Attributes.Name.MAIN_CLASS, mainClass);
        }
        if (startClass != null) {
            mf.getMainAttributes().putValue("Start-Class", startClass);
        }

        Path jarPath = tempDir.resolve("test.jar");
        try (JarOutputStream jos = new JarOutputStream(Files.newOutputStream(jarPath), mf)) {
            // Add a dummy entry so the JAR is valid
            jos.putNextEntry(new ZipEntry("dummy.class"));
            jos.write(new byte[]{0x00});
            jos.closeEntry();
        }
        return jarPath.toFile();
    }

    /** Builds a JAR containing the given (empty) entries, for native-lib scanning. */
    private File createJarWithEntries(String... entries) throws IOException {
        Path jarPath = tempDir.resolve("native-" + System.nanoTime() + ".jar");
        try (JarOutputStream jos = new JarOutputStream(Files.newOutputStream(jarPath))) {
            for (String name : entries) {
                jos.putNextEntry(new ZipEntry(name));
                jos.closeEntry();
            }
        }
        return jarPath.toFile();
    }

    /**
     * Builds a real modular JAR on the fly for {@code module com.acme.orders {}}
     * with {@code ModuleMainClass=com.acme.orders.Main}. The {@code module-info.class}
     * is compiled and packaged at test time using the running JDK's compiler and
     * jar tools, so its class-file version always matches the JVM reading it back.
     * This avoids checking a version-pinned {@code .class} binary into the repo,
     * which would fail on JDKs older than the one that produced it.
     */
    private File createModularJar() throws IOException {
        JavaCompiler compiler = ToolProvider.getSystemJavaCompiler();
        assertNotNull(compiler, "a JDK (not just a JRE) is required to run this test");

        Path srcDir = tempDir.resolve("src");
        Path pkgDir = srcDir.resolve("com/acme/orders");
        Files.createDirectories(pkgDir);
        Files.writeString(srcDir.resolve("module-info.java"), "module com.acme.orders {}\n");
        Files.writeString(pkgDir.resolve("Main.java"),
                "package com.acme.orders;\npublic class Main { public static void main(String[] a) {} }\n");

        Path classes = tempDir.resolve("classes");
        Files.createDirectories(classes);
        int rc = compiler.run(null, null, null,
                "-d", classes.toString(),
                srcDir.resolve("module-info.java").toString(),
                pkgDir.resolve("Main.java").toString());
        assertEquals(0, rc, "compiling the modular fixture failed");

        Path jarPath = tempDir.resolve("modular.jar");
        var jarTool = java.util.spi.ToolProvider.findFirst("jar")
                .orElseThrow(() -> new IllegalStateException("jar tool unavailable"));
        int jrc = jarTool.run(System.out, System.err,
                "--create", "--file", jarPath.toString(),
                "--main-class", "com.acme.orders.Main",
                "-C", classes.toString(), ".");
        assertEquals(0, jrc, "packaging the modular fixture failed");
        return jarPath.toFile();
    }
}
