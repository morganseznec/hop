# Contributing

Thanks for helping. Bug reports, ideas and pull requests are all welcome.

## Before you start

For anything bigger than a small fix, open an issue first so we can agree on the
approach. hop aims to stay a single, dependency-light binary with sensible defaults.

## Development

You need Go (see `go.mod` for the version) and, to try real sessions, the AWS
`session-manager-plugin`.

```bash
make test     # go vet + go test
make lint     # golangci-lint, same version as CI
make build    # ./bin/hop
```

Tests never call AWS. Keep it that way: put AWS calls behind small functions and test
the logic around them with plain data.

When you try your change against real accounts, use accounts you own or are allowed
to use, and prefer `hop ls` and `hop doctor`, which are read-only.

## Pull requests

- One topic per pull request, with a short description of the why.
- `gofmt`, `go vet`, the tests and the linter must pass (CI runs them).
- Add or update tests for behaviour changes, and the README for user-facing ones.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org)
  (`feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`): the release changelog is
  built from them.

## Releases

Maintainers tag `vX.Y.Z` on `main`. GoReleaser then builds the binaries for macOS and
Linux (amd64 and arm64) and publishes the GitHub release.
