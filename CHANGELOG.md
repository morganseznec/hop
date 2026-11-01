# Changelog

All notable changes to hop are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and hop uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- The README and the help of `hop run` and `hop push` said that CloudTrail keeps the
  command and the pushed file. CloudTrail records the call but hides its parameters:
  the command and the file content stay in the SSM command history of the account.

## [0.3.1] - 2026-10-31

Fixes from an audit of the experimental GCP support.

### Fixed

- GCP (experimental): instances sort after the AWS ones, grouped by project, instead of
  among the first AWS profile's. Tab completion no longer starts gcloud in the
  background, which would open a console window on Windows. A project answering 403
  for a quota, or a zone that does not answer, is reported instead of looking empty.
  `-p gcp:<project>` that matches nothing, and `-p gcp` with `-r`, now say so. The
  gcloud token is no longer waited for past the timeout on Windows, and a project
  listed twice is queried once. `-p` completion offers `gcp`.
- The documentation covers GCP in the Docker notes, the security notes, exit codes,
  limits and environment variables, and `hop --help`.

## [0.3.0] - 2026-10-31

This release adds experimental GCP support: hop finds Compute Engine instances across
your GCP projects and opens shells on them through IAP.

### Added

- GCP support, experimental: with `gcp_projects` in the configuration (`["*"]` for every
  project gcloud's account can see), `hop ls` and the search list the Compute Engine
  instances of those projects as `gcp:<project>`, in their zone, and `hop <name>`
  opens a shell through IAP with `gcloud compute ssh --tunnel-through-iap`, after
  checking that the instance is still the one listed. GCP does not record shell
  sessions: hop says so, and `transcript: require` refuses them. `run`, `tunnel`,
  `push` and `ssh-proxy` are AWS only for now: on a GCP instance they print the
  gcloud command that does it. `hop doctor` checks gcloud.
- An animated demo in the README and on the site, recorded with VHS against a
  fictional fleet (`docs/demo`). It replaces the site's typed mockup, which showed a
  line hop never prints.

## [0.2.0] - 2026-10-31

This release brings ssh, scp and rsync by instance name, and the shell integration to
bash and fish.

### Added

- `hop ssh-proxy`, a `ProxyCommand` for ssh, and `hop init ssh`, which prints the
  `~/.ssh/config` block for it. After `hop init ssh >> ~/.ssh/config`,
  `ssh ec2-user@tools.hop` reaches the instance hop finds for `tools`, through
  Session Manager (`AWS-StartSSHSession`), and so do `scp`, `rsync`, `git`, `sftp` and
  editors' remote mode. ssh gives hop no terminal, so only an unambiguous match
  connects. With `transcript: require`, hop refuses: Session Manager does not log
  SSH. The block runs hop by its full path, since editors start ssh with a short
  `PATH`.
- `hop init bash` and `hop init fish`, like `hop init zsh`: completion, and an
  optional `--alias` with the same completion. The bash version works with or without
  the `bash-completion` package.
- README: a comparison with other Session Manager clients, with what each does
  better.

### Changed

- When `session-manager-plugin` is not on `PATH`, hop also looks where its installers
  put it (`/opt/homebrew/bin`, `/usr/local/bin`, `/usr/local/sessionmanagerplugin/bin`,
  `/usr/bin`, or `Program Files\Amazon\SessionManagerPlugin\bin` on Windows).
  `hop doctor` looks there too.
- `--alias` refuses names that start with `-`, which a shell would read as an option.
- IAM policy in the README: `ssm:StartSession` on the `AWS-StartSSHSession` document,
  for ssh.

## [0.1.7] - 2026-10-30

This release publishes the renamed Homebrew cask through the release pipeline and
fixes concurrent cache writes on Windows.

### Fixed

- Windows: two hop processes saving the cache at the same time could overwrite each
  other. A lock file being deleted answers "access is denied" on Windows, which was
  taken as no lock; it now counts as held, and the atomic rename of the cache and of
  `recents.json` is retried while another process has the file open.

### Changed

- The Homebrew cask is now `hop-cli` (`brew install --cask morganseznec/tap/hop-cli`).
  It still installs the `hop` command. Homebrew's main repository has an unrelated
  `hop` cask, which `brew upgrade --cask hop` picked instead of this one. Installs of
  `morganseznec/tap/hop` are migrated on the next `brew upgrade`.

## [0.1.6] - 2026-10-30

The changes below shipped in 0.1.6. The 0.1.5 tag points to the same commit, but its
GitHub release never started: the list of allowed GitHub Actions did not include the
one the SLSA provenance generator uses to upload its file. Only the 0.1.5 container
image was published.

### Added

- README: a quick start, the instance requirements, a ready-to-paste IAM policy and
  the instance role's needs for session logging, a comparison with other tools, SSH
  and `scp` through `hop tunnel`, and a sample of what hop prints.
- [docs/reference.md](docs/reference.md): exit codes, every field of `hop ls --json`,
  limits, environment variables, the `AGENT` and `LOGGING` values, file locations.
- This changelog.
- `hop ls --json` gives each row `fetched_at`, `stale` and `error`, and reports the
  unreachable accounts on stderr, as `hop ls` does (#6).
- `-e` accepts the words that already counted as prod or test (`dev`, `staging`,
  `qa`, `uat`, `recette`...) and refuses any other (#6).
- Help: every command starts with its summary, and `run`, `push`, `login`, `scan` and
  `doctor` have examples. Errors point to the next step: the IAM permissions for a
  denied call, the region to enable or drop, the network for a timeout, `hop login
  --status` for an unknown portal, `hop completion` for a shell other than zsh.

### Changed

- `hop run --timeout` above one hour also raises the command's execution timeout,
  which `AWS-RunShellScript` otherwise caps at one hour (#6).
- `transcript: require` reads the session logging settings again right before a
  shell, instead of trusting the cache (#6).
- `hop push` checks the remote path and the file size before looking up the instance
  or signing in (#6).
- Messages name hop's real configuration file (XDG, Windows) and the AWS config file
  in use, instead of `~/.config/hop/config.yaml` and `~/.aws/config`.
- Profiles with `mfa_serial` are skipped with an explanation instead of failing at
  every refresh, and `hop doctor` reports them (#6).
- Release pipeline: the image is scanned by digest for both platforms before it is
  signed and tagged, the publish job uses no GitHub Actions cache, the BuildKit and
  binfmt images are pinned by digest, the vendored code of `session-manager-plugin`
  is scanned, the job that publishes the Homebrew cask runs in a `release`
  environment restricted to version tags, and Dependabot waits 7 days before
  proposing a new version (security updates are not delayed) (#5).

### Fixed

- Ctrl-C at a yes/no question takes effect at once and exits with 130. Before, the
  Enter that followed gave the default answer, which could start a sign-in (#6).
- `hop run` keeps polling through throttling, network errors and AWS 5xx errors until
  its timeout, and every error carries the command ID (#6).
- Builds made with `go install` report their module version instead of `dev` (#6).
- A first run without credentials, or a scope emptied by `-p`, `-e`, `-r` or
  `include`/`exclude`, says why and what to do (#6).
- Smaller fixes: long match lists are capped, the cache drops the profiles removed
  from the AWS files, `-r` is validated, no relative paths are used without a home
  directory, `recents.json` is written atomically, `TerminateSession` has its own
  timeout, and completion offers the newer regions (#6).

## [0.1.4] - 2026-10-30

The GitHub release of v0.1.4 is incomplete: a GitHub Actions incident interrupted the
release workflow, so it carries only the provenance file, without archives, Linux
packages or checksums, and the Homebrew cask stayed at 0.1.3. The Docker image
`ghcr.io/morganseznec/hop:0.1.4` is complete.

### Changed

- A word search no longer queries every account again: on a miss, hop queries only
  the profiles and regions whose data is more than a minute old (#4).
- Before acting on an instance whose data is more than a minute old, hop looks it up
  again. A gone, stopped, renamed or re-addressed instance is not acted on: hop says
  what changed and searches once more. If the lookup fails, a shell or a tunnel goes
  ahead, while `run` and `push` ask first and refuse without a terminal (#4).
- `run` and `push` ask before acting on a private IP when several profiles are in
  scope. Hybrid nodes (`mi-`) report their own name and IP, so only their ID connects
  without a question (#4).
- Faster inventory: agent states read 50 at a time, the EC2, SSM agent and session
  logging calls made in parallel, 16 profile/region pairs at a time, and the
  credentials of a profile resolved once for all its regions (#4).
- `hop login` also refreshes the assumed roles whose `source_profile` leads to the
  portal (#4).
- The cache merges what other hop processes saved meanwhile, and keeps the most
  recent data of each profile and region (#4).

### Fixed

- When one inventory call fails, the last known session logging settings and agent
  states are kept (#4).
- An instance seen through two profiles is listed under the profile whose data is
  healthy and recent (#4).

### Added

- Ko-fi link in the README and on the repository page (#3).

## [0.1.3] - 2026-10-30

### Security

- Error messages are scrubbed of access keys, secret keys and session tokens before
  they are printed or cached. A `credential_process` whose output the SDK could not
  parse had it quoted in full (#2).
- `hop push` decodes the file in a private `mktemp -d` folder, instead of a
  predictable name in `/tmp` that another local user could create first (#2).
- `hop scan` removes terminal escape sequences from `Name` tags before printing
  them (#2).
- The release workflow builds only tagged commits that are on `main`, and runs the
  tests in a separate job with a read-only token (#2).

### Fixed

- A comment after a section header (`[profile prod] # production`) no longer hides
  the profile and moves its keys to the previous one (#2).
- `hop scan` lists the regions it could not read, instead of reporting them as
  empty (#2).

### Changed

- README: the hop logo (#1); the verification commands pin the exact workflow and
  tag, and the Docker example mounts `~/.aws` read-only (#2); the Go Report Card
  badge is gone.

## [0.1.2] - 2026-10-30

### Added

- Each release carries a Sigstore signature of `checksums.txt`
  (`checksums.txt.sigstore.json`) and a SLSA provenance (`hop.intoto.jsonl`).
- Fuzz tests for the INI parser, the search, shell quoting and tag cleaning, run by
  CI.
- SECURITY.md links the private report form and states response times.

## [0.1.1] - 2026-10-30

### Fixed

- The Homebrew cask is published: v0.1.0 was released without it.
- Release assets are attested even when a later step of the release fails.

## [0.1.0] - 2026-10-30

First release.

### Added

- Inventory of the EC2 instances of every AWS profile on the machine, each profile in
  its own region, queried in parallel and cached. SSO logins grouped by portal.
- Search by name, words, instance ID or private IP that never guesses: IDs and IPs
  match exactly, partial matches ask first, and nothing acts without a terminal.
- Session logging check before every shell.
- `hop run`, `hop tunnel` and `hop push`; `hop doctor` and `hop scan`; zsh
  integration (`hop init zsh`).
- Binaries for macOS, Linux and Windows on amd64 and arm64, `.deb`, `.rpm`, `.apk`
  and Arch Linux packages, and a hardened Alpine image on GHCR. The Homebrew cask
  came with v0.1.1.

[Unreleased]: https://github.com/morganseznec/hop/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/morganseznec/hop/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/morganseznec/hop/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/morganseznec/hop/compare/v0.1.7...v0.2.0
[0.1.7]: https://github.com/morganseznec/hop/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/morganseznec/hop/compare/v0.1.4...v0.1.6
[0.1.4]: https://github.com/morganseznec/hop/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/morganseznec/hop/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/morganseznec/hop/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/morganseznec/hop/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/morganseznec/hop/releases/tag/v0.1.0
