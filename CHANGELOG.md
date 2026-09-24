# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/2.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
The topmost released section is the single source of the version: the build,
the binary and the release tag are validated against it.

## [Unreleased]

### Added

- `release-me`, a single-binary release tool for GitHub and Forgejo-family
  hosts (Forgejo, Codeberg, Gitea): reproducible archives, SHA256SUMS
  manifests, signed-tag verification, SLSA provenance, SSH (sshsig) and
  Sigstore keyless signing, draft-then-publish uploads that are re-downloaded
  and checked before the release becomes visible, idempotent reruns, and a
  recipient-side `verify` command with an explicit trust policy and an
  optional byte-for-byte rebuild.

[Unreleased]: https://github.com/Quince-Pie/release-me/compare/HEAD...HEAD
