package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/publish"
	"github.com/Quince-Pie/release-me/internal/semver"
	"github.com/Quince-Pie/release-me/internal/verify"
)

func newHost(kind, server, repo, token string) (host.Host, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	c := &host.Client{HTTP: &http.Client{Timeout: 30 * time.Minute}, Token: token, UserAgent: "release-me/" + version, Logf: logf}
	switch kind {
	case "github":
		return host.NewGitHub(c, envOr("GITHUB_API_URL"), "", server, owner, name), nil
	case "forgejo", "gitea", "codeberg":
		if server == "" {
			if kind == "codeberg" {
				server = "https://codeberg.org"
			} else {
				return nil, errors.New("--server is required for forgejo/gitea hosts")
			}
		}
		return host.NewForgejo(c, server, owner, name), nil
	}
	return nil, fmt.Errorf("unknown host %q (github, forgejo, gitea, codeberg)", kind)
}

func cmdPublish(ctx context.Context, args []string) error {
	fs := flagSet("publish", "publish --host github|forgejo|codeberg --repo OWNER/NAME --tag vX --commit SHA --dir DIR [--server URL] [--name N] [--notes-file F] [--prerelease auto|true|false] [--latest auto|true|false] [--trust-server-digest] [--json]")
	kind := fs.String("host", "", "hosting platform")
	server := fs.String("server", envOr("GITHUB_SERVER_URL"), "instance URL (Forgejo/Gitea; GitHub Enterprise)")
	repo := fs.String("repo", envOr("GITHUB_REPOSITORY"), "owner/name")
	tag := fs.String("tag", "", "release tag (must exist remotely and point at --commit)")
	commit := fs.String("commit", "", "commit the verified tag points at")
	dir := fs.String("dir", "", "directory of assets including the manifest")
	name := fs.String("name", "", "release title (default: the tag)")
	notesFile := fs.String("notes-file", "", "release notes file")
	pre := fs.String("prerelease", "auto", "auto (from the version), true or false")
	latest := fs.String("latest", "auto", "auto (only if newer than every published release), true or false")
	trust := fs.Bool("trust-server-digest", false, "skip re-downloading assets whose platform digest matches (GitHub)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kind == "" || *repo == "" || *tag == "" || *commit == "" || *dir == "" {
		fs.Usage()
		return errors.New("publish: --host, --repo, --tag, --commit and --dir are required")
	}
	token := envOr("RELEASE_TOKEN")
	if token == "" {
		if *kind == "github" {
			token = envOr("GITHUB_TOKEN", "GH_TOKEN")
		} else {
			token = envOr("FORGEJO_TOKEN", "GITEA_TOKEN", "CODEBERG_TOKEN")
		}
	}
	if token == "" {
		return errors.New("publish: no token (RELEASE_TOKEN, GITHUB_TOKEN or FORGEJO_TOKEN)")
	}
	h, err := newHost(*kind, *server, *repo, token)
	if err != nil {
		return err
	}
	v, err := semver.ParseTag(*tag)
	if err != nil {
		return err
	}
	o := publish.Options{Host: h, Tag: *tag, Commit: *commit, Name: *name, Dir: *dir, TrustServerDigest: *trust, Logf: logf}
	switch *pre {
	case "auto":
		o.Prerelease = v.IsPrerelease()
	case "true":
		o.Prerelease = true
	case "false":
	default:
		return errors.New("publish: --prerelease must be auto, true or false")
	}
	switch *latest {
	case "auto":
	case "true", "false":
		b := *latest == "true"
		o.Latest = &b
	default:
		return errors.New("publish: --latest must be auto, true or false")
	}
	if *notesFile != "" {
		b, err := os.ReadFile(*notesFile)
		if err != nil {
			return err
		}
		o.Body = strings.TrimSpace(string(b))
	}
	res, err := publish.Run(ctx, o)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"release_url": res.Release.HTMLURL, "release_id": res.Release.ID, "tag": *tag,
			"already_published": res.AlreadyPublished, "latest": res.Latest, "immutable": res.Release.Immutable,
			"uploaded": res.Uploaded, "reused": res.Reused, "deleted": res.Deleted, "assets": res.Assets,
		})
	}
	if res.AlreadyPublished {
		fmt.Printf("%s was already published with identical assets: %s\n", *tag, res.Release.HTMLURL)
		return nil
	}
	fmt.Printf("published %s: %s (%d assets uploaded, %d reused)\n", *tag, res.Release.HTMLURL, len(res.Uploaded), len(res.Reused))
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flagSet("verify", "verify --policy FILE --tag vX [--dir DIR] [--reproduce [--repo-dir DIR]] [--json]")
	policyPath := fs.String("policy", "release-policy.json", "policy file")
	tag := fs.String("tag", "", "release tag")
	dir := fs.String("dir", "", "directory to download into (default: a temporary directory)")
	reproduce := fs.Bool("reproduce", false, "rebuild the assets from a checkout of the tag and compare")
	repoDir := fs.String("repo-dir", ".", "checkout of the tag for --reproduce")
	cache := fs.String("cache-dir", envOr("RELEASE_ME_TUF_CACHE"), "TUF cache directory")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tag == "" {
		fs.Usage()
		return errors.New("verify: --tag is required")
	}
	p, err := verify.LoadPolicy(*policyPath)
	if err != nil {
		return err
	}
	// A token is optional (it raises API rate limits); never send another
	// platform's token to a host.
	token := envOr("RELEASE_TOKEN")
	if token == "" {
		if p.Host == "github" {
			token = envOr("GITHUB_TOKEN", "GH_TOKEN")
		} else {
			token = envOr("FORGEJO_TOKEN", "GITEA_TOKEN", "CODEBERG_TOKEN")
		}
	}
	report, err := verify.Run(ctx, verify.Options{Policy: p, Tag: *tag, Dir: *dir, Token: token, Reproduce: *reproduce, RepoDir: *repoDir, CacheDir: *cache, Logf: logf})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Printf("%s %s verified (%s)\n", report.Repository, report.Tag, report.Release)
	for _, c := range report.Checks {
		fmt.Printf("  - %s\n", c)
	}
	return nil
}
