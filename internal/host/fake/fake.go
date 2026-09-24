// Package fake is an in-memory stand-in for the GitHub and Forgejo release
// APIs with fault injection, used to test the client and the publish
// algorithm against the platform behaviours that matter: server digests
// (GitHub) or none (Forgejo), duplicate-name rejection (GitHub) or silent
// acceptance (Forgejo), one-release-per-tag (Forgejo), "starter" assets left
// by failed uploads (GitHub), immutability (GitHub, optional), rate limits,
// transient 5xx and dropped connections. It models documented and probed
// behaviour only; live tests establish the rest.
package fake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind selects the API dialect.
type Kind string

const (
	GitHub  Kind = "github"
	Forgejo Kind = "forgejo"
)

// Asset is a stored attachment.
type Asset struct {
	ID    int64
	Name  string
	Data  []byte
	State string // "uploaded" or "starter" (GitHub failed upload)
	UUID  string
}

// Release is a stored release.
type Release struct {
	ID         int64
	Tag        string
	Name       string
	Body       string
	Draft      bool
	Prerelease bool
	Immutable  bool
	Latest     bool
	Target     string
	Assets     []*Asset
}

// Fault is an injected failure for the next matching request.
type Fault struct {
	Method   string
	PathPart string
	// Status, when non-zero, is answered with a JSON error body and the given headers.
	Status  int
	Headers map[string]string
	// Drop closes the connection without a response.
	Drop bool
	// AfterEffect applies the request's effect before failing (uncertain outcome).
	AfterEffect bool
	// Truncate stores a "starter" asset from a partial upload (GitHub) and answers 502.
	Truncate  bool
	Remaining int
}

// Server is one fake host.
type Server struct {
	Kind     Kind
	Owner    string
	Repo     string
	Token    string
	PageSize int
	// ImmutableReleases makes published releases immutable (GitHub).
	ImmutableReleases bool

	mu           sync.Mutex
	Releases     map[int64]*Release
	Tags         map[string]string // tag -> commit
	Attestations [][]byte
	Log          []string
	faults       []*Fault
	nextID       int64
	srv          *httptest.Server
}

// New starts a fake server.
func New(kind Kind, owner, repo, token string) *Server {
	s := &Server{Kind: kind, Owner: owner, Repo: repo, Token: token, PageSize: 100, Releases: map[int64]*Release{}, Tags: map[string]string{}, nextID: 1}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the server's base URL.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// AddFault schedules a failure for the next n matching requests.
func (s *Server) AddFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Remaining == 0 {
		f.Remaining = 1
	}
	s.faults = append(s.faults, &f)
}

// Release returns the single release for tag, or nil.
func (s *Server) Release(tag string) *Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.Releases {
		if r.Tag == tag {
			return r
		}
	}
	return nil
}

// Count counts requests whose "METHOD path" contains part.
func (s *Server) Count(part string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.Log {
		if strings.Contains(l, part) {
			n++
		}
	}
	return n
}

func (s *Server) takeFault(r *http.Request) *Fault {
	for i, f := range s.faults {
		if (f.Method == "" || f.Method == r.Method) && strings.Contains(r.URL.Path+"?"+r.URL.RawQuery, f.PathPart) {
			f.Remaining--
			if f.Remaining == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
			return f
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

func drop(w http.ResponseWriter) {
	if h, ok := w.(http.Hijacker); ok {
		conn, _, err := h.Hijack()
		if err == nil {
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			conn.Close()
			return
		}
	}
	panic("cannot hijack")
}

func (s *Server) authed(r *http.Request) bool {
	a := r.Header.Get("Authorization")
	return a == "Bearer "+s.Token || a == "token "+s.Token
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Log = append(s.Log, r.Method+" "+r.URL.Path)
	f := s.takeFault(r)
	s.mu.Unlock()
	if f == nil && s.serveDownload(w, r) {
		return
	}
	if f != nil && !f.AfterEffect && !f.Truncate {
		if f.Drop {
			io.Copy(io.Discard, r.Body)
			drop(w)
			return
		}
		for k, v := range f.Headers {
			w.Header().Set(k, v)
		}
		fail(w, f.Status, "injected fault")
		return
	}
	// Serve object storage redirect targets and public downloads without auth.
	switch s.Kind {
	case GitHub:
		s.github(w, r, f)
	case Forgejo:
		s.forgejo(w, r, f)
	}
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func (s *Server) newID() int64 {
	id := s.nextID
	s.nextID++
	return id
}

func (s *Server) findRelease(tag string) *Release {
	for _, r := range s.Releases {
		if r.Tag == tag {
			return r
		}
	}
	return nil
}

func (s *Server) sortedReleases() []*Release {
	var out []*Release
	for id := int64(1); id < s.nextID; id++ {
		if r, ok := s.Releases[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) paginate(w http.ResponseWriter, r *http.Request, n int) (lo, hi int) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	per := s.PageSize
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 && v < per {
		per = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v < per {
		per = v
	}
	lo = (page - 1) * per
	hi = lo + per
	if lo > n {
		lo = n
	}
	if hi > n {
		hi = n
	}
	if hi < n && s.Kind == GitHub {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, s.srv.URL, r.URL.Path, q.Encode()))
	}
	return lo, hi
}

// ---------------------------------------------------------------- GitHub

func (s *Server) ghRelease(r *Release) map[string]any {
	assets := []any{}
	for _, a := range r.Assets {
		assets = append(assets, s.ghAsset(a))
	}
	return map[string]any{
		"id": r.ID, "tag_name": r.Tag, "name": r.Name, "body": r.Body, "draft": r.Draft,
		"prerelease": r.Prerelease, "immutable": r.Immutable, "target_commitish": r.Target,
		"html_url":   fmt.Sprintf("%s/%s/%s/releases/tag/%s", s.srv.URL, s.Owner, s.Repo, r.Tag),
		"upload_url": fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets{?name,label}", s.srv.URL, s.Owner, s.Repo, r.ID),
		"assets":     assets,
	}
}

func (s *Server) ghAsset(a *Asset) map[string]any {
	var d any
	if a.State == "uploaded" {
		d = digest(a.Data)
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "size": len(a.Data), "state": a.State, "digest": d,
		"url":                  fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", s.srv.URL, s.Owner, s.Repo, a.ID),
		"browser_download_url": fmt.Sprintf("%s/%s/%s/releases/download/x/%s", s.srv.URL, s.Owner, s.Repo, a.Name),
	}
}

func (s *Server) github(w http.ResponseWriter, r *http.Request, f *Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := "/repos/" + s.Owner + "/" + s.Repo
	p := r.URL.Path
	if strings.HasPrefix(p, "/objects/") {
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/objects/"), 10, 64)
		for _, rel := range s.Releases {
			for _, a := range rel.Assets {
				if a.ID == id {
					w.Write(a.Data)
					return
				}
			}
		}
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(p, base) {
		http.NotFound(w, r)
		return
	}
	p = strings.TrimPrefix(p, base)
	authed := s.authed(r)
	write := r.Method != http.MethodGet
	if write && !authed {
		fail(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	switch {
	case strings.HasPrefix(p, "/git/ref/tags/"):
		tag := strings.TrimPrefix(p, "/git/ref/tags/")
		if c, ok := s.Tags[tag]; ok {
			writeJSON(w, 200, map[string]any{"ref": "refs/tags/" + tag, "object": map[string]any{"sha": c, "type": "commit"}})
			return
		}
		fail(w, 404, "Not Found")
	case p == "/immutable-releases":
		writeJSON(w, 200, map[string]any{"enabled": s.ImmutableReleases})
	case p == "/releases" && r.Method == http.MethodGet:
		all := s.sortedReleases()
		var visible []any
		for _, rel := range all {
			if rel.Draft && !authed {
				continue
			}
			visible = append(visible, s.ghRelease(rel))
		}
		lo, hi := s.paginate(w, r, len(visible))
		if visible == nil {
			visible = []any{}
		}
		writeJSON(w, 200, visible[lo:hi])
	case p == "/releases" && r.Method == http.MethodPost:
		var in struct {
			Tag, Target, Name, Body string
			Draft, Prerelease       bool
		}
		var raw map[string]any
		json.NewDecoder(r.Body).Decode(&raw)
		in.Tag, _ = raw["tag_name"].(string)
		in.Target, _ = raw["target_commitish"].(string)
		in.Name, _ = raw["name"].(string)
		in.Body, _ = raw["body"].(string)
		in.Draft, _ = raw["draft"].(bool)
		in.Prerelease, _ = raw["prerelease"].(bool)
		if in.Tag == "" {
			fail(w, 422, "Validation Failed")
			return
		}
		if !in.Draft {
			if ex := s.findRelease(in.Tag); ex != nil && !ex.Draft {
				fail(w, 422, "Validation Failed: already_exists")
				return
			}
			if _, ok := s.Tags[in.Tag]; !ok {
				s.Tags[in.Tag] = in.Target
			}
		}
		rel := &Release{ID: s.newID(), Tag: in.Tag, Name: in.Name, Body: in.Body, Draft: in.Draft, Prerelease: in.Prerelease, Target: in.Target}
		s.Releases[rel.ID] = rel
		if f != nil && f.AfterEffect {
			drop(w)
			return
		}
		writeJSON(w, 201, s.ghRelease(rel))
	case strings.HasPrefix(p, "/releases/tags/"):
		tag := strings.TrimPrefix(p, "/releases/tags/")
		if rel := s.findRelease(tag); rel != nil && !rel.Draft {
			writeJSON(w, 200, s.ghRelease(rel))
			return
		}
		fail(w, 404, "Not Found")
	case strings.HasPrefix(p, "/releases/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/releases/assets/"), 10, 64)
		var rel *Release
		var asset *Asset
		var idx int
		for _, rr := range s.Releases {
			for i, a := range rr.Assets {
				if a.ID == id {
					rel, asset, idx = rr, a, i
				}
			}
		}
		if asset == nil {
			fail(w, 404, "Not Found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			if r.Header.Get("Accept") == "application/octet-stream" {
				if rel.Draft && !authed {
					fail(w, 404, "Not Found")
					return
				}
				http.Redirect(w, r, fmt.Sprintf("%s/objects/%d", s.srv.URL, id), http.StatusFound)
				return
			}
			writeJSON(w, 200, s.ghAsset(asset))
		case http.MethodDelete:
			if rel.Immutable {
				fail(w, 422, "Release is immutable")
				return
			}
			rel.Assets = append(rel.Assets[:idx], rel.Assets[idx+1:]...)
			w.WriteHeader(204)
		default:
			fail(w, 405, "method")
		}
	case strings.HasPrefix(p, "/releases/"):
		rest := strings.TrimPrefix(p, "/releases/")
		idStr, sub, _ := strings.Cut(rest, "/")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		rel := s.Releases[id]
		if rel == nil || (rel.Draft && !authed) {
			fail(w, 404, "Not Found")
			return
		}
		switch {
		case sub == "" && r.Method == http.MethodGet:
			writeJSON(w, 200, s.ghRelease(rel))
		case sub == "" && r.Method == http.MethodPatch:
			var raw map[string]any
			json.NewDecoder(r.Body).Decode(&raw)
			if d, ok := raw["draft"].(bool); ok && !d && rel.Draft {
				if ex := s.findRelease(rel.Tag); ex != nil && ex != rel && !ex.Draft {
					fail(w, 422, "Validation Failed: tag_name already_exists")
					return
				}
				rel.Draft = false
				if _, ok := s.Tags[rel.Tag]; !ok {
					s.Tags[rel.Tag] = rel.Target
				}
				if s.ImmutableReleases {
					rel.Immutable = true
				}
			}
			if ml, ok := raw["make_latest"].(string); ok {
				rel.Latest = ml == "true"
			}
			if f != nil && f.AfterEffect {
				drop(w)
				return
			}
			writeJSON(w, 200, s.ghRelease(rel))
		case sub == "" && r.Method == http.MethodDelete:
			delete(s.Releases, id)
			w.WriteHeader(204)
		case sub == "assets" && r.Method == http.MethodGet:
			list := []any{}
			for _, a := range rel.Assets {
				list = append(list, s.ghAsset(a))
			}
			lo, hi := s.paginate(w, r, len(list))
			writeJSON(w, 200, list[lo:hi])
		case sub == "assets" && r.Method == http.MethodPost:
			name := r.URL.Query().Get("name")
			if rel.Immutable {
				fail(w, 422, "Release is immutable")
				return
			}
			for _, a := range rel.Assets {
				if a.Name == name {
					io.Copy(io.Discard, r.Body)
					writeJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []any{map[string]any{"resource": "ReleaseAsset", "code": "already_exists", "field": "name"}}})
					return
				}
			}
			if f != nil && f.Truncate {
				part := make([]byte, 3)
				io.ReadFull(r.Body, part)
				rel.Assets = append(rel.Assets, &Asset{ID: s.newID(), Name: name, Data: part, State: "starter"})
				fail(w, 502, "Bad Gateway")
				return
			}
			data, _ := io.ReadAll(r.Body)
			a := &Asset{ID: s.newID(), Name: name, Data: data, State: "uploaded"}
			rel.Assets = append(rel.Assets, a)
			if f != nil && f.AfterEffect {
				drop(w)
				return
			}
			writeJSON(w, 201, s.ghAsset(a))
		default:
			fail(w, 404, "Not Found")
		}
	case p == "/attestations" && r.Method == http.MethodPost:
		var in struct {
			Bundle json.RawMessage `json:"bundle"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		s.Attestations = append(s.Attestations, in.Bundle)
		writeJSON(w, 201, map[string]any{"id": len(s.Attestations)})
	case strings.HasPrefix(p, "/attestations/"):
		list := []any{}
		for _, b := range s.Attestations {
			list = append(list, map[string]any{"bundle": json.RawMessage(b), "bundle_url": ""})
		}
		writeJSON(w, 200, map[string]any{"attestations": list})
	default:
		fail(w, 404, "Not Found")
	}
}

// ---------------------------------------------------------------- Forgejo

func (s *Server) fjRelease(r *Release) map[string]any {
	assets := []any{}
	for _, a := range r.Assets {
		assets = append(assets, s.fjAsset(a))
	}
	return map[string]any{
		"id": r.ID, "tag_name": r.Tag, "name": r.Name, "body": r.Body, "draft": r.Draft, "prerelease": r.Prerelease,
		"html_url":   fmt.Sprintf("%s/%s/%s/releases/tag/%s", s.srv.URL, s.Owner, s.Repo, r.Tag),
		"upload_url": fmt.Sprintf("%s/api/v1/repos/%s/%s/releases/%d/assets", s.srv.URL, s.Owner, s.Repo, r.ID),
		"assets":     assets,
	}
}

func (s *Server) fjAsset(a *Asset) map[string]any {
	return map[string]any{
		"id": a.ID, "name": a.Name, "size": len(a.Data), "uuid": a.UUID, "type": "attachment",
		"browser_download_url": fmt.Sprintf("%s/attachments/%s", s.srv.URL, a.UUID),
	}
}

func (s *Server) forgejo(w http.ResponseWriter, r *http.Request, f *Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	authed := s.authed(r)
	p := r.URL.Path
	if strings.HasPrefix(p, "/attachments/") {
		uuid := strings.TrimPrefix(p, "/attachments/")
		for _, rel := range s.Releases {
			for _, a := range rel.Assets {
				if a.UUID == uuid {
					if rel.Draft && !authed {
						http.NotFound(w, r)
						return
					}
					w.Write(a.Data)
					return
				}
			}
		}
		http.NotFound(w, r)
		return
	}
	if p == "/api/v1/settings/attachment" {
		writeJSON(w, 200, map[string]any{"enabled": true, "allowed_types": "*/*", "max_size": 100, "max_files": 20})
		return
	}
	base := "/api/v1/repos/" + s.Owner + "/" + s.Repo
	if !strings.HasPrefix(p, base) {
		http.NotFound(w, r)
		return
	}
	p = strings.TrimPrefix(p, base)
	if r.Method != http.MethodGet && !authed {
		fail(w, 403, "token does not have at least one of required scope(s)")
		return
	}
	switch {
	case strings.HasPrefix(p, "/tags/"):
		tag := strings.TrimPrefix(p, "/tags/")
		if c, ok := s.Tags[tag]; ok {
			writeJSON(w, 200, map[string]any{"name": tag, "id": "tagobj", "commit": map[string]any{"sha": c}})
			return
		}
		fail(w, 404, "The target couldn't be found.")
	case p == "/releases" && r.Method == http.MethodGet:
		visible := []any{}
		for _, rel := range s.sortedReleases() {
			if rel.Draft && !authed {
				continue
			}
			visible = append(visible, s.fjRelease(rel))
		}
		lo, hi := s.paginate(w, r, len(visible))
		writeJSON(w, 200, visible[lo:hi])
	case p == "/releases" && r.Method == http.MethodPost:
		var raw map[string]any
		json.NewDecoder(r.Body).Decode(&raw)
		tag, _ := raw["tag_name"].(string)
		if tag == "" {
			fail(w, 422, "tag_name required")
			return
		}
		if s.findRelease(tag) != nil {
			fail(w, 409, "Release has no Tag")
			return
		}
		if _, ok := s.Tags[tag]; !ok {
			target, _ := raw["target_commitish"].(string)
			s.Tags[tag] = target // Forgejo creates the tag
		}
		rel := &Release{ID: s.newID(), Tag: tag}
		rel.Name, _ = raw["name"].(string)
		rel.Body, _ = raw["body"].(string)
		rel.Draft, _ = raw["draft"].(bool)
		rel.Prerelease, _ = raw["prerelease"].(bool)
		s.Releases[rel.ID] = rel
		if f != nil && f.AfterEffect {
			drop(w)
			return
		}
		writeJSON(w, 201, s.fjRelease(rel))
	case strings.HasPrefix(p, "/releases/tags/"):
		tag := strings.TrimPrefix(p, "/releases/tags/")
		if rel := s.findRelease(tag); rel != nil && (!rel.Draft || authed) {
			writeJSON(w, 200, s.fjRelease(rel))
			return
		}
		fail(w, 404, "The target couldn't be found.")
	case strings.HasPrefix(p, "/releases/"):
		rest := strings.TrimPrefix(p, "/releases/")
		parts := strings.Split(rest, "/")
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		rel := s.Releases[id]
		if rel == nil || (rel.Draft && !authed) {
			fail(w, 404, "The target couldn't be found.")
			return
		}
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			writeJSON(w, 200, s.fjRelease(rel))
		case len(parts) == 1 && r.Method == http.MethodPatch:
			var raw map[string]any
			json.NewDecoder(r.Body).Decode(&raw)
			if d, ok := raw["draft"].(bool); ok {
				rel.Draft = d
			}
			if f != nil && f.AfterEffect {
				drop(w)
				return
			}
			writeJSON(w, 200, s.fjRelease(rel))
		case len(parts) == 1 && r.Method == http.MethodDelete:
			delete(s.Releases, id)
			w.WriteHeader(204)
		case len(parts) == 2 && parts[1] == "assets" && r.Method == http.MethodGet:
			list := []any{}
			for _, a := range rel.Assets {
				list = append(list, s.fjAsset(a))
			}
			writeJSON(w, 200, list)
		case len(parts) == 2 && parts[1] == "assets" && r.Method == http.MethodPost:
			name := r.URL.Query().Get("name")
			if f != nil && f.Truncate {
				io.CopyN(io.Discard, r.Body, 3)
				fail(w, 500, "internal error")
				return
			}
			data, _ := io.ReadAll(r.Body)
			a := &Asset{ID: s.newID(), Name: name, Data: data, State: "uploaded", UUID: fmt.Sprintf("uuid-%d-%d", rel.ID, s.nextID)}
			rel.Assets = append(rel.Assets, a) // duplicates allowed, as on the real platform
			if f != nil && f.AfterEffect {
				drop(w)
				return
			}
			writeJSON(w, 201, s.fjAsset(a))
		case len(parts) == 3 && parts[1] == "assets":
			aid, _ := strconv.ParseInt(parts[2], 10, 64)
			for i, a := range rel.Assets {
				if a.ID == aid {
					switch r.Method {
					case http.MethodGet:
						writeJSON(w, 200, s.fjAsset(a))
					case http.MethodDelete:
						rel.Assets = append(rel.Assets[:i], rel.Assets[i+1:]...)
						w.WriteHeader(204)
					default:
						fail(w, 405, "method")
					}
					return
				}
			}
			fail(w, 404, "The target couldn't be found.")
		default:
			fail(w, 404, "The target couldn't be found.")
		}
	default:
		fail(w, 404, "The target couldn't be found.")
	}
}

// RateLimitHeaders builds GitHub-style secondary rate limit headers that
// clear in `after`.
func RateLimitHeaders(after time.Duration) map[string]string {
	return map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(time.Now().Add(after).Unix(), 10),
	}
}

// ClearFaults removes every pending fault.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// serveDownload answers the public /{owner}/{repo}/releases/download/{tag}/{name}
// URL for published releases, as both platforms do.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) bool {
	prefix := "/" + s.Owner + "/" + s.Repo + "/releases/download/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	tag, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if !ok {
		http.NotFound(w, r)
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rel := s.findRelease(tag)
	if rel == nil || rel.Draft {
		http.NotFound(w, r)
		return true
	}
	for _, a := range rel.Assets {
		if a.Name == name && a.State == "uploaded" {
			w.Write(a.Data)
			return true
		}
	}
	http.NotFound(w, r)
	return true
}
