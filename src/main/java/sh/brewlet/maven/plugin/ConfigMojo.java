package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.File;
import java.io.IOException;

/**
 * <strong>brewlet:config</strong> — Generate {@code target/brewlet/jvm-config.json}
 * from POM metadata and the built JAR's manifest. Bound to the {@code package}
 * phase so it runs automatically after {@code mvn package}.
 *
 * <p>The generated file can be inspected/committed and is used as input by
 * {@code brewlet:push} and {@code brewlet:build}.
 *
 * <p>Example:
 * <pre>
 * mvn brewlet:config
 * cat target/brewlet/jvm-config.json
 * </pre>
 */
@Mojo(name = "config",
      defaultPhase = LifecyclePhase.PACKAGE,
      requiresProject = true,
      threadSafe = true)
public class ConfigMojo extends AbstractBrewletMojo {

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        JvmConfig cfg = buildConfig();

        outputDirectory.mkdirs();
        File configFile = new File(outputDirectory, "jvm-config.json");

        try {
            MAPPER.writeValue(configFile, cfg);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to write jvm-config.json", e);
        }

        getLog().info("Brewlet: wrote launch config → " + configFile.getPath());
        if (getLog().isDebugEnabled()) {
            try {
                getLog().debug(MAPPER.writeValueAsString(cfg));
            } catch (IOException ignored) {}
        }
    }
}
