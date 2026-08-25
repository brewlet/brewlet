//go:build !linux

// containerd-shim-brewlet-v2 is the Brewlet containerd Runtime v2 shim.
//
// It is the JVM analogue of SpinKube's containerd-shim-spin/runwasi integration:
// instead of running a Spin-compatible Wasm application, it disassembles the
// Brewlet JAR *artifact*, assembles a sandbox from the node-resident JDK + the
// JAR, and launches `java -jar app.jar` under runc with the pod's CPU/memory
// limits applied as cgroups.
//
// The real containerd Runtime v2 TTRPC Task service (Create/Start/Kill/…) is
// implemented in main_linux.go / service_linux.go against the containerd shim
// framework — it only builds on Linux, where runc lives. On other platforms
// (e.g. a macOS dev box) only the portable bundle-assembly core is available so
// the harness and unit tests still build.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "prepare-bundle" {
		if err := prepareBundle(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "containerd-shim-brewlet-v2: TTRPC mode is Linux-only; on this platform use 'prepare-bundle <imageConfig.json> <bundleDir>'")
	os.Exit(2)
}
