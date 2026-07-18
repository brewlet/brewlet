package com.example.legacy;

/**
 * A deliberately non-modular ("legacy") helper that ships on the supplementary
 * class path in the mixed class-path + module-path demo (see
 * docs/layered-classpath-deployment.md §8.1). It lives in the unnamed module and
 * is unpacked to {@code /app/lib}, fed to {@code java -cp} alongside the module
 * path. Its mere presence, probed by {@code OrdersApp}, proves the mixed launch
 * assembled both {@code -cp} and {@code -p}.
 */
public final class Legacy {

    public static String banner() {
        return "legacy helper on the class path";
    }

    private Legacy() {
    }
}
