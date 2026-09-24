package sshsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func TestSignVerifyPolicy(t *testing.T) {
	signer, pubLine := newKey(t)
	other, otherLine := newKey(t)
	msg := []byte("hello release\n")
	sig, err := Sign(signer, bytes.NewReader(msg), "release")
	if err != nil {
		t.Fatal(err)
	}
	as, err := ParseAllowedSigners(strings.NewReader(
		"# comment\n" +
			"alice@example.com,bob@example.com namespaces=\"release,git\" " + pubLine + " alice\n" +
			"eve@example.com valid-before=\"20200101Z\" " + otherLine + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := as.Verify(bytes.NewReader(msg), sig, Options{Namespace: "release", Principal: "alice@example.com"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Principal != "alice@example.com" || res.Signer.Line != 2 {
		t.Errorf("result = %+v", res)
	}
	if res, err := as.Verify(bytes.NewReader(msg), sig, Options{Namespace: "release"}); err != nil || res.Principal != "alice@example.com,bob@example.com" {
		t.Errorf("find-principals mode: %+v %v", res, err)
	}
	bad := []struct {
		name string
		msg  []byte
		sig  []byte
		opts Options
	}{
		{"tampered message", []byte("hello release!\n"), sig, Options{Namespace: "release"}},
		{"wrong namespace", msg, sig, Options{Namespace: "git"}},
		{"unlisted principal", msg, sig, Options{Namespace: "release", Principal: "mallory@example.com"}},
		{"namespace not allowed for key", msg, sig, Options{Namespace: "file"}},
		{"revoked", msg, sig, Options{Namespace: "release", Revoked: []ssh.PublicKey{signer.PublicKey()}}},
		{"empty namespace", msg, sig, Options{}},
		{"truncated signature", msg, sig[:len(sig)-40], Options{Namespace: "release"}},
		{"garbage", msg, []byte("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"), Options{Namespace: "release"}},
	}
	for _, c := range bad {
		if _, err := as.Verify(bytes.NewReader(c.msg), c.sig, c.opts); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	// A valid signature by a listed but expired key.
	sig2, _ := Sign(other, bytes.NewReader(msg), "release")
	if _, err := as.Verify(bytes.NewReader(msg), sig2, Options{Namespace: "release"}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired key: %v", err)
	}
	if _, err := as.Verify(bytes.NewReader(msg), sig2, Options{Namespace: "release", Now: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Errorf("key valid at 2019: %v", err)
	}
	// Unknown key with a valid signature.
	stranger, _ := newKey(t)
	sig3, _ := Sign(stranger, bytes.NewReader(msg), "release")
	if _, err := as.Verify(bytes.NewReader(msg), sig3, Options{Namespace: "release"}); err == nil || !strings.Contains(err.Error(), "not an allowed signer") {
		t.Errorf("stranger: %v", err)
	}
}

func TestAllowedSignersParsing(t *testing.T) {
	_, pubLine := newKey(t)
	for _, bad := range []string{
		"",
		"# only comments\n",
		"alice cert-authority " + pubLine + "\n",
		"alice bogus=1 " + pubLine + "\n",
		"alice valid-after=\"yesterday\" " + pubLine + "\n",
		"alice namespaces= " + pubLine + "\n",
		"alice ssh-ed25519 AAAA\n",
		"alice, " + pubLine + "\n",
	} {
		if _, err := ParseAllowedSigners(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	as, err := ParseAllowedSigners(strings.NewReader("a,b valid-after=\"20260101\",valid-before=\"202612312359Z\",namespaces=\"x\" " + pubLine + " c\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := as.Signers[0]
	if len(s.Principals) != 2 || s.Namespaces[0] != "x" || s.ValidAfter.IsZero() || s.ValidBefore.Year() != 2026 || s.Comment != "c" {
		t.Errorf("parsed %+v", s)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pats []string
		s    string
		want bool
	}{
		{[]string{"*@example.com"}, "a@example.com", true},
		{[]string{"*@example.com"}, "a@example.org", false},
		{[]string{"*@example.com", "!eve@example.com"}, "eve@example.com", false},
		{[]string{"!eve@example.com"}, "bob@example.com", false},
		{[]string{"a?c"}, "abc", true},
		{[]string{"a?c"}, "ac", false},
		{[]string{"git"}, "git", true},
		{[]string{"git"}, "gitx", false},
	}
	for _, c := range cases {
		if got := Match(c.pats, c.s); got != c.want {
			t.Errorf("Match(%v, %q) = %v", c.pats, c.s, got)
		}
	}
}

// Interoperability with OpenSSH in both directions, when ssh-keygen is available.
func TestOpenSSHInterop(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	run := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("ssh-keygen", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen %v: %v\n%s", args, err, out)
		}
		return out
	}
	run("-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key)
	pubLine, _ := os.ReadFile(key + ".pub")
	msg := filepath.Join(dir, "msg")
	os.WriteFile(msg, []byte("manifest contents\n"), 0o644)
	allowed := filepath.Join(dir, "allowed_signers")
	os.WriteFile(allowed, []byte("rel@example.com namespaces=\"release\" "+string(pubLine)), 0o644)

	// 1. OpenSSH signs, we verify.
	run("-Y", "sign", "-f", key, "-n", "release", msg)
	sig, _ := os.ReadFile(msg + ".sig")
	as, err := ParseAllowedSigners(bytes.NewReader([]byte("rel@example.com namespaces=\"release\" " + string(pubLine))))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := os.ReadFile(msg)
	if _, err := as.Verify(bytes.NewReader(m), sig, Options{Namespace: "release", Principal: "rel@example.com"}); err != nil {
		t.Fatalf("verify OpenSSH signature: %v", err)
	}
	if _, err := as.Verify(bytes.NewReader(m), sig, Options{Namespace: "file"}); err == nil {
		t.Fatal("OpenSSH signature accepted under the wrong namespace")
	}

	// 2. We sign, OpenSSH verifies.
	priv, _ := os.ReadFile(key)
	signer, err := LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := Sign(signer, bytes.NewReader(m), "release")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(msg+".ours.sig", ours, 0o644)
	cmd := exec.Command("ssh-keygen", "-Y", "verify", "-f", allowed, "-I", "rel@example.com", "-n", "release", "-s", msg+".ours.sig")
	cmd.Stdin = bytes.NewReader(m)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen rejected our signature: %v\n%s", err, out)
	}
	cmd = exec.Command("ssh-keygen", "-Y", "verify", "-f", allowed, "-I", "rel@example.com", "-n", "release", "-s", msg+".ours.sig")
	cmd.Stdin = bytes.NewReader([]byte("manifest contents!\n"))
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("ssh-keygen accepted our signature over a different message")
	}
	// Deterministic: Ed25519 signatures are deterministic, so signing twice gives identical bytes.
	again, _ := Sign(signer, bytes.NewReader(m), "release")
	if !bytes.Equal(ours, again) {
		t.Error("Ed25519 signature is not deterministic")
	}
}

func TestTrailingDataRejected(t *testing.T) {
	signer, _ := newKey(t)
	sig, _ := Sign(signer, bytes.NewReader([]byte("m")), "release")
	blob, err := Dearmor(sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(Armor(append(blob, 0))); err == nil {
		t.Error("trailing byte accepted")
	}
	if _, err := Parse(Armor(blob[:len(blob)-1])); err == nil {
		t.Error("truncated blob accepted")
	}
	if _, err := Parse(sig); err != nil {
		t.Errorf("original rejected: %v", err)
	}
}
