// Package gittag decides whether a git tag is an authorized release tag:
// annotated, SSH-signed by an allowed signer (namespace "git", exactly as git
// itself signs), naming the expected commit type, and reachable from the
// release branch. It reads tag objects through the git command but verifies
// the signature in-process, so CI needs no ssh-keygen and the check is the
// same everywhere.
package gittag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Quince-Pie/release-me/internal/sshsig"
)

// Namespace is the SSHSIG namespace git uses for commits and tags.
const Namespace = "git"

// Tag is a parsed annotated tag object.
type Tag struct {
	Name       string // from the "tag" header
	Object     string // the tagged object's hash
	ObjectType string // "commit" for a release tag
	Tagger     string // "Name <email>"
	TaggerTime time.Time
	Message    string // the annotation without the signature
	payload    []byte // bytes covered by the signature
	signature  []byte // armored SSHSIG, nil if unsigned
	pgp        bool
}

// Repo runs git in a working tree.
type Repo struct {
	Dir string
}

func (r Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ErrLightweight is returned for a tag that is not an annotated tag object.
var ErrLightweight = errors.New("tag is lightweight, not an annotated tag object")

// Read loads the tag object for refs/tags/<name>.
func (r Repo) Read(ctx context.Context, name string) (*Tag, error) {
	ref := "refs/tags/" + name
	typ, err := r.git(ctx, "cat-file", "-t", ref)
	if err != nil {
		return nil, fmt.Errorf("tag %s: %w", name, err)
	}
	if strings.TrimSpace(string(typ)) != "tag" {
		return nil, fmt.Errorf("tag %s: %w", name, ErrLightweight)
	}
	raw, err := r.git(ctx, "cat-file", "tag", ref)
	if err != nil {
		return nil, err
	}
	t, err := ParseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("tag %s: %w", name, err)
	}
	if t.Name != name {
		return nil, fmt.Errorf("tag %s: tag object names %q", name, t.Name)
	}
	return t, nil
}

// ParseObject parses the raw content of a tag object (as printed by
// git cat-file tag) into headers, message and trailing signature.
func ParseObject(raw []byte) (*Tag, error) {
	t := &Tag{}
	headerEnd := bytes.Index(raw, []byte("\n\n"))
	if headerEnd < 0 {
		return nil, errors.New("malformed tag object: no header terminator")
	}
	for _, line := range strings.Split(string(raw[:headerEnd]), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("malformed tag header %q", line)
		}
		switch key {
		case "object":
			t.Object = value
		case "type":
			t.ObjectType = value
		case "tag":
			t.Name = value
		case "tagger":
			t.Tagger, t.TaggerTime = parseIdent(value)
		}
	}
	if t.Object == "" || t.ObjectType == "" || t.Name == "" {
		return nil, errors.New("malformed tag object: missing object, type or tag header")
	}
	body := raw[headerEnd+2:]
	// git appends the signature to the message; the signed payload is
	// everything before the line that starts the signature block.
	for _, marker := range []string{"-----BEGIN SSH SIGNATURE-----", "-----BEGIN PGP SIGNATURE-----"} {
		if i := signatureStart(body, marker); i >= 0 {
			t.payload = raw[:headerEnd+2+i]
			t.signature = body[i:]
			t.pgp = strings.Contains(marker, "PGP")
			t.Message = string(body[:i])
			return t, nil
		}
	}
	t.payload = raw
	t.Message = string(body)
	return t, nil
}

// signatureStart returns the index in body where a line equal to marker
// begins, or -1.
func signatureStart(body []byte, marker string) int {
	m := []byte(marker)
	if bytes.HasPrefix(body, m) {
		return 0
	}
	if i := bytes.Index(body, append([]byte("\n"), m...)); i >= 0 {
		return i + 1
	}
	return -1
}

func parseIdent(s string) (string, time.Time) {
	// "Name <email> 1700000000 +0100"
	i := strings.LastIndexByte(s, '>')
	if i < 0 {
		return s, time.Time{}
	}
	who := s[:i+1]
	rest := strings.Fields(s[i+1:])
	if len(rest) < 1 {
		return who, time.Time{}
	}
	secs, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return who, time.Time{}
	}
	return who, time.Unix(secs, 0).UTC()
}

// Signed reports whether the tag carries an SSH signature.
func (t *Tag) Signed() bool { return t.signature != nil && !t.pgp }

// VerifySignature checks the tag's SSH signature against the policy. PGP
// signatures are rejected: this contract accepts SSH signatures only, so
// that the trust anchor is an allowed_signers file every host and every
// clone can carry.
func (t *Tag) VerifySignature(policy *sshsig.AllowedSigners, opts sshsig.Options) (*sshsig.Result, error) {
	if t.signature == nil {
		return nil, errors.New("tag is not signed")
	}
	if t.pgp {
		return nil, errors.New("tag is PGP-signed; only SSH signatures are accepted")
	}
	opts.Namespace = Namespace
	return policy.Verify(bytes.NewReader(t.payload), t.signature, opts)
}

// Commit resolves the tag to the commit it points at (following nested tags).
func (r Repo) Commit(ctx context.Context, name string) (string, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "refs/tags/"+name+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAncestor reports whether commit is reachable from ref.
func (r Repo) IsAncestor(ctx context.Context, commit, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", r.Dir, "merge-base", "--is-ancestor", commit, ref)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base: %v", err)
}

// ShowFile returns the contents of path at revision.
func (r Repo) ShowFile(ctx context.Context, revision, path string) ([]byte, error) {
	return r.git(ctx, "show", revision+":"+path)
}

// CommitTime returns the committer time of a commit.
func (r Repo) CommitTime(ctx context.Context, commit string) (time.Time, error) {
	out, err := r.git(ctx, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(secs, 0).UTC(), nil
}
