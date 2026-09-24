package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GitHub implements Host against the GitHub REST API (version 2022-11-28).
//
// Platform facts this relies on (docs.github.com/en/rest/releases, verified
// 2026-09-24): release assets carry a server-computed "digest"
// (sha256:<hex>) since 2025-06-03; a second asset with an existing name is
// rejected with 422; a failed upload can leave an asset in state "starter";
// drafts are invisible to the by-tag endpoint and are listed only for
// tokens with write access; a draft is published with PATCH draft=false and
// make_latest is only accepted at that point; each asset must be under
// 2 GiB and a release holds at most 1000 assets.
type GitHub struct {
	c        *Client
	api      string // https://api.github.com
	uploads  string // https://uploads.github.com
	server   string // https://github.com
	owner    string
	repo     string
	repoPath string
}

// NewGitHub creates a client. apiURL, uploadsURL and serverURL may be empty
// for github.com.
func NewGitHub(c *Client, apiURL, uploadsURL, serverURL, owner, repo string) *GitHub {
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	if uploadsURL == "" {
		uploadsURL = "https://uploads.github.com"
	}
	if serverURL == "" {
		serverURL = "https://github.com"
	}
	if c.AuthStyle == "" {
		c.AuthStyle = "Bearer"
	}
	if c.Headers == nil {
		c.Headers = map[string]string{}
	}
	c.Headers["Accept"] = "application/vnd.github+json"
	c.Headers["X-GitHub-Api-Version"] = "2022-11-28"
	return &GitHub{
		c: c, api: strings.TrimRight(apiURL, "/"), uploads: strings.TrimRight(uploadsURL, "/"),
		server: strings.TrimRight(serverURL, "/"), owner: owner, repo: repo,
		repoPath: "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo),
	}
}

func (g *GitHub) Kind() string    { return "github" }
func (g *GitHub) RepoURL() string { return g.server + "/" + g.owner + "/" + g.repo }

func (g *GitHub) DownloadURL(tag, name string) string {
	return g.RepoURL() + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
}

// Limits are documented constants; there is no API to query them.
func (g *GitHub) Limits(context.Context) (Limits, error) {
	return Limits{MaxAssetSize: 2 << 30, MaxAssets: 1000}, nil
}

type ghRelease struct {
	ID         int64     `json:"id"`
	TagName    string    `json:"tag_name"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Immutable  bool      `json:"immutable"`
	HTMLURL    string    `json:"html_url"`
	UploadURL  string    `json:"upload_url"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest"`
	URL    string `json:"url"`
}

func (r ghRelease) toRelease() *Release {
	rel := &Release{ID: r.ID, Tag: r.TagName, Name: r.Name, Body: r.Body, Draft: r.Draft, Prerelease: r.Prerelease, Immutable: r.Immutable, HTMLURL: r.HTMLURL}
	if i := strings.Index(r.UploadURL, "{"); i >= 0 {
		rel.uploadURL = r.UploadURL[:i]
	} else {
		rel.uploadURL = r.UploadURL
	}
	for _, a := range r.Assets {
		rel.Assets = append(rel.Assets, a.toAsset())
	}
	return rel
}

func (a ghAsset) toAsset() Asset {
	return Asset{ID: a.ID, Name: a.Name, Size: a.Size, State: a.State, Digest: a.Digest, URL: a.URL}
}

func (g *GitHub) TagCommit(ctx context.Context, tag string) (string, error) {
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.c.JSON(ctx, http.MethodGet, g.api+g.repoPath+"/git/ref/tags/"+url.PathEscape(tag), nil, &ref); err != nil {
		return "", err
	}
	if ref.Object.Type == "commit" {
		return ref.Object.SHA, nil
	}
	if ref.Object.Type != "tag" {
		return "", fmt.Errorf("tag %s points at a %s object", tag, ref.Object.Type)
	}
	var obj struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.c.JSON(ctx, http.MethodGet, g.api+g.repoPath+"/git/tags/"+ref.Object.SHA, nil, &obj); err != nil {
		return "", err
	}
	if obj.Object.Type != "commit" {
		return "", fmt.Errorf("tag %s: annotated tag points at a %s object", tag, obj.Object.Type)
	}
	return obj.Object.SHA, nil
}

func (g *GitHub) FindRelease(ctx context.Context, tag string) (*Release, error) {
	var r ghRelease
	err := g.c.JSON(ctx, http.MethodGet, g.api+g.repoPath+"/releases/tags/"+url.PathEscape(tag), nil, &r)
	if err == nil {
		return r.toRelease(), nil
	}
	if !IsNotFound(err) {
		return nil, err
	}
	// The by-tag endpoint never returns drafts; scan the list.
	all, err := g.ListReleases(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Tag == tag {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("release for %s: %w", tag, ErrNotFound)
}

// IsNotFound reports whether err wraps ErrNotFound.
func IsNotFound(err error) bool {
	return err != nil && (err == ErrNotFound || strings.HasSuffix(err.Error(), ErrNotFound.Error()))
}

func (g *GitHub) ListReleases(ctx context.Context) ([]Release, error) {
	var out []Release
	next := g.api + g.repoPath + "/releases?per_page=100"
	for next != "" {
		resp, err := g.c.DoIdempotent(ctx, http.MethodGet, next, nil, "")
		if err != nil {
			return nil, err
		}
		var page []ghRelease
		link := nextPage(resp.Header)
		if err := decode(http.MethodGet, next, resp, &page); err != nil {
			return nil, err
		}
		for _, r := range page {
			out = append(out, *r.toRelease())
		}
		next = link
	}
	return out, nil
}

func (g *GitHub) CreateDraft(ctx context.Context, p CreateParams) (*Release, error) {
	body := map[string]any{
		"tag_name": p.Tag, "target_commitish": p.Commit, "name": p.Name, "body": p.Body,
		"draft": true, "prerelease": p.Prerelease,
	}
	var r ghRelease
	if err := g.c.PostJSON(ctx, g.api+g.repoPath+"/releases", body, &r); err != nil {
		return nil, err
	}
	return r.toRelease(), nil
}

func (g *GitHub) Assets(ctx context.Context, rel *Release) ([]Asset, error) {
	var out []Asset
	next := fmt.Sprintf("%s%s/releases/%d/assets?per_page=100", g.api, g.repoPath, rel.ID)
	for next != "" {
		resp, err := g.c.DoIdempotent(ctx, http.MethodGet, next, nil, "")
		if err != nil {
			return nil, err
		}
		var page []ghAsset
		link := nextPage(resp.Header)
		if err := decode(http.MethodGet, next, resp, &page); err != nil {
			return nil, err
		}
		for _, a := range page {
			out = append(out, a.toAsset())
		}
		next = link
	}
	return out, nil
}

func (g *GitHub) Upload(ctx context.Context, rel *Release, name, contentType string, size int64, body io.Reader) (*Asset, error) {
	u := rel.uploadURL
	if u == "" {
		u = fmt.Sprintf("%s%s/releases/%d/assets", g.uploads, g.repoPath, rel.ID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+"?name="+url.QueryEscape(name), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	resp, err := g.c.Do(req)
	if err != nil {
		return nil, err
	}
	var a ghAsset
	if err := decode(http.MethodPost, u, resp, &a); err != nil {
		return nil, err
	}
	asset := a.toAsset()
	return &asset, nil
}

func (g *GitHub) DeleteAsset(ctx context.Context, _ *Release, a Asset) error {
	err := g.c.JSON(ctx, http.MethodDelete, fmt.Sprintf("%s%s/releases/assets/%d", g.api, g.repoPath, a.ID), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (g *GitHub) OpenAsset(ctx context.Context, _ *Release, a Asset) (io.ReadCloser, error) {
	u := a.URL
	if u == "" {
		u = fmt.Sprintf("%s%s/releases/assets/%d", g.api, g.repoPath, a.ID)
	}
	// The API answers with a redirect to object storage; net/http follows it
	// and drops the Authorization header when the host changes.
	return g.c.Stream(ctx, u, "application/octet-stream")
}

func (g *GitHub) Publish(ctx context.Context, rel *Release, p PublishParams) (*Release, error) {
	body := map[string]any{"draft": false, "make_latest": fmt.Sprint(p.Latest)}
	var r ghRelease
	if err := g.c.JSON(ctx, http.MethodPatch, fmt.Sprintf("%s%s/releases/%d", g.api, g.repoPath, rel.ID), body, &r); err != nil {
		return nil, err
	}
	return r.toRelease(), nil
}

func (g *GitHub) DeleteRelease(ctx context.Context, rel *Release) error {
	err := g.c.JSON(ctx, http.MethodDelete, fmt.Sprintf("%s%s/releases/%d", g.api, g.repoPath, rel.ID), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// GetRelease re-reads a release by id.
func (g *GitHub) GetRelease(ctx context.Context, id int64) (*Release, error) {
	var r ghRelease
	if err := g.c.JSON(ctx, http.MethodGet, fmt.Sprintf("%s%s/releases/%d", g.api, g.repoPath, id), nil, &r); err != nil {
		return nil, err
	}
	return r.toRelease(), nil
}

// ImmutableReleases reports whether the repository enforces immutable
// releases; ok is false when the token cannot read the setting.
func (g *GitHub) ImmutableReleases(ctx context.Context) (enabled, ok bool) {
	var s struct {
		Enabled bool `json:"enabled"`
	}
	if err := g.c.JSON(ctx, http.MethodGet, g.api+g.repoPath+"/immutable-releases", nil, &s); err != nil {
		return false, false
	}
	return s.Enabled, true
}

// StoreAttestation uploads a Sigstore bundle to the repository's attestation index.
func (g *GitHub) StoreAttestation(ctx context.Context, bundle []byte) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	req := struct {
		Bundle rawJSON `json:"bundle"`
	}{Bundle: rawJSON(bundle)}
	if err := g.c.PostJSON(ctx, g.api+g.repoPath+"/attestations", req, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// Attestations fetches the bundles recorded for a subject digest
// ("sha256:<hex>"). Bundles are served inline under API version 2022-11-28
// when present, otherwise through bundle_url as raw-snappy-compressed JSON.
func (g *GitHub) Attestations(ctx context.Context, digest, predicateType string) ([][]byte, error) {
	u := g.api + g.repoPath + "/attestations/" + url.PathEscape(digest) + "?per_page=100"
	if predicateType != "" {
		u += "&predicate_type=" + url.QueryEscape(predicateType)
	}
	var page struct {
		Attestations []struct {
			Bundle    rawMessage `json:"bundle"`
			BundleURL string     `json:"bundle_url"`
		} `json:"attestations"`
	}
	if err := g.c.JSON(ctx, http.MethodGet, u, nil, &page); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out [][]byte
	for _, a := range page.Attestations {
		if len(a.Bundle) > 0 && string(a.Bundle) != "null" {
			out = append(out, []byte(a.Bundle))
			continue
		}
		if a.BundleURL == "" {
			continue
		}
		rc, err := g.c.Stream(ctx, a.BundleURL, "")
		if err != nil {
			return nil, err
		}
		compressed, err := io.ReadAll(io.LimitReader(rc, 32<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		b, err := DecodeSnappy(compressed)
		if err != nil {
			return nil, fmt.Errorf("attestation bundle %s: %v", a.BundleURL, err)
		}
		out = append(out, b)
	}
	return out, nil
}

type rawMessage []byte

func (m *rawMessage) UnmarshalJSON(b []byte) error { *m = append((*m)[:0], b...); return nil }
