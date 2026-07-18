package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.util.TarWriter;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Verifies the {@link TarWriter} produces valid USTAR archives and, crucially,
 * <strong>reproducible</strong> output — identical inputs (in any order) yield an
 * identical byte stream, which is what makes classpath-layer digests stable and
 * dedup-able.
 */
class TarWriterTest {

    private static final int BLOCK = 512;

    @Test
    void reproducible_regardlessOfInsertionOrder() throws IOException {
        byte[] a = "class-a".getBytes(StandardCharsets.UTF_8);
        byte[] b = "class-bb".getBytes(StandardCharsets.UTF_8);

        byte[] first = new TarWriter()
                .addFile("alpha.jar", a)
                .addFile("beta.jar", b)
                .toByteArray();

        byte[] second = new TarWriter()
                .addFile("beta.jar", b)
                .addFile("alpha.jar", a)
                .toByteArray();

        assertArrayEquals(first, second,
                "tar output must be independent of addFile order");
    }

    @Test
    void archiveIsBlockAlignedAndTerminated() throws IOException {
        byte[] tar = new TarWriter()
                .addFile("x.jar", new byte[]{1, 2, 3})
                .toByteArray();

        assertEquals(0, tar.length % BLOCK, "archive must be 512-byte aligned");

        // Last two blocks must be all zeros (end-of-archive marker).
        for (int i = tar.length - 2 * BLOCK; i < tar.length; i++) {
            assertEquals(0, tar[i], "trailing blocks must be zeroed at " + i);
        }
    }

    @Test
    void headerHasValidChecksumAndZeroedMtime() throws IOException {
        byte[] content = "hello".getBytes(StandardCharsets.UTF_8);
        byte[] tar = new TarWriter().addFile("h.jar", content).toByteArray();

        // Recompute checksum over the first header with the checksum field spaced.
        byte[] header = new byte[BLOCK];
        System.arraycopy(tar, 0, header, 0, BLOCK);
        int stored = parseOctal(header, 148, 8);

        for (int i = 148; i < 156; i++) header[i] = ' ';
        int computed = 0;
        for (byte x : header) computed += (x & 0xff);
        assertEquals(computed, stored, "checksum field must match header bytes");

        // mtime (136..147) must be zeroed for reproducibility.
        assertEquals(0, parseOctal(tar, 136, 12), "mtime must be zeroed");
        // size field must equal content length.
        assertEquals(content.length, parseOctal(tar, 124, 12));
    }

    @Test
    void roundTripsThroughJdkTarReader() throws IOException {
        // The JDK has no tar reader, so parse the USTAR stream manually and
        // confirm names + contents survive.
        Map<String, byte[]> expected = new LinkedHashMap<>();
        expected.put("a.jar", "aaa".getBytes(StandardCharsets.UTF_8));
        expected.put("bb.jar", "bbbb".getBytes(StandardCharsets.UTF_8));

        TarWriter w = new TarWriter();
        expected.forEach(w::addFile);
        byte[] tar = w.toByteArray();

        Map<String, byte[]> parsed = readTar(tar);
        assertEquals(expected.keySet(), parsed.keySet());
        expected.forEach((name, bytes) ->
                assertArrayEquals(bytes, parsed.get(name), "content mismatch for " + name));
    }

    @Test
    void rejectsOverlongName() {
        String longName = "x".repeat(101) + ".jar";
        TarWriter w = new TarWriter().addFile(longName, new byte[]{0});
        assertThrows(IOException.class, w::toByteArray);
    }

    // --- helpers ---

    private static int parseOctal(byte[] buf, int offset, int len) {
        int value = 0;
        for (int i = offset; i < offset + len; i++) {
            byte c = buf[i];
            if (c == 0 || c == ' ') break;
            value = value * 8 + (c - '0');
        }
        return value;
    }

    private static Map<String, byte[]> readTar(byte[] tar) {
        Map<String, byte[]> out = new LinkedHashMap<>();
        ByteArrayInputStream in = new ByteArrayInputStream(tar);
        byte[] header = new byte[BLOCK];
        while (in.read(header, 0, BLOCK) == BLOCK) {
            // Zero header → end of archive.
            boolean allZero = true;
            for (byte b : header) if (b != 0) { allZero = false; break; }
            if (allZero) break;

            String name = new String(header, 0, nameLen(header), StandardCharsets.UTF_8);
            int size = parseOctal(header, 124, 12);
            byte[] content = new byte[size];
            assertEquals(size, in.read(content, 0, size));
            out.put(name, content);
            int pad = size % BLOCK;
            if (pad != 0) in.skip(BLOCK - pad);
        }
        return out;
    }

    private static int nameLen(byte[] header) {
        int i = 0;
        while (i < 100 && header[i] != 0) i++;
        return i;
    }
}
