package com.example.orders;

import com.example.greeter.Greeter;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.OutputStream;
import java.lang.management.ManagementFactory;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

/**
 * Modular (JPMS) twin of the {@code com.example.Hello} demo: a dependency-light
 * HTTP server that is launched on the module path via
 * {@code java -p <module-path> -m com.example.orders/com.example.orders.OrdersApp}.
 *
 * <p>It {@code requires com.example.greeter}, a separate module shipped in the
 * Brewlet module layer (unpacked to {@code /app/mods}). {@code /hello} returns
 * text produced by that other module, so a live response proves the module path
 * was assembled and cross-module resolution worked end-to-end.
 */
public final class OrdersApp {

    public static void main(String[] args) throws IOException {
        int port = Integer.getInteger("server.port", 8080);
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);

        server.createContext("/hello", exchange -> {
            boolean mixed = !System.getProperty("java.class.path", "").isEmpty();
            String greeting = mixed ? "MIXED: " + Greeter.greeting() : Greeter.greeting();
            respond(exchange, 200, greeting + "\n");
        });

        server.createContext("/info", exchange -> {
            Runtime rt = Runtime.getRuntime();
            long maxMb = rt.maxMemory() / (1024 * 1024);
            String body = String.format(
                    "java.version       = %s%n"
                  + "java.vendor        = %s%n"
                  + "main.module        = %s%n"
                  + "greeter.module     = %s%n"
                  + "java.class.path    = %s%n"
                  + "legacy.classpath   = %s%n"
                  + "availableProcessors= %d   (cgroup/JVM aware)%n"
                  + "Runtime.maxMemory  = %d MB (driven by -XX:MaxRAMPercentage)%n"
                  + "jvm.input.args     = %s%n",
                    System.getProperty("java.version"),
                    System.getProperty("java.vendor"),
                    OrdersApp.class.getModule().getName(),
                    Greeter.class.getModule().getName(),
                    classPath(),
                    legacyClasspathStatus(),
                    rt.availableProcessors(),
                    maxMb,
                    ManagementFactory.getRuntimeMXBean().getInputArguments());
            respond(exchange, 200, body);
        });

        server.createContext("/healthz", exchange -> respond(exchange, 200, "ok\n"));

        server.setExecutor(null);
        System.out.printf("[demo-module-app] listening on :%d  (pid=%d, module=%s, java=%s)%n",
                port, ProcessHandle.current().pid(),
                OrdersApp.class.getModule().getName(),
                System.getProperty("java.version"));
        server.start();
    }

    private static void respond(HttpExchange exchange, int code, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().add("Content-Type", "text/plain; charset=utf-8");
        exchange.sendResponseHeaders(code, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
    }

    /** Class path as the JVM sees it; empty in pure module mode, populated in the mixed form. */
    private static String classPath() {
        String cp = System.getProperty("java.class.path", "");
        return cp.isEmpty() ? "(empty — pure module path)" : cp;
    }

    /**
     * Reports whether the supplementary class path (mixed form) is live by probing
     * for a legacy, non-modular helper that only exists on the {@code -cp} entry
     * (unpacked to {@code /app/lib}). Finding it proves both {@code -cp} and
     * {@code -p} were assembled together. Absent in the pure module-path scenario.
     */
    private static String legacyClasspathStatus() {
        try {
            Class<?> c = Class.forName("com.example.legacy.Legacy", false,
                    ClassLoader.getSystemClassLoader());
            return "present (" + c.getName() + " on -cp)";
        } catch (ClassNotFoundException e) {
            return "(none)";
        }
    }

    private OrdersApp() {
    }
}
