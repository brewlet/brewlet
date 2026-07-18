package sh.brewlet.maven.plugin.util;

import org.apache.maven.model.Plugin;
import org.apache.maven.project.MavenProject;

import java.util.Properties;

/**
 * Resolves the JDK feature version from Maven project metadata, using the
 * following precedence (first non-null wins):
 *
 * <ol>
 *   <li>{@code maven.compiler.release} property.</li>
 *   <li>{@code maven.compiler.target} property.</li>
 *   <li>{@code maven.compiler.source} property.</li>
 *   <li>JDK major version of the running JVM ({@code System.getProperty("java.version")}).</li>
 * </ol>
 */
public class JdkVersionResolver {

    private JdkVersionResolver() {}

    /**
     * Resolves the JDK feature (major) version for the given project.
     *
     * @param project Maven project
     * @return JDK major version integer (e.g. {@code 21})
     */
    public static int resolve(MavenProject project) {
        Properties props = project.getProperties();

        // 1. maven.compiler.release
        String release = props.getProperty("maven.compiler.release");
        if (release != null) return parseFeature(release);

        // 2. maven.compiler.target
        String target = props.getProperty("maven.compiler.target");
        if (target != null) return parseFeature(target);

        // 3. maven.compiler.source
        String source = props.getProperty("maven.compiler.source");
        if (source != null) return parseFeature(source);

        // 4. Running JVM
        return runningJdkFeature();
    }

    /** Parses a running JVM's major version from {@code System.getProperty("java.version")}. */
    public static int runningJdkFeature() {
        String v = System.getProperty("java.version", "17");
        return parseFeature(v);
    }

    /**
     * Parses a JDK version string to its feature (major) version.
     * Handles both new style ({@code "21"}, {@code "21.0.1"}) and
     * old style ({@code "1.8"}, {@code "1.8.0_391"}).
     */
    public static int parseFeature(String version) {
        if (version == null || version.isBlank()) return 17;
        String v = version.trim();
        // Strip early-access suffix like "-ea"
        v = v.replaceAll("-.*$", "");
        String[] parts = v.split("[.+]");
        if (parts.length == 0) return 17;
        try {
            int first = Integer.parseInt(parts[0]);
            if (first == 1 && parts.length >= 2) {
                // Old-style: 1.8 → 8
                return Integer.parseInt(parts[1]);
            }
            return first;
        } catch (NumberFormatException e) {
            return 17;
        }
    }
}
