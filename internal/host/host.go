// Package host abstracts the release API of a git hosting platform behind
// the small set of operations the publish and verify algorithms need, and
// records precisely where platforms differ (see docs/HOSTS.md).
package host

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when a release, tag or asset does not exist.
var ErrNotFound = errors.New("not found")

// Release is the platform's release object.
type Release struct {
	ID         int64
	Tag        string
	Name       string
	Body       string
	Draft      bool
	Prerelease bool
	// Immutable is set when the platform reports the release as immutable
	// (GitHub); false elsewhere or when unknown.
	Immutable bool
	HTMLURL   string
	Assets    []Asset
	// uploadURL is the GitHub-specific upload endpoint for this release.
	uploadURL string
}

// Asset is a file attached to a release.
type Asset struct {
	ID   int64
	Name string
	Size int64
	// Digest is "sha256:<hex>" when the platform computed it (GitHub);
	// empty when the platform does not expose one and the bytes must be
	// downloaded to be checked.
	Digest string
	// State is the platform's upload state ("uploaded" on GitHub; a
	// "starter" asset is a failed upload that must be deleted).
	State string
	URL   string // API URL of the asset
}

// Limits are the platform's upload constraints, learned before uploading so
// that a release that cannot be completed is refused before anything is created.
type Limits struct {
	MaxAssetSize int64 // bytes; 0 means unknown/unlimited
	MaxAssets    int   // per release; 0 means unknown/unlimited
	// AllowedTypes is Forgejo/Gitea's "[attachment] ALLOWED_TYPES" list
	// (extensions and MIME types, "*/*" for anything); empty means no restriction.
	AllowedTypes string
}

// CreateParams describe a new draft release.
type CreateParams struct {
	Tag        string
	Name       string
	Body       string
	Prerelease bool
	// Commit is the commit the tag must already point at; hosts that would
	// create a tag on the fly are told to use it, and the caller has already
	// checked the tag exists remotely.
	Commit string
}

// PublishParams describe the draft -> published transition.
type PublishParams struct {
	// Latest, when the platform has an explicit "latest" flag, sets it.
	Latest bool
}

// Host is implemented per platform.
type Host interface {
	Kind() string
	Limits(ctx context.Context) (Limits, error)
	// TagCommit resolves the remote tag to the commit it points at,
	// following annotated tag objects. ErrNotFound if the tag does not exist.
	TagCommit(ctx context.Context, tag string) (string, error)
	// FindRelease returns the release for tag, published or draft
	// (drafts are visible only with write access). ErrNotFound otherwise.
	FindRelease(ctx context.Context, tag string) (*Release, error)
	// ListReleases lists every release the token can see, drafts included.
	ListReleases(ctx context.Context) ([]Release, error)
	CreateDraft(ctx context.Context, p CreateParams) (*Release, error)
	Assets(ctx context.Context, r *Release) ([]Asset, error)
	// Upload sends one asset. The caller verifies the stored bytes afterwards.
	Upload(ctx context.Context, r *Release, name, contentType string, size int64, body io.Reader) (*Asset, error)
	DeleteAsset(ctx context.Context, r *Release, a Asset) error
	// OpenAsset downloads an asset's bytes through the authenticated API so
	// that draft assets can be checked before publication.
	OpenAsset(ctx context.Context, r *Release, a Asset) (io.ReadCloser, error)
	Publish(ctx context.Context, r *Release, p PublishParams) (*Release, error)
	DeleteRelease(ctx context.Context, r *Release) error
	// DownloadURL is the public, unauthenticated URL of a published asset.
	DownloadURL(tag, name string) string
	// RepoURL is the canonical web URL of the repository.
	RepoURL() string
}

// AttestationStore is implemented by platforms that index Sigstore bundles
// by subject digest (GitHub).
type AttestationStore interface {
	StoreAttestation(ctx context.Context, bundle []byte) (id int64, err error)
	// Attestations returns the raw bundles recorded for "sha256:<hex>".
	Attestations(ctx context.Context, digest string, predicateType string) ([][]byte, error)
}

// Clock lets tests control time and sleeping.
type Clock struct {
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// RealClock uses the wall clock.
var RealClock = Clock{
	Now: time.Now,
	Sleep: func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	},
}
