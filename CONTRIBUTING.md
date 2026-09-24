# Contributing

## Development

```sh
nix develop            # Go 1.27.1 (upstream), gopls, cosign, forgejo, forgejo-runner, linters
go test ./...          # unit and fault-injection tests (no network needed)
nix flake check        # build with tests, gofmt, changelog lint, shell and workflow lint
nix build .#release-assets --rebuild   # determinism on this machine
```

Every pull request must touch `CHANGELOG.md` under `[Unreleased]` unless it
changes nothing a user can observe.

## Live tests

The unit tests run both platform dialects against an in-memory fake. Live
behaviour is exercised by the pipelines themselves and, for Forgejo, by a
local instance: `forgejo web` with a repository, a runner registered with
the label `self-hosted:host`, the secrets `RELEASE_SIGNING_KEY` and
`RELEASE_TOKEN`, then a signed tag push. `docs/QUALIFICATION.md` records
the runs that established the current claims.

## Releasing

See `docs/OPERATIONS.md`. Only a signed annotated tag by a key in
`allowed_signers` releases anything.
