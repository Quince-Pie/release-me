package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Quince-Pie/release-me/internal/host"
	"github.com/Quince-Pie/release-me/internal/intoto"
	"github.com/Quince-Pie/release-me/internal/manifest"
	"github.com/Quince-Pie/release-me/internal/sigstore"
	"github.com/Quince-Pie/release-me/internal/sshsig"
	"github.com/Quince-Pie/release-me/internal/verify"
)

func cmdProvenance(_ context.Context, args []string) error {
	fs := flagSet("provenance", "provenance --manifest FILE --source-uri git+URL --commit SHA --tag vX --builder ID --out FILE [--platform P] [--build-command CMD] [--toolchain k=v]... [--started T --finished T]")
	mpath := fs.String("manifest", "SHA256SUMS", "manifest whose entries become the subjects")
	sourceURI := fs.String("source-uri", "", "source URI, e.g. git+https://github.com/o/r (\"@refs/tags/TAG\" is appended)")
	commit := fs.String("commit", "", "source commit")
	tag := fs.String("tag", "", "release tag")
	builder := fs.String("builder", "", "builder identity (default: the CI workflow identity from the environment)")
	platform := fs.String("platform", "", "CI platform: github, forgejo, local (default: detected)")
	command := fs.String("build-command", "", "the build invocation recipients repeat")
	invocation := fs.String("invocation", "", "invocation id (default: the CI run URL from the environment)")
	started := fs.String("started", "", "build start time (RFC3339 or epoch)")
	finished := fs.String("finished", "", "build finish time (RFC3339 or epoch)")
	out := fs.String("out", "", "statement file to write")
	var toolchain multiFlag
	fs.Var(&toolchain, "toolchain", "toolchain pin as name=uri (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sourceURI == "" || *commit == "" || *tag == "" || *out == "" {
		fs.Usage()
		return errors.New("provenance: --source-uri, --commit, --tag and --out are required")
	}
	m, err := loadManifest(*mpath)
	if err != nil {
		return err
	}
	ci := detectCI()
	if *platform == "" {
		*platform = ci.platform
	}
	if *builder == "" {
		*builder = ci.builder
	}
	if *builder == "" {
		return errors.New("provenance: --builder is required outside a CI run")
	}
	if *invocation == "" {
		*invocation = ci.invocation
	}
	in := intoto.BuildInputs{
		Source:    intoto.Source{URI: *sourceURI + "@refs/tags/" + *tag, Commit: *commit, Tag: *tag},
		Command:   *command,
		Toolchain: map[string]string{},
		Builder:   *builder, Platform: *platform, InvocationID: *invocation, ToolVersion: version,
	}
	for _, kv := range toolchain {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("provenance: --toolchain %q is not name=uri", kv)
		}
		in.Toolchain[k] = v
	}
	if *started != "" {
		if in.Started, err = parseTime(*started); err != nil {
			return err
		}
	}
	if *finished != "" {
		if in.Finished, err = parseTime(*finished); err != nil {
			return err
		}
	}
	st, err := intoto.ProvenanceStatement(m, in)
	if err != nil {
		return err
	}
	data, err := st.Marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(data, '\n'), 0o644)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

type ciInfo struct {
	platform, builder, invocation string
}

// detectCI derives the builder identity from GitHub Actions or Forgejo
// Actions environment variables (Forgejo exports the GITHUB_* names too).
func detectCI() ciInfo {
	server := os.Getenv("GITHUB_SERVER_URL")
	repo := os.Getenv("GITHUB_REPOSITORY")
	if server == "" || repo == "" {
		return ciInfo{platform: "local"}
	}
	platform := "github"
	if os.Getenv("GITEA_ACTIONS") == "true" || os.Getenv("FORGEJO_TOKEN") != "" || os.Getenv("GITHUB_ACTIONS") != "true" {
		platform = "forgejo"
	}
	ref := os.Getenv("GITHUB_WORKFLOW_REF") // owner/repo/.github/workflows/x.yml@refs/tags/v1
	builder := ""
	if ref != "" {
		builder = server + "/" + ref
	} else if wf := os.Getenv("GITHUB_WORKFLOW"); wf != "" {
		builder = server + "/" + repo + "/workflows/" + wf + "@" + os.Getenv("GITHUB_REF")
	}
	inv := ""
	if run := os.Getenv("GITHUB_RUN_ID"); run != "" {
		inv = server + "/" + repo + "/actions/runs/" + run
		if a := os.Getenv("GITHUB_RUN_ATTEMPT"); a != "" {
			inv += "/attempts/" + a
		}
	}
	return ciInfo{platform: platform, builder: builder, invocation: inv}
}

func cmdSign(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("sign: expected sshsig | sigstore")
	}
	switch args[0] {
	case "sshsig":
		return signSSH(ctx, args[1:])
	case "sigstore":
		return signSigstore(ctx, args[1:])
	}
	return fmt.Errorf("sign: unknown subcommand %q", args[0])
}

func signSSH(_ context.Context, args []string) error {
	fs := flagSet("sign sshsig", "sign sshsig --key FILE [--namespace release] [--allowed-signers FILE --principal P] FILE...")
	key := fs.String("key", envOr("RELEASE_SIGNING_KEY_FILE"), "OpenSSH private key file (default $RELEASE_SIGNING_KEY_FILE)")
	ns := fs.String("namespace", verify.SSHSigNamespace, "signature namespace")
	signers := fs.String("allowed-signers", "", "verify the new signature against this allowed-signers file before writing it")
	principal := fs.String("principal", "", "principal to verify as")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *key == "" || fs.NArg() == 0 {
		fs.Usage()
		return errors.New("sign sshsig: --key and at least one file are required")
	}
	pem, err := os.ReadFile(*key)
	if err != nil {
		return err
	}
	signer, err := sshsig.LoadSigner(pem)
	if err != nil {
		return err
	}
	var policy *sshsig.AllowedSigners
	if *signers != "" {
		data, err := os.ReadFile(*signers)
		if err != nil {
			return err
		}
		if policy, err = sshsig.ParseAllowedSigners(bytes.NewReader(data)); err != nil {
			return err
		}
	}
	for _, path := range fs.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sig, err := sshsig.Sign(signer, bytes.NewReader(data), *ns)
		if err != nil {
			return err
		}
		if policy != nil {
			if _, err := policy.Verify(bytes.NewReader(data), sig, sshsig.Options{Namespace: *ns, Principal: *principal}); err != nil {
				return fmt.Errorf("the signing key is not accepted by %s: %w", *signers, err)
			}
		}
		if err := os.WriteFile(path+verify.SignatureSuffix, sig, 0o644); err != nil {
			return err
		}
		logf("signed %s (%s)", path, sshsigFingerprint(signer))
	}
	return nil
}

func sshsigFingerprint(s interface{ PublicKey() sshPublicKey }) string {
	return sshsig.Fingerprint(s.PublicKey())
}

func signSigstore(ctx context.Context, args []string) error {
	fs := flagSet("sign sigstore", "sign sigstore --statement FILE --out FILE [--github-actions | --id-token-file FILE] [--timestamp] [--store-github OWNER/REPO]")
	statement := fs.String("statement", "", "in-toto statement to sign")
	out := fs.String("out", "", "bundle file to write")
	gha := fs.Bool("github-actions", false, "obtain the OIDC token from the GitHub Actions runtime")
	tokenFile := fs.String("id-token-file", "", "file containing an OIDC token with audience sigstore")
	timestamp := fs.Bool("timestamp", true, "also request an RFC 3161 timestamp (verifiers that do not read Rekor timestamps need it)")
	store := fs.String("store-github", "", "also store the bundle in this GitHub repository's attestations (needs GITHUB_TOKEN with attestations: write)")
	cache := fs.String("cache-dir", envOr("RELEASE_ME_TUF_CACHE"), "TUF cache directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *statement == "" || *out == "" {
		fs.Usage()
		return errors.New("sign sigstore: --statement and --out are required")
	}
	data, err := os.ReadFile(*statement)
	if err != nil {
		return err
	}
	var idToken string
	switch {
	case *gha:
		idToken, err = sigstore.GitHubActionsIDToken(ctx, "sigstore")
		if err != nil {
			return err
		}
	case *tokenFile != "":
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		idToken = strings.TrimSpace(string(b))
	default:
		return errors.New("sign sigstore: --github-actions or --id-token-file is required")
	}
	bundle, err := sigstore.SignStatement(ctx, bytes.TrimSpace(data), sigstore.SignOptions{IDToken: idToken, CacheDir: *cache, Timestamp: *timestamp})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(bundle, '\n'), 0o644); err != nil {
		return err
	}
	logf("wrote %s", *out)
	if *store != "" {
		owner, repo, err := splitRepo(*store)
		if err != nil {
			return err
		}
		tok := envOr("GITHUB_TOKEN", "GH_TOKEN")
		if tok == "" {
			return errors.New("sign sigstore: --store-github needs GITHUB_TOKEN")
		}
		c := &host.Client{HTTP: &http.Client{Timeout: 2 * time.Minute}, Token: tok, UserAgent: "release-me/" + version, Logf: logf}
		gh := host.NewGitHub(c, envOr("GITHUB_API_URL"), "", envOr("GITHUB_SERVER_URL"), owner, repo)
		id, err := gh.StoreAttestation(ctx, bundle)
		if err != nil {
			return fmt.Errorf("storing the attestation: %w", err)
		}
		logf("stored attestation %d in %s/%s", id, owner, repo)
	}
	_ = manifest.FileName
	return nil
}

func cmdTrustedRoot(_ context.Context, args []string) error {
	fs := flagSet("trusted-root", "trusted-root --out FILE")
	out := fs.String("out", "trusted_root.json", "file to write")
	cache := fs.String("cache-dir", envOr("RELEASE_ME_TUF_CACHE"), "TUF cache directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := sigstore.TrustedRootJSON(*cache)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, data, 0o644)
}
