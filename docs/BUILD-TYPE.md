# Build type `release-me` v1

`https://github.com/Quince-Pie/release-me/blob/main/docs/BUILD-TYPE.md#v1`

The SLSA Provenance v1 predicate written by `release-me provenance` uses this
build type. It describes a build of release assets from a git tag with a
declared command and declared toolchain pins, run by a CI workflow or a
person, and packaged by `release-me`.

## externalParameters

| Field | Meaning |
| --- | --- |
| `source.uri` | `git+<clone URL>@refs/tags/<tag>`: the authorized source state |
| `source.digest.gitCommit` | the commit the signed tag points at |
| `source.tag` | the tag name |
| `buildCommand` | the exact command recipients repeat to rebuild the assets |

## internalParameters

| Field | Meaning |
| --- | --- |
| `platform` | `github`, `forgejo` or `local` |

## resolvedDependencies

The first entry is the source (`uri` = `source.uri`, `digest.gitCommit`).
Further entries are named toolchain pins passed with `--toolchain name=uri`,
for example `go` (the upstream toolchain archive URL) and `nixpkgs` (the
locked nixpkgs revision). They are declared inputs, not fetched artifacts.

## runDetails

| Field | Meaning |
| --- | --- |
| `builder.id` | the workflow identity (`<server>/<owner>/<repo>/.github/workflows/<file>@refs/tags/<tag>` on GitHub and Forgejo; a descriptive string for local builds) |
| `builder.version.release-me` | the version of the tool that wrote the statement |
| `metadata.invocationId` | the CI run URL when available |
| `metadata.startedOn` / `finishedOn` | optional timestamps |

## subject

Every entry of the release manifest (`SHA256SUMS`), by name and SHA-256.

## Verification expectations

A verifier checks `predicateType == https://slsa.dev/provenance/v1`,
`buildDefinition.buildType` equals this URI, the subjects equal the
manifest, `source.uri` equals the expected repository at the expected tag,
and, for a keyless bundle, that the certificate identity is the expected
release workflow at that tag. Nothing in the predicate is trusted on its
own: the signature binds it to a builder identity, and reproduction binds
it to reality.
