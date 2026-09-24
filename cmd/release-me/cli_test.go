package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const changelogSample = "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- Thing.\n\n[Unreleased]: https://example.com/compare/HEAD...HEAD\n"

func TestChangelogReleaseInterspersedFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CHANGELOG.md")
	os.WriteFile(path, []byte(changelogSample), 0o644)
	for _, args := range [][]string{
		{"--file", path, "release", "0.1.0", "--compare-url", "https://example.com/compare/{from}...{to}", "--date", "2026-09-24"},
		{"release", "--file", path, "--date", "2026-09-24", "0.1.0", "--compare-url", "https://example.com/compare/{from}...{to}"},
	} {
		os.WriteFile(path, []byte(changelogSample), 0o644)
		if err := cmdChangelog(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		out, _ := os.ReadFile(path)
		if !strings.Contains(string(out), "## [0.1.0] - 2026-09-24") {
			t.Fatalf("release section missing:\n%s", out)
		}
	}
	if err := cmdChangelog(context.Background(), []string{"--file", path, "lint"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdChangelog(context.Background(), []string{"--file", path, "section", "0.1.0"}); err != nil {
		t.Fatal(err)
	}
}

func TestPackAndManifestInterspersedFlags(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bin"), []byte("x"), 0o755)
	out := filepath.Join(dir, "a.tar.gz")
	if err := cmdPack(context.Background(), []string{"tool=" + filepath.Join(dir, "bin"), "--out", out, "--mtime", "1700000000"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdManifest(context.Background(), []string{"create", dir, "--out", "SUMS"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdManifest(context.Background(), []string{"check", "--name", "SUMS", dir}); err != nil {
		t.Fatal(err)
	}
}
