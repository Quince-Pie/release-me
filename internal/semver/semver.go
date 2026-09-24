// Package semver implements Semantic Versioning 2.0.0 (https://semver.org/spec/v2.0.0.html):
// strict parsing with the specification's own grammar and precedence as in item 11.
//
// It exists because the release contract needs two things no host provides
// consistently: a strict acceptance test for tag names and a total order to
// decide which release is "latest".
package semver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version. Pre holds the dot-separated
// pre-release identifiers (nil for a normal release); Build holds the build
// metadata without the leading '+' (ignored for precedence, as the spec says).
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

var errInvalid = errors.New("invalid semantic version")

// Parse parses s strictly: no leading "v", no whitespace, no leading zeroes in
// numeric fields (spec items 2 and 9), only [0-9A-Za-z-] in identifiers.
func Parse(s string) (Version, error) {
	var v Version
	rest := s
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		v.Build = rest[i+1:]
		rest = rest[:i]
		if v.Build == "" {
			return Version{}, fmt.Errorf("%w %q: empty build metadata", errInvalid, s)
		}
		for _, id := range strings.Split(v.Build, ".") {
			if !validIdentifier(id) {
				return Version{}, fmt.Errorf("%w %q: build identifier %q", errInvalid, s, id)
			}
		}
	}
	core := rest
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		core = rest[:i]
		pre := rest[i+1:]
		if pre == "" {
			return Version{}, fmt.Errorf("%w %q: empty pre-release", errInvalid, s)
		}
		for _, id := range strings.Split(pre, ".") {
			if !validIdentifier(id) || (isNumeric(id) && len(id) > 1 && id[0] == '0') {
				return Version{}, fmt.Errorf("%w %q: pre-release identifier %q", errInvalid, s, id)
			}
			v.Pre = append(v.Pre, id)
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w %q: expected MAJOR.MINOR.PATCH", errInvalid, s)
	}
	nums := make([]uint64, 3)
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("%w %q: numeric field %q", errInvalid, s, p)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("%w %q: %v", errInvalid, s, err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// ParseTag accepts "vX.Y.Z[-pre][+build]" (the tag convention) and returns the version.
func ParseTag(tag string) (Version, error) {
	if !strings.HasPrefix(tag, "v") {
		return Version{}, fmt.Errorf("tag %q does not start with 'v'", tag)
	}
	return Parse(tag[1:])
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func validIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-') {
			return false
		}
	}
	return true
}

// String renders the version without a "v" prefix.
func (v Version) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		b.WriteByte('-')
		b.WriteString(strings.Join(v.Pre, "."))
	}
	if v.Build != "" {
		b.WriteByte('+')
		b.WriteString(v.Build)
	}
	return b.String()
}

// IsPrerelease reports whether the version has pre-release identifiers.
func (v Version) IsPrerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or 1 by precedence (spec item 11). Build metadata is ignored.
func Compare(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return cmpU(a.Major, b.Major)
	case a.Minor != b.Minor:
		return cmpU(a.Minor, b.Minor)
	case a.Patch != b.Patch:
		return cmpU(a.Patch, b.Patch)
	}
	// A pre-release version has lower precedence than the associated normal version.
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := cmpIdentifier(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	// A larger set of pre-release fields has a higher precedence if all preceding are equal.
	return cmpU(uint64(len(a.Pre)), uint64(len(b.Pre)))
}

func cmpIdentifier(x, y string) int {
	xn, yn := isNumeric(x), isNumeric(y)
	switch {
	case xn && yn:
		// Numeric identifiers are compared numerically; they have no leading zeros,
		// so length then lexical order equals numeric order without overflow.
		if len(x) != len(y) {
			return cmpU(uint64(len(x)), uint64(len(y)))
		}
		return strings.Compare(x, y)
	case xn:
		return -1 // numeric identifiers always have lower precedence than alphanumeric
	case yn:
		return 1
	default:
		return strings.Compare(x, y) // ASCII sort order
	}
}

func cmpU(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
