package main

import (
	"os"
	"path/filepath"
	"testing"
)

// mkJDK creates a fake node-resident JDK root with a bin/java under rootsDir.
func mkJDK(t *testing.T, rootsDir, name string) {
	t.Helper()
	bin := filepath.Join(rootsDir, name, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "java"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSelectJDKBareFeaturePicksLexicallyFirst(t *testing.T) {
	dir := t.TempDir()
	// Install two distributions of feature 21, out of lexical order, plus an
	// unrelated feature to prove filtering.
	mkJDK(t, dir, "temurin-21")
	mkJDK(t, dir, "microsoft-21")
	mkJDK(t, dir, "temurin-25")

	got, err := selectJDK(dir, "21")
	if err != nil {
		t.Fatalf("selectJDK: %v", err)
	}

	// No vendor preference: lexically-first among {microsoft-21, temurin-21}.
	if want := filepath.Join(dir, "microsoft-21"); got != want {
		t.Errorf("selectJDK(21) = %q, want %q (lexically-first, not temurin)", got, want)
	}
}

func TestSelectJDKResolvesImageRootJavaHome(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "zulu-21")
	home := filepath.Join(root, "usr", "lib", "jvm", "zulu21")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "java"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, jdkHomeMetadata), []byte("/usr/lib/jvm/zulu21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gotRoot, err := selectJDK(dir, "zulu-21")
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != root {
		t.Fatalf("selectJDK() = %q, want %q", gotRoot, root)
	}
	gotHome, err := resolveJDKHome(root)
	if err != nil {
		t.Fatal(err)
	}
	wantHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if gotHome != wantHome {
		t.Fatalf("resolveJDKHome() = %q, want %q", gotHome, wantHome)
	}
}

func TestResolveJDKHomeRejectsInvalidMetadata(t *testing.T) {
	for _, value := range []string{"", "usr/lib/jvm/zulu21", "/", "/usr/../etc"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, jdkHomeMetadata), []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveJDKHome(root); err == nil {
				t.Fatalf("resolveJDKHome() accepted invalid metadata %q", value)
			}
		})
	}
}

func TestResolveJDKHomeRejectsSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "bin", "java"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "runtime")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, jdkHomeMetadata), []byte("/runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJDKHome(root); err == nil {
		t.Fatal("resolveJDKHome() accepted a Java home symlink outside the runtime root")
	}
}

func TestSelectJDKEmptyRequestDefaultsFeature21(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "zulu-21")

	got, err := selectJDK(dir, "")
	if err != nil {
		t.Fatalf("selectJDK: %v", err)
	}
	if want := filepath.Join(dir, "zulu-21"); got != want {
		t.Errorf("selectJDK(\"\") = %q, want %q (feature 21, only installed distro)", got, want)
	}
}

func TestSelectJDKExplicitDistributionIsExact(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "temurin-21")

	got, err := selectJDK(dir, "temurin-21")
	if err != nil {
		t.Fatalf("selectJDK: %v", err)
	}
	if want := filepath.Join(dir, "temurin-21"); got != want {
		t.Errorf("selectJDK(temurin-21) = %q, want %q", got, want)
	}

	// A pinned distribution must NOT fall back to another vendor's build.
	if _, err := selectJDK(dir, "microsoft-21"); err == nil {
		t.Error("selectJDK(microsoft-21) succeeded, want NoCompatibleJDK (no silent temurin fallback)")
	}
}

func TestSelectJDKNoMatchingFeature(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "temurin-25")

	if _, err := selectJDK(dir, "21"); err == nil {
		t.Error("selectJDK(21) succeeded with only feature 25 installed, want NoCompatibleJDK")
	}
}

func TestSelectJDKIgnoresInactiveRoots(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "microsoft-21")
	mkJDK(t, dir, "temurin-21")
	if err := os.WriteFile(filepath.Join(dir, jdkActiveInventory), []byte("temurin-21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := selectJDK(dir, "21")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "temurin-21"); got != want {
		t.Fatalf("selectJDK(21) = %q, want active root %q", got, want)
	}
	if _, err := selectJDK(dir, "microsoft-21"); err == nil {
		t.Fatal("selectJDK(microsoft-21) selected an inactive root")
	}
}
