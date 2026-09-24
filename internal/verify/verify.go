package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Quince-Pie/release-me/internal/gittag"
	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/intoto"
	"github.com/Quince-Pie/release-me/internal/manifest"
	"github.com/Quince-Pie/release-me/internal/sigstore"
	"github.com/Quince-Pie/release-me/internal/sshsig"
)

// Names of the evidence assets, relative to the policy prefix.
const (
	ProvenanceName  = "provenance.intoto.json"
	BundleName      = "provenance.sigstore.json"
	SignatureSuffix = ".sig"
	SSHSigNamespace = "release"
)

// Options configure a verification run.
type Options struct {
	Policy *Policy
	Tag    string
	// Dir receives the downloaded files (a temporary directory when empty).
	Dir string
	// Token optionally raises API rate limits; never required.
	Token string
	// Reproduce runs the policy's rebuild command in RepoDir (a checkout of
	// the tag) and compares manifests.
	Reproduce bool
	RepoDir   string
	// CacheDir for Sigstore's TUF client.
	CacheDir string
	// APIURL overrides the platform API base (GitHub Enterprise: <server>/api/v3).
	APIURL string
	Logf   func(string, ...any)
	Now    time.Time
}

// Report is the machine-readable outcome.
type Report struct {
	Repository string            `json:"repository"`
	Tag        string            `json:"tag"`
	Release    string            `json:"release_url"`
	Assets     []AssetReport     `json:"assets"`
	Manifest   string            `json:"manifest_sha256"`
	SSHSig     *SSHSigReport     `json:"sshsig,omitempty"`
	Sigstore   *SigstoreReport   `json:"sigstore,omitempty"`
	Provenance *ProvenanceReport `json:"provenance,omitempty"`
	Reproduced *bool             `json:"reproduced,omitempty"`
	Checks     []string          `json:"checks"`
}

type AssetReport struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type SSHSigReport struct {
	Principal   string `json:"principal"`
	Fingerprint string `json:"fingerprint"`
	// ProvenanceSigned reports whether the provenance statement carried a
	// valid signature too.
	ProvenanceSigned bool `json:"provenance_signed"`
}

type SigstoreReport struct {
	Identity   string      `json:"identity"`
	Issuer     string      `json:"issuer"`
	Timestamps []time.Time `json:"timestamps"`
}

type ProvenanceReport struct {
	SourceURI string `json:"source_uri"`
	Commit    string `json:"commit"`
	Builder   string `json:"builder"`
	BuildType string `json:"build_type"`
	Command   string `json:"build_command"`
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

func (o *Options) hostClient() (host.Host, error) {
	p := o.Policy
	owner, repo, _ := strings.Cut(p.Repository, "/")
	c := &host.Client{HTTP: &http.Client{Timeout: 5 * time.Minute}, Token: o.Token, UserAgent: "release-me-verify", Logf: o.Logf}
	switch p.Host {
	case "github":
		api := o.APIURL
		if api == "" && p.Server != "" && p.Server != "https://github.com" {
			api = strings.TrimRight(p.Server, "/") + "/api/v3"
		}
		return host.NewGitHub(c, api, "", p.Server, owner, repo), nil
	case "forgejo":
		return host.NewForgejo(c, p.Server, owner, repo), nil
	}
	return nil, fmt.Errorf("unknown host %q", p.Host)
}

// download fetches a public release asset by its browser URL.
func download(ctx context.Context, url, dest string) error {
	c := &host.Client{HTTP: &http.Client{Timeout: 10 * time.Minute}, UserAgent: "release-me-verify"}
	rc, err := c.Stream(ctx, url, "")
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(f, rc)
	if err := errors.Join(cerr, f.Close()); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}

// Run verifies a release. Any failure is an error and the report is nil.
func Run(ctx context.Context, o Options) (*Report, error) {
	p := o.Policy
	if p == nil || o.Tag == "" {
		return nil, errors.New("verify: policy and tag are required")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	h, err := o.hostClient()
	if err != nil {
		return nil, err
	}
	rel, err := h.FindRelease(ctx, o.Tag)
	if err != nil {
		return nil, fmt.Errorf("verify: release %s on %s: %w", o.Tag, p.Repository, err)
	}
	if rel.Draft {
		return nil, fmt.Errorf("verify: release %s is a draft", o.Tag)
	}
	report := &Report{Repository: p.Repository, Tag: o.Tag, Release: rel.HTMLURL}
	dir := o.Dir
	if dir == "" {
		dir, err = os.MkdirTemp("", "release-me-verify-")
		if err != nil {
			return nil, err
		}
		o.logf("downloading into %s", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Asset names come from the platform; reject anything unsafe before
	// touching the filesystem.
	published := map[string]host.Asset{}
	for _, a := range rel.Assets {
		if !manifest.ValidName(a.Name) {
			return nil, fmt.Errorf("verify: release lists an unsafe asset name %q", a.Name)
		}
		if _, dup := published[a.Name]; dup {
			return nil, fmt.Errorf("verify: release lists %q twice; which bytes a download serves is undefined", a.Name)
		}
		published[a.Name] = a
	}
	fetch := func(name string) (string, error) {
		if _, ok := published[name]; !ok {
			return "", fmt.Errorf("verify: release has no asset %q", name)
		}
		dest := filepath.Join(dir, name)
		if err := download(ctx, h.DownloadURL(o.Tag, name), dest); err != nil {
			return "", fmt.Errorf("verify: downloading %s: %w", name, err)
		}
		return dest, nil
	}
	fetchOptional := func(name string) (string, bool, error) {
		if _, ok := published[name]; !ok {
			return "", false, nil
		}
		dest, err := fetch(name)
		return dest, err == nil, err
	}

	// 1. Manifest and the assets it lists.
	mname := p.manifestName()
	mpath, err := fetch(mname)
	if err != nil {
		return nil, err
	}
	mbytes, _ := os.ReadFile(mpath)
	m, err := manifest.Parse(bytes.NewReader(mbytes))
	if err != nil {
		return nil, fmt.Errorf("verify: %s: %w", mname, err)
	}
	report.Manifest, _ = manifest.HashReader(bytes.NewReader(mbytes))
	for _, e := range m.Entries {
		if _, err := fetch(e.Name); err != nil {
			return nil, err
		}
	}
	if err := m.Check(dir); err != nil {
		return nil, fmt.Errorf("verify: downloaded assets do not match %s: %w", mname, err)
	}
	for _, e := range m.Entries {
		report.Assets = append(report.Assets, AssetReport{Name: e.Name, SHA256: e.SHA256, Size: published[e.Name].Size})
	}
	report.Checks = append(report.Checks, fmt.Sprintf("%d assets match %s", len(m.Entries), mname))
	// A platform digest, where the platform provides one, must agree too.
	for _, e := range m.Entries {
		if d := published[e.Name].Digest; d != "" && d != "sha256:"+e.SHA256 {
			return nil, fmt.Errorf("verify: platform digest for %s is %s, manifest says sha256:%s", e.Name, d, e.SHA256)
		}
	}

	prefix := p.prefix(o.Tag)
	var statement *intoto.Statement
	var statementBytes []byte

	// 2. SSH signatures.
	if p.Requires("sshsig") {
		sigPath, err := fetch(mname + SignatureSuffix)
		if err != nil {
			return nil, err
		}
		policy, revoked, err := o.sshPolicy()
		if err != nil {
			return nil, err
		}
		ns := p.SSHSig.Namespace
		if ns == "" {
			ns = SSHSigNamespace
		}
		sig, _ := os.ReadFile(sigPath)
		res, err := policy.Verify(bytes.NewReader(mbytes), sig, sshsig.Options{Namespace: ns, Principal: p.SSHSig.Principal, Revoked: revoked, Now: o.Now})
		if err != nil {
			return nil, fmt.Errorf("verify: %s%s: %w", mname, SignatureSuffix, err)
		}
		report.SSHSig = &SSHSigReport{Principal: res.Principal, Fingerprint: res.Fingerprint}
		report.Checks = append(report.Checks, fmt.Sprintf("%s signed by %s (%s)", mname, res.Principal, res.Fingerprint))
		// The provenance statement, when published, must be signed by the same policy.
		if stPath, ok, err := fetchOptional(prefix + ProvenanceName); err != nil {
			return nil, err
		} else if ok {
			stSigPath, err := fetch(prefix + ProvenanceName + SignatureSuffix)
			if err != nil {
				return nil, err
			}
			statementBytes, _ = os.ReadFile(stPath)
			stSig, _ := os.ReadFile(stSigPath)
			if _, err := policy.Verify(bytes.NewReader(statementBytes), stSig, sshsig.Options{Namespace: ns, Principal: p.SSHSig.Principal, Revoked: revoked, Now: o.Now}); err != nil {
				return nil, fmt.Errorf("verify: %s: %w", prefix+ProvenanceName+SignatureSuffix, err)
			}
			statement, err = intoto.ParseStatement(statementBytes)
			if err != nil {
				return nil, err
			}
			report.SSHSig.ProvenanceSigned = true
			report.Checks = append(report.Checks, "provenance statement signed by the same key")
		}
	}

	// 3. Sigstore bundle.
	if p.Requires("sigstore") {
		bPath, err := fetch(prefix + BundleName)
		if err != nil {
			return nil, err
		}
		bundleJSON, _ := os.ReadFile(bPath)
		trusted, err := sigstore.TrustedRoot(p.Sigstore.TrustedRoot, o.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("verify: trusted root: %w", err)
		}
		digests := make([]string, 0, len(m.Entries))
		for _, e := range m.Entries {
			digests = append(digests, e.SHA256)
		}
		id := sigstore.Identity{Issuer: p.Sigstore.Issuer, SAN: substitute(p.Sigstore.Identity, o.Tag), SANRegex: substitute(p.Sigstore.IdentityRegex, o.Tag)}
		res, err := sigstore.Verify(bundleJSON, sigstore.VerifyOptions{Trusted: trusted, Identity: id, Digests: digests})
		if err != nil {
			return nil, fmt.Errorf("verify: %s: %w", prefix+BundleName, err)
		}
		report.Sigstore = &SigstoreReport{Identity: res.SAN, Issuer: res.Issuer, Timestamps: res.Timestamps}
		report.Checks = append(report.Checks, fmt.Sprintf("Sigstore bundle signed by %s (issuer %s)", res.SAN, res.Issuer))
		if statement != nil && !bytes.Equal(res.Payload, statementBytes) {
			return nil, errors.New("verify: the Sigstore bundle's statement differs from the published provenance statement")
		}
		statement, statementBytes = res.Statement, res.Payload
	}

	// 4. Provenance contents.
	if statement != nil {
		subjects, err := statement.Subjects()
		if err != nil {
			return nil, err
		}
		if err := manifest.Diff(m, subjects); err != nil {
			return nil, fmt.Errorf("verify: provenance subjects differ from %s: %w", mname, err)
		}
		pred, err := statement.ProvenancePredicate()
		if err != nil {
			return nil, err
		}
		src, err := pred.SourceOf()
		if err != nil {
			return nil, err
		}
		if src.Tag != o.Tag {
			return nil, fmt.Errorf("verify: provenance is for tag %s, not %s", src.Tag, o.Tag)
		}
		if p.Source != "" && src.URI != p.Source+"@refs/tags/"+o.Tag {
			return nil, fmt.Errorf("verify: provenance source %s is not %s@refs/tags/%s", src.URI, p.Source, o.Tag)
		}
		if pred.BuildDefinition.BuildType != intoto.BuildType {
			return nil, fmt.Errorf("verify: unexpected build type %s", pred.BuildDefinition.BuildType)
		}
		cmd, _ := pred.BuildDefinition.ExternalParameters["buildCommand"].(string)
		report.Provenance = &ProvenanceReport{SourceURI: src.URI, Commit: src.Commit, Builder: pred.RunDetails.Builder.ID, BuildType: pred.BuildDefinition.BuildType, Command: cmd}
		report.Checks = append(report.Checks, fmt.Sprintf("provenance: %s at %s built by %s", src.URI, src.Commit, pred.RunDetails.Builder.ID))
	}

	// 5. Independent reproduction.
	if o.Reproduce {
		if p.Reproduce == nil {
			return nil, errors.New("verify: the policy has no reproduce section")
		}
		if err := o.reproduce(ctx, m, report); err != nil {
			return nil, err
		}
		yes := true
		report.Reproduced = &yes
	}
	return report, nil
}

func substitute(s, tag string) string {
	return strings.NewReplacer("{tag}", tag, "{version}", strings.TrimPrefix(tag, "v")).Replace(s)
}

func (o *Options) sshPolicy() (*sshsig.AllowedSigners, []sshsigPublicKey, error) {
	p := o.Policy
	data, err := p.resolve(p.SSHSig.AllowedSigners)
	if err != nil {
		return nil, nil, fmt.Errorf("verify: allowed signers: %w", err)
	}
	policy, err := sshsig.ParseAllowedSigners(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	var revoked []sshsigPublicKey
	if p.SSHSig.RevokedKeys != "" {
		rdata, err := p.resolve(p.SSHSig.RevokedKeys)
		if err != nil {
			return nil, nil, fmt.Errorf("verify: revoked keys: %w", err)
		}
		revoked, err = sshsig.ParseRevokedKeys(rdata)
		if err != nil {
			return nil, nil, err
		}
	}
	return policy, revoked, nil
}

type sshsigPublicKey = sshsigKey

// reproduce runs the rebuild in the checkout and compares manifests.
func (o *Options) reproduce(ctx context.Context, published *manifest.Manifest, report *Report) error {
	p := o.Policy
	repoDir := o.RepoDir
	if repoDir == "" {
		repoDir = "."
	}
	// The checkout must be the tagged commit named by the provenance (when
	// there is one) and clean.
	repo := gittag.Repo{Dir: repoDir}
	head, err := repo.Commit(ctx, o.Tag)
	if err != nil {
		return fmt.Errorf("verify: reproduce: tag %s in %s: %w", o.Tag, repoDir, err)
	}
	if report.Provenance != nil && report.Provenance.Commit != head {
		return fmt.Errorf("verify: reproduce: local tag %s is %s, provenance says %s", o.Tag, head, report.Provenance.Commit)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("verify: reproduce: %w", err)
	}
	if strings.TrimSpace(string(out)) != head {
		return fmt.Errorf("verify: reproduce: %s is not checked out at %s (HEAD is %s)", repoDir, o.Tag, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "status", "--porcelain", "--untracked-files=no").Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		return fmt.Errorf("verify: reproduce: working tree of %s is not clean", repoDir)
	}
	o.logf("reproduce: running %q in %s", p.Reproduce.Command, repoDir)
	cmd := exec.CommandContext(ctx, "sh", "-c", p.Reproduce.Command)
	cmd.Dir = repoDir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("verify: reproduce: build failed: %w", err)
	}
	rebuilt, err := manifest.FromDir(filepath.Join(repoDir, p.Reproduce.Dir))
	if err != nil {
		return fmt.Errorf("verify: reproduce: %w", err)
	}
	if err := manifest.Diff(published, rebuilt); err != nil {
		return fmt.Errorf("verify: reproduce: rebuilt assets differ from the published ones: %w", err)
	}
	report.Checks = append(report.Checks, "rebuilt assets are byte-identical to the published ones")
	return nil
}
