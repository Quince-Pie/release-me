// Package verify is the recipient's side of the release contract: given a
// policy (which repository, which signers, which identities) and a tag, it
// downloads the published release exactly as a user would, checks every
// asset against the signed manifest, verifies the signatures and
// attestations the policy requires, and optionally rebuilds the assets from
// the tagged source and compares them byte for byte.
package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Policy is the recipient's trust policy, normally committed with the
// project as release-policy.json and also published so that a recipient can
// read it once and pin it.
type Policy struct {
	Version int `json:"version"`
	// Host is "github" or "forgejo"; Server is the instance root URL (for
	// Forgejo/Gitea/Codeberg; GitHub defaults to https://github.com).
	Host       string `json:"host"`
	Server     string `json:"server,omitempty"`
	Repository string `json:"repository"`
	// Manifest is the manifest asset name (default SHA256SUMS).
	Manifest string `json:"manifest,omitempty"`
	// Prefix is prepended to evidence asset names, e.g. "release-me_{version}_"
	// with "{version}" and "{tag}" substituted.
	Prefix string `json:"prefix,omitempty"`
	// Require lists the mechanisms that must all pass: "sshsig", "sigstore".
	Require  []string        `json:"require"`
	SSHSig   *SSHSigPolicy   `json:"sshsig,omitempty"`
	Sigstore *SigstorePolicy `json:"sigstore,omitempty"`
	// Source is the expected "git+<clone URL>" of the provenance; the tag is
	// appended as "@refs/tags/<tag>" when comparing.
	Source string `json:"source,omitempty"`
	// Reproduce, when present, is how a recipient rebuilds the assets.
	Reproduce *ReproducePolicy `json:"reproduce,omitempty"`
	dir       string
}

// SSHSigPolicy accepts a manifest signature by a key in an allowed-signers file.
type SSHSigPolicy struct {
	// AllowedSigners is a path (relative to the policy file) or inline
	// lines separated by "\n".
	AllowedSigners string `json:"allowed_signers"`
	Principal      string `json:"principal,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	RevokedKeys    string `json:"revoked_keys,omitempty"`
}

// SigstorePolicy accepts a bundle signed for an identity.
type SigstorePolicy struct {
	Issuer string `json:"issuer"`
	// Identity is the exact SAN, with "{tag}" substituted, e.g.
	// https://github.com/o/r/.github/workflows/release.yml@refs/tags/{tag}.
	Identity string `json:"identity,omitempty"`
	// IdentityRegex must be anchored (^...$): the underlying matcher is not.
	IdentityRegex string `json:"identity_regex,omitempty"`
	// TrustedRoot is an optional path to a pinned trusted_root.json; without
	// it the public-good root is fetched through TUF.
	TrustedRoot string `json:"trusted_root,omitempty"`
}

// ReproducePolicy describes the rebuild.
type ReproducePolicy struct {
	// Command is run with sh -c in a checkout of the tag; it must leave the
	// rebuilt assets (with their manifest) in Dir.
	Command string `json:"command"`
	Dir     string `json:"dir"`
}

// LoadPolicy reads and validates a policy file.
func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("policy: %v", err)
	}
	p.dir = filepath.Dir(path)
	return &p, p.Validate()
}

// Validate checks the policy's consistency.
func (p *Policy) Validate() error {
	var errs []error
	if p.Version != 1 {
		errs = append(errs, fmt.Errorf("policy: unsupported version %d", p.Version))
	}
	if p.Host != "github" && p.Host != "forgejo" {
		errs = append(errs, fmt.Errorf("policy: host must be github or forgejo"))
	}
	if p.Host == "forgejo" && p.Server == "" {
		errs = append(errs, errors.New("policy: forgejo requires server"))
	}
	if !strings.Contains(p.Repository, "/") {
		errs = append(errs, errors.New("policy: repository must be owner/name"))
	}
	if len(p.Require) == 0 {
		errs = append(errs, errors.New("policy: require must list at least one of sshsig, sigstore"))
	}
	for _, r := range p.Require {
		switch r {
		case "sshsig":
			if p.SSHSig == nil || p.SSHSig.AllowedSigners == "" {
				errs = append(errs, errors.New("policy: sshsig requires allowed_signers"))
			}
		case "sigstore":
			if p.Sigstore == nil || (p.Sigstore.Identity == "" && p.Sigstore.IdentityRegex == "") {
				errs = append(errs, errors.New("policy: sigstore requires identity or identity_regex"))
			} else if re := p.Sigstore.IdentityRegex; re != "" && (!strings.HasPrefix(re, "^") || !strings.HasSuffix(re, "$")) {
				errs = append(errs, errors.New("policy: identity_regex must be anchored with ^ and $ (the matcher is unanchored)"))
			}
		default:
			errs = append(errs, fmt.Errorf("policy: unknown mechanism %q", r))
		}
	}
	if p.Reproduce != nil && (p.Reproduce.Command == "" || p.Reproduce.Dir == "") {
		errs = append(errs, errors.New("policy: reproduce requires command and dir"))
	}
	return errors.Join(errs...)
}

// Requires reports whether mechanism is required.
func (p *Policy) Requires(mechanism string) bool {
	for _, r := range p.Require {
		if r == mechanism {
			return true
		}
	}
	return false
}

// resolve reads a policy value that is either a path relative to the policy
// file or inline content (when it contains a newline or starts with a key type).
func (p *Policy) resolve(v string) ([]byte, error) {
	if strings.Contains(v, "\n") || strings.HasPrefix(v, "ssh-") || strings.HasPrefix(v, "ecdsa-") {
		return []byte(v), nil
	}
	path := v
	if !filepath.IsAbs(path) && p.dir != "" {
		path = filepath.Join(p.dir, path)
	}
	return os.ReadFile(path)
}

func (p *Policy) manifestName() string {
	if p.Manifest == "" {
		return "SHA256SUMS"
	}
	return p.Manifest
}

func (p *Policy) prefix(tag string) string {
	return strings.NewReplacer("{tag}", tag, "{version}", strings.TrimPrefix(tag, "v")).Replace(p.Prefix)
}
