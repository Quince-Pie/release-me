package sshsig

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Signer is one line of an allowed-signers file.
type Signer struct {
	Principals  []string // pattern list; see Match
	Key         ssh.PublicKey
	Namespaces  []string  // pattern list; empty means any namespace
	ValidAfter  time.Time // zero means unbounded
	ValidBefore time.Time
	Comment     string
	Line        int
}

// AllowedSigners is a parsed ssh-keygen(1) ALLOWED SIGNERS file.
type AllowedSigners struct {
	Signers []Signer
}

// ParseAllowedSigners parses the file. Lines with the cert-authority option
// are rejected because certificate signatures are not supported here; a
// policy that needs them cannot be enforced, so it must not pass silently.
func ParseAllowedSigners(r io.Reader) (*AllowedSigners, error) {
	as := &AllowedSigners{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("allowed_signers: line %d: expected \"principals [options] keytype key\"", n)
		}
		s := Signer{Principals: strings.Split(fields[0], ","), Line: n}
		rest := strings.TrimSpace(line[len(fields[0]):])
		key, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			return nil, fmt.Errorf("allowed_signers: line %d: %v", n, err)
		}
		for _, p := range s.Principals {
			if p == "" {
				return nil, fmt.Errorf("allowed_signers: line %d: empty principal", n)
			}
		}
		s.Key, s.Comment = key, comment
		for _, opt := range options {
			name, value, hasValue := strings.Cut(opt, "=")
			value = strings.Trim(value, "\"")
			switch name {
			case "cert-authority":
				return nil, fmt.Errorf("allowed_signers: line %d: cert-authority is not supported", n)
			case "namespaces":
				if !hasValue || value == "" {
					return nil, fmt.Errorf("allowed_signers: line %d: namespaces needs a value", n)
				}
				s.Namespaces = strings.Split(value, ",")
			case "valid-after", "valid-before":
				t, err := parseTime(value)
				if err != nil {
					return nil, fmt.Errorf("allowed_signers: line %d: %s: %v", n, name, err)
				}
				if name == "valid-after" {
					s.ValidAfter = t
				} else {
					s.ValidBefore = t
				}
			default:
				return nil, fmt.Errorf("allowed_signers: line %d: unknown option %q", n, name)
			}
		}
		as.Signers = append(as.Signers, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(as.Signers) == 0 {
		return nil, errors.New("allowed_signers: no keys")
	}
	return as, nil
}

// parseTime accepts ssh-keygen's YYYYMMDD[Z] and YYYYMMDDHHMM[SS][Z] forms.
// Times without a Z suffix are read in the local time zone, as ssh-keygen does.
func parseTime(s string) (time.Time, error) {
	loc := time.Local
	if strings.HasSuffix(s, "Z") {
		loc = time.UTC
		s = strings.TrimSuffix(s, "Z")
	}
	for _, layout := range []string{"20060102", "200601021504", "20060102150405"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (want YYYYMMDD[Z] or YYYYMMDDHHMM[SS][Z])", s)
}

// Match implements ssh's pattern-list matching: comma-separated patterns
// with '*' and '?' wildcards, where a leading '!' negates a pattern; a
// negated match rejects the whole list, and at least one positive pattern
// must match.
func Match(patterns []string, s string) bool {
	matched := false
	for _, p := range patterns {
		negate := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if wildcardMatch(p, s) {
			if negate {
				return false
			}
			matched = true
		}
	}
	return matched
}

func wildcardMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if wildcardMatch(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && wildcardMatch(pattern[1:], s[1:])
	default:
		return s != "" && s[0] == pattern[0] && wildcardMatch(pattern[1:], s[1:])
	}
}

// Result records which policy line accepted a signature.
type Result struct {
	Principal   string // the principal that matched, or the line's pattern when none was requested
	Fingerprint string
	Signer      *Signer
}

// Options constrains a verification.
type Options struct {
	// Namespace the signature must carry (required).
	Namespace string
	// Principal, when set, must be matched by the accepting line. When empty,
	// any line holding the key accepts (ssh-keygen -Y find-principals semantics).
	Principal string
	// Revoked keys reject the signature even if a line holds the key.
	Revoked []ssh.PublicKey
	// Now is the time used for valid-after/valid-before; zero means time.Now().
	Now time.Time
}

// Verify parses the armored signature, verifies it over message, and applies
// the allowed-signers policy. Every failure is an error; there is no partial
// success.
func (as *AllowedSigners) Verify(message io.Reader, armored []byte, opts Options) (*Result, error) {
	if opts.Namespace == "" {
		return nil, errors.New("sshsig: verification requires a namespace")
	}
	sig, err := Parse(armored)
	if err != nil {
		return nil, err
	}
	if err := sig.Verify(message, opts.Namespace); err != nil {
		return nil, err
	}
	keyBytes := sig.PublicKey.Marshal()
	for _, r := range opts.Revoked {
		if bytes.Equal(r.Marshal(), keyBytes) {
			return nil, fmt.Errorf("sshsig: key %s is revoked", sig.Fingerprint())
		}
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var reasons []string
	for i := range as.Signers {
		s := &as.Signers[i]
		if !bytes.Equal(s.Key.Marshal(), keyBytes) {
			continue
		}
		switch {
		case opts.Principal != "" && !Match(s.Principals, opts.Principal):
			reasons = append(reasons, fmt.Sprintf("line %d: principal %q not in %v", s.Line, opts.Principal, s.Principals))
		case len(s.Namespaces) > 0 && !Match(s.Namespaces, opts.Namespace):
			reasons = append(reasons, fmt.Sprintf("line %d: namespace %q not in %v", s.Line, opts.Namespace, s.Namespaces))
		case !s.ValidAfter.IsZero() && now.Before(s.ValidAfter):
			reasons = append(reasons, fmt.Sprintf("line %d: key not valid before %s", s.Line, s.ValidAfter.Format(time.RFC3339)))
		case !s.ValidBefore.IsZero() && !now.Before(s.ValidBefore):
			reasons = append(reasons, fmt.Sprintf("line %d: key expired at %s", s.Line, s.ValidBefore.Format(time.RFC3339)))
		default:
			principal := opts.Principal
			if principal == "" {
				principal = strings.Join(s.Principals, ",")
			}
			return &Result{Principal: principal, Fingerprint: sig.Fingerprint(), Signer: s}, nil
		}
	}
	if len(reasons) == 0 {
		return nil, fmt.Errorf("sshsig: valid signature by %s, but that key is not an allowed signer", sig.Fingerprint())
	}
	return nil, fmt.Errorf("sshsig: valid signature by %s, but no allowed-signers line accepts it: %s", sig.Fingerprint(), strings.Join(reasons, "; "))
}

// ParseRevokedKeys reads a list of public keys, one per line. OpenSSH KRL
// binaries are rejected explicitly rather than misread as an empty list.
func ParseRevokedKeys(data []byte) ([]ssh.PublicKey, error) {
	if bytes.HasPrefix(data, []byte("SSHKRL\n\x00")) {
		return nil, errors.New("revoked keys: binary KRL files are not supported; list public keys instead")
	}
	var keys []ssh.PublicKey
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("revoked keys: line %d: %v", i+1, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// LoadSigner parses an unencrypted OpenSSH private key.
func LoadSigner(pemBytes []byte) (ssh.Signer, error) {
	s, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %v", err)
	}
	return s, nil
}
