package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.util.JdkVersionResolver;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Tests JDK version inference logic.
 */
class JdkVersionResolverTest {

    @Test
    void parseFeature_simpleVersion() {
        assertEquals(21, JdkVersionResolver.parseFeature("21"));
    }

    @Test
    void parseFeature_patchVersion() {
        assertEquals(21, JdkVersionResolver.parseFeature("21.0.1"));
    }

    @Test
    void parseFeature_oldStyleJava8() {
        assertEquals(8, JdkVersionResolver.parseFeature("1.8"));
    }

    @Test
    void parseFeature_oldStyleJava8WithPatch() {
        assertEquals(8, JdkVersionResolver.parseFeature("1.8.0_391"));
    }

    @Test
    void parseFeature_java17() {
        assertEquals(17, JdkVersionResolver.parseFeature("17.0.8+7"));
    }

    @Test
    void parseFeature_earlyAccessSuffix() {
        assertEquals(22, JdkVersionResolver.parseFeature("22-ea"));
    }

    @Test
    void parseFeature_nullFallsBackToDefault() {
        assertEquals(17, JdkVersionResolver.parseFeature(null));
    }

    @Test
    void parseFeature_emptyFallsBackToDefault() {
        assertEquals(17, JdkVersionResolver.parseFeature(""));
    }

    @Test
    void runningJdkFeature_returnsPositiveVersion() {
        int feature = JdkVersionResolver.runningJdkFeature();
        assertTrue(feature >= 8, "Running JDK feature should be >= 8, got " + feature);
    }
}
