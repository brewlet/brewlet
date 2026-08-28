package artifact

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// writeJar builds a JAR (zip) at path containing the given entry names (empty
// contents) so the native-arch scanner can inspect them.
func writeJar(t *testing.T, entries []string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "app.jar")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range entries {
		if _, err := zw.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanNativeArch(t *testing.T) {
	cases := []struct {
		name         string
		entries      []string
		wantArches   []string
		wantLibs     int
		wantUnrecogn int
	}{
		{
			name:       "pure bytecode jar",
			entries:    []string{"com/acme/App.class", "META-INF/MANIFEST.MF"},
			wantArches: nil,
			wantLibs:   0,
		},
		{
			name:       "linux x86_64 native only",
			entries:    []string{"com/acme/App.class", "META-INF/native/libnetty_tcnative_linux_x86_64.so"},
			wantArches: []string{"amd64"},
			wantLibs:   1,
		},
		{
			name:       "jna aarch64 native only",
			entries:    []string{"com/sun/jna/linux-aarch64/libjnidispatch.so"},
			wantArches: []string{"arm64"},
			wantLibs:   1,
		},
		{
			name:       "both arches bundled",
			entries:    []string{"native/linux-x86_64/libz.so", "native/linux-aarch64/libz.so", "native/win32-x86-64/z.dll"},
			wantArches: []string{"amd64", "arm64"},
			wantLibs:   3,
		},
		{
			name:         "native with no inferable arch",
			entries:      []string{"lib/libfoo.so"},
			wantArches:   nil,
			wantLibs:     1,
			wantUnrecogn: 1,
		},
		{
			name:       "dylib amd64",
			entries:    []string{"darwin/x86_64/libfoo.dylib"},
			wantArches: []string{"amd64"},
			wantLibs:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jar := writeJar(t, tc.entries)
			scan, err := ScanNativeArch(jar)
			if err != nil {
				t.Fatalf("ScanNativeArch: %v", err)
			}
			if got := scan.Arches; !equalStrings(got, tc.wantArches) {
				t.Errorf("Arches = %v, want %v", got, tc.wantArches)
			}
			if scan.NativeLibs != tc.wantLibs {
				t.Errorf("NativeLibs = %d, want %d", scan.NativeLibs, tc.wantLibs)
			}
			if len(scan.Unrecognized) != tc.wantUnrecogn {
				t.Errorf("Unrecognized = %v, want %d entries", scan.Unrecognized, tc.wantUnrecogn)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
