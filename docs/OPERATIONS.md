# Operating a release

## Cutting a release

1. Merge the changes; `CHANGELOG.md` accumulates entries under
   `[Unreleased]`.
2. Rotate the changelog and commit it (a pull request or a direct commit):

   ```sh
   release-me changelog release 0.1.0 --compare-url 'https://github.com/OWNER/REPO/compare/{from}...{to}'
   git commit -am "Release 0.1.0"
   ```

3. Create the signed tag and push it. The command validates the changelog,
   creates an annotated tag signed with your SSH key (`git tag -s` under
   `gpg.format=ssh`; agent-held and hardware keys work), verifies it with
   the same predicate CI uses, and only then pushes:

   ```sh
   release-me tag create --tag v0.1.0 --allowed-signers allowed_signers --push origin
   ```

   For a Forgejo remote as well: `--push forgejo`. Both pipelines start from
   the tag push.

4. Watch the pipeline. It fails closed: no release object is published until
   every gate passed. The `Verify as a recipient` job then downloads and
   checks the published release and rebuilds it from the tag.

## What can go wrong, what you see, what to do

| Symptom | Meaning | Action |
| --- | --- | --- |
| `tag verify` fails in CI | The tag is lightweight, unsigned, signed by an unlisted key, not reachable from `main`, or does not match `CHANGELOG.md` | Fix the cause and cut the *next* version. Never move or re-sign a pushed tag: tags are immutable by policy (and by ruleset). A tag without a release is an acceptable state. |
| Reproducibility gate fails (the two builders disagree, or `--rebuild` differs) | Nondeterminism entered the build | Inspect the two `SHA256SUMS` artifacts; compare archives with `diffoscope`. Fix and release the next version. |
| `publish` fails while uploading | Network or platform errors beyond the bounded retries | Rerun the job. It reuses the draft, keeps verified assets, replaces the rest. Nothing is public until the final publish call. |
| `publish` fails with "already exists" and different assets | A published release for the tag holds other bytes (someone published manually, or a previous run published different content) | Investigate. The tool will not touch the published release. If the published bytes are wrong, the remedy is the next version plus a yanked note in the changelog. |
| `publish` reports "already published with identical assets" | A rerun after success | Nothing to do; this is the idempotent path. |
| The publish call itself timed out | Uncertain outcome | The tool re-reads the release; if it is published it reports success. A rerun is safe either way. |
| `verify` job fails after publish | The published release does not verify as a recipient would see it | Treat as a broken release: publish the next version. Do not edit the release. |
| A signing key must be rotated | Key compromise or scheduled rotation | Add the new key to `allowed_signers`, set `valid-before` on the old one (or list it in `revoked_keys`), commit; recipients who pin `allowed_signers` must update their copy. For the Forgejo CI key, replace the `RELEASE_SIGNING_KEY` secret. |
| Sigstore/TUF unavailable during signing or verification | Network or infrastructure outage | Signing fails closed (retry later). Verification can use a pinned `trusted_root.json` and the bundle asset (`docs/VERIFY.md`). |

## Determining the actual outcome after an interrupted run

- GitHub: `gh release view vX.Y.Z` (or the API) shows a draft (`isDraft`)
  or a published release; a draft is invisible to everyone else.
- Forgejo: `GET /api/v1/repos/{o}/{r}/releases/tags/vX.Y.Z` with a write
  token shows the draft; without a token it is 404 until published.
- The tool's `--json` output records `release_url`, `already_published`,
  `uploaded`, `reused` and `deleted` names.

## Retention and cleanup

Nothing is deleted automatically except stale drafts for the same tag
(GitHub) and wrong or duplicate assets inside a draft. Published releases
are never modified or deleted by the tool.

## Enabling platform-side protections (GitHub)

Recommended repository settings, all reachable through the REST API and
described in `docs/QUALIFICATION.md`: immutable releases
(`PUT /repos/{o}/{r}/immutable-releases`), a tag ruleset for `v*`
(creation restricted, no update, no deletion), `sha_pinning_required` for
actions, default `GITHUB_TOKEN` permissions read-only.
