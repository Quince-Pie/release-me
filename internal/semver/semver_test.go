package semver

import "testing"

func TestSpecOrdering(t *testing.T) {
	// The ordered example of spec item 11, plus the pre-release example of item 9.
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "2.0.0", "2.1.0", "2.1.1",
	}
	for i := range ordered {
		for j := range ordered {
			a, err := Parse(ordered[i])
			if err != nil {
				t.Fatal(err)
			}
			b, err := Parse(ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestBuildMetadataIgnored(t *testing.T) {
	a, _ := Parse("1.0.0+20130313144700")
	b, _ := Parse("1.0.0+exp.sha.5114f85")
	if Compare(a, b) != 0 {
		t.Error("build metadata must not affect precedence")
	}
	if a.Build != "20130313144700" || a.String() != "1.0.0+20130313144700" {
		t.Errorf("round trip: %q", a.String())
	}
}

func TestInvalid(t *testing.T) {
	for _, s := range []string{
		"", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "v1.2.3", " 1.2.3", "1.2.3 ",
		"1.2.3-", "1.2.3-01", "1.2.3-a..b", "1.2.3-a_b", "1.2.3+", "1.2.3+a..b", "1.2.3-α",
		"-1.2.3", "1.2.3-+", "18446744073709551616.0.0",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted, want error", s)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{
		"0.0.0", "1.2.3", "1.2.3-0", "1.2.3-0.3.7", "1.2.3-x.7.z.92", "1.2.3-x-y-z.--",
		"1.2.3+build", "1.2.3-beta+exp.sha.5114f85", "1.0.0-alpha0.valid", "1.0.0-0A.is.legal",
		"18446744073709551615.0.0",
	} {
		v, err := Parse(s)
		if err != nil {
			t.Errorf("Parse(%q): %v", s, err)
			continue
		}
		if v.String() != s {
			t.Errorf("Parse(%q).String() = %q", s, v.String())
		}
	}
}

func TestParseTag(t *testing.T) {
	if v, err := ParseTag("v1.2.3-rc.1"); err != nil || v.String() != "1.2.3-rc.1" || !v.IsPrerelease() {
		t.Errorf("ParseTag: %v %v", v, err)
	}
	if _, err := ParseTag("1.2.3"); err == nil {
		t.Error("tag without v accepted")
	}
}
