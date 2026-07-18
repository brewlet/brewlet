package sh.brewlet.maven.plugin.util;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;

/**
 * Minimal, dependency-free USTAR tar writer that produces <strong>reproducible</strong>
 * archives: entries are emitted in a stable (name-sorted) order with zeroed
 * timestamps and fixed mode/uid/gid, so identical inputs always yield an
 * identical byte stream (and therefore an identical layer digest).
 *
 * <p>This is the discipline the layered-classpath design calls for — see the
 * "Determinism caveat" in
 * https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md — and it
 * is what makes cross-build and cross-app dependency-layer dedup actually work.
 *
 * <p>Only regular files are supported, which is all a classpath layer (a flat
 * set of JARs) needs. The implementation writes plain 512-byte USTAR headers;
 * entry names are limited to 100 bytes, which comfortably fits
 * {@code lib/<artifact>.jar} names.
 */
public final class TarWriter {

    private static final int BLOCK = 512;

    /** A single regular-file entry to be written into the archive. */
    public static final class Entry {
        final String name;
        final byte[] content;

        public Entry(String name, byte[] content) {
            this.name = name;
            this.content = content;
        }
    }

    private final List<Entry> entries = new ArrayList<>();

    /**
     * Adds a regular file to the archive. Order of {@code addFile} calls is
     * irrelevant — entries are sorted by name at {@link #toByteArray()} time to
     * guarantee reproducibility.
     */
    public TarWriter addFile(String name, byte[] content) {
        entries.add(new Entry(name, content));
        return this;
    }

    /** Serializes the archive to a deterministic byte array. */
    public byte[] toByteArray() throws IOException {
        List<Entry> sorted = new ArrayList<>(entries);
        sorted.sort(Comparator.comparing(e -> e.name));

        ByteArrayOutputStream out = new ByteArrayOutputStream();
        for (Entry e : sorted) {
            writeEntry(out, e);
        }
        // Two zero blocks terminate the archive.
        out.write(new byte[BLOCK * 2]);
        return out.toByteArray();
    }

    private void writeEntry(ByteArrayOutputStream out, Entry e) throws IOException {
        byte[] nameBytes = e.name.getBytes(StandardCharsets.UTF_8);
        if (nameBytes.length > 100) {
            throw new IOException("tar entry name too long (>100 bytes): " + e.name);
        }

        byte[] header = new byte[BLOCK];
        // name (0..99)
        System.arraycopy(nameBytes, 0, header, 0, nameBytes.length);
        // mode (100..107) — 0644, octal, NUL-terminated
        putOctal(header, 100, 8, 0644);
        // uid (108..115), gid (116..123) — 0, root
        putOctal(header, 108, 8, 0);
        putOctal(header, 116, 8, 0);
        // size (124..135)
        putOctal(header, 124, 12, e.content.length);
        // mtime (136..147) — zeroed for reproducibility
        putOctal(header, 136, 12, 0);
        // typeflag (156) — '0' regular file
        header[156] = '0';
        // USTAR magic (257..262) "ustar\0" and version (263..264) "00"
        System.arraycopy("ustar".getBytes(StandardCharsets.US_ASCII), 0, header, 257, 5);
        header[263] = '0';
        header[264] = '0';

        // checksum (148..155): computed with the checksum field filled with spaces.
        for (int i = 148; i < 156; i++) {
            header[i] = ' ';
        }
        int checksum = 0;
        for (byte b : header) {
            checksum += (b & 0xff);
        }
        // 6 octal digits, NUL, space
        putOctal(header, 148, 7, checksum);
        header[155] = ' ';

        out.write(header);
        out.write(e.content);
        // Pad the file data to a 512-byte boundary.
        int rem = e.content.length % BLOCK;
        if (rem != 0) {
            out.write(new byte[BLOCK - rem]);
        }
    }

    /**
     * Writes {@code value} as a zero-padded octal string of {@code len-1} digits
     * followed by a NUL terminator, per the USTAR field convention.
     */
    private static void putOctal(byte[] header, int offset, int len, long value) {
        String octal = Long.toOctalString(value);
        int digits = len - 1;
        StringBuilder sb = new StringBuilder();
        for (int i = octal.length(); i < digits; i++) {
            sb.append('0');
        }
        sb.append(octal);
        byte[] bytes = sb.toString().getBytes(StandardCharsets.US_ASCII);
        System.arraycopy(bytes, 0, header, offset, digits);
        header[offset + digits] = 0;
    }

}
