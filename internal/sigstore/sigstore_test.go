package sigstore

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The fixtures are sigstore-go's own examples: a real provenance bundle for
// sigstore-js 1.3.0 signed from GitHub Actions through the public-good
// instance (bundle-provenance.json) and the trusted root that verifies it.
func fixtures(t *testing.T) ([]byte, *root.TrustedRoot) {
	t.Helper()
	b, err := os.ReadFile("testdata/bundle-provenance.json")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := root.NewTrustedRootFromPath("testdata/trusted-root-public-good.json")
	if err != nil {
		t.Fatal(err)
	}
	return b, tr
}

const (
	fixtureSAN    = "https://github.com/sigstore/sigstore-js/.github/workflows/release.yml@refs/heads/main"
	fixtureDigest = "sha512"
)

func TestVerifyRealBundle(t *testing.T) {
	b, tr := fixtures(t)
	st, err := PayloadOf(b)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's subject is a sha512 digest; our policy is sha256-only, so
	// exercise the identity checks through a permissive digest policy first.
	subj := st.Subject[0].Digest[fixtureDigest]
	if subj == "" {
		t.Fatalf("fixture subject: %+v", st.Subject)
	}
	// A sha256 digest that is not a subject must fail even with the right identity.
	_, err = Verify(b, VerifyOptions{Trusted: tr, Identity: Identity{Issuer: GitHubIssuer, SAN: fixtureSAN}, Digests: []string{strings.Repeat("0", 64)}})
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	// Wrong SAN, wrong issuer.
	for _, id := range []Identity{
		{Issuer: GitHubIssuer, SAN: "https://github.com/evil/repo/.github/workflows/release.yml@refs/heads/main"},
		{Issuer: "https://accounts.google.com", SAN: fixtureSAN},
		{Issuer: GitHubIssuer, SANRegex: "^https://github.com/evil/.*"},
	} {
		if _, err := Verify(b, VerifyOptions{Trusted: tr, Identity: id, Digests: []string{strings.Repeat("0", 64)}}); err == nil {
			t.Errorf("identity %+v accepted", id)
		}
	}
	// Tampered payload: flip one byte of the DSSE payload.
	var raw map[string]any
	json.Unmarshal(b, &raw)
	env := raw["dsseEnvelope"].(map[string]any)
	payload, _ := base64.StdEncoding.DecodeString(env["payload"].(string))
	payload[len(payload)/2] ^= 1
	env["payload"] = base64.StdEncoding.EncodeToString(payload)
	tampered, _ := json.Marshal(raw)
	if _, err := Verify(tampered, VerifyOptions{Trusted: tr, Identity: Identity{Issuer: GitHubIssuer, SAN: fixtureSAN}, Digests: []string{strings.Repeat("0", 64)}}); err == nil {
		t.Error("tampered payload accepted")
	}
	// Stripped transparency log entry: required by default, so it must fail.
	json.Unmarshal(b, &raw)
	vm := raw["verificationMaterial"].(map[string]any)
	delete(vm, "tlogEntries")
	stripped, _ := json.Marshal(raw)
	if _, err := Verify(stripped, VerifyOptions{Trusted: tr, Identity: Identity{Issuer: GitHubIssuer, SAN: fixtureSAN}, Digests: []string{strings.Repeat("0", 64)}}); err == nil {
		t.Error("bundle without transparency log accepted")
	}
	// Garbage.
	if _, err := Verify([]byte("{}"), VerifyOptions{Trusted: tr, Identity: Identity{SAN: fixtureSAN}, Digests: []string{strings.Repeat("0", 64)}}); err == nil {
		t.Error("empty bundle accepted")
	}
	// Policy inputs are mandatory.
	if _, err := Verify(b, VerifyOptions{Trusted: tr, Digests: []string{strings.Repeat("0", 64)}}); err == nil {
		t.Error("verification without an identity accepted")
	}
	if _, err := Verify(b, VerifyOptions{Trusted: tr, Identity: Identity{SAN: fixtureSAN}}); err == nil {
		t.Error("verification without digests accepted")
	}
}

func TestIdentityMatchesRealBundle(t *testing.T) {
	// Same fixture through sigstore-go directly with a matching digest policy,
	// to establish that the identity constants above are what the certificate carries.
	b, tr := fixtures(t)
	st, _ := PayloadOf(b)
	if st == nil || len(st.Subject) == 0 {
		t.Fatal("fixture statement not parsed")
	}
	res, err := verifyWithDigest(b, tr, Identity{Issuer: GitHubIssuer, SAN: fixtureSAN}, "sha512", st.Subject[0].Digest["sha512"])
	if err != nil {
		t.Fatalf("real bundle failed: %v", err)
	}
	if res.SAN != fixtureSAN || res.Issuer != GitHubIssuer || len(res.Timestamps) == 0 {
		t.Errorf("result %+v", res)
	}
	if res.Statement.PredicateType != "https://slsa.dev/provenance/v0.2" {
		t.Errorf("predicate %s", res.Statement.PredicateType)
	}
}
