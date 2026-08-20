package com.example.greeter;

/**
 * A trivial library module used by the modular (JPMS) demo app. It lives in its
 * own module ({@code com.example.greeter}) that is shipped as a separate JAR in
 * the Brewlet {@code modulepath.layer.v1+tar} layer (unpacked to {@code /app/mods}).
 * The main module {@code com.example.orders} {@code requires} it, so a successful
 * response proves the module path was assembled and resolved at runtime.
 */
public final class Greeter {

    public static String greeting() {
        return "Hello from a MODULAR JPMS app on the module path via Brewlet!";
    }

    private Greeter() {
    }
}
