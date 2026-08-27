// This file scans a JAR for bundled native libraries (JNI .so/.dll/.dylib) and
// infers the architecture(s) they target, so the CLI and Maven plugin can
// default the optional launch-config `arch` constraint for NON-portable
// artifacts. A pure-bytecode JAR bundles no natives and yields no arch (runs
// anywhere). See https://github.com/brewlet/site/blob/main/docs/multi-arch.md.
package artifact

import (
	"archive/zip"
	"fmt"
	"sort"
	"strings"
)

// NativeArchScan is the outcome of scanning a JAR for bundled native libraries.
type NativeArchScan struct {
	// Arches is the sorted set of recognized architectures inferred from bundled
	// native libraries (a subset of KnownArches). It is empty when the JAR
	// bundles no natives, or when none of the natives could be mapped to a known
	// arch (in which case Unrecognized is populated so tooling can warn).
	Arches []string
	// NativeLibs is the number of native-library entries found
	// (.so/.dll/.dylib/.jnilib).
	NativeLibs int
	// Unrecognized lists distinct native-library entry names whose architecture
	// could not be inferred, so tooling can warn rather than silently apply a
	// wrong (or no) constraint.
	Unrecognized []string
}

// nativeLibSuffixes are the file extensions of JNI-loadable native libraries
// across the platforms a JDK runs on.
var nativeLibSuffixes = []string{".so", ".dll", ".dylib", ".jnilib"}

// archTokens maps a recognized arch to the substrings that identify it in a
// native-library path or filename, following common packaging conventions
// (os-maven-plugin classifiers, JNA's <os>-<arch> dirs, netty-tcnative's shaded
// names, etc.). 32-bit tokens (x86, i386, arm) are deliberately excluded.
var archTokens = map[string][]string{
	"amd64": {"x86_64", "x86-64", "amd64", "x64"},
	"arm64": {"aarch64", "aarch_64", "arm64"},
}

// isNativeLib reports whether a zip entry name is a JNI-loadable native library.
func isNativeLib(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range nativeLibSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

// archOf infers the architecture(s) a native-library entry targets from its
// path/filename, returning the matching recognized arch tokens (usually one,
// occasionally none).
func archOf(name string) []string {
	lower := strings.ToLower(name)
	var out []string
	for arch, tokens := range archTokens {
		for _, t := range tokens {
			if strings.Contains(lower, t) {
				out = append(out, arch)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// ScanNativeArch opens jarPath and scans its entries for bundled native
// libraries, inferring the architecture(s) they target. It reads only entry
// names (no decompression) and is safe on any JAR. Note: it inspects the JAR's
// own entries and does not recurse into nested dependency JARs (e.g. a Spring
// Boot fat JAR's BOOT-INF/lib/*.jar); shaded/uber JARs — the usual carriers of
// bundled natives — extract natives to top-level paths and are handled.
func ScanNativeArch(jarPath string) (NativeArchScan, error) {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return NativeArchScan{}, fmt.Errorf("open jar %q: %w", jarPath, err)
	}
	defer zr.Close()

	set := map[string]struct{}{}
	var scan NativeArchScan
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isNativeLib(f.Name) {
			continue
		}
		scan.NativeLibs++
		arches := archOf(f.Name)
		if len(arches) == 0 {
			scan.Unrecognized = append(scan.Unrecognized, f.Name)
			continue
		}
		for _, a := range arches {
			set[a] = struct{}{}
		}
	}
	for a := range set {
		scan.Arches = append(scan.Arches, a)
	}
	sort.Strings(scan.Arches)
	return scan, nil
}
