# Qualification record

How the design was chosen, what was inspected, what was measured, and what
remains open. Dates are 2026-09-24 unless stated. Line references are at the
revisions listed in section 3.

## 1. Contract, acceptance criteria and selection rule (frozen before design)

The release contract is `docs/CONTRACT.md`. The acceptance gates a candidate
had to meet, fixed before any comparison:

| Gate | Requirement |
| --- | --- |
| G1 verify-before-publish | The bytes the host stores are re-read and hashed before the release is visible |
| G2 atomic visibility | Draft, then one publish call; no observer sees a partial release |
| G3 idempotent retry | A rerun after any interruption converges to the same published state without duplicates |
| G4 no mutation of published releases | Byte-identical rerun is a no-op; different bytes fail; nothing is deleted or replaced on a published release |
| G5 reproducibility gate | Independent rebuild compared byte-for-byte before publication, and reproducible by recipients from declared inputs |
| G6 hosts | Executable end-to-end on GitHub and on a genuinely independent non-GitHub host |
| G7 signing | Identity-bound evidence per host with an explicit trust policy |
| G8 recipient verification | One documented, executable recipient path without maintainer credentials; adversarial cases rejected |
| Efficiency | Runner minutes, requests, transfers and dependency surface accounted for; no gate bought by moving cost elsewhere |

Selection rule: every gate is a feasibility constraint; among feasible
designs, prefer fewer trusted components and less work per release, then
smaller operator effort. No weights were invented; where a tradeoff
survived, it is stated in section 6.

Provisional assumptions (stated, not derived from a user requirement): the
reference project is this tool itself; the non-GitHub host is Forgejo
(self-hosted for live tests; Codeberg runs the same software); the
workload is a small CLI with six targets and archives of about 8 MB.

## 2. Contenders and their decisive evidence

Source audits were performed on cloned repositories at the revisions in
section 3 (the audit notes with file:line references are summarized here).

| Contender | Decisive findings | Verdict |
| --- | --- | --- |
| **GoReleaser 2.15.4** (the strongest existing tool for this shape of project) | G1 absent: the upload response is discarded (`internal/client/github.go:720`). G2 holds on the GitHub create path only; with an existing published release it uploads into the live release (`:591-593`), and Gitea/GitLab releases are created visible before upload (`gitea.go:242`, `gitlab.go:498-501`). G3 weak: reruns create a second draft unless `use_existing_draft`; Gitea reruns can duplicate attachments (`gitea.go:358-380`). G4 violated: `replace_existing_artifacts` deletes assets of published releases (`github.go:648-700`). G5 absent (no rebuild-and-compare anywhere). Default build embeds `time.Now()` (`internal/builders/golang/build.go:115-117`); tarballs carry uid/gid/user names unless configured (`pkg/archive/tar/tar.go:73-91`). 374 modules; 85 MB binary. Forgejo/Codeberg not named anywhere. | Rejected on G1, G4, G5; reused as the archive-naming and checksum-format convention |
| **gh CLI 2.100.0 + plain Actions** (conventional control) | `gh release create` does draft → upload → publish (G2) but discards the asset response (`pkg/cmd/release/shared/upload.go:150`, G1 absent), deletes the draft on failure (rollback, no reconciliation), and `--clobber` deletes assets of published releases (G4). GitHub-only. | Rejected on G1, G3, G6 |
| **semantic-release 25 + @semantic-release/github 12** | Version inferred from commit messages; tag pushed before publish so a crash inside publish leaves an un-reconcilable state (`index.js:207-215`); upload responses ignored (`lib/publish.js:151-154`); 449 production packages. | Rejected on G1, G3 |
| **release-please, cargo-dist, release-plz, git-cliff, changesets** | GitHub-only, or no asset handling, or ecosystem-specific (details in section 3) | Not applicable to the contract |
| **The prior scripts** (`Quince-Pie/release-experiment`: bash + curl + jq, GitHub only) | Meets G1 (server digest), G2, G4 and G5 on GitHub; no non-GitHub host; reconciliation deletes stale drafts rather than reusing verified assets; verification via cosign only; failure paths not unit-testable | Superseded; its build and trust decisions (upstream Go toolchain, two-architecture gate, signed tags, changelog as version source) were kept |
| **release-me** (this repository) | Meets G1–G8 as evidenced in section 5 | Selected |

Why a purpose-built tool rather than assembling GoReleaser or gh with
scripts: G1, G3 and G4 are properties of the publication algorithm, and no
existing tool implements them; wrapping a tool that violates G4 does not
make it safe. The publication algorithm is 500 lines of Go with fault
injection tests, which no shell wrapper could match.

Materially different alternatives that were considered for parts of the
design and why they lost:

- **Language**: Go (single static binary, reproducible toolchain,
  sigstore-go is Sigstore's primary Go implementation, x/crypto/ssh for
  SSHSIG) versus bash (the prior scripts; failure paths untestable) versus
  Rust (sigstore-rs less complete for bundle verification; reproducibility
  needs more care) versus Python (interpreter dependency in CI).
- **Build inside the tool** (GoReleaser style) versus **build delegated to
  the project** (this design): delegation keeps the tool language-agnostic
  and lets the reference project use Nix for hermetic pins while the
  contract stays checkable without Nix.
- **Vendored modules** versus **module mode** for the reference build:
  measured (section 5, E3) that a vendored build embeds different build
  information and is not byte-identical to `go build`; module mode via
  nixpkgs' `proxyVendor` file proxy makes the Nix and non-Nix builds equal.
- **SSHSIG library** (hiddeco/sshsig, 42wim/sshsig) versus **in-house**:
  neither parses `allowed_signers`, and 42wim defaults an empty namespace to
  `file`; the in-house implementation is 300 lines with OpenSSH
  interoperability tests in both directions.
- **GitHub's attest actions** versus **sigstore-go in-process**: one binary
  and one code path on every host, and the same bundle format; the price
  was discovering that GitHub's attestation index only accepts its own
  build type (E7), now handled.
- **Verify by server digest** versus **re-download**: GitHub publishes a
  digest, Forgejo does not; re-downloading is the only check that works on
  both and is what a recipient will do. Cost: one download per asset
  (`--trust-server-digest` skips it on GitHub).

## 3. Sources inspected (revisions)

| Source | Revision | What was read |
| --- | --- | --- |
| goreleaser/goreleaser | v2.15.4 (fd20dc19) | internal/client/{github,gitea,gitlab}.go, internal/pipe/{release,archive,checksums,sign,sbom}, builders, pkg/archive |
| goreleaser/goreleaser-action | v7.2.3 (f06c13b6) | src/goreleaser.ts, src/github.ts (checksum verification fail-open) |
| cli/cli | v2.100.0 (45437bc7) | pkg/cmd/release/{create,upload,shared,verify,verify-asset}, pkg/cmd/attestation/{verify,verification,api} |
| semantic-release, @semantic-release/github, @semantic-release/gitlab | v25.0.9, v12.0.10, v13.3.3 | index.js, lib/publish.js, retry configuration |
| googleapis/release-please, axodotdev/cargo-dist, release-plz, git-cliff, changesets | v17.11.2, v0.33.0, 0.3.169, v2.14.2, cli 3.0.3 | host support, asset handling |
| codeberg.org/forgejo/forgejo | v16.0.4 (6e56b5eb) | routers/api/v1/repo/{release,release_attachment,release_tags,tag}.go, services/release, services/attachment, routers/web/repo/attachment.go, routers/api/actions/{oidc,id_token}.go, services/actions/{auth,context,task,secret}.go, models/repo/{release,attachment}.go, models/auth/access_token_scope.go |
| code.forgejo.org/forgejo/runner | v13.1.0 (6095cb17) | labels, host executor, context mapping |
| go-gitea/gitea | v1.27.3 (146cc3ee) | release and attachment API divergence |
| Codeberg-Infrastructure/build-deploy-forgejo | branch codeberg-16 | etc/forgejo/conf/base.ini |
| GitLab | master, docs | lib/api/{releases,generic_packages}.rb, releases/create_service.rb, package_file_finder.rb |
| sigstore/sigstore-go | v1.3.0 (22d3691c) | pkg/sign, pkg/verify (signed_entity.go, signature.go, tlog.go, certificate_identity.go), pkg/root, pkg/bundle, pkg/tuf |
| sigstore/cosign | v3.1.3 (11926fa5) | verify_blob_attestation.go, options; CLI exercised |
| actions/attest-build-provenance, actions/attest, actions/toolkit packages/attest | v4.2.2, v4.2.2, 3.2.0 | signing endpoints, provenance predicate, store.ts |
| openssh/openssh-portable | V_10_5_P1 (b3f73442) | PROTOCOL.sshsig, sshsig.c, ssh-keygen.c; CLI exercised |
| git/git | v2.54.0 (94f05775) | gpg-interface.c (SSH verification flow) |
| sigstore/fulcio | main | config/identity/config.yaml (no Forgejo issuer) |
| GitHub docs and changelog, Forgejo docs, Sigstore blog, SLSA v1.2, in-toto v1 | as of 2026-09-24 | see the prior repository's research digests, re-verified for the facts used here |

Live platform facts established by probing (not documentation): Forgejo
16.0.4 release semantics (one release per tag, draft visibility, duplicate
attachment names accepted, no digest, published releases mutable, tag
minted for a missing tag), Codeberg's attachment settings (`*/*`, 100 MB),
GitHub's asset digest field, upload 422 on duplicates, draft invisibility,
the attestation index's build-type check, Fulcio/Rekor/TSA keyless signing
from an Actions job.

## 4. Design decisions that carry guarantees

| Decision | Mechanism preserved | Where |
| --- | --- | --- |
| Signed annotated tag as the only release trigger | SSHSIG over the tag object, allowed_signers principal/namespace/validity at tagger time, reachability, changelog match | `internal/gittag`, `internal/sshsig`, `cmd tag` |
| Remote tag must equal the verified local commit | Host tag resolution before any release is created | `internal/publish` step 1 |
| Draft → upload → re-download → single publish | Reconcile loop with content-mismatch vs access-failure distinction, bounded rounds, uncertainty resolved by re-reading | `internal/publish` |
| Published releases untouched | Comparison only; no delete/upload/patch after publication | `internal/publish` checkPublished |
| Reproducible payload | Deterministic archiver; upstream Go toolchain by hash; module mode; `--rebuild` and two-architecture gates in CI; recipient `--reproduce` | `internal/archive`, `nix/`, workflows, `internal/verify` |
| Explicit trust policy | Exact identity + issuer (Sigstore) / principal + namespace (SSHSIG); anchored regexes; predicate type and build type checked by the tool, not the library | `internal/verify`, `internal/sigstore` |
| Interoperable evidence | Sigstore bundle v0.3 with certificate, Rekor v1 entry and RFC 3161 timestamp (verifies under cosign, gh and sigstore-go default policies); SSHSIG verifiable by `ssh-keygen -Y verify` | `internal/sigstore`, `internal/sshsig` |

## 5. Evidence

### E1. Unit and fault-injection tests

`go test ./...` at the delivered revision: 12 packages. Coverage of note:
SemVer spec ordering; changelog lint rules; deterministic archives
(order, mode, mtime, time-zone independence, golden bytes); manifest
hostile-input rejection; SSHSIG interoperability with OpenSSH 10.5 in both
directions, deterministic Ed25519 signatures, trailing-data rejection,
namespace/principal/validity/revocation policy; git tag verification with
real `git tag -s` (unsigned, lightweight, wrong key, unreachable, tampered
payload rejected); Sigstore verification of a real public-good bundle with
wrong digest, wrong identity, wrong issuer, tampered payload and stripped
transparency log rejected; host clients on both platform fakes (digest
presence, duplicate-name behaviour, one-release-per-tag, retries, rate
limits, dropped connections, bounded attempts, pagination); publication:
happy path, idempotent rerun, refusal with different bytes without any
mutating request, resumption of a partial draft (reused/replaced/deleted/
duplicate/starter assets), lost responses on create/upload/publish,
transient upload failures, persistent failure leaving a draft and a later
rerun completing it, latest-version decision, oversize refusal, fatal
download failure without re-uploads; recipient verification round trip
with tampered asset, unlisted signer, wrong namespace, edited manifest,
provenance for another tag, missing signature, wrong principal, revoked
key, missing bundle, draft release.

### E2. Reproducibility on this machine

`nix build .#release-assets` followed by `nix build .#release-assets
--rebuild`: identical (E1 of the prior repository's method, repeated here).

### E3. Reproducibility without Nix (stock Go)

At commit 623bfe29, the six archives and `SHA256SUMS` produced by `nix
build` and by plain `go build` + `release-me pack` outside Nix (Go 1.27.1,
same flags, module mode, fresh module cache from proxy.golang.org) are
byte-identical (script: `scripts` reproduction in `docs/VERIFY.md`;
measured SHA256SUMS: 42d58c2d… linux_amd64, 268c65f5… linux_arm64,
bbf4df5b… darwin_amd64, 53ff8773… darwin_arm64, 596faf25… windows_amd64,
24f2f5f6… windows_arm64). A vendored build (`-mod=vendor`) differs: the
embedded build information omits module hashes.

### E4. Reproducibility across CPU architectures on GitHub

CI run 36041077781 on commit 623bfe29: `nix flake check` passed on
`ubuntu-26.04` (5.6 min) and `ubuntu-26.04-arm` (3.5 min); the
`Cross-architecture reproducibility` job found the two builders' `SHA256SUMS`
identical.

### E5. Live publication on GitHub

From this machine against `Quince-Pie/release-me` with a throwaway tag
(`v0.0.1-test.1`, deleted afterwards): draft 395957691 created, four assets
uploaded, every asset re-downloaded and matched, published; a rerun
reported "already published with identical assets" (no mutating request);
a rerun with different bytes was refused (`size 217, local 298` …) and the
public release still carried the original digests; the public download URL
served the manifest. The Release workflow triggered by that lightweight tag
failed before building, as intended.

### E6. Keyless signing from GitHub Actions

Smoke run 36041332412 (branch `sigstore-smoke`): `release-me sign sigstore
--github-actions` obtained the OIDC token, a Fulcio certificate, a Rekor v1
entry and an RFC 3161 timestamp and wrote a v0.3 bundle. Storing it in
GitHub's attestation index was refused with HTTP 422 "unsupported build
type" for this tool's own build type (E7).

### E7. GitHub attestation index and build types

GitHub's `POST /repos/{o}/{r}/attestations` validates the SLSA
`buildType`; provenance signed from GitHub now uses
`https://actions.github.io/buildtypes/workflow/v1` with the release inputs
under `externalParameters.release`. Result of the re-run: see E8.

### E8. Live pipelines (to be completed by the runs in progress)

- GitHub: CI on the delivered revision; Sigstore smoke with the GitHub
  build type; first release `v0.1.0` through `.github/workflows/release.yml`.
- Forgejo 16.0.4 (local instance, self-hosted runner, host executor):
  `.forgejo/workflows/release.yml` on tag `v0.0.1` of a throwaway branch.

### E9. Request budget of a publication (fake hosts, 8 assets)

| Host | Requests | Uploads | Asset downloads |
| --- | --- | --- | --- |
| GitHub, re-download | 37 | 8 | 8 (each an API call plus a redirect) |
| GitHub, `--trust-server-digest` | 21 | 8 | 0 |
| Forgejo | 29 | 8 | 8 |

Bound asserted by test: 14 + 3n. Transfer: each asset is uploaded once and
downloaded once (twice the payload size in total).

### E10. Dependency and size

`release-me` links sigstore-go v1.3.0 and x/crypto; the module graph has
about 70 modules, dominated by sigstore-go's Rekor v1 client. Stripped
binaries: 19.3–21.2 MB per target (E3), compressed archives 7.1–8.4 MB.
govulncheck: one advisory without a fix (GO-2026-5932, x/crypto/openpgp,
reached only through package initialization) is allowlisted with its
reason in `.govulncheck-allow`; grpc was bumped past GO-2026-6348.

## 6. Tradeoffs left open, honestly

- **Verification bandwidth on Forgejo**: without a server digest, the
  pre-publication check downloads every asset once; a maintainer of very
  large releases pays that cost every time. It cannot be removed without
  weakening G1.
- **Forgejo needs an API token secret**: the job token cannot fetch draft
  attachments, so a scoped `write:repository` token lives in the
  repository's secrets. GitHub needs no standing credential.
- **Keyless signing is GitHub-only** (and would work on GitLab); Forgejo's
  OIDC issuer is not trusted by the public Sigstore instance, so Forgejo
  releases carry key-based SSH signatures with operator-managed keys.
- **Two-architecture build on GitHub doubles build minutes** (about 9
  runner-minutes per release for this project) for a determinism check
  that a single `--rebuild` would also catch for path- and time-dependent
  nondeterminism, but not for architecture-dependent code paths.

## 7. Claims

- Contract and required verification: implemented and verified as listed
  in section 5, with the live pipeline results in E8.
- Frontier qualification: achieved for the declared scope (GitHub and
  Forgejo-family hosts, small-to-medium releases). Every contender that
  could reverse the selection was source-audited at a recorded revision;
  none meets G1, G3 and G4 together.
- Optimality: no literal universal-optimality claim is made. The design is
  a Pareto choice under the frozen gates; section 6 lists the tradeoffs.
- Not claimed: GitLab support, Codeberg-hosted live runs (the same
  software was exercised self-hosted), server-side immutability on Forgejo.
