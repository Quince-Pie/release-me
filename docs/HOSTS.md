# Host differences and how they are handled

Facts below were established by reading the platforms' source at the
recorded revisions and by probing live instances on 2026-09-24 (Forgejo
16.0.4 locally, codeberg.org, api.github.com). Revision and line references
are in `docs/QUALIFICATION.md`.

| Behaviour | GitHub | Forgejo / Codeberg / Gitea | Consequence in `release-me` |
| --- | --- | --- | --- |
| Server-computed asset digest | Yes (`digest`, sha256, since 2025-06) | No | Every stored asset is re-downloaded and hashed on every host; on GitHub the digest field must additionally agree (`--trust-server-digest` skips the download when it does) |
| Second asset with an existing name | Rejected, HTTP 422 | Accepted silently; which one a name-based download serves is undefined | Duplicate names on a draft are all deleted and uploaded once; on a published release they are a verification failure |
| Failed upload leaves a trace | Possibly an asset in state `starter` | No attachment | `starter` assets are deleted and re-uploaded |
| Releases per tag | Several drafts may coexist; one published | Exactly one row per tag (409 on a second create, draft or not) | Stale extra drafts are deleted on GitHub; Forgejo needs no such step |
| Draft visibility through the by-tag endpoint | Never | With a write token | GitHub drafts are found by scanning the release list |
| Creating a release for a missing tag | Creates the tag at publish time | Creates a lightweight tag at create time (published) or publish time (draft) | The remote tag is resolved and compared with the verified commit before any release is created |
| Immutability after publish | Optional repository setting (tag and assets frozen; GitHub-signed release attestation) | None: assets and the release remain editable; deleting the git tag turns a published release back into a draft | The tool never modifies a published release on any host; on Forgejo, signatures are the only protection against later edits |
| Attachment limits | 2 GiB per asset, 1000 per release | `[attachment] MAX_SIZE` is reported by `/api/v1/settings/attachment` (Codeberg: 100 MB) but not enforced on the API upload path by Forgejo itself; quotas and reverse proxies may enforce it | Sizes are checked against the reported limit before anything is created |
| Draft asset download | Authenticated API redirect to object storage | `/attachments/<uuid>` with a token | Both are used for the pre-publication check |
| Public download URL | `/<owner>/<repo>/releases/download/<tag>/<name>` | same shape | Recipients download the same way on both |
| OIDC identity for Sigstore | Yes; public-good Fulcio trusts the GitHub issuer | Forgejo issues OIDC tokens (`enable-openid-connect`), but the public Sigstore instance does not trust Forgejo issuers | Keyless signing on GitHub; Ed25519 SSHSIG signing with a repository secret on Forgejo |
| Attestation index | `POST /repos/{o}/{r}/attestations`, searchable by digest, `gh attestation verify` | None | The bundle is stored with GitHub and shipped as a release asset; on Forgejo the evidence is assets only |
| CI token permissions | Declared per job (`contents: write`, `id-token: write`, `attestations: write`) | Job token has write access to the repository's units and `permissions:` is ignored, but the job token is not accepted on the web route that serves draft attachments (only API and git routes authenticate it) | GitHub: job token only. Forgejo: an API token with `write:repository` held as the `RELEASE_TOKEN` secret publishes (the tool must re-download draft assets); the job token is used only to check out |
| Workflow location | `.github/workflows/` | First existing of `.forgejo/workflows/`, `.gitea/workflows/`, `.github/workflows/` | Both directories are kept; Forgejo picks `.forgejo/workflows/` |
| Tag name matching | Exact | Case-insensitive on the release row; leading `--` stripped | Tag names are validated as strict SemVer before any lookup |

Gitea 1.27 is API-compatible for everything the tool uses; it additionally
enforces `[repository.release] FILE_MAX_SIZE` with HTTP 413, which the tool
reports as an upload failure.

GitLab is not implemented. Its model differs in kind: releases hold links
rather than files, binaries go to the generic package registry (which
reports `file_sha256` and allows duplicate file names by default), and
there is no draft state, so staging would have to be "upload and verify
packages first, then create the release with its links in one call". The
host interface (`internal/host`) has the shape needed for such a client;
nothing in this repository claims GitLab support.
