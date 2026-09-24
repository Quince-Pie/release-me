package intoto

import (
	"strings"
	"testing"
	"time"

	"github.com/Quince-Pie/release-me/internal/manifest"
)

func TestProvenanceRoundTrip(t *testing.T) {
	m, _ := manifest.Parse(strings.NewReader(strings.Repeat("a", 64) + "  x.tar.gz\n" + strings.Repeat("b", 64) + "  y.zip\n"))
	in := BuildInputs{
		Source:    Source{URI: "git+https://github.com/o/r@refs/tags/v1.0.0", Commit: "c0ffee", Tag: "v1.0.0"},
		Command:   "nix build .#release-assets",
		Toolchain: map[string]string{"go": "https://go.dev/dl/go1.27.1", "nixpkgs": "github:NixOS/nixpkgs/abc"},
		Builder:   "https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1.0.0",
		Platform:  "github", InvocationID: "run-1", Started: time.Unix(1, 0), Finished: time.Unix(2, 0), ToolVersion: "0.1.0",
	}
	st, err := ProvenanceStatement(m, in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := st.Marshal()
	b, _ := st.Marshal()
	if string(a) != string(b) {
		t.Error("marshal is not deterministic")
	}
	back, err := ParseStatement(a)
	if err != nil {
		t.Fatal(err)
	}
	subj, _ := back.Subjects()
	if manifest.Diff(m, subj) != nil {
		t.Error("subjects do not round trip")
	}
	p, err := back.ProvenancePredicate()
	if err != nil {
		t.Fatal(err)
	}
	src, err := p.SourceOf()
	if err != nil || src != in.Source {
		t.Errorf("source = %+v, %v", src, err)
	}
	if p.BuildDefinition.BuildType != BuildType || p.RunDetails.Builder.ID != in.Builder || p.RunDetails.Metadata.StartedOn.Unix() != 1 {
		t.Errorf("predicate %+v", p)
	}
	if len(p.BuildDefinition.ResolvedDependencies) != 3 || p.BuildDefinition.ResolvedDependencies[1].Name != "go" {
		t.Errorf("deps %+v", p.BuildDefinition.ResolvedDependencies)
	}
	for _, bad := range []string{`{}`, `{"_type":"x"}`, `{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"a","digest":{"sha1":"x"}}],"predicateType":"p"}`} {
		if _, err := ParseStatement([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := ProvenanceStatement(m, BuildInputs{}); err == nil {
		t.Error("missing inputs accepted")
	}
}
