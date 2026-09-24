// Package intoto defines the in-toto Statement v1 and SLSA Provenance v1
// structures this tool signs (https://github.com/in-toto/attestation,
// https://slsa.dev/spec/v1.2/provenance) and the build type it documents.
package intoto

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Quince-Pie/release-me/internal/manifest"
)

const (
	StatementType  = "https://in-toto.io/Statement/v1"
	ProvenanceType = "https://slsa.dev/provenance/v1"
	PayloadType    = "application/vnd.in-toto+json"
	// BuildType identifies the build definition documented in docs/BUILD-TYPE.md.
	BuildType = "https://github.com/Quince-Pie/release-me/blob/main/docs/BUILD-TYPE.md#v1"
)

// ResourceDescriptor is in-toto's descriptor of a software artifact.
type ResourceDescriptor struct {
	Name        string            `json:"name,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Digest      map[string]string `json:"digest,omitempty"`
	Content     []byte            `json:"content,omitempty"`
	MediaType   string            `json:"mediaType,omitempty"`
	Annotations map[string]any    `json:"annotations,omitempty"`
}

// Statement is an in-toto Statement v1.
type Statement struct {
	Type          string               `json:"_type"`
	Subject       []ResourceDescriptor `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     any                  `json:"predicate"`
}

// Provenance is the SLSA v1 provenance predicate.
type Provenance struct {
	BuildDefinition BuildDefinition `json:"buildDefinition"`
	RunDetails      RunDetails      `json:"runDetails"`
}

type BuildDefinition struct {
	BuildType            string               `json:"buildType"`
	ExternalParameters   map[string]any       `json:"externalParameters"`
	InternalParameters   map[string]any       `json:"internalParameters,omitempty"`
	ResolvedDependencies []ResourceDescriptor `json:"resolvedDependencies,omitempty"`
}

type RunDetails struct {
	Builder    Builder              `json:"builder"`
	Metadata   BuildMetadata        `json:"metadata,omitempty"`
	Byproducts []ResourceDescriptor `json:"byproducts,omitempty"`
}

type Builder struct {
	ID                  string               `json:"id"`
	Version             map[string]string    `json:"version,omitempty"`
	BuilderDependencies []ResourceDescriptor `json:"builderDependencies,omitempty"`
}

type BuildMetadata struct {
	InvocationID string     `json:"invocationId,omitempty"`
	StartedOn    *time.Time `json:"startedOn,omitempty"`
	FinishedOn   *time.Time `json:"finishedOn,omitempty"`
}

// Source identifies the authorized source state.
type Source struct {
	// URI is "git+<clone URL>@refs/tags/<tag>".
	URI    string
	Commit string
	Tag    string
}

// BuildInputs are the declared inputs of the release build; see docs/BUILD-TYPE.md.
type BuildInputs struct {
	Source Source
	// Command is the exact build invocation recipients repeat, e.g.
	// "nix build .#release-assets".
	Command string
	// Toolchain lists declared toolchain pins, e.g. {"go": "go1.27.1", "nixpkgs": "<rev>"}.
	Toolchain map[string]string
	// Builder identifies who ran the build: the CI workflow identity URI or a
	// key-signing releaser's description.
	Builder string
	// Platform names the CI platform ("github", "forgejo", "local").
	Platform     string
	InvocationID string
	Started      time.Time
	Finished     time.Time
	// ToolVersion is this tool's version string.
	ToolVersion string
}

// ProvenanceStatement builds the statement for a manifest: every manifest
// entry becomes a subject with its sha256.
func ProvenanceStatement(m *manifest.Manifest, in BuildInputs) (*Statement, error) {
	if in.Source.URI == "" || in.Source.Commit == "" || in.Source.Tag == "" {
		return nil, errors.New("provenance: source uri, commit and tag are required")
	}
	if in.Builder == "" {
		return nil, errors.New("provenance: builder id is required")
	}
	if len(m.Entries) == 0 {
		return nil, errors.New("provenance: empty manifest")
	}
	subjects := make([]ResourceDescriptor, 0, len(m.Entries))
	for _, e := range m.Entries {
		subjects = append(subjects, ResourceDescriptor{Name: e.Name, Digest: map[string]string{"sha256": e.SHA256}})
	}
	ext := map[string]any{
		"source": map[string]any{
			"uri":    in.Source.URI,
			"digest": map[string]string{"gitCommit": in.Source.Commit},
			"tag":    in.Source.Tag,
		},
		"buildCommand": in.Command,
	}
	internal := map[string]any{"platform": in.Platform}
	deps := []ResourceDescriptor{{
		URI:    in.Source.URI,
		Digest: map[string]string{"gitCommit": in.Source.Commit},
	}}
	keys := make([]string, 0, len(in.Toolchain))
	for k := range in.Toolchain {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		deps = append(deps, ResourceDescriptor{Name: k, URI: in.Toolchain[k]})
	}
	p := Provenance{
		BuildDefinition: BuildDefinition{
			BuildType:            BuildType,
			ExternalParameters:   ext,
			InternalParameters:   internal,
			ResolvedDependencies: deps,
		},
		RunDetails: RunDetails{
			Builder:  Builder{ID: in.Builder, Version: map[string]string{"release-me": in.ToolVersion}},
			Metadata: BuildMetadata{InvocationID: in.InvocationID},
		},
	}
	if !in.Started.IsZero() {
		s := in.Started.UTC()
		p.RunDetails.Metadata.StartedOn = &s
	}
	if !in.Finished.IsZero() {
		f := in.Finished.UTC()
		p.RunDetails.Metadata.FinishedOn = &f
	}
	return &Statement{Type: StatementType, Subject: subjects, PredicateType: ProvenanceType, Predicate: p}, nil
}

// Marshal renders the statement as canonical-enough JSON: Go's encoder emits
// struct fields in declaration order and map keys sorted, so the same
// statement always renders to the same bytes.
func (s *Statement) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// ParseStatement decodes a statement and validates the parts a verifier relies on.
func ParseStatement(data []byte) (*Statement, error) {
	var s Statement
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("statement: %v", err)
	}
	if s.Type != StatementType {
		return nil, fmt.Errorf("statement: unexpected _type %q", s.Type)
	}
	if len(s.Subject) == 0 {
		return nil, errors.New("statement: no subjects")
	}
	for _, sub := range s.Subject {
		if sub.Digest["sha256"] == "" {
			return nil, fmt.Errorf("statement: subject %q has no sha256 digest", sub.Name)
		}
	}
	return &s, nil
}

// Subjects converts the statement's subjects to a manifest for comparison
// with the published SHA256SUMS.
func (s *Statement) Subjects() (*manifest.Manifest, error) {
	m := &manifest.Manifest{}
	for _, sub := range s.Subject {
		if err := m.Add(sub.Name, sub.Digest["sha256"]); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// ProvenancePredicate re-decodes the predicate as Provenance.
func (s *Statement) ProvenancePredicate() (*Provenance, error) {
	if s.PredicateType != ProvenanceType {
		return nil, fmt.Errorf("statement: predicate type %q is not %s", s.PredicateType, ProvenanceType)
	}
	raw, err := json.Marshal(s.Predicate)
	if err != nil {
		return nil, err
	}
	var p Provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("provenance: %v", err)
	}
	return &p, nil
}

// SourceOf extracts the source description from a provenance predicate.
func (p *Provenance) SourceOf() (Source, error) {
	src, _ := p.BuildDefinition.ExternalParameters["source"].(map[string]any)
	if src == nil {
		return Source{}, errors.New("provenance: no externalParameters.source")
	}
	s := Source{}
	s.URI, _ = src["uri"].(string)
	s.Tag, _ = src["tag"].(string)
	if d, ok := src["digest"].(map[string]any); ok {
		s.Commit, _ = d["gitCommit"].(string)
	}
	if s.URI == "" || s.Commit == "" || s.Tag == "" {
		return Source{}, errors.New("provenance: incomplete source (uri, tag, digest.gitCommit)")
	}
	return s, nil
}

// ParseStatementLoose decodes a statement of any in-toto version and digest
// algorithm, for inspecting bundles produced by other tools.
func ParseStatementLoose(data []byte) (*Statement, error) {
	var s Statement
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("statement: %v", err)
	}
	if len(s.Subject) == 0 {
		return nil, errors.New("statement: no subjects")
	}
	return &s, nil
}
