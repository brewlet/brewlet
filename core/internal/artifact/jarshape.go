package artifact

import (
	"archive/zip"
	"fmt"
	"strings"
)

// ValidateThinJar rejects nested dependency JARs. It catches standard Spring
// Boot/WAR fat layouts and generic nested-JAR packaging. A trusted build
// attestation remains necessary to make claims about arbitrarily shaded classes.
func ValidateThinJar(path string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("open application JAR %q: %w", path, err)
	}
	defer reader.Close()
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry.Name), ".jar") {
			return fmt.Errorf("application JAR is not thin: embedded dependency %q", entry.Name)
		}
	}
	return nil
}
