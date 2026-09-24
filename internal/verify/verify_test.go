package verify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/host/fake"
	"github.com/Quince-Pie/release-me/internal/intoto"
	"github.com/Quince-Pie/release-me/internal/manifest"
	"github.com/Quince-Pie/release-me/internal/publish"
	"github.com/Quince-Pie/release-me/internal/sshsig"
)

// A complete round trip on both platform fakes: build assets, sign the
// manifest and provenance with an SSH key, publish, then verify as a
// recipient; then tamper with the published release in every way the policy
// must catch.
func TestRoundTripAndAdversarial(t *testing.T) {
	for _, kind := range []fake.Kind{fake.GitHub, fake.Forgejo} {
		t.Run(string(kind), func(t *testing.T) {
			s := fake.New(kind, "o", "r", "tok")
			t.Cleanup(s.Close)
			s.Tags["v1.0.0"] = "c0ffee"
			c := &host.Client{HTTP: &http.Client{Timeout: 5 * time.Second}, Token: "tok"}
			var h host.Host
			if kind == fake.GitHub {
				h = host.NewGitHub(c, s.URL(), s.URL(), s.URL(), "o", "r")
			} else {
				h = host.NewForgejo(c, s.URL(), "o", "r")
			}

			// Assets, manifest, provenance, signatures.
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "tool_1.0.0_linux_amd64.tar.gz"), []byte("AAAA"), 0o644)
			os.WriteFile(filepath.Join(dir, "tool_1.0.0_windows_amd64.zip"), []byte("BBBB"), 0o644)
			m, _ := manifest.FromDir(dir)
			os.WriteFile(filepath.Join(dir, "SHA256SUMS"), m.Bytes(), 0o644)
			st, err := intoto.ProvenanceStatement(m, intoto.BuildInputs{
				Source:  intoto.Source{URI: "git+" + s.URL() + "/o/r@refs/tags/v1.0.0", Commit: "c0ffee", Tag: "v1.0.0"},
				Command: "make dist", Builder: "https://ci.example/o/r/release.yml@refs/tags/v1.0.0", Platform: "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			stBytes, _ := st.Marshal()
			os.WriteFile(filepath.Join(dir, "tool_1.0.0_provenance.intoto.json"), stBytes, 0o644)
			pub, priv, _ := ed25519.GenerateKey(rand.Reader)
			signer, _ := ssh.NewSignerFromKey(priv)
			sshPub, _ := ssh.NewPublicKey(pub)
			for _, name := range []string{"SHA256SUMS", "tool_1.0.0_provenance.intoto.json"} {
				data, _ := os.ReadFile(filepath.Join(dir, name))
				sig, err := sshsig.Sign(signer, bytes.NewReader(data), SSHSigNamespace)
				if err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(dir, name+".sig"), sig, 0o644)
			}
			// Publish.
			if _, err := publish.Run(context.Background(), publish.Options{Host: h, Tag: "v1.0.0", Commit: "c0ffee", Dir: dir}); err != nil {
				t.Fatal(err)
			}

			// Policy.
			policyDir := t.TempDir()
			os.WriteFile(filepath.Join(policyDir, "allowed_signers"), []byte("rel@example.com namespaces=\"release\" "+string(ssh.MarshalAuthorizedKey(sshPub))), 0o644)
			policy := &Policy{Version: 1, Host: string(kind), Server: s.URL(), Repository: "o/r", Prefix: "tool_{version}_",
				Require: []string{"sshsig"}, SSHSig: &SSHSigPolicy{AllowedSigners: "allowed_signers", Principal: "rel@example.com"},
				Source: "git+" + s.URL() + "/o/r", dir: policyDir}
			if kind == fake.GitHub {
				policy.Host = "github"
			}
			run := func() (*Report, error) {
				return Run(context.Background(), Options{Policy: policy, Tag: "v1.0.0", Dir: t.TempDir(), Logf: t.Logf, APIURL: s.URL()})
			}
			rep, err := run()
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if rep.SSHSig == nil || rep.SSHSig.Principal != "rel@example.com" || !rep.SSHSig.ProvenanceSigned || rep.Provenance == nil || rep.Provenance.Commit != "c0ffee" || len(rep.Assets) != 2 {
				t.Fatalf("report %+v", rep)
			}

			rel := s.Release("v1.0.0")
			find := func(name string) *fake.Asset {
				for _, a := range rel.Assets {
					if a.Name == name {
						return a
					}
				}
				t.Fatalf("no asset %s", name)
				return nil
			}
			// Tamper: change an archive's bytes (manifest no longer matches).
			orig := find("tool_1.0.0_linux_amd64.tar.gz").Data
			find("tool_1.0.0_linux_amd64.tar.gz").Data = []byte("EVIL")
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "do not match") {
				t.Errorf("tampered asset: %v", err)
			}
			find("tool_1.0.0_linux_amd64.tar.gz").Data = orig
			// Tamper: replace the manifest and re-sign with a different key (unlisted).
			mOrig := find("SHA256SUMS").Data
			sOrig := find("SHA256SUMS.sig").Data
			_, evilPriv, _ := ed25519.GenerateKey(rand.Reader)
			evil, _ := ssh.NewSignerFromKey(evilPriv)
			evilSig, _ := sshsig.Sign(evil, bytes.NewReader(mOrig), SSHSigNamespace)
			find("SHA256SUMS.sig").Data = evilSig
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "not an allowed signer") {
				t.Errorf("unlisted signer: %v", err)
			}
			// Tamper: signature under another namespace by the right key.
			nsSig, _ := sshsig.Sign(signer, bytes.NewReader(mOrig), "file")
			find("SHA256SUMS.sig").Data = nsSig
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "namespace") {
				t.Errorf("wrong namespace: %v", err)
			}
			find("SHA256SUMS.sig").Data = sOrig
			// Tamper: manifest swapped for one listing different digests (signature no longer valid).
			find("SHA256SUMS").Data = bytes.Replace(mOrig, []byte("tool_1.0.0_windows_amd64.zip"), []byte("tool_1.0.0_windows_arm64.zip"), 1)
			if _, err := run(); err == nil {
				t.Error("edited manifest accepted")
			}
			find("SHA256SUMS").Data = mOrig
			// Tamper: provenance for a different tag (re-signed by the right key).
			st2, _ := intoto.ProvenanceStatement(m, intoto.BuildInputs{Source: intoto.Source{URI: "git+" + s.URL() + "/o/r@refs/tags/v0.9.0", Commit: "c0ffee", Tag: "v0.9.0"}, Builder: "b"})
			st2Bytes, _ := st2.Marshal()
			st2Sig, _ := sshsig.Sign(signer, bytes.NewReader(st2Bytes), SSHSigNamespace)
			pOrig, psOrig := find("tool_1.0.0_provenance.intoto.json").Data, find("tool_1.0.0_provenance.intoto.json.sig").Data
			find("tool_1.0.0_provenance.intoto.json").Data, find("tool_1.0.0_provenance.intoto.json.sig").Data = st2Bytes, st2Sig
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "for tag v0.9.0") {
				t.Errorf("provenance for another tag: %v", err)
			}
			find("tool_1.0.0_provenance.intoto.json").Data, find("tool_1.0.0_provenance.intoto.json.sig").Data = pOrig, psOrig
			// Missing signature.
			for i, a := range rel.Assets {
				if a.Name == "SHA256SUMS.sig" {
					rel.Assets = append(rel.Assets[:i], rel.Assets[i+1:]...)
					break
				}
			}
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "no asset") {
				t.Errorf("missing signature: %v", err)
			}
			// Wrong principal in the policy.
			policy.SSHSig.Principal = "someone@else"
			rel.Assets = append(rel.Assets, &fake.Asset{ID: 999, Name: "SHA256SUMS.sig", Data: sOrig, State: "uploaded", UUID: "u999"})
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "principal") {
				t.Errorf("wrong principal: %v", err)
			}
			policy.SSHSig.Principal = "rel@example.com"
			// Revoked key.
			policy.SSHSig.RevokedKeys = string(ssh.MarshalAuthorizedKey(sshPub))
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "revoked") {
				t.Errorf("revoked key: %v", err)
			}
			policy.SSHSig.RevokedKeys = ""
			// Sigstore required but no bundle published.
			policy.Require = []string{"sshsig", "sigstore"}
			policy.Sigstore = &SigstorePolicy{Issuer: "https://token.actions.githubusercontent.com", Identity: "x"}
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "no asset") {
				t.Errorf("missing bundle: %v", err)
			}
			policy.Require = []string{"sshsig"}
			// Draft releases are not verifiable.
			rel.Draft = true
			if _, err := run(); err == nil {
				t.Error("draft release verified")
			}
			rel.Draft = false
			// Everything restored: passes again.
			if _, err := run(); err != nil {
				t.Fatalf("restored release fails: %v", err)
			}
		})
	}
}

func TestPolicyValidation(t *testing.T) {
	for _, bad := range []Policy{
		{},
		{Version: 1, Host: "gitlab", Repository: "o/r", Require: []string{"sshsig"}},
		{Version: 1, Host: "forgejo", Repository: "o/r", Require: []string{"sshsig"}, SSHSig: &SSHSigPolicy{AllowedSigners: "x"}},
		{Version: 1, Host: "github", Repository: "or", Require: []string{"sshsig"}, SSHSig: &SSHSigPolicy{AllowedSigners: "x"}},
		{Version: 1, Host: "github", Repository: "o/r"},
		{Version: 1, Host: "github", Repository: "o/r", Require: []string{"pgp"}},
		{Version: 1, Host: "github", Repository: "o/r", Require: []string{"sigstore"}, Sigstore: &SigstorePolicy{}},
		{Version: 1, Host: "github", Repository: "o/r", Require: []string{"sshsig"}, SSHSig: &SSHSigPolicy{AllowedSigners: "x"}, Reproduce: &ReproducePolicy{Command: "make"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	good := Policy{Version: 1, Host: "github", Repository: "o/r", Require: []string{"sshsig", "sigstore"},
		SSHSig: &SSHSigPolicy{AllowedSigners: "x"}, Sigstore: &SigstorePolicy{Identity: "y"}, Reproduce: &ReproducePolicy{Command: "make", Dir: "dist"}}
	if err := good.Validate(); err != nil {
		t.Error(err)
	}
	if good.prefix("v1.2.3") != "" || (&Policy{Prefix: "t_{version}_{tag}"}).prefix("v1.2.3") != "t_1.2.3_v1.2.3" {
		t.Error("prefix substitution")
	}
}
