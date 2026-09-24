package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Forgejo implements Host against the Forgejo/Gitea API (also Codeberg).
//
// Platform facts this relies on (probed live against Forgejo 16.0.4 on
// 2026-09-24, see docs/HOSTS.md): exactly one release may exist per tag,
// drafts included (a second create answers 409); the by-tag endpoint returns
// a draft to a token with write access; attachments have no server digest,
// so bytes are re-downloaded to be checked; two attachments may carry the
// same name, so the client must check for duplicates itself; assets of a
// published release can still be added and deleted, so immutability is a
// policy this tool enforces, not the platform; creating a release for a tag
// that does not exist creates the tag, so the tag is checked first.
type Forgejo struct {
	c        *Client
	root     string // https://codeberg.org
	owner    string
	repo     string
	repoPath string
}

// NewForgejo creates a client for an instance root URL such as https://codeberg.org.
func NewForgejo(c *Client, rootURL, owner, repo string) *Forgejo {
	if c.AuthStyle == "" {
		c.AuthStyle = "token"
	}
	if c.Headers == nil {
		c.Headers = map[string]string{}
	}
	c.Headers["Accept"] = "application/json"
	return &Forgejo{
		c: c, root: strings.TrimRight(rootURL, "/"), owner: owner, repo: repo,
		repoPath: "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo),
	}
}

func (f *Forgejo) Kind() string    { return "forgejo" }
func (f *Forgejo) RepoURL() string { return f.root + "/" + f.owner + "/" + f.repo }
func (f *Forgejo) api() string     { return f.root + f.repoPath }

func (f *Forgejo) DownloadURL(tag, name string) string {
	return f.RepoURL() + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
}

// Limits reads the instance's attachment settings (public endpoint).
func (f *Forgejo) Limits(ctx context.Context) (Limits, error) {
	var s struct {
		Enabled      bool   `json:"enabled"`
		AllowedTypes string `json:"allowed_types"`
		MaxSize      int64  `json:"max_size"`
		MaxFiles     int    `json:"max_files"`
	}
	if err := f.c.JSON(ctx, http.MethodGet, f.root+"/api/v1/settings/attachment", nil, &s); err != nil {
		return Limits{}, err
	}
	if !s.Enabled {
		return Limits{}, fmt.Errorf("%s: attachments are disabled on this instance", f.root)
	}
	return Limits{MaxAssetSize: s.MaxSize << 20, AllowedTypes: s.AllowedTypes}, nil
}

type fjRelease struct {
	ID         int64     `json:"id"`
	TagName    string    `json:"tag_name"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	HTMLURL    string    `json:"html_url"`
	Assets     []fjAsset `json:"assets"`
}

type fjAsset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"browser_download_url"`
}

func (r fjRelease) toRelease() *Release {
	rel := &Release{ID: r.ID, Tag: r.TagName, Name: r.Name, Body: r.Body, Draft: r.Draft, Prerelease: r.Prerelease, HTMLURL: r.HTMLURL}
	for _, a := range r.Assets {
		rel.Assets = append(rel.Assets, a.toAsset())
	}
	return rel
}

func (a fjAsset) toAsset() Asset {
	return Asset{ID: a.ID, Name: a.Name, Size: a.Size, State: "uploaded", URL: a.DownloadURL}
}

func (f *Forgejo) TagCommit(ctx context.Context, tag string) (string, error) {
	var t struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := f.c.JSON(ctx, http.MethodGet, f.api()+"/tags/"+url.PathEscape(tag), nil, &t); err != nil {
		return "", err
	}
	if t.Commit.SHA == "" {
		return "", fmt.Errorf("tag %s: no commit in response", tag)
	}
	return t.Commit.SHA, nil
}

func (f *Forgejo) FindRelease(ctx context.Context, tag string) (*Release, error) {
	var r fjRelease
	if err := f.c.JSON(ctx, http.MethodGet, f.api()+"/releases/tags/"+url.PathEscape(tag), nil, &r); err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("release for %s: %w", tag, ErrNotFound)
		}
		return nil, err
	}
	return r.toRelease(), nil
}

func (f *Forgejo) ListReleases(ctx context.Context) ([]Release, error) {
	var out []Release
	for page := 1; ; page++ {
		var batch []fjRelease
		u := fmt.Sprintf("%s/releases?limit=50&page=%d", f.api(), page)
		if err := f.c.JSON(ctx, http.MethodGet, u, nil, &batch); err != nil {
			return nil, err
		}
		// The instance may cap "limit" below 50 (MAX_RESPONSE_ITEMS), so only an
		// empty page ends the listing.
		if len(batch) == 0 {
			return out, nil
		}
		for _, r := range batch {
			out = append(out, *r.toRelease())
		}
	}
}

func (f *Forgejo) CreateDraft(ctx context.Context, p CreateParams) (*Release, error) {
	body := map[string]any{"tag_name": p.Tag, "name": p.Name, "body": p.Body, "draft": true, "prerelease": p.Prerelease}
	var r fjRelease
	if err := f.c.PostJSON(ctx, f.api()+"/releases", body, &r); err != nil {
		return nil, err
	}
	return r.toRelease(), nil
}

func (f *Forgejo) Assets(ctx context.Context, rel *Release) ([]Asset, error) {
	var list []fjAsset
	if err := f.c.JSON(ctx, http.MethodGet, fmt.Sprintf("%s/releases/%d/assets", f.api(), rel.ID), nil, &list); err != nil {
		return nil, err
	}
	out := make([]Asset, 0, len(list))
	for _, a := range list {
		out = append(out, a.toAsset())
	}
	return out, nil
}

func (f *Forgejo) Upload(ctx context.Context, rel *Release, name, contentType string, size int64, body io.Reader) (*Asset, error) {
	u := fmt.Sprintf("%s/releases/%d/assets?name=%s", f.api(), rel.ID, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := f.c.Do(req)
	if err != nil {
		return nil, err
	}
	var a fjAsset
	if err := decode(http.MethodPost, u, resp, &a); err != nil {
		return nil, err
	}
	asset := a.toAsset()
	return &asset, nil
}

func (f *Forgejo) DeleteAsset(ctx context.Context, rel *Release, a Asset) error {
	err := f.c.JSON(ctx, http.MethodDelete, fmt.Sprintf("%s/releases/%d/assets/%d", f.api(), rel.ID, a.ID), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (f *Forgejo) OpenAsset(ctx context.Context, rel *Release, a Asset) (io.ReadCloser, error) {
	u := a.URL
	if u == "" {
		return nil, fmt.Errorf("asset %s has no download URL", a.Name)
	}
	return f.c.Stream(ctx, u, "application/octet-stream")
}

func (f *Forgejo) Publish(ctx context.Context, rel *Release, _ PublishParams) (*Release, error) {
	var r fjRelease
	if err := f.c.JSON(ctx, http.MethodPatch, fmt.Sprintf("%s/releases/%d", f.api(), rel.ID), map[string]any{"draft": false}, &r); err != nil {
		return nil, err
	}
	return r.toRelease(), nil
}

func (f *Forgejo) DeleteRelease(ctx context.Context, rel *Release) error {
	err := f.c.JSON(ctx, http.MethodDelete, fmt.Sprintf("%s/releases/%d", f.api(), rel.ID), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// GetRelease re-reads a release by id.
func (f *Forgejo) GetRelease(ctx context.Context, id int64) (*Release, error) {
	var r fjRelease
	if err := f.c.JSON(ctx, http.MethodGet, fmt.Sprintf("%s/releases/%d", f.api(), id), nil, &r); err != nil {
		return nil, err
	}
	return r.toRelease(), nil
}
