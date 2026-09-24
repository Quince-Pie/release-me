package sigstore

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/Quince-Pie/release-me/internal/intoto"
)

// verifyWithDigest is the test-only variant of Verify that accepts any
// digest algorithm the fixture uses.
func verifyWithDigest(bundleJSON []byte, tr root.TrustedMaterial, ident Identity, alg, digestHex string) (*Result, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return nil, err
	}
	v, err := verify.NewVerifier(tr, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return nil, err
	}
	id, err := verify.NewShortCertificateIdentity(ident.Issuer, ident.IssuerRegex, ident.SAN, ident.SANRegex)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(digestHex)
	if err != nil {
		return nil, err
	}
	res, err := v.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest(alg, raw), verify.WithCertificateIdentity(id)))
	if err != nil {
		return nil, fmt.Errorf("verification failed: %w", err)
	}
	env := b.GetDsseEnvelope()
	st, err := intoto.ParseStatementLoose(env.GetPayload())
	if err != nil {
		return nil, err
	}
	out := &Result{Statement: st, Payload: env.GetPayload(), SAN: res.VerifiedIdentity.SubjectAlternativeName.SubjectAlternativeName, Issuer: res.VerifiedIdentity.Issuer.Issuer}
	for _, t := range res.VerifiedTimestamps {
		out.Timestamps = append(out.Timestamps, t.Timestamp)
	}
	_ = time.Now
	return out, nil
}
