# release-me

Verifiable, reproducible release automation for GitHub and Forgejo-family
hosts (Forgejo, Codeberg, Gitea), as one small Go binary plus two workflow
files. It publishes releases that recipients can check, not just download.

What a release made with it means, in one paragraph: a maintainer signed
an annotated tag; CI verified that signature, rebuilt the assets
deterministically (twice, on two CPU architectures on GitHub), wrote a
SLSA provenance statement, signed it with the workflow's Sigstore identity
(GitHub) or a project SSH key (Forgejo), uploaded everything to a draft,
re-downloaded every byte and checked it, and only then published; a
separate job then verified the published release exactly as a recipient
would and rebuilt it from the tag. The full contract, with the code that
enforces each clause, is in [`docs/CONTRACT.md`](docs/CONTRACT.md).

## Verify a release of this project

```sh
release-me verify --policy release-policy.github.json --tag v0.1.0            # GitHub
release-me verify --policy release-policy.github.json --tag v0.1.0 --reproduce # + rebuild and compare
```

or with standard tools only (cosign, gh, ssh-keygen, sha256sum): see
[`docs/VERIFY.md`](docs/VERIFY.md).

## Use it in your project

1. Put a `CHANGELOG.md` in Keep a Changelog format, an `allowed_signers`
   file with your maintainers' SSH public keys (`namespaces="git"`) and,
   for Forgejo, the CI signing key (`namespaces="release"`), and a
   `release-policy.*.json` describing what recipients should require.
   Forgejo repositories need two secrets: `RELEASE_SIGNING_KEY` (the CI
   key, an unencrypted OpenSSH private key) and `RELEASE_TOKEN` (an API
   token with `write:repository`, because the Actions job token cannot read
   draft attachments back for checking).
2. Add the workflows: [`.github/workflows/release.yml`](.github/workflows/release.yml)
   for GitHub and [`.forgejo/workflows/release.yml`](.forgejo/workflows/release.yml)
   for Forgejo/Codeberg. Point their build steps at your own build command;
   the tool only needs a directory of assets and its `SHA256SUMS`.
3. Cut a release:

   ```sh
   release-me changelog release 1.2.0 --compare-url 'https://github.com/OWNER/REPO/compare/{from}...{to}'
   git commit -am "Release 1.2.0"
   release-me tag create --tag v1.2.0 --allowed-signers allowed_signers --push origin
   ```

Operating and recovery notes: [`docs/OPERATIONS.md`](docs/OPERATIONS.md).
Platform differences and how each is handled: [`docs/HOSTS.md`](docs/HOSTS.md).
Threat model: [`docs/THREAT-MODEL.md`](docs/THREAT-MODEL.md).

## Commands

| Command | Purpose |
| --- | --- |
| `pack` | byte-reproducible `.tar.gz` / `.zip` from files, with one fixed timestamp |
| `manifest create\|check\|diff` | `SHA256SUMS` in `sha256sum` format; strict parsing |
| `changelog lint\|latest\|section\|release` | Keep a Changelog as the version source |
| `tag verify\|create` | the release-tag predicate: annotated, SSH-signed by an allowed signer, reachable, matching the changelog |
| `provenance` | in-toto Statement v1 + SLSA Provenance v1 for a manifest ([`docs/BUILD-TYPE.md`](docs/BUILD-TYPE.md)) |
| `sign sshsig` | OpenSSH SSHSIG signatures (`ssh-keygen -Y` compatible), checked against `allowed_signers` before writing |
| `sign sigstore` | keyless Sigstore bundle (Fulcio, Rekor, RFC 3161 timestamp) from a GitHub Actions job; optional storage in GitHub's attestation index |
| `publish` | draft, upload, re-download and hash, publish once; idempotent reruns; never modifies a published release |
| `verify` | the recipient path from a policy file, with optional rebuild |
| `trusted-root` | fetch the Sigstore trusted root for offline verification |

## Building

```sh
nix build .#release-me          # the tool, tests included
nix build .#release-assets      # the six archives + SHA256SUMS, reproducibly
nix build .#release-assets --rebuild   # prove it on this machine
go build ./cmd/release-me       # with Go 1.27.1, module mode: same bytes as the Nix build
```

## Layout

```
cmd/release-me/           the command-line interface
internal/archive          reproducible tar.gz and zip
internal/manifest         SHA256SUMS
internal/semver           Semantic Versioning 2.0.0
internal/changelog        Keep a Changelog parsing, lint, release rotation
internal/sshsig           OpenSSH SSHSIG signing/verification and allowed_signers policy
internal/gittag           annotated-tag parsing and signature verification
internal/intoto           in-toto Statement v1 and SLSA Provenance v1
internal/sigstore         keyless signing and bundle verification (sigstore-go)
internal/host             GitHub and Forgejo clients, retrying HTTP layer, in-memory fakes with fault injection
internal/publish          the draft/upload/verify/publish algorithm
internal/verify           the recipient's policy-driven verification
nix/                      upstream Go toolchain pin, package, release assets, changelog-derived version
docs/                     contract, threat model, hosts, verification, operations, build type, qualification record
```

## Status and limits

- GitHub and Forgejo (self-hosted; Codeberg uses the same API) are the
  supported hosts. GitLab's model (links + package registry, no drafts) is
  analysed in `docs/HOSTS.md` but not implemented.
- Keyless signing needs an OIDC issuer the public Sigstore instance trusts;
  Forgejo's is not, so Forgejo releases carry SSH signatures instead.
- Forgejo has no server-side release immutability; the signatures make
  tampering detectable, not impossible.

License: Apache-2.0.
