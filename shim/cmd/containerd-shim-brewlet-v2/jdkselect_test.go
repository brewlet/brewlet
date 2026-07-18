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
