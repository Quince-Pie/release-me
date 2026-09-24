# Verifying a release

Two ways: with `release-me` (applies the whole policy and, optionally,
rebuilds) or with standard tools only.

## With release-me

Get a `release-me` binary you trust (build it from source: `go build
./cmd/release-me` with Go 1.27.1, or `nix build .#release-me`), then:

```sh
release-me verify --policy release-policy.github.json --tag v0.1.0
release-me verify --policy release-policy.forgejo.json --tag v0.1.0
```

The policy file is the recipient's statement of trust. Read it once, keep a
copy, and pass your copy (its content is what makes the check
meaningful). Fields:

```json
{
  "version": 1,
  "host": "github",                     // or "forgejo" (+ "server": "https://codeberg.org")
  "repository": "Quince-Pie/release-me",
  "manifest": "SHA256SUMS",
  "prefix": "release-me_{version}_",    // evidence asset names are <prefix>provenance.*
  "require": ["sigstore"],              // every listed mechanism must pass
  "sigstore": {
    "issuer": "https://token.actions.githubusercontent.com",
    "identity": "https://github.com/Quince-Pie/release-me/.github/workflows/release.yml@refs/tags/{tag}",
    "trusted_root": "trusted_root.json" // optional pin for offline verification
  },
  "sshsig": {                           // when "sshsig" is required
    "allowed_signers": "allowed_signers",
    "principal": "release-me-forgejo-ci",
    "namespace": "release",
    "revoked_keys": "revoked_keys"      // optional
  },
  "source": "git+https://github.com/Quince-Pie/release-me",
  "reproduce": { "command": "nix build .#release-assets --out-link rebuilt-assets", "dir": "rebuilt-assets" }
}
```

What `verify` does, in order, and what a failure at each step means:

1. reads the release object; a draft is refused;
2. downloads `SHA256SUMS` and every asset it lists from the public release
   URL, refusing unsafe or duplicate names; checks every digest and, on
   GitHub, the platform's own `digest`;
3. `sshsig`: verifies `SHA256SUMS.sig` (and the provenance signature if the
   statement is published) against `allowed_signers` with the given
   principal, namespace, validity windows and revocation list;
4. `sigstore`: verifies `<prefix>provenance.sigstore.json` against the trust
   root: certificate chain, certificate-transparency entry, Rekor inclusion
   proof, a verified timestamp, exact issuer and identity (with `{tag}`
   substituted), and that every manifest entry is a subject;
5. provenance: subjects equal the manifest, the source is
   `<source>@refs/tags/<tag>`, the build type is the documented one;
6. `--reproduce`: in a clean checkout of the tag (`--repo-dir`, default `.`),
   runs the policy's command and requires the rebuilt manifest to be
   byte-identical to the published one.

`--json` prints a report with the verified identity, timestamps, provenance
and per-asset digests. Exit status 0 means every required check passed;
anything else is a failure, printed on stderr.

Offline verification: fetch the trust root once while online
(`release-me trusted-root --out trusted_root.json`), set
`sigstore.trusted_root` in your policy, pre-download the assets into a
directory and pass `--dir`.

## With standard tools

GitHub release (Sigstore):

```sh
tag=v0.1.0; ver=${tag#v}; base=https://github.com/Quince-Pie/release-me/releases/download/$tag
curl -fsSLO "$base/SHA256SUMS"; curl -fsSLO "$base/release-me_${ver}_linux_amd64.tar.gz"
curl -fsSLO "$base/release-me_${ver}_provenance.sigstore.json"
sha256sum --check --ignore-missing SHA256SUMS
cosign verify-blob-attestation \
  --bundle "release-me_${ver}_provenance.sigstore.json" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/Quince-Pie/release-me/.github/workflows/release.yml@refs/tags/$tag" \
  --type slsaprovenance1 "release-me_${ver}_linux_amd64.tar.gz"
# or, with the GitHub CLI (uses GitHub's attestation index):
gh attestation verify "release-me_${ver}_linux_amd64.tar.gz" --repo Quince-Pie/release-me \
  --cert-identity "https://github.com/Quince-Pie/release-me/.github/workflows/release.yml@refs/tags/$tag"
```

Forgejo/Codeberg release (SSH signature):

```sh
tag=v0.1.0; base=https://codeberg.org/OWNER/release-me/releases/download/$tag
curl -fsSLO "$base/SHA256SUMS"; curl -fsSLO "$base/SHA256SUMS.sig"; curl -fsSLO "$base/release-me_${tag#v}_linux_amd64.tar.gz"
ssh-keygen -Y verify -f allowed_signers -I release-me-forgejo-ci -n release -s SHA256SUMS.sig < SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
```

`allowed_signers` is in the repository at the tag; pin your own copy.

Verifying the release tag itself (any host, any clone):

```sh
git -c gpg.ssh.allowedSignersFile=allowed_signers verify-tag v0.1.0
# or, in-process, with the same predicate CI uses:
release-me tag verify --tag v0.1.0 --allowed-signers allowed_signers --branch origin/main
```

## Reproducing the payload without Nix

With Go 1.27.1 (the version in `go.mod`'s `toolchain` line) in a checkout of
the tag:

```sh
export CGO_ENABLED=0 GOFLAGS="-trimpath -buildvcs=false -mod=mod" GOTOOLCHAIN=local
ver=$(release-me changelog latest)                 # or read CHANGELOG.md
epoch=$(git show -s --format=%ct HEAD)
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${t%/*} arch=${t#*/} exe=release-me; [ "$os" = windows ] && exe=release-me.exe
  GOOS=$os GOARCH=$arch go build -ldflags "-s -w -buildid= -X main.version=$ver" -o "out/$os-$arch/$exe" ./cmd/release-me
  ext=tar.gz; [ "$os" = windows ] && ext=zip
  release-me pack --out "dist/release-me_${ver}_${os}_${arch}.$ext" --mtime "$epoch" "$exe=out/$os-$arch/$exe" LICENSE=LICENSE
done
release-me manifest create dist && diff dist/SHA256SUMS SHA256SUMS
```

`release-me pack` here can be the native binary you just built for your own
platform. The archives depend only on the binaries, `LICENSE`, the member
names, the commit time and the packer's Go version.
