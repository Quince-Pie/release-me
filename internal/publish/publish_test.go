package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/host/fake"
	"github.com/Quince-Pie/release-me/internal/manifest"
)

type env struct {
	h    host.Host
	s    *fake.Server
	dir  string
	opts Options
}

func setup(t *testing.T, kind fake.Kind) *env {
	t.Helper()
	s := fake.New(kind, "o", "r", "tok")
	t.Cleanup(s.Close)
	s.Tags["v1.0.0"] = "c0ffee"
	c := &host.Client{HTTP: &http.Client{Timeout: 5 * time.Second}, Token: "tok",
		Clock: host.Clock{Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }}}
	var h host.Host
	if kind == fake.GitHub {
		h = host.NewGitHub(c, s.URL(), s.URL(), s.URL(), "o", "r")
	} else {
		h = host.NewForgejo(c, s.URL(), "o", "r")
	}
	dir := t.TempDir()
	write(t, dir, "tool_1.0.0_linux_amd64.tar.gz", strings.Repeat("A", 5000))
	write(t, dir, "tool_1.0.0_windows_amd64.zip", strings.Repeat("B", 3000))
	m, _ := manifest.FromDir(dir)
	os.WriteFile(filepath.Join(dir, manifest.FileName), m.Bytes(), 0o644)
	write(t, dir, "SHA256SUMS.sig", "signature bytes")
	return &env{h: h, s: s, dir: dir, opts: Options{Host: h, Tag: "v1.0.0", Commit: "c0ffee", Name: "v1.0.0", Body: "notes", Dir: dir, Logf: t.Logf}}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func kinds() []fake.Kind { return []fake.Kind{fake.GitHub, fake.Forgejo} }

func assertPublished(t *testing.T, e *env) {
	t.Helper()
	rel := e.s.Release("v1.0.0")
	if rel == nil || rel.Draft {
		t.Fatalf("release not published: %+v", rel)
	}
	assets, _, _ := LoadAssets(e.dir, "")
	if len(rel.Assets) != len(assets) {
		t.Fatalf("stored %d assets, want %d", len(rel.Assets), len(assets))
	}
	for _, a := range assets {
		found := false
		for _, s := range rel.Assets {
			if s.Name == a.Name {
				found = true
				if sum(string(s.Data)) != a.SHA256 || s.State != "uploaded" {
					t.Errorf("%s: stored bytes differ or state %s", a.Name, s.State)
				}
			}
		}
		if !found {
			t.Errorf("%s missing", a.Name)
		}
	}
}

func TestHappyPathAndIdempotentRerun(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			res, err := Run(context.Background(), e.opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.AlreadyPublished || len(res.Uploaded) != 4 || res.Release.Draft || !res.Latest {
				t.Fatalf("result %+v", res)
			}
			assertPublished(t, e)
			// Re-running is a verified no-op.
			res2, err := Run(context.Background(), e.opts)
			if err != nil || !res2.AlreadyPublished {
				t.Fatalf("rerun: %+v %v", res2, err)
			}
			// A rerun with different bytes must fail and leave the release untouched.
			write(t, e.dir, "tool_1.0.0_linux_amd64.tar.gz", "changed")
			m, _ := manifest.FromDir(e.dir)
			os.WriteFile(filepath.Join(e.dir, manifest.FileName), m.Bytes(), 0o644)
			before := len(e.s.Log)
			if _, err := Run(context.Background(), e.opts); err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("rerun with different bytes: %v", err)
			}
			for _, l := range e.s.Log[before:] {
				if strings.HasPrefix(l, "POST") || strings.HasPrefix(l, "PATCH") || strings.HasPrefix(l, "DELETE") {
					t.Errorf("published release was modified: %s", l)
				}
			}
		})
	}
}

func TestResumesAPartialDraft(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			ctx := context.Background()
			rel, err := e.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"})
			if err != nil {
				t.Fatal(err)
			}
			up := func(name, data string) {
				if _, err := e.h.Upload(ctx, rel, name, "application/octet-stream", int64(len(data)), strings.NewReader(data)); err != nil {
					t.Fatal(err)
				}
			}
			up("tool_1.0.0_linux_amd64.tar.gz", strings.Repeat("A", 5000)) // correct: reused
			up("tool_1.0.0_windows_amd64.zip", "wrong bytes")              // wrong: replaced
			up("stale.txt", "old")                                         // extra: deleted
			if kind == fake.Forgejo {
				up("SHA256SUMS.sig", "signature bytes")
				up("SHA256SUMS.sig", "signature bytes") // duplicate name: both removed, re-uploaded once
			} else {
				e.s.AddFault(fake.Fault{Method: "POST", PathPart: "/assets?name=SHA256SUMS.sig", Truncate: true})
				if _, err := e.h.Upload(ctx, rel, "SHA256SUMS.sig", "application/octet-stream", 15, strings.NewReader("signature bytes")); err == nil {
					t.Fatal("truncated upload should fail")
				}
			}
			res, err := Run(ctx, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !contains(res.Reused, "tool_1.0.0_linux_amd64.tar.gz") || !contains(res.Uploaded, "tool_1.0.0_windows_amd64.zip") || !contains(res.Deleted, "stale.txt") || !contains(res.Uploaded, "SHA256SUMS.sig") {
				t.Fatalf("result %+v", res)
			}
			assertPublished(t, e)
		})
	}
}

func TestUncertainOutcomes(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			// Draft creation takes effect but the response is lost.
			e.s.AddFault(fake.Fault{Method: "POST", PathPart: "/releases", AfterEffect: true})
			// One upload takes effect but the response is lost.
			e.s.AddFault(fake.Fault{Method: "POST", PathPart: "/assets?name=tool_1.0.0_linux", AfterEffect: true})
			// Publishing takes effect but the response is lost.
			e.s.AddFault(fake.Fault{Method: "PATCH", PathPart: "/releases/", AfterEffect: true})
			res, err := Run(context.Background(), e.opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.Release.Draft {
				t.Fatal("not published")
			}
			assertPublished(t, e)
			if n := len(e.s.Release("v1.0.0").Assets); n != 4 {
				t.Fatalf("%d assets stored, want 4 (no duplicates from the lost upload response)", n)
			}
		})
	}
}

func TestTransientUploadFailureIsRetriedNextRound(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			e.s.AddFault(fake.Fault{Method: "POST", PathPart: "/assets?name=tool_1.0.0_windows", Status: 502})
			e.s.AddFault(fake.Fault{Method: "POST", PathPart: "/assets?name=SHA256SUMS.sig", Drop: true})
			if _, err := Run(context.Background(), e.opts); err != nil {
				t.Fatal(err)
			}
			assertPublished(t, e)
		})
	}
}

func TestRefusals(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			ctx := context.Background()
			// Tag missing remotely: nothing is created.
			o := e.opts
			o.Tag = "v2.0.0"
			if _, err := Run(ctx, o); err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("missing tag: %v", err)
			}
			// Tag points elsewhere remotely.
			o = e.opts
			o.Commit = "deadbeef"
			if _, err := Run(ctx, o); err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("moved tag: %v", err)
			}
			if len(e.s.Releases) != 0 {
				t.Fatalf("a release was created despite the refusal")
			}
			// Manifest does not match the files.
			write(t, e.dir, "tool_1.0.0_windows_amd64.zip", "tampered")
			if _, err := Run(ctx, e.opts); err == nil || !strings.Contains(err.Error(), "do not match") {
				t.Fatalf("manifest mismatch: %v", err)
			}
			// Hostile asset name (with the manifest consistent again).
			write(t, e.dir, "tool_1.0.0_windows_amd64.zip", strings.Repeat("B", 3000))
			write(t, e.dir, "..evil", "x")
			if _, err := Run(ctx, e.opts); err == nil || !strings.Contains(err.Error(), "not publishable") {
				t.Fatalf("bad name: %v", err)
			}
			if e.s.Count("POST") != 0 {
				t.Fatal("something was uploaded despite refusals")
			}
		})
	}
}

func TestPersistentFailureLeavesDraft(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			e.s.AddFault(fake.Fault{Method: "POST", PathPart: "/assets?name=SHA256SUMS.sig", Status: 502, Remaining: 100})
			_, err := Run(context.Background(), e.opts)
			if err == nil || !strings.Contains(err.Error(), "did not converge") {
				t.Fatalf("expected convergence failure, got %v", err)
			}
			rel := e.s.Release("v1.0.0")
			if rel == nil || !rel.Draft {
				t.Fatalf("draft should remain for inspection: %+v", rel)
			}
			// Once the fault clears, a rerun completes from the same draft.
			e.s.ClearFaults()
			res, err := Run(context.Background(), e.opts)
			if err != nil || res.Release.ID != rel.ID || !contains(res.Reused, "tool_1.0.0_linux_amd64.tar.gz") {
				t.Fatalf("rerun after fault cleared: %+v %v", res, err)
			}
			assertPublished(t, e)
		})
	}
}

func TestLatestDecision(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			ctx := context.Background()
			// An existing published v2.0.0 means v1.0.0 is not latest.
			e.s.Tags["v2.0.0"] = "c2"
			r, _ := e.h.CreateDraft(ctx, host.CreateParams{Tag: "v2.0.0", Commit: "c2"})
			e.h.Publish(ctx, r, host.PublishParams{})
			res, err := Run(ctx, e.opts)
			if err != nil || res.Latest {
				t.Fatalf("v1.0.0 after v2.0.0: latest=%v err=%v", res.Latest, err)
			}
			if kind == fake.GitHub && e.s.Release("v1.0.0").Latest {
				t.Fatal("make_latest sent as true")
			}
		})
	}
}

func TestPrereleaseNeverLatest(t *testing.T) {
	e := setup(t, fake.GitHub)
	e.s.Tags["v1.1.0-rc.1"] = "c0ffee"
	o := e.opts
	o.Tag, o.Prerelease = "v1.1.0-rc.1", true
	res, err := Run(context.Background(), o)
	if err != nil || res.Latest || !res.Release.Prerelease {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestGitHubStaleDraftsRemoved(t *testing.T) {
	e := setup(t, fake.GitHub)
	ctx := context.Background()
	e.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"})
	e.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"})
	if _, err := Run(ctx, e.opts); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range e.s.Releases {
		if r.Tag == "v1.0.0" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d releases for the tag, want 1", n)
	}
}

func TestOversizeAssetRefused(t *testing.T) {
	e := setup(t, fake.GitHub)
	// Pretend the platform allows 1 KiB by wrapping the host.
	small := limited{e.h, host.Limits{MaxAssetSize: 1024}}
	o := e.opts
	o.Host = small
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "allows at most") {
		t.Fatalf("oversize: %v", err)
	}
	if len(e.s.Releases) != 0 {
		t.Fatal("release created despite the size refusal")
	}
	_ = bytes.Compare
}

type limited struct {
	host.Host
	l host.Limits
}

func (l limited) Limits(context.Context) (host.Limits, error) { return l.l, nil }

// A token that can upload but cannot download draft assets (Forgejo's
// Actions job token on the web download route) must stop the run at once,
// not delete and re-upload every asset for four rounds.
func TestDownloadFailureIsFatalNotReplaced(t *testing.T) {
	for _, kind := range kinds() {
		t.Run(string(kind), func(t *testing.T) {
			e := setup(t, kind)
			part := "/objects/"
			if kind == fake.Forgejo {
				part = "/attachments/"
			}
			e.s.AddFault(fake.Fault{Method: "GET", PathPart: part, Status: 404, Remaining: 1000})
			_, err := Run(context.Background(), e.opts)
			if err == nil || !strings.Contains(err.Error(), "cannot check stored asset") {
				t.Fatalf("expected a fatal download error, got %v", err)
			}
			if n := e.s.Count("DELETE"); n != 0 {
				t.Fatalf("%d assets were deleted despite the access failure", n)
			}
			if n := e.s.Count("POST"); n != 5 { // 1 draft + 4 uploads, once
				t.Fatalf("%d POSTs, want 5 (no re-uploads)", n)
			}
		})
	}
}
