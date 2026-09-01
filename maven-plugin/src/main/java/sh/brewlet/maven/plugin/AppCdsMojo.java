package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URI;
import java.net.URISyntaxException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.FileTime;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * <strong>brewlet:appcds</strong> — Generate a dynamic Application Class-Data
 * Sharing archive with a self-terminating training run.
 *
 * <p>Usage:
 * <pre>
 * mvn package brewlet:appcds \
 *   -Dbrewlet.appcds.trainingArgs=--warmup \
 *   -Dbrewlet.appcds.timeoutSeconds=180
 * mvn brewlet:push -Dbrewlet.image=registry.example.com/team/app:1.0 \
 *   -Dbrewlet.cdsArchive=target/brewlet/app.jsa
 * </pre>
 *
 * <p>Two termination strategies (see {@code brewlet.appcds.mode}):
 * <ul>
 *   <li><strong>{@code exit}</strong> (default) — the app is expected to boot,
 *       exercise startup paths, and exit on its own within the timeout.</li>
 *   <li><strong>{@code signal}</strong> — for long-running servers: the goal
 *       starts the app, waits for a readiness signal
 *       ({@code readyLog} / {@code readyHttp} / {@code readyDelaySeconds}), then
 *       sends {@code SIGTERM} so the JVM shutdown hook runs and dynamic CDS writes
 *       the archive at exit. Unix-oriented (a graceful {@code SIGTERM} is required
 *       for the archive to flush); Brewlet nodes are Linux.</li>
 * </ul>
 *
 * <p>Bind it to {@code pre-integration-test} if your build already has a short
 * startup/warmup path. Training matches how the artifact is pushed: a fat JAR
 * ({@code -jar}), a layered class-path app ({@code -cp <mainJar>:lib/*}, with the
 * resolved runtime dependencies staged into {@code lib/}), or a JPMS module
 * ({@code -p <mainJar>:mods -m …}, dependencies staged into {@code mods/}) — run
 * the goal with the same {@code -Dbrewlet.layered=true} you push with. All staged
 * files get a canonical mtime so the archive maps against the shim's layout.
 * See https://github.com/brewlet/brewlet/blob/main/docs/appcds.md#42-turnkey-generation-in-the-maven-plugin--cli.
 */
@Mojo(name = "appcds",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class AppCdsMojo extends AbstractBrewletMojo {

    static final String MODE_EXIT = "exit";
    static final String MODE_SIGNAL = "signal";

    static final FileTime CANONICAL_APP_MTIME =
            FileTime.from(Instant.ofEpochSecond(946684800L));

    /**
     * Output path for the generated dynamic-CDS archive.
     */
    @Parameter(defaultValue = "${project.build.directory}/brewlet/app.jsa")
    private File cdsArchiveOutput;

    /**
     * Extra program arguments passed after {@code -jar <mainJar>} for the
     * self-terminating training run.
     */
    @Parameter(property = "brewlet.appcds.trainingArgs")
    private List<String> trainingArgs;

    /**
     * Maximum time to wait for the app to boot, exercise startup paths, and exit
     * ({@code exit} mode) or become ready ({@code signal} mode).
     */
    @Parameter(property = "brewlet.appcds.timeoutSeconds", defaultValue = "120")
    private int timeoutSeconds;

    /**
     * JDK used for training. Defaults to the JDK running Maven
     * ({@code java.home}); turnkey AppCDS requires JDK 21+ per
     * https://github.com/brewlet/brewlet/blob/main/docs/appcds.md#22-minimum-jdk-version.
     */
    @Parameter(property = "brewlet.appcds.javaHome")
    private File trainingJavaHome;

    /**
     * Training termination strategy: {@code exit} (self-terminating, default) or
     * {@code signal} (start, wait for readiness, then {@code SIGTERM}).
     */
    @Parameter(property = "brewlet.appcds.mode", defaultValue = MODE_EXIT)
    private String mode;

    /**
     * {@code signal} mode readiness: a regular expression matched line-by-line
     * against the training JVM's combined stdout/stderr. The app is considered
     * warmed up as soon as a line matches (e.g. {@code "Started .* in .* seconds"}
     * for Spring Boot).
     */
    @Parameter(property = "brewlet.appcds.readyLog")
    private String readyLog;

    /**
     * {@code signal} mode readiness: an HTTP(S) URL polled until it returns a
     * 2xx/3xx status (e.g. {@code http://localhost:8080/actuator/health}).
     */
    @Parameter(property = "brewlet.appcds.readyHttp")
    private String readyHttp;

    /**
     * {@code signal} mode readiness: a fixed warmup delay (seconds) before
     * {@code SIGTERM}. Used alone as a simple fallback, or as a settle time after
     * {@code readyLog}/{@code readyHttp} fires.
     */
    @Parameter(property = "brewlet.appcds.readyDelaySeconds", defaultValue = "0")
    private int readyDelaySeconds;

    /**
     * {@code signal} mode: how long to wait after {@code SIGTERM} for the app to
     * shut down and flush the archive before force-killing.
     */
    @Parameter(property = "brewlet.appcds.shutdownGraceSeconds", defaultValue = "30")
    private int shutdownGraceSeconds;

    /**
     * {@code signal} mode: poll interval (milliseconds) for {@code readyHttp}.
     */
    @Parameter(property = "brewlet.appcds.readyPollMillis", defaultValue = "500")
    private long readyPollMillis;

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        if (timeoutSeconds <= 0) {
            throw new MojoExecutionException("brewlet.appcds.timeoutSeconds must be greater than zero.");
        }
        String trainingMode = normalizeMode(mode);
        if (MODE_SIGNAL.equals(trainingMode)) {
            validateReadiness(readyLog, readyHttp, readyDelaySeconds);
            if (shutdownGraceSeconds <= 0) {
                throw new MojoExecutionException(
                        "brewlet.appcds.shutdownGraceSeconds must be greater than zero.");
            }
        }

        File jar = resolveJarFile();
        JvmConfig cfg = buildConfig();
        Entry entry = cfg.getEntry();
        String entryMode = entry == null || entry.getMode() == null || entry.getMode().isBlank()
                ? "jar"
                : entry.getMode();

        File javaHome = trainingJavaHome != null
                ? trainingJavaHome
                : new File(System.getProperty("java.home"));
        File java = javaBinary(javaHome);
        Path trainingDir = outputDirectory.toPath().resolve("appcds-training");
        File output = cdsArchiveOutput.getAbsoluteFile();
        List<String> command = buildTrainingCommand(java, cfg, output, cfg.getMainJar(), trainingArgs);

        if (dryRun) {
            getLog().info("Brewlet: dry-run mode — would generate AppCDS archive:");
            getLog().info("  mode: " + trainingMode
                    + (MODE_SIGNAL.equals(trainingMode) ? " (start → wait for readiness → SIGTERM)" : " (self-terminating)"));
            getLog().info("  entry: " + entryMode);
            getLog().info("  working directory: " + trainingDir);
            getLog().info("  command: " + String.join(" ", command));
            getLog().info("  training JAR copy mtime: 2000-01-01T00:00:00Z");
            return;
        }

        if (!java.isFile()) {
            throw new MojoExecutionException("Training java binary not found: " + java.getAbsolutePath());
        }

        int feature = detectTrainingJdkFeature(java);
        if (feature < 21) {
            throw new MojoExecutionException("brewlet:appcds requires a JDK 21+ training runtime "
                    + "(https://github.com/brewlet/brewlet/blob/main/docs/appcds.md#22-minimum-jdk-version); "
                    + java.getAbsolutePath()
                    + " reports feature " + feature + ".");
        }

        try {
            Files.createDirectories(output.toPath().getParent());
            Files.deleteIfExists(output.toPath());
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to prepare AppCDS archive output", e);
        }
        Path trainingJar = stageTrainingInputs(cfg, entryMode, jar, trainingDir);

        getLog().info("Brewlet: generating AppCDS archive ("
                + (MODE_SIGNAL.equals(trainingMode) ? "signal mode: readiness → SIGTERM" : "self-terminating training run") + ")");
        getLog().info("  entry mode: " + entryMode);
        getLog().info("  java: " + java.getAbsolutePath() + " (feature " + feature + ")");
        getLog().info("  training jar: " + trainingJar + " (mtime pinned to 2000-01-01T00:00:00Z)");
        getLog().info("  archive: " + output.getAbsolutePath());
        getLog().info("  command: " + String.join(" ", command));

        int exitCode = MODE_SIGNAL.equals(trainingMode)
                ? runSignalTraining(command, trainingDir.toFile())
                : runSelfTerminating(command, trainingDir.toFile());

        if (!output.isFile() || output.length() == 0) {
            throw new MojoExecutionException("AppCDS training exited with code " + exitCode
                    + " but did not write a non-empty archive at " + output.getAbsolutePath()
                    + (MODE_SIGNAL.equals(trainingMode)
                        ? ". In signal mode the app's shutdown hook must let the JVM exit cleanly so "
                          + "dynamic CDS can flush the archive; check that SIGTERM triggers a graceful shutdown."
                        : ""));
        }
        if (exitCode != 0) {
            getLog().warn("AppCDS training exited with code " + exitCode
                    + " but produced a non-empty archive; keeping it.");
        }

        getLog().info("Brewlet: wrote AppCDS archive → " + output.getAbsolutePath());
        getLog().info("Attach it with brewlet:build/push using -Dbrewlet.cdsArchive="
                + output.getAbsolutePath());
    }

    /** {@code exit} mode: run the self-terminating training JVM to completion. */
    private int runSelfTerminating(List<String> command, File workingDir)
            throws MojoExecutionException {
        Process process;
        try {
            process = new ProcessBuilder(command).directory(workingDir).inheritIO().start();
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to launch AppCDS training JVM", e);
        }

        boolean finished;
        try {
            finished = process.waitFor(timeoutSeconds, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while waiting for AppCDS training JVM", e);
        }

        if (!finished) {
            destroy(process, 5);
            throw new MojoExecutionException("AppCDS training timed out after " + timeoutSeconds
                    + "s. This mode expects a self-terminating app: it must boot, exercise startup "
                    + "paths, and exit. Raise -Dbrewlet.appcds.timeoutSeconds=..., use "
                    + "-Dbrewlet.appcds.mode=signal for long-running servers, or supply a prebuilt "
                    + "archive with -Dbrewlet.cdsArchive=...");
        }
        return process.exitValue();
    }

    /**
     * {@code signal} mode: start the app, wait for readiness, then {@code SIGTERM}
     * so the JVM shutdown hook runs and dynamic CDS flushes the archive at exit.
     */
    private int runSignalTraining(List<String> command, File workingDir)
            throws MojoExecutionException {
        CountDownLatch logReady = new CountDownLatch(1);
        Pattern readyPattern = readyLog == null || readyLog.isBlank() ? null : Pattern.compile(readyLog);

        Process process;
        try {
            process = new ProcessBuilder(command)
                    .directory(workingDir)
                    .redirectErrorStream(true)
                    .start();
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to launch AppCDS training JVM", e);
        }

        // Tee the app's combined output to the Maven log and, when readyLog is set,
        // trip the latch on the first matching line.
        Thread pump = new Thread(() -> pumpOutput(process.getInputStream(), readyPattern, logReady),
                "brewlet-appcds-training-io");
        pump.setDaemon(true);
        pump.start();

        try {
            awaitReadiness(process, readyPattern, logReady);
            if (readyDelaySeconds > 0) {
                getLog().info("  readiness reached; settling for " + readyDelaySeconds + "s before SIGTERM");
                if (process.waitFor(readyDelaySeconds, TimeUnit.SECONDS)) {
                    getLog().warn("Training JVM exited during the settle delay; archive may be incomplete.");
                    return process.exitValue();
                }
            }

            getLog().info("  sending SIGTERM to let the app shut down and flush the archive");
            process.destroy();
            if (!process.waitFor(shutdownGraceSeconds, TimeUnit.SECONDS)) {
                process.destroyForcibly();
                process.waitFor(5, TimeUnit.SECONDS);
                throw new MojoExecutionException("Training JVM did not exit within "
                        + shutdownGraceSeconds + "s of SIGTERM; the archive was likely not flushed. "
                        + "Ensure the app shuts down gracefully on SIGTERM, or raise "
                        + "-Dbrewlet.appcds.shutdownGraceSeconds=...");
            }
            return process.exitValue();
        } catch (InterruptedException e) {
            destroy(process, 5);
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while training AppCDS archive", e);
        } finally {
            pump.interrupt();
        }
    }

    /**
     * Blocks until the training app signals readiness or the timeout elapses.
     * Honors {@code readyLog} (latch), {@code readyHttp} (poll), and a bare
     * {@code readyDelaySeconds} (handled by the caller). Fails fast if the process
     * dies before becoming ready.
     */
    private void awaitReadiness(Process process, Pattern readyPattern, CountDownLatch logReady)
            throws MojoExecutionException, InterruptedException {
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(timeoutSeconds);

        if (readyHttp != null && !readyHttp.isBlank()) {
            getLog().info("  waiting for readiness: HTTP 2xx/3xx from " + readyHttp);
            while (System.nanoTime() < deadline) {
                if (!process.isAlive()) {
                    throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                            + ") before " + readyHttp + " became ready.");
                }
                if (httpReady(readyHttp)) {
                    getLog().info("  readiness reached via HTTP probe");
                    return;
                }
                Thread.sleep(Math.max(50L, readyPollMillis));
            }
            throw new MojoExecutionException("Timed out after " + timeoutSeconds
                    + "s waiting for " + readyHttp + " to become ready.");
        }

        if (readyPattern != null) {
            getLog().info("  waiting for readiness: log match /" + readyPattern.pattern() + "/");
            long waitMs = Math.max(0L, TimeUnit.NANOSECONDS.toMillis(deadline - System.nanoTime()));
            if (!logReady.await(waitMs, TimeUnit.MILLISECONDS)) {
                if (!process.isAlive()) {
                    throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                            + ") before the readyLog pattern matched.");
                }
                throw new MojoExecutionException("Timed out after " + timeoutSeconds
                        + "s waiting for a log line matching /" + readyPattern.pattern() + "/.");
            }
            getLog().info("  readiness reached via log match");
            return;
        }

        // readyDelaySeconds-only: readiness is simply "still alive after the delay",
        // which the caller applies. Guard against an immediate crash here.
        if (!process.isAlive()) {
            throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                    + ") immediately; nothing to train.");
        }
    }

    /** Reads the process output, echoes each line, and trips {@code ready} on match. */
    private void pumpOutput(InputStream in, Pattern readyPattern, CountDownLatch ready) {
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(in, StandardCharsets.UTF_8))) {
            String line;
            while ((line = reader.readLine()) != null) {
                getLog().info("  [training] " + line);
                if (readyPattern != null && ready.getCount() > 0 && readyPattern.matcher(line).find()) {
                    ready.countDown();
                }
            }
        } catch (IOException ignored) {
            // Stream closes when the process exits; nothing actionable.
        }
    }

    private static void destroy(Process process, int graceSeconds) {
        process.destroy();
        try {
            if (!process.waitFor(graceSeconds, TimeUnit.SECONDS)) {
                process.destroyForcibly();
            }
        } catch (InterruptedException e) {
            process.destroyForcibly();
            Thread.currentThread().interrupt();
        }
    }

    /** Returns true when an HTTP(S) GET to {@code url} answers with a 2xx/3xx status. */
    static boolean httpReady(String url) {
        HttpURLConnection conn = null;
        try {
            conn = (HttpURLConnection) new URI(url).toURL().openConnection();
            conn.setRequestMethod("GET");
            conn.setConnectTimeout(2000);
            conn.setReadTimeout(2000);
            int code = conn.getResponseCode();
            return code >= 200 && code < 400;
        } catch (IOException | URISyntaxException | IllegalArgumentException e) {
            return false;
        } finally {
            if (conn != null) {
                conn.disconnect();
            }
        }
    }

    /** Normalizes and validates the training termination mode. */
    static String normalizeMode(String raw) throws MojoExecutionException {
        String m = raw == null ? MODE_EXIT : raw.trim().toLowerCase();
        if (m.isEmpty()) {
            return MODE_EXIT;
        }
        if (!MODE_EXIT.equals(m) && !MODE_SIGNAL.equals(m)) {
            throw new MojoExecutionException("brewlet.appcds.mode \"" + raw
                    + "\" is not recognized (expected \"exit\" or \"signal\").");
        }
        return m;
    }

    /** In signal mode at least one readiness signal must be configured. */
    static void validateReadiness(String readyLog, String readyHttp, int readyDelaySeconds)
            throws MojoExecutionException {
        boolean hasLog = readyLog != null && !readyLog.isBlank();
        boolean hasHttp = readyHttp != null && !readyHttp.isBlank();
        if (hasLog) {
            try {
                Pattern.compile(readyLog);
            } catch (RuntimeException e) {
                throw new MojoExecutionException("brewlet.appcds.readyLog is not a valid regex: "
                        + e.getMessage(), e);
            }
        }
        if (!hasLog && !hasHttp && readyDelaySeconds <= 0) {
            throw new MojoExecutionException("brewlet.appcds.mode=signal requires a readiness signal: "
                    + "set one of -Dbrewlet.appcds.readyLog=<regex>, "
                    + "-Dbrewlet.appcds.readyHttp=<url>, or -Dbrewlet.appcds.readyDelaySeconds=<n>.");
        }
    }

    private int detectTrainingJdkFeature(File java) throws MojoExecutionException {
        if (trainingJavaHome == null) {
            return parseJavaFeatureVersion(System.getProperty("java.version"));
        }
        Process process;
        try {
            process = new ProcessBuilder(java.getAbsolutePath(), "-version")
                    .redirectErrorStream(true)
                    .start();
            String output = new String(process.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
            if (!process.waitFor(10, TimeUnit.SECONDS)) {
                process.destroyForcibly();
                throw new MojoExecutionException("Timed out running " + java.getAbsolutePath()
                        + " -version to verify the JDK 21+ AppCDS floor.");
            }
            return parseJavaFeatureVersion(output);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to run " + java.getAbsolutePath()
                    + " -version to verify the JDK 21+ AppCDS floor", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while checking training JDK version", e);
        } catch (IllegalArgumentException e) {
            throw new MojoExecutionException("Could not parse training JDK version from "
                    + java.getAbsolutePath() + " -version", e);
        }
    }

    static File javaBinary(File javaHome) {
        String exe = System.getProperty("os.name", "").toLowerCase().contains("win")
                ? "java.exe"
                : "java";
        return javaHome.toPath().resolve("bin").resolve(exe).toFile();
    }

    static List<String> buildTrainingCommand(File javaBinary, JvmConfig cfg, File archiveOutput,
                                             String mainJar, List<String> trainingArgs) {
        List<String> command = new ArrayList<>();
        command.add(javaBinary.getAbsolutePath());
        command.addAll(appIntrinsicJvmArgs(cfg));
        command.add("-XX:ArchiveClassesAtExit=" + archiveOutput.getAbsolutePath());
        command.addAll(launchSelector(cfg, mainJar));
        if (trainingArgs != null) {
            command.addAll(trainingArgs);
        }
        return command;
    }

    /**
     * Builds the launch selector (everything after the JVM options) that mirrors
     * how the shim launches the artifact, so the training classpath/module path —
     * and therefore the recorded archive — matches production:
     * <ul>
     *   <li>{@code jar} → {@code -jar <mainJar>}</li>
     *   <li>{@code classpath} → {@code -cp <classPath> <mainClass>} (relative,
     *       {@code lib/*} left literal for the JVM to expand)</li>
     *   <li>{@code module} → {@code -p <modulePath> -m <module>[/<mainClass>]}</li>
     * </ul>
     * Path lists are joined with the platform separator so the training JVM starts
     * locally; HotSpot validates each classpath entry by basename+size+mtime (not
     * absolute path), so the archive still maps under the shim's own layout.
     */
    static List<String> launchSelector(JvmConfig cfg, String mainJar) {
        Entry entry = cfg.getEntry();
        String mode = entry == null || entry.getMode() == null || entry.getMode().isBlank()
                ? "jar"
                : entry.getMode();
        List<String> out = new ArrayList<>();
        switch (mode) {
            case "classpath": {
                List<String> cp = entry.getClassPath() != null && !entry.getClassPath().isEmpty()
                        ? entry.getClassPath()
                        : List.of(mainJar);
                out.add("-cp");
                out.add(String.join(File.pathSeparator, cp));
                if (entry.getMainClass() != null && !entry.getMainClass().isBlank()) {
                    out.add(entry.getMainClass());
                }
                break;
            }
            case "module": {
                List<String> mp = entry.getModulePath() != null && !entry.getModulePath().isEmpty()
                        ? entry.getModulePath()
                        : List.of(mainJar);
                out.add("-p");
                out.add(String.join(File.pathSeparator, mp));
                out.add("-m");
                out.add(entry.getMainClass() != null && !entry.getMainClass().isBlank()
                        ? entry.getModule() + "/" + entry.getMainClass()
                        : entry.getModule());
                break;
            }
            default:
                out.add("-jar");
                out.add(mainJar);
        }
        return out;
    }

    /**
     * Stages the training inputs into {@code trainingDir} so the runtime layout is
     * reproduced: the app JAR at {@code <mainJar>}, and — for layered classpath /
     * module apps — the resolved runtime dependencies under {@code lib/} or
     * {@code mods/} (matching the shim's {@code /app/lib} and {@code /app/mods}).
     * Every staged file's mtime is pinned to the canonical value so it matches the
     * node-side pinning and the archive maps. Returns the staged app JAR path.
     */
    private Path stageTrainingInputs(JvmConfig cfg, String entryMode, File jar, Path trainingDir)
            throws MojoExecutionException {
        try {
            Files.createDirectories(trainingDir);
            Path trainingJar = trainingDir.resolve(cfg.getMainJar());
            Files.copy(jar.toPath(), trainingJar, StandardCopyOption.REPLACE_EXISTING);
            Files.setLastModifiedTime(trainingJar, CANONICAL_APP_MTIME);

            Entry entry = cfg.getEntry();
            if ("classpath".equals(entryMode) && referencesDir(entry.getClassPath(), "lib")) {
                stageDeps(trainingDir.resolve("lib"), "class-path");
            } else if ("module".equals(entryMode) && referencesDir(entry.getModulePath(), "mods")) {
                stageDeps(trainingDir.resolve("mods"), "module-path");
            }
            return trainingJar;
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to stage AppCDS training inputs", e);
        }
    }

    /** Copies the resolved runtime dependency JARs into {@code dir}, pinning mtimes. */
    private void stageDeps(Path dir, String kind) throws IOException, MojoExecutionException {
        List<LayerBuilder.Dep> deps = collectRuntimeDeps();
        if (deps.isEmpty()) {
            throw new MojoExecutionException("entry references a " + kind + " directory but the "
                    + "project has no resolved runtime dependencies to stage; run 'mvn package' "
                    + "first, or push as a fat JAR.");
        }
        Files.createDirectories(dir);
        for (LayerBuilder.Dep dep : deps) {
            Path dest = dir.resolve(dep.fileName());
            Files.copy(dep.path(), dest, StandardCopyOption.REPLACE_EXISTING);
            Files.setLastModifiedTime(dest, CANONICAL_APP_MTIME);
        }
        getLog().info("  staged " + deps.size() + " dependency JAR(s) into "
                + dir.getFileName() + "/ (mtimes pinned)");
    }

    /** True when a class-path/module-path list references the given staging dir. */
    static boolean referencesDir(List<String> pathList, String dir) {
        if (pathList == null) {
            return false;
        }
        for (String p : pathList) {
            if (p == null) {
                continue;
            }
            String t = p.trim();
            if (t.equals(dir) || t.equals(dir + "/*") || t.startsWith(dir + "/")) {
                return true;
            }
        }
        return false;
    }

    static List<String> appIntrinsicJvmArgs(JvmConfig cfg) {
        List<String> args = new ArrayList<>();
        if (Boolean.TRUE.equals(cfg.getEnablePreview())) {
            args.add("--enable-preview");
        }
        if (cfg.getAddModules() != null && !cfg.getAddModules().isEmpty()) {
            args.add("--add-modules=" + String.join(",", cfg.getAddModules()));
        }
        if (cfg.getAddOpens() != null) {
            for (String token : cfg.getAddOpens()) {
                args.add("--add-opens");
                args.add(token);
            }
        }
        if (cfg.getAddExports() != null) {
            for (String token : cfg.getAddExports()) {
                args.add("--add-exports");
                args.add(token);
            }
        }
        if (cfg.getSystemProperties() != null) {
            cfg.getSystemProperties().entrySet().stream()
                    .sorted(Comparator.comparing(e -> e.getKey()))
                    .forEach(e -> args.add("-D" + e.getKey() + "=" + e.getValue()));
        }
        return args;
    }

    static int parseJavaFeatureVersion(String versionText) {
        if (versionText == null || versionText.isBlank()) {
            throw new IllegalArgumentException("empty java version");
        }
        Matcher matcher = Pattern.compile("version\\s+\"([^\"]+)\"").matcher(versionText);
        String token = matcher.find()
                ? matcher.group(1)
                : versionText.trim().split("\\s+")[0].replace("\"", "");
        if (token.startsWith("1.")) {
            Matcher legacy = Pattern.compile("^1\\.(\\d+)").matcher(token);
            if (legacy.find()) {
                return Integer.parseInt(legacy.group(1));
            }
        }
        Matcher current = Pattern.compile("^(\\d+)").matcher(token);
        if (current.find()) {
            return Integer.parseInt(current.group(1));
        }
        throw new IllegalArgumentException("unrecognized java version: " + versionText);
    }
}
