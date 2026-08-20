package sh.brewlet.maven.plugin.util;

import org.apache.maven.model.Dependency;
import org.apache.maven.model.Plugin;
import org.apache.maven.project.MavenProject;

import java.util.List;

/**
 * Detects the application framework from POM metadata so the plugin can set
 * the {@code org.brewlet.framework} label automatically.
 *
 * <p>Detection logic (first match wins):
 * <ol>
 *   <li><strong>spring-boot</strong> — presence of {@code spring-boot-maven-plugin}
 *       in the build plugins, OR any dependency on
 *       {@code org.springframework.boot:spring-boot}.</li>
 *   <li><strong>quarkus</strong> — presence of {@code quarkus-maven-plugin}
 *       in the build plugins, OR any dependency on {@code io.quarkus:quarkus-core}.</li>
 *   <li><strong>plain</strong> — fallback when no framework is detected.</li>
 * </ol>
 */
public class FrameworkDetector {

    /** Possible framework values for the {@code org.brewlet.framework} label. */
    public enum Framework {
        SPRING_BOOT("spring-boot"),
        QUARKUS("quarkus"),
        PLAIN("plain");

        private final String label;
        Framework(String label) { this.label = label; }
        public String label() { return label; }
    }

    private FrameworkDetector() {}

    /**
     * Detects the framework for the given Maven project.
     *
     * @param project the Maven project model
     * @return detected framework (never {@code null})
     */
    public static Framework detect(MavenProject project) {
        if (hasPlugin(project, "org.springframework.boot", "spring-boot-maven-plugin")
                || hasSpringBootDependency(project)) {
            return Framework.SPRING_BOOT;
        }
        if (hasPlugin(project, "io.quarkus", "quarkus-maven-plugin")
                || hasQuarkusDependency(project)) {
            return Framework.QUARKUS;
        }
        return Framework.PLAIN;
    }

    /**
     * Returns the default HTTP port for the detected framework, or {@code -1}
     * if there is no sensible default (user must supply {@code <ports>}).
     */
    public static int defaultPort(Framework framework) {
        return switch (framework) {
            case SPRING_BOOT -> 8080;
            case QUARKUS -> 8080;
            case PLAIN -> -1;
        };
    }

    private static boolean hasPlugin(MavenProject project, String groupId, String artifactId) {
        List<Plugin> plugins = project.getBuildPlugins();
        if (plugins == null) return false;
        return plugins.stream().anyMatch(p ->
                groupId.equals(p.getGroupId()) && artifactId.equals(p.getArtifactId()));
    }

    private static boolean hasSpringBootDependency(MavenProject project) {
        return hasDependency(project, "org.springframework.boot", "spring-boot");
    }

    private static boolean hasQuarkusDependency(MavenProject project) {
        return hasDependency(project, "io.quarkus", "quarkus-core");
    }

    private static boolean hasDependency(MavenProject project, String groupId, String artifactId) {
        List<Dependency> deps = project.getDependencies();
        if (deps == null) return false;
        return deps.stream().anyMatch(d ->
                groupId.equals(d.getGroupId()) && artifactId.equals(d.getArtifactId()));
    }
}
