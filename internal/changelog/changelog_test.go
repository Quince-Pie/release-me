package changelog

import (
	"strings"
	"testing"
	"time"

	"github.com/Quince-Pie/release-me/internal/semver"
)

const sample = `# Changelog

Intro text.

## [Unreleased]

## [0.2.0] - 2026-09-24

### Added

- Something new.

## [0.1.0] - 2026-09-01 [YANKED]

### Fixed

- A bug.

[Unreleased]: https://github.com/o/r/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/o/r/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/o/r/releases/tag/v0.1.0
`

func TestParseAndLint(t *testing.T) {
	c, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Lint(); err != nil {
		t.Fatalf("lint: %v", err)
	}
	if !c.UnreleasedEmpty() {
		t.Error("unreleased should be empty")
	}
	latest, ok := c.Latest()
	if !ok || latest.Version.String() != "0.2.0" || latest.Date != "2026-09-24" || latest.Yanked {
		t.Errorf("latest = %+v", latest)
	}
	if body, ok := c.Section(mustV("0.1.0")); !ok || body != "### Fixed\n\n- A bug." || !c.Releases[1].Yanked {
		t.Errorf("section 0.1.0 = %q", body)
	}
}

func mustV(s string) semver.Version {
	v, err := semver.Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestLintFailures(t *testing.T) {
	cases := map[string]string{
		"ascending":     strings.Replace(sample, "[0.2.0] - 2026-09-24", "[0.0.1] - 2026-09-24", 1),
		"bad date":      strings.Replace(sample, "2026-09-01", "2026-13-01", 1),
		"bad type":      strings.Replace(sample, "### Added", "### Dependencies", 1),
		"empty sub":     strings.Replace(sample, "- Something new.\n", "", 1),
		"missing link":  strings.Replace(sample, "[0.1.0]: https://github.com/o/r/releases/tag/v0.1.0\n", "", 1),
		"bad link":      strings.Replace(sample, "https://github.com/o/r/releases/tag/v0.1.0", "https///github.com:o/r", 1),
		"no unreleased": strings.Replace(sample, "## [Unreleased]\n", "", 1),
		"release first": strings.Replace(sample, "## [Unreleased]\n\n## [0.2.0] - 2026-09-24", "## [0.2.0] - 2026-09-24\n\n## [Unreleased]", 1),
	}
	for name, text := range cases {
		c, err := Parse(text)
		if err != nil {
			continue // a parse error is also a failure
		}
		if err := c.Lint(); err == nil {
			t.Errorf("%s: lint passed", name)
		}
	}
	if _, err := Parse(strings.Replace(sample, "## [0.2.0] - 2026-09-24", "## Version 0.2.0", 1)); err == nil {
		t.Error("unrecognized heading accepted")
	}
	if _, err := Parse(strings.Replace(sample, "## [0.2.0] - 2026-09-24", "## [02.0.0] - 2026-09-24", 1)); err == nil {
		t.Error("invalid version accepted")
	}
}

func TestRelease(t *testing.T) {
	text := strings.Replace(sample, "## [Unreleased]\n", "## [Unreleased]\n\n### Changed\n\n- New thing.\n", 1)
	c, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	cmp := func(from, to string) string { return "https://github.com/o/r/compare/" + from + "..." + to }
	out, err := c.Release(mustV("0.3.0"), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), cmp)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.Lint(); err != nil {
		t.Fatalf("released changelog fails lint: %v\n%s", err, out)
	}
	if !c2.UnreleasedEmpty() {
		t.Error("unreleased not emptied")
	}
	if l, _ := c2.Latest(); l.Version.String() != "0.3.0" || l.Date != "2026-10-01" || l.Body != "### Changed\n\n- New thing." {
		t.Errorf("latest = %+v", l)
	}
	if c2.Links["0.3.0"] != "https://github.com/o/r/compare/v0.2.0...v0.3.0" || c2.Links["Unreleased"] != "https://github.com/o/r/compare/v0.3.0...HEAD" {
		t.Errorf("links = %v", c2.Links)
	}
	if _, err := c.Release(mustV("0.1.5"), time.Now(), cmp); err == nil {
		t.Error("non-following version accepted")
	}
	c3, _ := Parse(sample)
	if _, err := c3.Release(mustV("0.3.0"), time.Now(), cmp); err == nil {
		t.Error("empty unreleased released")
	}
}
