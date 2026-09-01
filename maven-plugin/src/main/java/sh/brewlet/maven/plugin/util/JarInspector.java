package sh.brewlet.maven.plugin.util;

import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.lang.module.ModuleDescriptor;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.TreeSet;
import java.util.Set;
import java.util.jar.Attributes;
import java.util.jar.JarEntry;
import java.util.jar.JarFile;
import java.util.jar.Manifest;

/**
 * Inspects a JAR file to extract build-time metadata relevant to the
 * Brewlet launch descriptor (main class, module descriptor, etc.).
 */
public class JarInspector {

    private JarInspector() {}

    /**
     * Returns the value of {@code Main-Class} from the JAR's
     * {@code META-INF/MANIFEST.MF}, or {@code null} if not present.
     *
     * @param jarFile the JAR file to inspect
     */
    public static String mainClass(File jarFile) throws IOException {
        try (JarFile jar = new JarFile(jarFile)) {
            Manifest mf = jar.getManifest();
            if (mf == null) return null;
            return mf.getMainAttributes().getValue(Attributes.Name.MAIN_CLASS);
        }
    }

    /**
     * Returns the value of {@code Start-Class} (Spring Boot repackaged JARs) or
     * {@code Main-Class} from the JAR manifest, in that order.
     * Returns {@code null} if neither is set.
     */
    public static String effectiveMainClass(File jarFile) throws IOException {
        try (JarFile jar = new JarFile(jarFile)) {
            Manifest mf = jar.getManifest();
            if (mf == null) return null;
            Attributes attrs = mf.getMainAttributes();
            // Spring Boot fat jars use Start-Class for the user's main class
            String startClass = attrs.getValue("Start-Class");
            if (startClass != null && !startClass.isEmpty()) {
                return startClass;
            }
            return attrs.getValue(Attributes.Name.MAIN_CLASS);
        }
    }

    /**
     * Reads the JPMS module descriptor from the JAR's root
     * {@code module-info.class}, or {@code null} if the JAR is not an explicit
     * module. Automatic modules (a plain JAR with only an
     * {@code Automatic-Module-Name}) are intentionally not reported here: they
     * have no descriptor and are better shipped as a modulepath layer entry than
     * launched directly. See https://github.com/brewlet/brewlet/blob/main/docs/jpms-support.md.
     */
    public static ModuleDescriptor moduleDescriptor(File jarFile) throws IOException {
        try (JarFile jar = new JarFile(jarFile)) {
            JarEntry entry = jar.getJarEntry("module-info.class");
            if (entry == null) return null;
            try (InputStream in = jar.getInputStream(entry)) {
                return ModuleDescriptor.read(in);
            }
        }
    }

    /**
     * Returns {@code true} when the JAR is an explicit JPMS module (has a root
     * {@code module-info.class}).
     */
    public static boolean isModular(File jarFile) throws IOException {
        return moduleDescriptor(jarFile) != null;
    }

    /**
     * Returns the module name declared in the JAR's {@code module-info.class}, or
     * {@code null} when the JAR is not an explicit module.
     */
    public static String moduleName(File jarFile) throws IOException {
        ModuleDescriptor d = moduleDescriptor(jarFile);
        return d == null ? null : d.name();
    }

    /**
     * Returns the main class declared by the JAR's module descriptor
     * ({@code ModuleMainClass}), or {@code null} if the JAR is not an explicit
     * module or declares no main class.
     */
    public static String moduleMainClass(File jarFile) throws IOException {
        ModuleDescriptor d = moduleDescriptor(jarFile);
        if (d == null) return null;
        return d.mainClass().orElse(null);
    }

    /**
     * Determines the preferred entry mode.
     *
     * <ul>
     *   <li>{@code "module"} — when the JAR is an explicit JPMS module (has a root
     *       {@code module-info.class}); launched via {@code java -p … -m …}.</li>
     *   <li>{@code "jar"} — when {@code Main-Class} (or {@code Start-Class}) is
     *       present in the manifest, indicating {@code java -jar} is valid.</li>
     *   <li>{@code "classpath"} — when no manifest entry point is present; the caller
     *       must supply {@code mainClass} explicitly.</li>
     * </ul>
     */
    public static String entryMode(File jarFile) throws IOException {
        if (isModular(jarFile)) {
            return "module";
        }
        return mainClass(jarFile) != null ? "jar" : "classpath";
    }

    /**
     * Outcome of scanning a JAR for bundled native libraries. Mirrors the Go
     * {@code NativeArchScan} in {@code src/internal/artifact/jarnative.go}.
     *
     * @param arches       sorted recognized architectures inferred from bundled
     *                     natives (a subset of {@link JarInspector#KNOWN_ARCHES});
     *                     empty when the JAR bundles no natives or none map to a
     *                     known arch
     * @param nativeLibs   number of native-library entries found
     * @param unrecognized native-library entry names whose architecture could not
     *                     be inferred
     */
    public record NativeArchScan(List<String> arches, int nativeLibs, List<String> unrecognized) {}

    /** Recognized architecture tokens, matching Go's GOARCH / kubernetes.io/arch. */
    public static final Set<String> KNOWN_ARCHES = Set.of("amd64", "arm64");

    private static final String[] NATIVE_LIB_SUFFIXES = {".so", ".dll", ".dylib", ".jnilib"};

    // Substrings that identify a recognized arch in a native-library path/filename,
    // following common packaging conventions (os-maven-plugin classifiers, JNA's
    // <os>-<arch> dirs, netty-tcnative's shaded names). 32-bit tokens are excluded.
    private static final Map<String, String[]> ARCH_TOKENS = Map.of(
            "amd64", new String[]{"x86_64", "x86-64", "amd64", "x64"},
            "arm64", new String[]{"aarch64", "aarch_64", "arm64"});

    private static boolean isNativeLib(String name) {
        String lower = name.toLowerCase(Locale.ROOT);
        for (String s : NATIVE_LIB_SUFFIXES) {
            if (lower.endsWith(s)) {
                return true;
            }
        }
        return false;
    }

    private static List<String> archOf(String name) {
        String lower = name.toLowerCase(Locale.ROOT);
        List<String> out = new ArrayList<>();
        for (Map.Entry<String, String[]> e : ARCH_TOKENS.entrySet()) {
            for (String t : e.getValue()) {
                if (lower.contains(t)) {
                    out.add(e.getKey());
                    break;
                }
            }
        }
        return out;
    }

    /**
     * Scans a JAR for bundled native libraries ({@code .so}/{@code .dll}/
     * {@code .dylib}/{@code .jnilib}) and infers the architecture(s) they target,
     * so tooling can default the optional launch-config {@code arch} constraint
     * for a NON-portable artifact. A pure-bytecode JAR bundles no natives and
     * yields an empty arch set (runs anywhere). Mirrors {@code ScanNativeArch} in
     * {@code src/internal/artifact/jarnative.go}. It inspects the JAR's own
     * entries and does not recurse into nested dependency JARs; shaded/uber JARs
     * — the usual carriers of bundled natives — extract natives to top-level
     * paths and are handled.
     *
     * @param jarFile the JAR to scan
     */
    public static NativeArchScan scanNativeArch(File jarFile) throws IOException {
        Set<String> set = new TreeSet<>();
        List<String> unrecognized = new ArrayList<>();
        int nativeLibs = 0;
        try (JarFile jar = new JarFile(jarFile)) {
            var entries = jar.entries();
            while (entries.hasMoreElements()) {
                JarEntry entry = entries.nextElement();
                if (entry.isDirectory() || !isNativeLib(entry.getName())) {
                    continue;
                }
                nativeLibs++;
                List<String> arches = archOf(entry.getName());
                if (arches.isEmpty()) {
                    unrecognized.add(entry.getName());
                } else {
                    set.addAll(arches);
                }
            }
        }
        return new NativeArchScan(new ArrayList<>(set), nativeLibs, unrecognized);
    }

    /**
     * Returns nested JAR entries that make an application JAR unsuitable for
     * managed dependency mode. Any embedded JAR is rejected, including the
     * well-known Spring Boot and WAR library layouts.
     */
    public static List<String> embeddedJars(File jarFile) throws IOException {
        List<String> nested = new ArrayList<>();
        try (JarFile jar = new JarFile(jarFile)) {
            var entries = jar.entries();
            while (entries.hasMoreElements()) {
                JarEntry entry = entries.nextElement();
                if (!entry.isDirectory()
                        && entry.getName().toLowerCase(Locale.ROOT).endsWith(".jar")) {
                    nested.add(entry.getName());
                }
            }
        }
        nested.sort(String::compareTo);
        return nested;
    }
}
