package host_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/host/fake"
)

func newClient(token string) *host.Client {
	return &host.Client{
		HTTP: &http.Client{Timeout: 10 * time.Second}, Token: token, UserAgent: "test",
		Clock: host.Clock{Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }},
	}
}

func hosts(t *testing.T) map[string]struct {
	h host.Host
	s *fake.Server
} {
	gh := fake.New(fake.GitHub, "o", "r", "tok")
	fj := fake.New(fake.Forgejo, "o", "r", "tok")
	t.Cleanup(gh.Close)
	t.Cleanup(fj.Close)
	gh.Tags["v1.0.0"] = "c0ffee"
	fj.Tags["v1.0.0"] = "c0ffee"
	return map[string]struct {
		h host.Host
		s *fake.Server
	}{
		"github":  {host.NewGitHub(newClient("tok"), gh.URL(), gh.URL(), gh.URL(), "o", "r"), gh},
		"forgejo": {host.NewForgejo(newClient("tok"), fj.URL(), "o", "r"), fj},
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	for name, hs := range hosts(t) {
		t.Run(name, func(t *testing.T) {
			h := hs.h
			if c, err := h.TagCommit(ctx, "v1.0.0"); err != nil || c != "c0ffee" {
				t.Fatalf("TagCommit: %s %v", c, err)
			}
			if _, err := h.TagCommit(ctx, "v9.9.9"); !host.IsNotFound(err) {
				t.Fatalf("missing tag: %v", err)
			}
			if _, err := h.FindRelease(ctx, "v1.0.0"); !host.IsNotFound(err) {
				t.Fatalf("FindRelease before create: %v", err)
			}
			rel, err := h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Name: "v1.0.0", Body: "notes", Commit: "c0ffee"})
			if err != nil {
				t.Fatal(err)
			}
			if !rel.Draft || rel.Tag != "v1.0.0" {
				t.Fatalf("draft: %+v", rel)
			}
			found, err := h.FindRelease(ctx, "v1.0.0")
			if err != nil || found.ID != rel.ID || !found.Draft {
				t.Fatalf("FindRelease draft: %+v %v", found, err)
			}
			data := []byte("asset bytes")
			a, err := h.Upload(ctx, rel, "x.tar.gz", "application/octet-stream", int64(len(data)), bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if a.Name != "x.tar.gz" || a.Size != int64(len(data)) {
				t.Fatalf("asset: %+v", a)
			}
			if name == "github" && a.Digest != "sha256:"+sum(data) {
				t.Fatalf("github digest: %q", a.Digest)
			}
			if name == "forgejo" && a.Digest != "" {
				t.Fatalf("forgejo should have no digest")
			}
			rc, err := h.OpenAsset(ctx, rel, *a)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, data) {
				t.Fatalf("download of draft asset: %q", got)
			}
			list, err := h.Assets(ctx, rel)
			if err != nil || len(list) != 1 {
				t.Fatalf("Assets: %v %v", list, err)
			}
			pub, err := h.Publish(ctx, rel, host.PublishParams{Latest: true})
			if err != nil || pub.Draft {
				t.Fatalf("Publish: %+v %v", pub, err)
			}
			if got, err := h.FindRelease(ctx, "v1.0.0"); err != nil || got.Draft || len(got.Assets) != 1 {
				t.Fatalf("after publish: %+v %v", got, err)
			}
			if err := h.DeleteAsset(ctx, rel, *a); err != nil {
				t.Fatal(err)
			}
			if err := h.DeleteAsset(ctx, rel, *a); err != nil {
				t.Fatalf("second delete should be a no-op: %v", err)
			}
			if err := h.DeleteRelease(ctx, rel); err != nil {
				t.Fatal(err)
			}
			if _, err := h.FindRelease(ctx, "v1.0.0"); !host.IsNotFound(err) {
				t.Fatalf("after delete: %v", err)
			}
		})
	}
}

func TestPlatformDifferences(t *testing.T) {
	ctx := context.Background()
	hs := hosts(t)
	gh, fj := hs["github"], hs["forgejo"]
	data := []byte("d")

	// GitHub rejects a duplicate name with 422; Forgejo silently accepts it.
	rel, _ := gh.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"})
	gh.h.Upload(ctx, rel, "a", "application/octet-stream", 1, bytes.NewReader(data))
	if _, err := gh.h.Upload(ctx, rel, "a", "application/octet-stream", 1, bytes.NewReader(data)); !host.IsStatus(err, 422) {
		t.Errorf("github duplicate: %v", err)
	}
	frel, _ := fj.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"})
	fj.h.Upload(ctx, frel, "a", "application/octet-stream", 1, bytes.NewReader(data))
	if _, err := fj.h.Upload(ctx, frel, "a", "application/octet-stream", 1, bytes.NewReader(data)); err != nil {
		t.Errorf("forgejo duplicate should be accepted by the platform: %v", err)
	}
	if list, _ := fj.h.Assets(ctx, frel); len(list) != 2 {
		t.Errorf("forgejo should now hold two assets named a: %v", list)
	}
	// Forgejo: one release per tag.
	if _, err := fj.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"}); !host.IsStatus(err, 409) {
		t.Errorf("forgejo second release: %v", err)
	}
	// GitHub: a second draft for the same tag is allowed by the platform.
	if _, err := gh.h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"}); err != nil {
		t.Errorf("github second draft: %v", err)
	}
	// Limits.
	if l, _ := gh.h.Limits(ctx); l.MaxAssetSize != 2<<30 || l.MaxAssets != 1000 {
		t.Errorf("github limits: %+v", l)
	}
	if l, _ := fj.h.Limits(ctx); l.MaxAssetSize != 100<<20 || l.AllowedTypes != "*/*" {
		t.Errorf("forgejo limits: %+v", l)
	}
}

func TestRetriesAndFaults(t *testing.T) {
	ctx := context.Background()
	for name, hs := range hosts(t) {
		t.Run(name, func(t *testing.T) {
			h, s := hs.h, hs.s
			// Transient 500 then success on an idempotent GET.
			s.AddFault(fake.Fault{Method: "GET", PathPart: "/tags/v1.0.0", Status: 500})
			if c, err := h.TagCommit(ctx, "v1.0.0"); err != nil || c != "c0ffee" {
				t.Fatalf("retry after 500: %s %v", c, err)
			}
			// Rate limited then success.
			s.AddFault(fake.Fault{Method: "GET", PathPart: "/tags/v1.0.0", Status: 403, Headers: fake.RateLimitHeaders(time.Second)})
			if _, err := h.TagCommit(ctx, "v1.0.0"); err != nil {
				t.Fatalf("retry after rate limit: %v", err)
			}
			// Dropped connection on GET retried.
			s.AddFault(fake.Fault{Method: "GET", PathPart: "/tags/v1.0.0", Drop: true})
			if _, err := h.TagCommit(ctx, "v1.0.0"); err != nil {
				t.Fatalf("retry after drop: %v", err)
			}
			// Persistent 500 gives up after bounded attempts.
			s.AddFault(fake.Fault{Method: "GET", PathPart: "/tags/v1.0.0", Status: 500, Remaining: 100})
			before := s.Count("GET")
			if _, err := h.TagCommit(ctx, "v1.0.0"); err == nil || !host.IsStatus(err, 500) {
				t.Fatalf("persistent 500: %v", err)
			}
			if n := s.Count("GET") - before; n != 5 {
				t.Fatalf("attempts = %d, want 5", n)
			}
			// A POST is never retried blindly: a dropped connection surfaces as an error.
			s.AddFault(fake.Fault{Method: "POST", PathPart: "/releases", Drop: true, Remaining: 100})
			if _, err := h.CreateDraft(ctx, host.CreateParams{Tag: "v1.0.0", Commit: "c0ffee"}); err == nil {
				t.Fatal("dropped POST reported success")
			}
		})
	}
}

func TestPagination(t *testing.T) {
	ctx := context.Background()
	for name, hs := range hosts(t) {
		t.Run(name, func(t *testing.T) {
			hs.s.PageSize = 2
			for _, tag := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0", "v1.4.0"} {
				hs.s.Tags[tag] = "c"
				if _, err := hs.h.CreateDraft(ctx, host.CreateParams{Tag: tag, Commit: "c"}); err != nil {
					t.Fatal(err)
				}
			}
			list, err := hs.h.ListReleases(ctx)
			if err != nil || len(list) != 5 {
				t.Fatalf("ListReleases: %d %v", len(list), err)
			}
			if rel, err := hs.h.FindRelease(ctx, "v1.4.0"); err != nil || rel.Tag != "v1.4.0" {
				t.Fatalf("FindRelease on last page: %v %v", rel, err)
			}
		})
	}
}

func TestSnappy(t *testing.T) {
	// "hello" as one literal; "abababab" as literal "ab" + copy(offset 2, length 6).
	if got, err := host.DecodeSnappy([]byte{0x05, 0x10, 'h', 'e', 'l', 'l', 'o'}); err != nil || string(got) != "hello" {
		t.Errorf("literal: %q %v", got, err)
	}
	if got, err := host.DecodeSnappy([]byte{0x08, 0x04, 'a', 'b', 0x09, 0x02}); err != nil || string(got) != "abababab" {
		t.Errorf("copy: %q %v", got, err)
	}
	for _, bad := range [][]byte{{}, {0x05, 0x10, 'h'}, {0x02, 0x09, 0x05}, {0x01, 0x10, 'h', 'e'}} {
		if _, err := host.DecodeSnappy(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	_ = strings.Repeat
}
