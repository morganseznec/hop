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

Tests never call AWS or GCP. Keep it that way: put cloud calls behind small functions
and test the logic around them with plain data, or against a local server.

When you try your change against real accounts, use accounts you own or are allowed
to use, and prefer `hop ls` and `hop doctor`, which are read-only.

The demo in the README and on the site plays a fictional fleet (`docs/demo`). After a
change that shows in it, record it again with [VHS](https://github.com/charmbracelet/vhs):
`vhs docs/demo/demo.tape` rewrites `docs/demo.gif` and `site/demo.mp4`.

## Tests

Every change of behaviour comes with tests, in the same pull request: a new feature
gets tests of what it does, and a bug fix gets a test that fails without the fix. A
pull request that changes behaviour without a test is not merged. Changes to
documentation, CI or comments alone need none.

## Pull requests

- One topic per pull request, with a short description of the why.
- `gofmt`, `go vet`, the tests and the linter must pass (CI runs them).
- Tests for every behaviour change (see above); the README, `docs/reference.md` and
  the `Unreleased` section of [CHANGELOG.md](CHANGELOG.md) for user-facing ones.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org)
  (`feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`): the release notes are built
  from them.

## Releases

Maintainers tag `vX.Y.Z` on `main`. The release workflow then runs GoReleaser, which
builds hop for macOS, Linux and Windows (amd64 and arm64), packages it as `.tar.gz`
and `.zip` archives and as `.deb`, `.rpm`, `.apk` and Arch Linux packages, signs the
checksums, publishes the GitHub release and updates the Homebrew cask in
`morganseznec/homebrew-tap`. The Docker workflow builds the multi-platform image
(linux/amd64 and linux/arm64), scans, signs and pushes it to
`ghcr.io/morganseznec/hop`.
