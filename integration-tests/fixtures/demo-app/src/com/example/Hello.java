package com.example;

import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.OutputStream;
import java.lang.management.ManagementFactory;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

/**
 * Minimal dependency-free HTTP server used to prove the Brewlet PoC:
 * it is a self-executable JAR launched via {@code java -jar app.jar}.
 */
public final class Hello {

    public static void main(String[] args) throws IOException {
        int port = Integer.getInteger("server.port", 8080);
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);

        server.createContext("/hello", exchange -> {
            String body = "Hello from a JAR running directly on the node via Brewlet!\n"
                    + managedDependencyGreeting();
            respond(exchange, 200, body);
        });

        server.createContext("/info", exchange -> {
            Runtime rt = Runtime.getRuntime();
            long maxMb = rt.maxMemory() / (1024 * 1024);
            String body = String.format(
                    "java.version       = %s%n"
                  + "java.vendor        = %s%n"
                  + "availableProcessors= %d   (cgroup/JVM aware)%n"
                  + "Runtime.maxMemory  = %d MB (driven by -XX:MaxRAMPercentage)%n"
                  + "jvm.input.args     = %s%n",
                    System.getProperty("java.version"),
                    System.getProperty("java.vendor"),
                    rt.availableProcessors(),
                    maxMb,
                    ManagementFactory.getRuntimeMXBean().getInputArguments());
            respond(exchange, 200, body);
        });

        server.createContext("/healthz", exchange -> respond(exchange, 200, "ok\n"));

        server.setExecutor(null);
        System.out.printf("[demo-app] listening on :%d  (pid=%d, java=%s)%n",
                port, ProcessHandle.current().pid(), System.getProperty("java.version"));
        server.start();
    }

    private static void respond(com.sun.net.httpserver.HttpExchange exchange, int code, String body)
            throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().add("Content-Type", "text/plain; charset=utf-8");
        exchange.sendResponseHeaders(code, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
    }

    private static String managedDependencyGreeting() {
        try {
            Class<?> greeting = Class.forName("com.example.approved.Greeting");
            return greeting.getMethod("message").invoke(null) + "\n";
        } catch (ClassNotFoundException absent) {
            return "";
        } catch (ReflectiveOperationException invalidDependency) {
            throw new IllegalStateException("Could not invoke managed dependency", invalidDependency);
        }
    }

    private Hello() {
    }
}
