// Package sshsig implements OpenSSH's SSHSIG signature format
// (openssh-portable PROTOCOL.sshsig) and its allowed-signers policy files, so
// that release manifests and git tags signed with ordinary SSH keys can be
// verified in-process, with exactly the semantics of
//
//	ssh-keygen -Y verify -f allowed_signers -I principal -n namespace -s file.sig < file
//
// and produced so that ssh-keygen accepts them. Only what the release
// contract needs is implemented: file-mode signatures (no certificates, no
// KRL binaries), sha256/sha512 message hashing, and the Ed25519, ECDSA and
// RSA (rsa-sha2-512) key types that x/crypto/ssh verifies.
package sshsig

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	magic      = "SSHSIG"
	sigVersion = 1
	beginLine  = "-----BEGIN SSH SIGNATURE-----"
	endLine    = "-----END SSH SIGNATURE-----"
)

// Signature is a parsed SSHSIG blob.
type Signature struct {
	PublicKey ssh.PublicKey
	Namespace string
	HashAlg   string // "sha256" or "sha512"
	sig       *ssh.Signature
	reserved  []byte
}

// wire is the PROTOCOL.sshsig outer structure after the magic preamble.
type wire struct {
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  string
	HashAlg   string
	Signature []byte
}

// signedData is the blob the key actually signs.
type signedData struct {
	Namespace string
	Reserved  string
	HashAlg   string
	Hash      []byte
}

func hasher(alg string) (hash.Hash, error) {
	switch alg {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("sshsig: unsupported hash algorithm %q", alg)
}

func toBeSigned(namespace, alg string, digest []byte) []byte {
	return append([]byte(magic), ssh.Marshal(signedData{Namespace: namespace, HashAlg: alg, Hash: digest})...)
}

// Sign signs message under namespace with signer, producing the armored
// signature ssh-keygen -Y sign would (RSA keys use rsa-sha2-512, as OpenSSH does).
func Sign(signer ssh.Signer, message io.Reader, namespace string) ([]byte, error) {
	if namespace == "" {
		return nil, errors.New("sshsig: namespace must not be empty")
	}
	const alg = "sha512" // ssh-keygen's default
	h, _ := hasher(alg)
	if _, err := io.Copy(h, message); err != nil {
		return nil, err
	}
	data := toBeSigned(namespace, alg, h.Sum(nil))
	var sig *ssh.Signature
	var err error
	if as, ok := signer.(ssh.AlgorithmSigner); ok && signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		sig, err = as.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA512)
	} else {
		sig, err = signer.Sign(rand.Reader, data)
	}
	if err != nil {
		return nil, err
	}
	blob := append([]byte(magic), ssh.Marshal(wire{
		Version:   sigVersion,
		PublicKey: signer.PublicKey().Marshal(),
		Namespace: namespace,
		HashAlg:   alg,
		Signature: ssh.Marshal(sig),
	})...)
	return Armor(blob), nil
}

// Armor wraps a raw signature blob in the PEM-like envelope, base64 wrapped
// at 70 columns as OpenSSH does.
func Armor(blob []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(blob)
	var out bytes.Buffer
	out.WriteString(beginLine)
	out.WriteByte('\n')
	for len(b64) > 70 {
		out.WriteString(b64[:70])
		out.WriteByte('\n')
		b64 = b64[70:]
	}
	out.WriteString(b64)
	out.WriteByte('\n')
	out.WriteString(endLine)
	out.WriteByte('\n')
	return out.Bytes()
}

// Dearmor extracts the raw blob from an armored signature.
func Dearmor(armored []byte) ([]byte, error) {
	s := string(armored)
	i := strings.Index(s, beginLine)
	if i < 0 {
		return nil, errors.New("sshsig: missing BEGIN SSH SIGNATURE line")
	}
	s = s[i+len(beginLine):]
	j := strings.Index(s, endLine)
	if j < 0 {
		return nil, errors.New("sshsig: missing END SSH SIGNATURE line")
	}
	body := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s[:j])
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("sshsig: invalid base64: %v", err)
	}
	return blob, nil
}

// Parse decodes an armored signature without verifying it.
func Parse(armored []byte) (*Signature, error) {
	blob, err := Dearmor(armored)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(blob, []byte(magic)) {
		return nil, errors.New("sshsig: bad magic preamble")
	}
	var w wire
	if err := ssh.Unmarshal(blob[len(magic):], &w); err != nil {
		return nil, fmt.Errorf("sshsig: malformed signature: %v", err)
	}
	// ssh.Unmarshal rejects trailing data unless the struct ends in a
	// rest-slice, so anything after the signature string is already an error.
	if w.Version != sigVersion {
		return nil, fmt.Errorf("sshsig: unsupported signature version %d", w.Version)
	}
	if w.Namespace == "" {
		return nil, errors.New("sshsig: empty namespace")
	}
	if _, err := hasher(w.HashAlg); err != nil {
		return nil, err
	}
	pub, err := ssh.ParsePublicKey(w.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("sshsig: bad public key: %v", err)
	}
	if _, isCert := pub.(*ssh.Certificate); isCert {
		return nil, errors.New("sshsig: certificate signatures are not supported")
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(w.Signature, &sig); err != nil {
		return nil, fmt.Errorf("sshsig: malformed inner signature: %v", err)
	}
	// PROTOCOL.sshsig: RSA signatures must use rsa-sha2-256 or rsa-sha2-512,
	// never the legacy SHA-1 "ssh-rsa" algorithm.
	if pub.Type() == ssh.KeyAlgoRSA && sig.Format != ssh.KeyAlgoRSASHA256 && sig.Format != ssh.KeyAlgoRSASHA512 {
		return nil, fmt.Errorf("sshsig: RSA signature algorithm %q is not allowed", sig.Format)
	}
	return &Signature{PublicKey: pub, Namespace: w.Namespace, HashAlg: w.HashAlg, sig: &sig, reserved: []byte(w.Reserved)}, nil
}

// Verify checks the signature over message under the expected namespace and
// returns the signing key. It establishes only that the holder of that key
// signed these bytes for that namespace; whether the key is trusted is the
// AllowedSigners policy's decision.
func (s *Signature) Verify(message io.Reader, namespace string) error {
	if s.Namespace != namespace {
		return fmt.Errorf("sshsig: namespace %q does not match expected %q", s.Namespace, namespace)
	}
	h, err := hasher(s.HashAlg)
	if err != nil {
		return err
	}
	if _, err := io.Copy(h, message); err != nil {
		return err
	}
	if err := s.PublicKey.Verify(toBeSigned(s.Namespace, s.HashAlg, h.Sum(nil)), s.sig); err != nil {
		return fmt.Errorf("sshsig: signature does not verify: %v", err)
	}
	return nil
}

// Fingerprint returns the OpenSSH SHA256 fingerprint of the signing key.
func (s *Signature) Fingerprint() string { return ssh.FingerprintSHA256(s.PublicKey) }

// ensure crypto is linked for the hash registration used by x/crypto/ssh.
var _ = crypto.SHA256

// Fingerprint returns the OpenSSH SHA256 fingerprint of a key.
func Fingerprint(k ssh.PublicKey) string { return ssh.FingerprintSHA256(k) }
