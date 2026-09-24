package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Quince-Pie/release-me/internal/changelog"
	"github.com/Quince-Pie/release-me/internal/gittag"
	"github.com/Quince-Pie/release-me/internal/semver"
	"github.com/Quince-Pie/release-me/internal/sshsig"
)

// tagReport is what `tag verify` prints, for the workflow to consume.
type tagReport struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	Principal   string `json:"principal"`
	Fingerprint string `json:"fingerprint"`
	Prerelease  bool   `json:"prerelease"`
	CommitTime  int64  `json:"commit_time"`
}

func cmdTag(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("tag: expected verify | create")
	}
	switch args[0] {
	case "verify":
		return tagVerify(ctx, args[1:])
	case "create":
		return tagCreate(ctx, args[1:])
	}
	return fmt.Errorf("tag: unknown subcommand %q", args[0])
}

// verifyTag is the single definition of a valid release tag, shared by
// `tag verify` (CI) and `tag create` (maintainer).
func verifyTag(ctx context.Context, repoDir, tag, signersPath, principal, branch, changelogPath string) (*tagReport, string, error) {
	if _, err := semver.ParseTag(tag); err != nil {
		return nil, "", fmt.Errorf("tag %q is not vMAJOR.MINOR.PATCH[-pre]: %v", tag, err)
	}
	signersData, err := os.ReadFile(signersPath)
	if err != nil {
		return nil, "", err
	}
	policy, err := sshsig.ParseAllowedSigners(strings.NewReader(string(signersData)))
	if err != nil {
		return nil, "", err
	}
	repo := gittag.Repo{Dir: repoDir}
	t, err := repo.Read(ctx, tag)
	if err != nil {
		return nil, "", err
	}
	if t.ObjectType != "commit" {
		return nil, "", fmt.Errorf("tag %s points at a %s, not a commit", tag, t.ObjectType)
	}
	res, err := t.VerifySignature(policy, sshsig.Options{Principal: principal, Now: t.TaggerTime})
	if err != nil {
		return nil, "", fmt.Errorf("tag %s: %w", tag, err)
	}
	if branch != "" {
		ok, err := repo.IsAncestor(ctx, t.Object, branch)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "", fmt.Errorf("tag %s: commit %s is not reachable from %s", tag, t.Object, branch)
		}
	}
	v, _ := semver.ParseTag(tag)
	notes := ""
	if changelogPath != "" && changelogPath != "none" {
		data, err := repo.ShowFile(ctx, t.Object, changelogPath)
		if err != nil {
			return nil, "", fmt.Errorf("tag %s: %s at %s: %w", tag, changelogPath, t.Object, err)
		}
		c, err := changelog.Parse(string(data))
		if err != nil {
			return nil, "", err
		}
		if err := c.Lint(); err != nil {
			return nil, "", fmt.Errorf("%s at %s:\n%v", changelogPath, t.Object, err)
		}
		latest, ok := c.Latest()
		if !ok || semver.Compare(latest.Version, v) != 0 {
			return nil, "", fmt.Errorf("tag %s: the latest released section of %s at the tagged commit is %s", tag, changelogPath, latest.Version)
		}
		if !c.UnreleasedEmpty() {
			return nil, "", fmt.Errorf("tag %s: the [Unreleased] section of %s is not empty at the tagged commit", tag, changelogPath)
		}
		notes = latest.Body
	}
	ct, err := repo.CommitTime(ctx, t.Object)
	if err != nil {
		return nil, "", err
	}
	return &tagReport{Tag: tag, Version: v.String(), Commit: t.Object, Principal: res.Principal, Fingerprint: res.Fingerprint, Prerelease: v.IsPrerelease(), CommitTime: ct.Unix()}, notes, nil
}

func tagVerify(ctx context.Context, args []string) error {
	fs := flagSet("tag verify", "tag verify --tag vX.Y.Z --allowed-signers FILE [--principal P] [--branch REF] [--changelog FILE|none] [--notes-out FILE] [--json]")
	tag := fs.String("tag", "", "tag name")
	signers := fs.String("allowed-signers", "allowed_signers", "allowed-signers file (namespace git)")
	principal := fs.String("principal", "", "principal the signing key must be listed for (default: any line holding the key)")
	branch := fs.String("branch", "origin/main", "ref the tagged commit must be reachable from (empty to skip)")
	clog := fs.String("changelog", "CHANGELOG.md", "changelog whose latest section must equal the tag (\"none\" to skip)")
	notesOut := fs.String("notes-out", "", "write the changelog section for the version to this file")
	repoDir := fs.String("repo", ".", "repository directory")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tag == "" {
		fs.Usage()
		return errors.New("tag verify: --tag is required")
	}
	r, notes, err := verifyTag(ctx, *repoDir, *tag, *signers, *principal, *branch, *clog)
	if err != nil {
		return err
	}
	if *notesOut != "" {
		if err := os.WriteFile(*notesOut, []byte(notes+"\n"), 0o644); err != nil {
			return err
		}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	fmt.Printf("tag %s OK: commit %s, signed by %s (%s)\n", r.Tag, r.Commit, r.Principal, r.Fingerprint)
	return nil
}

// tagCreate makes the signed annotated tag with git (so agent-held and
// hardware keys work), verifies it with the same predicate CI uses, and
// only then optionally pushes it.
func tagCreate(ctx context.Context, args []string) error {
	fs := flagSet("tag create", "tag create --tag vX.Y.Z --allowed-signers FILE [--principal P] [--branch REF] [--changelog FILE|none] [--key KEY] [--push REMOTE]")
	tag := fs.String("tag", "", "tag name")
	signers := fs.String("allowed-signers", "allowed_signers", "allowed-signers file")
	principal := fs.String("principal", "", "principal the key must be listed for")
	branch := fs.String("branch", "", "ref the commit must be reachable from (default: none, the tag is created at HEAD)")
	clog := fs.String("changelog", "CHANGELOG.md", "changelog to validate and take the tag message from (\"none\" to skip)")
	key := fs.String("key", "", "SSH signing key file (default: git's user.signingkey)")
	push := fs.String("push", "", "remote to push the tag to after it verifies")
	repoDir := fs.String("repo", ".", "repository directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tag == "" {
		fs.Usage()
		return errors.New("tag create: --tag is required")
	}
	v, err := semver.ParseTag(*tag)
	if err != nil {
		return err
	}
	message := *tag
	if *clog != "none" {
		data, err := os.ReadFile(*clog)
		if err != nil {
			return err
		}
		c, err := changelog.Parse(string(data))
		if err != nil {
			return err
		}
		if err := c.Lint(); err != nil {
			return fmt.Errorf("%s:\n%v", *clog, err)
		}
		latest, ok := c.Latest()
		if !ok || semver.Compare(latest.Version, v) != 0 || !c.UnreleasedEmpty() {
			return fmt.Errorf("%s must have %s as its latest section and an empty [Unreleased] section", *clog, v)
		}
		message = fmt.Sprintf("Release %s\n\n%s\n", v, latest.Body)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", *repoDir, "status", "--porcelain", "--untracked-files=no").Output(); err != nil || len(strings.TrimSpace(string(out))) != 0 {
		return errors.New("tag create: the working tree is not clean")
	}
	gitArgs := []string{"-C", *repoDir, "-c", "gpg.format=ssh"}
	if *key != "" {
		gitArgs = append(gitArgs, "-c", "user.signingkey="+*key)
	}
	gitArgs = append(gitArgs, "tag", "--sign", "--annotate", "--message", message, *tag)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git tag: %w", err)
	}
	r, _, err := verifyTag(ctx, *repoDir, *tag, *signers, *principal, *branch, *clog)
	if err != nil {
		exec.CommandContext(ctx, "git", "-C", *repoDir, "tag", "--delete", *tag).Run()
		return fmt.Errorf("the new tag does not pass verification and was deleted: %w", err)
	}
	fmt.Printf("created %s at %s, signed by %s (%s)\n", r.Tag, r.Commit, r.Principal, r.Fingerprint)
	if *push != "" {
		cmd := exec.CommandContext(ctx, "git", "-C", *repoDir, "push", *push, "refs/tags/"+*tag)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}
	return nil
}
