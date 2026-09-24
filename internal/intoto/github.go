package intoto

import (
	"errors"
	"fmt"
	"strings"
)

// GitHubBuildType is the build type GitHub's own attestations use
// (actions/attest-build-provenance) and the only family GitHub's attestation
// index accepts (its API answers 422 "unsupported build type" otherwise).
// Provenance signed from GitHub Actions uses this shape so that
// `gh attestation verify` can find it by digest; the release-specific
// inputs are carried in externalParameters.release.
const GitHubBuildType = "https://actions.github.io/buildtypes/workflow/v1"

// GitHubWorkflow describes the Actions run, from the runner environment.
type GitHubWorkflow struct {
	ServerURL         string // https://github.com
	Repository        string // owner/name
	Ref               string // refs/tags/v1.0.0
	WorkflowPath      string // .github/workflows/release.yml
	WorkflowRef       string // owner/name/.github/workflows/release.yml@refs/tags/v1.0.0
	EventName         string
	RepositoryID      string
	RepositoryOwnerID string
	RunnerEnvironment string // github-hosted | self-hosted
	RunID             string
	RunAttempt        string
}

// gitHubProvenance renders the GitHub Actions build type.
func gitHubProvenance(in BuildInputs, w *GitHubWorkflow) (Provenance, error) {
	if w.ServerURL == "" || w.Repository == "" || w.Ref == "" || w.WorkflowPath == "" {
		return Provenance{}, errors.New("provenance: the GitHub workflow context is incomplete (server, repository, ref, workflow path)")
	}
	wantRef := "refs/tags/" + in.Source.Tag
	if w.Ref != wantRef {
		return Provenance{}, fmt.Errorf("provenance: workflow ref %s is not %s", w.Ref, wantRef)
	}
	repoURL := strings.TrimRight(w.ServerURL, "/") + "/" + w.Repository
	ext := map[string]any{
		"workflow": map[string]any{"ref": w.Ref, "repository": repoURL, "path": w.WorkflowPath},
		"release":  releaseParameters(in),
	}
	internal := map[string]any{
		"github": map[string]any{
			"event_name": w.EventName, "repository_id": w.RepositoryID,
			"repository_owner_id": w.RepositoryOwnerID, "runner_environment": w.RunnerEnvironment,
		},
	}
	deps := []ResourceDescriptor{{URI: "git+" + repoURL + "@" + w.Ref, Digest: map[string]string{"gitCommit": in.Source.Commit}}}
	deps = append(deps, toolchainDeps(in)...)
	builderID := in.Builder
	if builderID == "" && w.WorkflowRef != "" {
		builderID = strings.TrimRight(w.ServerURL, "/") + "/" + w.WorkflowRef
	}
	invocation := in.InvocationID
	if invocation == "" && w.RunID != "" {
		invocation = repoURL + "/actions/runs/" + w.RunID + "/attempts/" + w.RunAttempt
	}
	p := Provenance{
		BuildDefinition: BuildDefinition{BuildType: GitHubBuildType, ExternalParameters: ext, InternalParameters: internal, ResolvedDependencies: deps},
		RunDetails:      RunDetails{Builder: Builder{ID: builderID, Version: map[string]string{"release-me": in.ToolVersion}}, Metadata: BuildMetadata{InvocationID: invocation}},
	}
	return p, nil
}

// releaseParameters are the release-specific inputs shared by both build types.
func releaseParameters(in BuildInputs) map[string]any {
	return map[string]any{
		"source":       map[string]any{"uri": in.Source.URI, "digest": map[string]string{"gitCommit": in.Source.Commit}, "tag": in.Source.Tag},
		"buildCommand": in.Command,
	}
}

// sourceFromGitHub derives the source from the first resolved dependency.
func (p *Provenance) sourceFromGitHub() (Source, error) {
	if len(p.BuildDefinition.ResolvedDependencies) == 0 {
		return Source{}, errors.New("provenance: no resolvedDependencies")
	}
	d := p.BuildDefinition.ResolvedDependencies[0]
	uri, commit := d.URI, d.Digest["gitCommit"]
	i := strings.LastIndex(uri, "@refs/tags/")
	if !strings.HasPrefix(uri, "git+") || i < 0 || commit == "" {
		return Source{}, fmt.Errorf("provenance: resolved dependency %q is not git+<repo>@refs/tags/<tag> with a gitCommit", uri)
	}
	s := Source{URI: uri, Commit: commit, Tag: uri[i+len("@refs/tags/"):]}
	// The release parameters, when present, must agree.
	if rel, ok := p.BuildDefinition.ExternalParameters["release"].(map[string]any); ok {
		if src, ok := rel["source"].(map[string]any); ok {
			if u, _ := src["uri"].(string); u != "" && u != s.URI {
				return Source{}, fmt.Errorf("provenance: release.source.uri %s disagrees with %s", u, s.URI)
			}
			if t, _ := src["tag"].(string); t != "" && t != s.Tag {
				return Source{}, fmt.Errorf("provenance: release.source.tag %s disagrees with %s", t, s.Tag)
			}
		}
	}
	if wf, ok := p.BuildDefinition.ExternalParameters["workflow"].(map[string]any); ok {
		if ref, _ := wf["ref"].(string); ref != "refs/tags/"+s.Tag {
			return Source{}, fmt.Errorf("provenance: workflow.ref %s is not refs/tags/%s", ref, s.Tag)
		}
	}
	return s, nil
}

// BuildCommand returns the declared build command, whichever shape the predicate has.
func (p *Provenance) BuildCommand() string {
	if c, ok := p.BuildDefinition.ExternalParameters["buildCommand"].(string); ok {
		return c
	}
	if rel, ok := p.BuildDefinition.ExternalParameters["release"].(map[string]any); ok {
		c, _ := rel["buildCommand"].(string)
		return c
	}
	return ""
}

// KnownBuildType reports whether a verifier understands the build type.
func KnownBuildType(t string) bool { return t == BuildType || t == GitHubBuildType }
