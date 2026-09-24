# Security

## What a release guarantees, and how to check it

See [`docs/CONTRACT.md`](docs/CONTRACT.md) for the guarantees and
[`docs/VERIFY.md`](docs/VERIFY.md) for the checks. In short: the tag is
signed by a key in [`allowed_signers`](allowed_signers); the archives are
listed in `SHA256SUMS`; on GitHub the provenance is signed by this
repository's release workflow identity through Sigstore; on Forgejo the
manifest and provenance are signed by the CI key in `allowed_signers`; the
archives are reproducible from the tag with Go 1.27.1.

## Reporting a vulnerability

Report privately through the repository's security advisories (GitHub:
Security → Report a vulnerability). Please include the affected version and
a way to reproduce. Reports about the release process itself (a signature
that verified when it should not have, a reproduction that fails) are in
scope.

## Key handling

- Maintainers' tag-signing keys never leave their machines; CI never signs
  tags.
- The Forgejo CI signing key is a repository secret injected only into the
  release job; rotating it is a change to `allowed_signers` plus a new
  secret (`docs/OPERATIONS.md`).
- On GitHub no long-lived signing material exists: the job's OIDC identity
  is exchanged for a short-lived certificate at signing time.
