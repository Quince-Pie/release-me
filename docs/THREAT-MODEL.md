# Threat model and trust policy

## Assets

- **The release payload**: the archives recipients run.
- **Release identity**: the mapping from a version to a source commit and to
  a set of bytes.
- **Signing authority**: the maintainers' tag-signing keys; the CI signing
  key (Forgejo) or the CI workflow identity (GitHub); the project's
  `allowed_signers` file.
- **Platform credentials**: CI job tokens, the Forgejo signing-key secret.

## Actors and trust

| Actor | Trusted for | Not trusted for |
| --- | --- | --- |
| Maintainer holding a key listed in `allowed_signers` (namespace `git`) | Deciding that a commit is version X (signing the tag) | Nothing else; the maintainer's machine never builds or uploads the payload |
| The release branch's protection (rulesets, protected tags) | Preventing tag creation, movement and deletion by others | Establishing who signed |
| CI build platform (GitHub-hosted runners; the self-hosted Forgejo runner) | Executing the declared build and the publish steps as configured at the tagged commit | Deciding what to release: it cannot mint a release without a signed tag |
| GitHub as identity provider + Sigstore public-good instance (Fulcio, Rekor, CT log, TSA) | Binding a signature to the workflow identity at a point in time, transparently logged | Deciding that the identity is the project's release workflow (the recipient's policy pins it) |
| The hosting platform's release storage | Serving the bytes it was given | Integrity of those bytes: every asset is re-downloaded and hashed before publication and checked by recipients against the signed manifest |
| Sigstore trust root (TUF) or a pinned `trusted_root.json` | The set of CAs, logs and TSAs valid at each time | — |
| `release-me` itself and its dependencies (sigstore-go, x/crypto) | Correct implementation of the checks | — |

## Attacker-controlled inputs

- Pull requests from anyone: they never run with write tokens
  (`permissions: {}` at the workflow level; Forgejo gives fork PRs read-only
  tokens) and cannot create a signed tag.
- Anyone with push access but no listed key: can push commits and even
  unsigned or wrongly-signed tags; `release-me tag verify` refuses them.
- A compromised CI job token (GitHub `contents: write` during the publish
  job): can create or edit releases of *this* repository through the API,
  but cannot produce a valid Sigstore signature for a different workflow
  identity, cannot sign with the Forgejo key it does not hold (the key is a
  secret injected only into the release job), and cannot alter what a
  recipient's policy accepts. On GitHub with immutable releases enabled it
  also cannot alter a published release.
- The platform's release UI/API used by an administrator to replace assets
  later (Forgejo): detected by recipients because the manifest signature or
  the bundle no longer matches the bytes.
- Network attackers between recipient and platform: TLS, plus the signature
  chain; a substituted asset fails the manifest check.
- Hostile data: asset names (validated against a safe character set before
  any file is written), manifests (strict format, no path components),
  signatures and bundles (parsed with bounded, strict parsers; every failure
  is fatal), changelog and tag objects (strict parsers).

## Consequences of compromise

| Compromised | Effect | Mitigation / detection |
| --- | --- | --- |
| A maintainer's tag-signing key | Attacker can mark arbitrary commits as releases | Remove the key from `allowed_signers` (also honoured by recipients who pin the file); `valid-before` windows and revocation lists limit the exposure; every release is tied to a public commit |
| The Forgejo signing key (repository secret) | Attacker with write access could sign a manifest for a release they can also create | Same removal/rotation; the tag signature by a maintainer key is still required for the pipeline, and recipients can require it in their policy by verifying the tag |
| The GitHub workflow identity (i.e. the ability to run `release.yml` at a tag) | Requires creating a `v*` tag that passes `tag verify`: needs a listed key | Tag rulesets restrict `v*` creation to administrators |
| Sigstore infrastructure | Forged certificates or log entries | Transparency logs make misissuance publicly detectable; recipients may additionally require the SSH signature |
| The hosting platform | Serve wrong bytes or a wrong release object | Recipients verify the signed manifest and the provenance's commit; reproduction from source detects a build that differs from the declared inputs |
| This tool's source | Everything | Reviewable in one repository; reproducible builds let anyone rebuild the published binary and compare |

## What cryptographic validity does not establish

- A valid SSHSIG signature proves that the holder of a key signed these
  bytes under this namespace. Whether that key is authorized comes from
  `allowed_signers`: principal, namespace, validity window, revocation.
- A valid Sigstore bundle proves that Fulcio issued a certificate for some
  OIDC identity and that the corresponding key signed the statement at a
  logged time. Whether that identity is the project's release workflow
  comes from the recipient's policy (`issuer` and exact `identity`, tag
  included).
- A valid provenance statement proves what the builder *claimed* about the
  source and inputs. Whether the claim is true is established by
  reproduction (`verify --reproduce`), not by the signature.
- None of the above establishes that the source is acceptable; that remains
  code review and the maintainers' judgement.

## Residual risks

- A self-hosted Forgejo runner in host mode offers no isolation; a malicious
  workflow change can destroy the host. The reference deployment restricts
  who can push workflow files and runs the runner on a machine dedicated to
  it.
- Forgejo has no server-side release immutability; the signatures make
  tampering detectable but not impossible.
- The Sigstore public-good instance does not trust Forgejo OIDC issuers, so
  the Forgejo path has no transparency-logged, identity-bound signature; it
  has a key-based one instead, whose key management is the operator's.
- GitHub-hosted runners share an image; the two-architecture reproducibility
  gate detects nondeterminism, not a compromised image that produces the
  same wrong bytes twice. Independent reproduction by recipients on their
  own machines closes that gap.
