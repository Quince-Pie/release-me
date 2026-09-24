// Package publish implements the release publication algorithm:
//
//  1. the remote tag must resolve to the locally verified commit;
//  2. a published release for the tag is never modified: if one exists it
//     must already hold byte-identical assets (then the run is an idempotent
//     no-op) or the run fails;
//  3. otherwise a draft is created or an existing draft reused, and its
//     assets reconciled with the local set: matching bytes are kept, anything
//     else (wrong bytes, failed uploads, stale extras, duplicate names) is
//     deleted and re-uploaded, with every stored asset re-downloaded and
//     hashed before the release becomes visible;
//  4. the draft is published in one call, and the published state re-read
//     and checked against the local set.
//
// Uncertain outcomes (a connection lost after a request may have taken
// effect) are resolved by re-reading the platform's state, never by blind
// retries of non-idempotent requests.
package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/manifest"
	"github.com/Quince-Pie/release-me/internal/semver"
)

// Options configure a publication.
type Options struct {
	Host host.Host
	Tag  string
	// Commit is the commit the locally verified tag points at.
	Commit     string
	Name       string
	Body       string
	Prerelease bool
	// Dir holds every file to publish, including the manifest and evidence files.
	Dir string
	// ManifestName is the manifest file inside Dir (default SHA256SUMS).
	ManifestName string
	// Latest overrides the computed "latest" flag.
	Latest *bool
	// TrustServerDigest skips re-downloading assets whose platform-computed
	// digest already matches (GitHub); the default re-downloads everything.
	TrustServerDigest bool
	Logf              func(format string, args ...any)
	// Rounds bounds the reconcile loop (default 4).
	Rounds int
}

// LocalAsset is a file to publish.
type LocalAsset struct {
	Name   string
	Path   string
	Size   int64
	SHA256 string
}

// Result reports what happened.
type Result struct {
	Release          *host.Release
	Assets           []LocalAsset
	Uploaded         []string
	Reused           []string
	Deleted          []string
	AlreadyPublished bool
	Latest           bool
}

// ErrAlreadyPublished is wrapped when a published release with different
// assets exists for the tag.
var ErrAlreadyPublished = errors.New("a published release for this tag already exists")

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// LoadAssets lists the files in dir, checks the manifest against them and
// validates names. Every manifest entry must exist; extra files (the
// manifest itself, signatures, bundles) are published too.
func LoadAssets(dir, manifestName string) ([]LocalAsset, *manifest.Manifest, error) {
	if manifestName == "" {
		manifestName = manifest.FileName
	}
	f, err := os.Open(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, nil, fmt.Errorf("manifest: %w", err)
	}
	m, err := manifest.Parse(f)
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	if err := m.Check(dir); err != nil {
		return nil, nil, fmt.Errorf("assets do not match %s: %w", manifestName, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var assets []LocalAsset
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if !manifest.ValidName(e.Name()) {
			return nil, nil, fmt.Errorf("asset name %q is not publishable (letters, digits, . _ - + ~ only, no leading . or -)", e.Name())
		}
		p := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return nil, nil, err
		}
		sum, err := manifest.HashFile(p)
		if err != nil {
			return nil, nil, err
		}
		if want, ok := m.Lookup(e.Name()); ok && want != sum {
			return nil, nil, fmt.Errorf("%s: sha256 %s but %s says %s", e.Name(), sum, manifestName, want)
		}
		assets = append(assets, LocalAsset{Name: e.Name(), Path: p, Size: info.Size(), SHA256: sum})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })
	return assets, m, nil
}

// Run publishes.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Host == nil || o.Tag == "" || o.Commit == "" || o.Dir == "" {
		return nil, errors.New("publish: host, tag, commit and dir are required")
	}
	rounds := o.Rounds
	if rounds <= 0 {
		rounds = 4
	}
	assets, _, err := LoadAssets(o.Dir, o.ManifestName)
	if err != nil {
		return nil, err
	}
	res := &Result{Assets: assets}

	// Limits before anything is created.
	limits, err := o.Host.Limits(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish: reading %s limits: %w", o.Host.Kind(), err)
	}
	if limits.MaxAssets > 0 && len(assets) > limits.MaxAssets {
		return nil, fmt.Errorf("publish: %d assets exceed the %s limit of %d per release", len(assets), o.Host.Kind(), limits.MaxAssets)
	}
	for _, a := range assets {
		if limits.MaxAssetSize > 0 && a.Size > limits.MaxAssetSize {
			return nil, fmt.Errorf("publish: %s is %d bytes; %s allows at most %d bytes per asset", a.Name, a.Size, o.Host.Kind(), limits.MaxAssetSize)
		}
	}

	// 1. The remote tag is the local tag.
	remote, err := o.Host.TagCommit(ctx, o.Tag)
	if err != nil {
		return nil, fmt.Errorf("publish: remote tag %s: %w", o.Tag, err)
	}
	if remote != o.Commit {
		return nil, fmt.Errorf("publish: remote tag %s points at %s, the verified local tag at %s; refusing to publish", o.Tag, remote, o.Commit)
	}

	// 2. Existing release.
	rel, err := o.Host.FindRelease(ctx, o.Tag)
	switch {
	case err == nil && !rel.Draft:
		o.logf("release %s is already published (%s); checking its assets against the local set", o.Tag, rel.HTMLURL)
		if err := o.checkPublished(ctx, rel, assets); err != nil {
			return nil, fmt.Errorf("publish: %w: %v", ErrAlreadyPublished, err)
		}
		res.Release, res.AlreadyPublished = rel, true
		return res, nil
	case err == nil:
		o.logf("reusing draft release %d for %s", rel.ID, o.Tag)
		if err := o.dropOtherDrafts(ctx, rel); err != nil {
			return nil, err
		}
	case host.IsNotFound(err):
		rel, err = o.createDraft(ctx)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("publish: looking up release %s: %w", o.Tag, err)
	}

	// 3. Reconcile assets.
	for round := 1; ; round++ {
		done, err := o.reconcile(ctx, rel, assets, res)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		if round == rounds {
			return nil, fmt.Errorf("publish: assets of draft %d did not converge after %d rounds; the draft is left for inspection", rel.ID, rounds)
		}
		o.logf("round %d left work to do; re-reading assets", round)
	}
	// Final check of exactly what is stored.
	stored, err := o.Host.Assets(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := o.compare(ctx, rel, stored, assets, !o.TrustServerDigest); err != nil {
		return nil, fmt.Errorf("publish: final check of draft %d failed: %w", rel.ID, err)
	}

	// 4. Publish.
	latest, err := o.decideLatest(ctx)
	if err != nil {
		return nil, err
	}
	res.Latest = latest
	// Guard against a release published for this tag while we were uploading.
	if cur, err := o.Host.FindRelease(ctx, o.Tag); err != nil {
		return nil, fmt.Errorf("publish: re-reading release %s: %w", o.Tag, err)
	} else if cur.ID != rel.ID {
		return nil, fmt.Errorf("publish: release %d for %s appeared during the upload (draft=%v); our draft %d is left untouched", cur.ID, o.Tag, cur.Draft, rel.ID)
	}
	published, err := o.Host.Publish(ctx, rel, host.PublishParams{Latest: latest})
	if err != nil {
		o.logf("publish call failed (%v); re-reading release state", err)
		cur, err2 := o.Host.FindRelease(ctx, o.Tag)
		if err2 != nil || cur.ID != rel.ID || cur.Draft {
			return nil, fmt.Errorf("publish: publishing draft %d: %w", rel.ID, err)
		}
		published = cur
	}
	if published.Draft {
		return nil, fmt.Errorf("publish: release %d is still a draft after publishing", rel.ID)
	}
	// Confirm from the platform's point of view.
	final, err := o.Host.FindRelease(ctx, o.Tag)
	if err != nil {
		return nil, fmt.Errorf("publish: re-reading published release: %w", err)
	}
	if final.ID != rel.ID || final.Draft {
		return nil, fmt.Errorf("publish: published release %d is not our draft %d", final.ID, rel.ID)
	}
	storedFinal, err := o.Host.Assets(ctx, final)
	if err != nil {
		return nil, err
	}
	if err := o.compare(ctx, final, storedFinal, assets, false); err != nil {
		return nil, fmt.Errorf("publish: published release %d does not match the local assets: %w", final.ID, err)
	}
	res.Release = final
	o.logf("published %s: %s", o.Tag, final.HTMLURL)
	return res, nil
}

func (o *Options) createDraft(ctx context.Context) (*host.Release, error) {
	p := host.CreateParams{Tag: o.Tag, Name: o.Name, Body: o.Body, Prerelease: o.Prerelease, Commit: o.Commit}
	if p.Name == "" {
		p.Name = o.Tag
	}
	rel, err := o.Host.CreateDraft(ctx, p)
	if err == nil {
		o.logf("created draft release %d for %s", rel.ID, o.Tag)
		return rel, nil
	}
	// The request may have taken effect; look again before failing.
	if cur, err2 := o.Host.FindRelease(ctx, o.Tag); err2 == nil && cur.Draft {
		o.logf("create returned %v but a draft exists; using draft %d", err, cur.ID)
		return cur, nil
	}
	return nil, fmt.Errorf("publish: creating draft for %s: %w", o.Tag, err)
}

// dropOtherDrafts deletes additional drafts for the same tag (GitHub allows
// several); they can only be leftovers of earlier attempts.
func (o *Options) dropOtherDrafts(ctx context.Context, keep *host.Release) error {
	all, err := o.Host.ListReleases(ctx)
	if err != nil {
		return err
	}
	for i := range all {
		r := &all[i]
		if r.Tag != o.Tag || r.ID == keep.ID {
			continue
		}
		if !r.Draft {
			return fmt.Errorf("publish: %w (release %d)", ErrAlreadyPublished, r.ID)
		}
		o.logf("deleting stale draft %d for %s", r.ID, o.Tag)
		if err := o.Host.DeleteRelease(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// reconcile performs one pass: delete what is wrong, upload what is missing.
// It returns done=true when nothing was changed and every local asset is
// stored correctly.
func (o *Options) reconcile(ctx context.Context, rel *host.Release, assets []LocalAsset, res *Result) (bool, error) {
	stored, err := o.Host.Assets(ctx, rel)
	if err != nil {
		return false, err
	}
	local := map[string]LocalAsset{}
	for _, a := range assets {
		local[a.Name] = a
	}
	byName := map[string][]host.Asset{}
	for _, s := range stored {
		byName[s.Name] = append(byName[s.Name], s)
	}
	changed := false
	ok := map[string]bool{}
	for name, group := range byName {
		want, isLocal := local[name]
		switch {
		case !isLocal:
			for _, s := range group {
				o.logf("deleting stale asset %s (id %d)", s.Name, s.ID)
				if err := o.Host.DeleteAsset(ctx, rel, s); err != nil {
					return false, err
				}
				res.Deleted = append(res.Deleted, name)
				changed = true
			}
		case len(group) > 1:
			// Duplicate names (Forgejo accepts them): which one a download
			// serves is undefined, so remove all and upload once.
			for _, s := range group {
				o.logf("deleting duplicate asset %s (id %d)", s.Name, s.ID)
				if err := o.Host.DeleteAsset(ctx, rel, s); err != nil {
					return false, err
				}
				res.Deleted = append(res.Deleted, name)
				changed = true
			}
		default:
			s := group[0]
			if err := o.check(ctx, rel, s, want, !o.TrustServerDigest); err != nil {
				o.logf("stored asset %s (id %d) is not the local file: %v; deleting", s.Name, s.ID, err)
				if err := o.Host.DeleteAsset(ctx, rel, s); err != nil {
					return false, err
				}
				res.Deleted = append(res.Deleted, name)
				changed = true
			} else {
				ok[name] = true
			}
		}
	}
	for _, a := range assets {
		if ok[a.Name] {
			if !contains(res.Reused, a.Name) && !contains(res.Uploaded, a.Name) {
				res.Reused = append(res.Reused, a.Name)
			}
			continue
		}
		changed = true
		if err := o.upload(ctx, rel, a); err != nil {
			if host.IsStatus(err, 422) {
				// GitHub: the name exists (a concurrent or half-failed upload); the next round checks it.
				o.logf("upload of %s answered 422; re-checking next round", a.Name)
				continue
			}
			var ae *host.APIError
			if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != 408 && ae.Status != 429 {
				return false, fmt.Errorf("publish: uploading %s: %w", a.Name, err)
			}
			o.logf("upload of %s failed (%v); its state will be re-read next round", a.Name, err)
			continue
		}
		res.Uploaded = append(res.Uploaded, a.Name)
	}
	return !changed, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (o *Options) upload(ctx context.Context, rel *host.Release, a LocalAsset) error {
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	o.logf("uploading %s (%d bytes, sha256:%s)", a.Name, a.Size, a.SHA256)
	stored, err := o.Host.Upload(ctx, rel, a.Name, "application/octet-stream", a.Size, f)
	if err != nil {
		return err
	}
	// Whatever the platform answered, the next round re-reads and checks it;
	// but a mismatching immediate answer is worth reporting now.
	if stored.Digest != "" && stored.Digest != "sha256:"+a.SHA256 {
		return fmt.Errorf("server digest %s for %s does not match local sha256:%s", stored.Digest, a.Name, a.SHA256)
	}
	return nil
}

// check verifies one stored asset against the local file: state, size, the
// platform digest when there is one, and the bytes themselves unless the
// caller trusts a matching platform digest.
func (o *Options) check(ctx context.Context, rel *host.Release, s host.Asset, want LocalAsset, download bool) error {
	if s.State != "" && s.State != "uploaded" {
		return fmt.Errorf("state %q", s.State)
	}
	if s.Size != want.Size {
		return fmt.Errorf("size %d, local %d", s.Size, want.Size)
	}
	if s.Digest != "" {
		if s.Digest != "sha256:"+want.SHA256 {
			return fmt.Errorf("server digest %s, local sha256:%s", s.Digest, want.SHA256)
		}
		if !download {
			return nil
		}
	}
	rc, err := o.Host.OpenAsset(ctx, rel, s)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	sum, err := manifest.HashReader(io.LimitReader(rc, want.Size+1))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if sum != want.SHA256 {
		return fmt.Errorf("downloaded bytes hash to %s, local %s", sum, want.SHA256)
	}
	return nil
}

// compare requires stored to be exactly the local set, each verified.
func (o *Options) compare(ctx context.Context, rel *host.Release, stored []host.Asset, assets []LocalAsset, download bool) error {
	byName := map[string][]host.Asset{}
	for _, s := range stored {
		byName[s.Name] = append(byName[s.Name], s)
	}
	var errs []error
	for _, a := range assets {
		group := byName[a.Name]
		switch {
		case len(group) == 0:
			errs = append(errs, fmt.Errorf("%s: missing", a.Name))
		case len(group) > 1:
			errs = append(errs, fmt.Errorf("%s: %d assets with this name", a.Name, len(group)))
		default:
			if err := o.check(ctx, rel, group[0], a, download); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
			}
		}
		delete(byName, a.Name)
	}
	for name := range byName {
		errs = append(errs, fmt.Errorf("%s: unexpected asset", name))
	}
	return errors.Join(errs...)
}

func (o *Options) checkPublished(ctx context.Context, rel *host.Release, assets []LocalAsset) error {
	stored, err := o.Host.Assets(ctx, rel)
	if err != nil {
		return err
	}
	return o.compare(ctx, rel, stored, assets, true)
}

// decideLatest marks the release latest only if it orders above every
// published non-pre-release version, so a patch of an older line never
// becomes "latest".
func (o *Options) decideLatest(ctx context.Context) (bool, error) {
	if o.Latest != nil {
		return *o.Latest, nil
	}
	if o.Prerelease {
		return false, nil
	}
	ours, err := semver.ParseTag(o.Tag)
	if err != nil || ours.IsPrerelease() {
		return false, nil
	}
	all, err := o.Host.ListReleases(ctx)
	if err != nil {
		return false, fmt.Errorf("publish: listing releases to decide latest: %w", err)
	}
	for _, r := range all {
		if r.Draft || r.Prerelease || r.Tag == o.Tag {
			continue
		}
		v, err := semver.ParseTag(r.Tag)
		if err != nil {
			continue
		}
		if semver.Compare(ours, v) <= 0 {
			return false, nil
		}
	}
	return true, nil
}
