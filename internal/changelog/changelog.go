// Package changelog parses CHANGELOG.md files in the Keep a Changelog format
// (https://keepachangelog.com/en/2.0.0/) and enforces the structure that
// makes the file usable as the single source of the version: the topmost
// released section names the version, its body is the release notes, and an
// empty [Unreleased] section means the checkout is exactly that release.
package changelog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Quince-Pie/release-me/internal/semver"
)

// Release is one released section.
type Release struct {
	Version semver.Version
	Date    string // YYYY-MM-DD
	Yanked  bool
	Body    string // the section body without the heading, trimmed
	line    int
}

// Changelog is a parsed file.
type Changelog struct {
	Unreleased string // body of the [Unreleased] section, trimmed
	Releases   []Release
	Links      map[string]string // "Unreleased" or version -> URL from reference definitions
	lines      []string
	unrelLine  int
}

var (
	releaseHeading = regexp.MustCompile(`^## \[([^\]]+)\] - (\d{4}-\d{2}-\d{2})( \[YANKED\])?\s*$`)
	unreleasedHead = "## [Unreleased]"
	linkDef        = regexp.MustCompile(`^\[([^\]]+)\]:\s*(\S+)\s*$`)
	urlShape       = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://[^/\s]+/\S*$`)
	changeTypes    = map[string]bool{"Added": true, "Changed": true, "Deprecated": true, "Removed": true, "Fixed": true, "Security": true}
)

// Parse splits the file into sections. It reports structural errors that
// make the version undeterminable; Lint applies the remaining rules.
func Parse(text string) (*Changelog, error) {
	c := &Changelog{Links: map[string]string{}, lines: strings.Split(text, "\n"), unrelLine: -1}
	type section struct {
		start, end int // line indexes of the body
		rel        *Release
		unreleased bool
	}
	var sections []*section
	for i, line := range c.lines {
		if !strings.HasPrefix(line, "## ") {
			if m := linkDef.FindStringSubmatch(line); m != nil {
				c.Links[m[1]] = m[2]
			}
			continue
		}
		if len(sections) > 0 {
			sections[len(sections)-1].end = i
		}
		s := &section{start: i + 1, end: len(c.lines)}
		if strings.TrimRight(line, " \t") == unreleasedHead {
			if c.unrelLine >= 0 {
				return nil, fmt.Errorf("changelog: line %d: second [Unreleased] section", i+1)
			}
			c.unrelLine = i
			s.unreleased = true
		} else if m := releaseHeading.FindStringSubmatch(line); m != nil {
			v, err := semver.Parse(m[1])
			if err != nil {
				return nil, fmt.Errorf("changelog: line %d: %v", i+1, err)
			}
			s.rel = &Release{Version: v, Date: m[2], Yanked: m[3] != "", line: i + 1}
		} else {
			return nil, fmt.Errorf("changelog: line %d: unrecognized heading %q (want \"## [Unreleased]\" or \"## [X.Y.Z] - YYYY-MM-DD\")", i+1, line)
		}
		sections = append(sections, s)
	}
	for _, s := range sections {
		body := strings.TrimSpace(strings.Join(stripLinks(c.lines[s.start:s.end]), "\n"))
		if s.unreleased {
			c.Unreleased = body
		} else {
			s.rel.Body = body
			c.Releases = append(c.Releases, *s.rel)
		}
	}
	return c, nil
}

// stripLinks removes reference-definition lines from a section body.
func stripLinks(lines []string) []string {
	out := lines[:0:0]
	for _, l := range lines {
		if linkDef.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Latest returns the topmost released section.
func (c *Changelog) Latest() (Release, bool) {
	if len(c.Releases) == 0 {
		return Release{}, false
	}
	return c.Releases[0], true
}

// UnreleasedEmpty reports whether the [Unreleased] section has no content.
func (c *Changelog) UnreleasedEmpty() bool { return c.Unreleased == "" }

// Section returns the body of the section for version.
func (c *Changelog) Section(version semver.Version) (string, bool) {
	for _, r := range c.Releases {
		if semver.Compare(r.Version, version) == 0 && r.Version.Build == version.Build {
			return r.Body, true
		}
	}
	return "", false
}

// Lint checks the rules a release depends on:
//   - exactly one [Unreleased] section and it comes first;
//   - release headings carry a valid date and strictly descending versions;
//   - "###" headings are one of the six Keep a Changelog change types and
//     every subsection has content;
//   - each version and Unreleased has a link reference of the form scheme://host/...
func (c *Changelog) Lint() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.unrelLine < 0 {
		fail("no [Unreleased] section")
	} else {
		for i, l := range c.lines[:c.unrelLine] {
			if strings.HasPrefix(l, "## ") {
				fail("line %d: a release section precedes [Unreleased]", i+1)
				break
			}
		}
	}
	for i, r := range c.Releases {
		if _, err := time.Parse("2006-01-02", r.Date); err != nil {
			fail("line %d: invalid date %q", r.line, r.Date)
		}
		if i > 0 && semver.Compare(c.Releases[i-1].Version, r.Version) <= 0 {
			fail("line %d: version %s does not descend from %s", r.line, r.Version, c.Releases[i-1].Version)
		}
		if _, ok := c.Links[r.Version.String()]; !ok {
			fail("missing link reference [%s]: URL", r.Version)
		}
	}
	if _, ok := c.Links["Unreleased"]; !ok && c.unrelLine >= 0 {
		fail("missing link reference [Unreleased]: URL")
	}
	for name, u := range c.Links {
		if !urlShape.MatchString(u) {
			fail("link reference [%s] is not scheme://host/...: %q", name, u)
		}
	}
	// Subsections.
	inSection := false
	var subName string
	subLine, subContent := 0, false
	flush := func() {
		if subName != "" && !subContent {
			fail("line %d: empty \"### %s\" subsection", subLine, subName)
		}
		subName, subContent = "", false
	}
	for i, l := range c.lines {
		switch {
		case strings.HasPrefix(l, "## "):
			flush()
			inSection = true
		case strings.HasPrefix(l, "### "):
			flush()
			if !inSection {
				fail("line %d: subsection outside a section", i+1)
			}
			subName = strings.TrimSpace(l[4:])
			subLine = i + 1
			if !changeTypes[subName] {
				fail("line %d: \"### %s\" is not a Keep a Changelog change type", i+1, subName)
			}
		case strings.HasPrefix(l, "#"):
			flush()
			if inSection {
				fail("line %d: unexpected heading inside a section", i+1)
			}
		default:
			if subName != "" && strings.TrimSpace(l) != "" && !linkDef.MatchString(l) {
				subContent = true
			}
		}
	}
	flush()
	return errors.Join(errs...)
}

// Release moves the [Unreleased] content under a new "## [version] - date"
// heading and returns the new text. The caller supplies the link URLs so the
// package stays host-agnostic: compareURL is used for the new version and the
// updated [Unreleased] reference.
func (c *Changelog) Release(version semver.Version, date time.Time, compareURL func(from, to string) string) (string, error) {
	if c.unrelLine < 0 {
		return "", errors.New("changelog: no [Unreleased] section")
	}
	if c.UnreleasedEmpty() {
		return "", errors.New("changelog: [Unreleased] is empty; nothing to release")
	}
	if prev, ok := c.Latest(); ok && semver.Compare(version, prev.Version) <= 0 {
		return "", fmt.Errorf("changelog: %s does not follow %s", version, prev.Version)
	}
	if _, ok := c.Section(version); ok {
		return "", fmt.Errorf("changelog: %s already has a section", version)
	}
	v := version.String()
	var out []string
	for i, l := range c.lines {
		if i == c.unrelLine {
			out = append(out, unreleasedHead, "", fmt.Sprintf("## [%s] - %s", v, date.UTC().Format("2006-01-02")))
			continue
		}
		if m := linkDef.FindStringSubmatch(l); m != nil && m[1] == "Unreleased" {
			from := "v" + v
			if prev, ok := c.Latest(); ok {
				out = append(out, fmt.Sprintf("[Unreleased]: %s", compareURL(from, "HEAD")))
				out = append(out, fmt.Sprintf("[%s]: %s", v, compareURL("v"+prev.Version.String(), from)))
			} else {
				out = append(out, fmt.Sprintf("[Unreleased]: %s", compareURL(from, "HEAD")))
				out = append(out, fmt.Sprintf("[%s]: %s", v, compareURL("", from)))
			}
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), nil
}
