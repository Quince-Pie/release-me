// Package sigstore signs in-toto statements as Sigstore bundles with a
// keyless (OIDC) identity and verifies bundles against an explicit identity
// and artifact policy, using sigstore-go, Sigstore's primary Go
// implementation. A bundle produced here is verifiable by cosign, by
// `gh attestation verify` (once stored with the repository) and by this tool.
//
// What a verified bundle proves: a certificate for the stated identity was
// issued by the trusted Fulcio at a time covered by a trusted timestamp
// (Rekor's integrated time or an RFC 3161 token), that key signed the DSSE
// envelope, and the statement it wraps names the given artifact digests as
// subjects. It does not by itself prove the identity was authorized to
// release: the recipient's policy supplies that.
package sigstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"

	"github.com/Quince-Pie/release-me/internal/intoto"
)

// GitHubIssuer is the OIDC issuer of GitHub Actions tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// TrustedRoot loads Sigstore trust material: from a file when path is set,
// otherwise from the public-good TUF repository (cached under cacheDir or
// the sigstore-go default).
func TrustedRoot(path, cacheDir string) (*root.TrustedRoot, error) {
	if path != "" {
		return root.NewTrustedRootFromPath(path)
	}
	client, err := tufClient(cacheDir)
	if err != nil {
		return nil, err
	}
	return root.GetTrustedRoot(client)
}

// TrustedRootJSON fetches the current public-good trusted_root.json, for
// recipients who want to pin it and verify offline later.
func TrustedRootJSON(cacheDir string) ([]byte, error) {
	client, err := tufClient(cacheDir)
	if err != nil {
		return nil, err
	}
	return client.GetTarget("trusted_root.json")
}

func tufClient(cacheDir string) (*tuf.Client, error) {
	opts := tuf.DefaultOptions()
	if cacheDir != "" {
		opts.CachePath = cacheDir
	}
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPUserAgent("release-me")
	opts.Fetcher = f
	return tuf.New(opts)
}

// GitHubActionsIDToken requests an OIDC token from the Actions runtime
// (requires the job permission id-token: write).
func GitHubActionsIDToken(ctx context.Context, audience string) (string, error) {
	reqURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	reqTok := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqTok == "" {
		return "", errors.New("not running in GitHub Actions with id-token: write (ACTIONS_ID_TOKEN_REQUEST_URL/TOKEN unset)")
	}
	u, err := url.Parse(reqURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "bearer "+reqTok)
	req.Header.Set("Accept", "application/json; api-version=2.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("id token request: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Value == "" {
		return "", fmt.Errorf("id token request: unexpected response")
	}
	return out.Value, nil
}

// SignOptions configure keyless signing.
type SignOptions struct {
	IDToken string
	// TrustedRoot, when set, makes sigstore-go verify the bundle it produced.
	TrustedRoot root.TrustedMaterial
	// CacheDir for the TUF signing-config/trusted-root cache.
	CacheDir string
	// RekorVersion selects the transparency log API (1 = the public-good
	// default that every current verifier understands).
	RekorVersion uint32
	// Timestamp requests an RFC 3161 timestamp in addition to the Rekor entry.
	Timestamp bool
}

// SignStatement signs an in-toto statement keylessly and returns the
// bundle as JSON (application/vnd.dev.sigstore.bundle.v0.3+json).
func SignStatement(ctx context.Context, statement []byte, o SignOptions) ([]byte, error) {
	if o.IDToken == "" {
		return nil, errors.New("sigstore: an OIDC identity token is required")
	}
	if _, err := intoto.ParseStatement(statement); err != nil {
		return nil, err
	}
	client, err := tufClient(o.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("sigstore: TUF: %w", err)
	}
	sc, err := root.GetSigningConfig(client)
	if err != nil {
		return nil, fmt.Errorf("sigstore: signing config: %w", err)
	}
	now := time.Now()
	fulcio, err := root.SelectService(sc.FulcioCertificateAuthorityURLs(), sign.FulcioAPIVersions, now)
	if err != nil {
		return nil, fmt.Errorf("sigstore: selecting Fulcio: %w", err)
	}
	version := o.RekorVersion
	if version == 0 {
		version = 1
	}
	rekors, err := root.SelectServices(sc.RekorLogURLs(), sc.RekorLogURLsConfig(), []uint32{version}, now)
	if err != nil {
		return nil, fmt.Errorf("sigstore: selecting Rekor v%d: %w", version, err)
	}
	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, err
	}
	opts := sign.BundleOptions{
		Context:                    ctx,
		CertificateProvider:        sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcio.URL, Timeout: 30 * time.Second, Retries: 2}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{IDToken: o.IDToken},
		TrustedRoot:                o.TrustedRoot,
	}
	for _, r := range rekors {
		opts.TransparencyLogs = append(opts.TransparencyLogs, sign.NewRekor(&sign.RekorOptions{BaseURL: r.URL, Timeout: 90 * time.Second, Retries: 2, Version: r.MajorAPIVersion}))
	}
	if o.Timestamp {
		tsas, err := root.SelectServices(sc.TimestampAuthorityURLs(), sc.TimestampAuthorityURLsConfig(), sign.TimestampAuthorityAPIVersions, now)
		if err != nil {
			return nil, fmt.Errorf("sigstore: selecting a timestamp authority: %w", err)
		}
		for _, t := range tsas {
			opts.TimestampAuthorities = append(opts.TimestampAuthorities, sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{URL: t.URL, Timeout: 30 * time.Second, Retries: 2}))
		}
	}
	if o.TrustedRoot == nil {
		// Verify what we sign with the same trust root recipients will use.
		tr, err := root.GetTrustedRoot(client)
		if err != nil {
			return nil, fmt.Errorf("sigstore: trusted root: %w", err)
		}
		opts.TrustedRoot = tr
	}
	pb, err := sign.Bundle(&sign.DSSEData{Data: statement, PayloadType: intoto.PayloadType}, keypair, opts)
	if err != nil {
		return nil, fmt.Errorf("sigstore: signing: %w", err)
	}
	b, err := bundle.NewBundle(pb)
	if err != nil {
		return nil, err
	}
	return b.MarshalJSON()
}

// Identity is the certificate identity a verifier requires. Exact values
// take precedence over regular expressions; an empty issuer with an empty
// issuer regex matches any issuer (needed for GitHub-issued release
// attestations, which carry no issuer extension).
type Identity struct {
	Issuer      string
	IssuerRegex string
	SAN         string
	SANRegex    string
}

// VerifyOptions configure verification.
type VerifyOptions struct {
	Trusted  root.TrustedMaterial
	Identity Identity
	// Digests the statement must list as subjects ("sha256" hex).
	Digests []string
	// TransparencyLog requires a Rekor inclusion proof (default true).
	// GitHub's own release attestations have none and carry a signed
	// timestamp instead; set it false and SignedTimestamp true for them.
	NoTransparencyLog bool
	SignedTimestamp   bool
}

// Result is what verification established.
type Result struct {
	Statement *intoto.Statement
	Payload   []byte
	SAN       string
	Issuer    string
	// Timestamps are the verified times the signature was observed.
	Timestamps []time.Time
}

// Verify checks a bundle against the policy.
func Verify(bundleJSON []byte, o VerifyOptions) (*Result, error) {
	if o.Trusted == nil {
		return nil, errors.New("sigstore: no trusted material")
	}
	if o.Identity.SAN == "" && o.Identity.SANRegex == "" {
		return nil, errors.New("sigstore: an identity (SAN or SAN regex) is required")
	}
	if len(o.Digests) == 0 {
		return nil, errors.New("sigstore: at least one artifact digest is required")
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, fmt.Errorf("sigstore: bundle: %w", err)
	}
	vopts := []verify.VerifierOption{verify.WithSignedCertificateTimestamps(1)}
	if o.NoTransparencyLog {
		if !o.SignedTimestamp {
			return nil, errors.New("sigstore: without a transparency log a signed timestamp is required")
		}
	} else {
		vopts = append(vopts, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	}
	if o.SignedTimestamp {
		vopts = append(vopts, verify.WithSignedTimestamps(1))
	}
	v, err := verify.NewVerifier(o.Trusted, vopts...)
	if err != nil {
		return nil, err
	}
	issuerRegex := o.Identity.IssuerRegex
	if o.Identity.Issuer == "" && issuerRegex == "" {
		issuerRegex = ".*"
	}
	id, err := verify.NewShortCertificateIdentity(o.Identity.Issuer, issuerRegex, o.Identity.SAN, o.Identity.SANRegex)
	if err != nil {
		return nil, err
	}
	digests := make([]verify.ArtifactDigest, 0, len(o.Digests))
	for _, d := range o.Digests {
		raw, err := hex.DecodeString(d)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("sigstore: invalid sha256 digest %q", d)
		}
		digests = append(digests, verify.ArtifactDigest{Algorithm: "sha256", Digest: raw})
	}
	res, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigests(digests), verify.WithCertificateIdentity(id)))
	if err != nil {
		return nil, fmt.Errorf("sigstore: verification failed: %w", err)
	}
	env := b.GetDsseEnvelope()
	if env == nil {
		return nil, errors.New("sigstore: bundle does not contain a DSSE envelope")
	}
	if env.GetPayloadType() != intoto.PayloadType {
		return nil, fmt.Errorf("sigstore: payload type %q is not %s", env.GetPayloadType(), intoto.PayloadType)
	}
	st, err := intoto.ParseStatement(env.GetPayload())
	if err != nil {
		return nil, err
	}
	out := &Result{Statement: st, Payload: env.GetPayload()}
	if res.VerifiedIdentity != nil {
		out.SAN = res.VerifiedIdentity.SubjectAlternativeName.SubjectAlternativeName
		out.Issuer = res.VerifiedIdentity.Issuer.Issuer
	}
	for _, t := range res.VerifiedTimestamps {
		out.Timestamps = append(out.Timestamps, t.Timestamp)
	}
	return out, nil
}

// PayloadOf extracts the DSSE payload of a bundle without verifying it, so
// that a caller can pick which bundle applies before verification.
func PayloadOf(bundleJSON []byte) (*intoto.Statement, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, fmt.Errorf("sigstore: bundle: %w", err)
	}
	env := b.GetDsseEnvelope()
	if env == nil {
		return nil, errors.New("sigstore: bundle does not contain a DSSE envelope")
	}
	return intoto.ParseStatementLoose(env.GetPayload())
}
