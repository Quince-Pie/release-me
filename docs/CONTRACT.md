# The release contract

This document states what a release produced with `release-me` is, what a
recipient may rely on, and what stays outside the guarantee. Everything here
is checked by code in this repository; the checks are named next to each
clause.

## 1. Identity

A release is identified by a git tag `vMAJOR.MINOR.PATCH[-prerelease]`
(Semantic Versioning 2.0.0, `internal/semver`). The tag must be:

- an annotated tag object, not a lightweight ref;
- SSH-signed (`ssh-keygen -Y sign`, as `git tag -s` does with
  `gpg.format=ssh`) by a key listed in the project's `allowed_signers` file
  for the `git` namespace, verified in-process against that file
  (`internal/gittag`, `internal/sshsig`), with the key's `valid-after` and
  `valid-before` windows evaluated at the tagger time, as git does;
- pointing at a commit reachable from the release branch;
- equal to `v` + the topmost released version in `CHANGELOG.md` at that
  commit, whose `[Unreleased]` section is empty (`internal/changelog`).

`release-me tag verify` is the single definition of that predicate; the
maintainer's `release-me tag create` runs it before pushing, the CI
pipelines run it before building, and a recipient can run it against a
clone. The release notes are the changelog section for the version.

Before a release object is created, the host's tag is resolved through the
platform API and must point at the same commit the local verified tag points
at (`internal/publish`, step 1). A host is never allowed to create a tag on
the tool's behalf.

## 2. Payload

The payload of a release is the set of files listed in `SHA256SUMS`: one
archive per target (`<name>_<version>_<os>_<arch>.tar.gz` or `.zip`),
containing the binary and the LICENSE. The manifest lists each archive's
SHA-256 in GNU `sha256sum` format (`internal/manifest`); names are
restricted to a safe character set so that a manifest cannot direct a
recipient outside its download directory.

The payload is **reproducible**: given the tagged source and the declared
inputs, an independent build produces byte-identical archives and therefore
an identical `SHA256SUMS`. The declared inputs are:

- the source tree at the tagged commit;
- the Go toolchain version pinned in `go.mod` (`toolchain go1.27.1`) and,
  for Nix builds, by SHA-256 in `nix/go-toolchain.nix` (the unpatched
  upstream distribution, so a non-Nix `go build` yields the same bytes);
- the module graph pinned by `go.sum` (a module-mode build; a vendored build
  embeds different build information and is not byte-identical);
- the build flags `CGO_ENABLED=0 GOFLAGS="-trimpath -buildvcs=false
  -mod=mod"` and `-ldflags "-s -w -buildid= -X main.version=<version>"`;
- the archive rules of `internal/archive` (sorted members, ownership 0/0,
  modes 0755/0644, one fixed modification time equal to the commit time,
  gzip without name/time, zip deflate with the same time) executed by the
  native `release-me` binary built from the same source;
- for Nix builds, the nixpkgs revision in `flake.lock` (only for packaging
  and development tools; it does not reach the binaries).

How this is checked: every CI run builds the assets, rebuilds them in place
(`nix build --rebuild`, which fails on any differing byte), and the release
pipeline on GitHub requires two builders of different CPU architectures to
produce identical manifests before anything is published. `release-me
verify --reproduce` repeats the build in a checkout of the tag and compares
the rebuilt manifest with the published one byte for byte. The evidence
collected for this repository is in `docs/QUALIFICATION.md`.

Outside the reproducible payload: release notes, the platform's release
object, provenance timestamps, signatures and Sigstore bundles (their
semantics legitimately depend on time and identity). They are verification
evidence, not payload, and are never compared for equality.

## 3. Evidence

Every release carries, next to the payload:

- `SHA256SUMS`, the manifest;
- `<name>_<version>_provenance.intoto.json`, an in-toto Statement v1 with a
  SLSA Provenance v1 predicate whose subjects are the manifest entries and
  whose build definition names the source URI and commit, the tag, the
  build command, the toolchain pins and the builder identity
  (`docs/BUILD-TYPE.md`);
- on GitHub: `<name>_<version>_provenance.sigstore.json`, a Sigstore bundle
  (DSSE over the statement) signed keylessly with the release workflow's
  OIDC identity through Fulcio, logged to Rekor and timestamped, and also
  stored in the repository's attestation index so that `gh attestation
  verify` finds it by digest;
- on Forgejo-family hosts: `SHA256SUMS.sig` and
  `<name>_<version>_provenance.intoto.json.sig`, OpenSSH SSHSIG signatures
  under the `release` namespace by a key listed in `allowed_signers`,
  verifiable with plain `ssh-keygen -Y verify`.

Signatures cover the manifest; the manifest covers the bytes. A recipient
who verifies the signature and then `sha256sum --check`s the manifest has
verified the payload.

## 4. Publication

`release-me publish` guarantees, per run (`internal/publish`):

1. **No host-created tags.** The remote tag must already exist and match the
   verified local commit.
2. **Nothing visible before it is complete.** The release is created as a
   draft; assets are uploaded to the draft; every stored asset is
   re-downloaded through the platform and hashed, and the stored set must
   equal the local set exactly (no missing, extra or duplicate names) before
   the single call that publishes. Observers, webhooks and `release`
   workflow triggers never see a partial release.
3. **Published releases are never modified.** If a published release exists
   for the tag, the run compares its assets with the local ones: identical
   bytes make the run a successful no-op (so a retried job is safe);
   different bytes make it fail without touching anything. Retroactive
   changes are impossible on GitHub when immutable releases are enabled and
   are refused by this tool on hosts without such a setting.
4. **Interrupted runs converge.** A rerun reuses an existing draft, keeps
   assets whose downloaded bytes match, deletes wrong, partial ("starter"),
   stale or duplicate-named assets and re-uploads what is missing, in
   bounded rounds. Requests whose outcome is unknown (a connection lost
   after the server may have acted) are resolved by re-reading the
   platform's state, never by blind retries of non-idempotent requests.
5. **Latest is explicit.** A release is marked latest only if its version
   orders above every published non-pre-release version.
6. **Limits are checked first.** Asset count, per-asset size and, on
   Forgejo, the instance's attachment settings are checked before anything
   is created.

What an operator sees after a failure: either no release object, or a draft
(invisible to the public) whose assets may be partial, plus the tool's
error naming the asset or request that failed. Rerunning the same job is
the recovery procedure; `docs/OPERATIONS.md` has the details.

Not guaranteed: atomicity across two hosts (each host is published
separately and independently), and platform-level immutability on Forgejo
(a repository administrator can still edit a published release through the
UI; the signatures make such a change detectable, not impossible).

## 5. Recipient verification

`release-me verify --policy release-policy.json --tag vX.Y.Z` performs, from
a trust policy the recipient controls (`docs/VERIFY.md`):

1. download of the manifest and every listed asset from the public release
   URL, exactly as a user would;
2. `SHA256SUMS` check of every asset, plus agreement with the platform's own
   digest where it publishes one;
3. the signature checks the policy requires: SSHSIG against
   `allowed_signers` (principal, namespace, validity, revocation) and/or the
   Sigstore bundle against an exact certificate identity and issuer with a
   pinned or TUF-fetched trust root, requiring a certificate transparency
   entry, a transparency-log inclusion proof and a verified timestamp;
4. consistency of the provenance statement: its subjects must equal the
   manifest, its source must be the expected repository at this tag, its
   build type must be the documented one;
5. optionally, an independent rebuild in a clean checkout of the tag and a
   byte-for-byte comparison of the rebuilt manifest with the published one.

Any failure is a failure of the whole verification; there is no partial
success and no silent downgrade when a mechanism is unavailable.
