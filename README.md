# hop

[![CI](https://github.com/morganseznec/hop/actions/workflows/ci.yml/badge.svg)](https://github.com/morganseznec/hop/actions/workflows/ci.yml)
[![Docker](https://github.com/morganseznec/hop/actions/workflows/docker.yml/badge.svg)](https://github.com/morganseznec/hop/actions/workflows/docker.yml)
[![CodeQL](https://github.com/morganseznec/hop/actions/workflows/codeql.yml/badge.svg)](https://github.com/morganseznec/hop/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/morganseznec/hop/badge)](https://scorecard.dev/viewer/?uri=github.com/morganseznec/hop)
[![Go Report Card](https://goreportcard.com/badge/github.com/morganseznec/hop)](https://goreportcard.com/report/github.com/morganseznec/hop)
[![Go Reference](https://pkg.go.dev/badge/github.com/morganseznec/hop.svg)](https://pkg.go.dev/github.com/morganseznec/hop)
[![Release](https://img.shields.io/github/v/release/morganseznec/hop?sort=semver)](https://github.com/morganseznec/hop/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Reach any EC2 instance in any of your AWS accounts and regions with one command.**

hop finds your instances across every AWS profile on your machine, lets you pick one
by name, and opens an AWS Systems Manager Session Manager shell. No SSH keys, no
bastion, no instance IDs to copy. macOS, Linux and Windows; amd64 and arm64.

```console
$ hop shop app
```

That's it. hop searched every profile in `~/.aws/config`, each in its own region,
found `acme-p-shop-app` in `acme-prod`, checked that your SSO token is valid and that
the account logs sessions, and connected.

## Features

- **Every account, every region.** Each profile is queried in its own region, in
  parallel, and cached for 15 minutes. `hop scan` finds instances hiding in regions
  you forgot about.
- **Search by what you remember.** A name, part of a name, several words
  (`hop shop prod app`), an instance ID or a private IP. An unambiguous match connects
  directly; anything else opens a fuzzy picker with a preview, or asks.
- **Safe with typos.** A fuzzy match never connects on its own: hop asks first, and
  only suggests when there is no terminal.
- **Knows whether you are being recorded.** Before a shell, hop reads the account's
  Session Manager preferences and tells you where the session is logged, or warns you
  that it is not. `transcript: require` refuses unlogged sessions.
- **SSO that just works.** Profiles are grouped by SSO portal, so one `hop login`
  covers every account behind it. An expired token is detected before connecting and
  hop offers to log in again.
- **More than shells.** `hop run` for one-off commands (exit code preserved),
  `hop tunnel` for port forwarding (also to hosts behind the instance), `hop push` for
  small files with a SHA-256 check.
- **A health check for your AWS setup.** `hop doctor` spots expired tokens, revoked or
  long-lived keys, profiles defined twice, unlogged accounts and more, each with the
  command that fixes it.

## Install

hop needs the AWS
[Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html),
and the [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
for `hop login` only.

### Homebrew (macOS)

```bash
brew install --cask morganseznec/tap/hop
```

The cask also installs `session-manager-plugin`.

### Linux packages

Every [release](https://github.com/morganseznec/hop/releases) has `.deb`, `.rpm`,
`.apk` (Alpine) and `.pkg.tar.zst` (Arch Linux) packages for amd64 and arm64:

```bash
sudo dpkg -i hop_<version>_amd64.deb             # Debian, Ubuntu
sudo rpm -i hop-<version>-1.x86_64.rpm           # Fedora, RHEL, Amazon Linux
sudo apk add --allow-untrusted hop_<version>_x86_64.apk
sudo pacman -U hop-<version>-1-x86_64.pkg.tar.zst
```

### Windows, or any platform

Download the archive for your platform from the
[releases page](https://github.com/morganseznec/hop/releases) (`.zip` on Windows,
`.tar.gz` elsewhere), or build it:

```bash
go install github.com/morganseznec/hop/cmd/hop@latest
```

### Docker

```bash
docker run --rm -it --user "$(id -u):$(id -g)" -v ~/.aws:/home/hop/.aws \
  ghcr.io/morganseznec/hop ls
```

The image (linux/amd64 and linux/arm64) holds hop and a `session-manager-plugin`
built from source, on Alpine, as a non-root user, without a package manager.

- `--user` runs hop as you, so it can read your `~/.aws` files (they are `0600`).
- Log in to SSO on the host. The mount is writable so that the SDK can save a
  refreshed token; add `:ro` if you prefer to refresh on the host only.
- It also runs with `--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges`.
- Port forwarding needs `--network host` (Linux only), since the plugin listens on the
  container's localhost.

### Verify what you downloaded

Release archives and images are built by GitHub Actions, with signed provenance:

```bash
gh attestation verify hop_<version>_linux_amd64.tar.gz --repo morganseznec/hop
gh attestation verify oci://ghcr.io/morganseznec/hop:latest --repo morganseznec/hop
```

Images are also signed with Sigstore:

```bash
cosign verify ghcr.io/morganseznec/hop:latest \
  --certificate-identity-regexp '^https://github.com/morganseznec/hop/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The Linux packages are not signed with a repository key (hence `--allow-untrusted`
for apk): check them with `gh attestation verify` instead. The macOS binary is not
notarized, so the Homebrew cask clears its quarantine flag after install.

### Shell completion

zsh, in `~/.zshrc` after `compinit`:

```zsh
eval "$(hop init zsh)"
```

Used to typing another name? `hop init zsh --alias ssm` also defines `ssm` as an
alias. For bash, fish and PowerShell, see `hop completion --help`.

Completion suggests instance names from the cache and respects the flags already
typed (`hop -p acme-prod <Tab>`).

## Usage

| What you want | Command |
|---|---|
| Pick an instance from the whole fleet | `hop` |
| Connect by name, words, ID or IP | `hop shop app`, `hop tools`, `hop i-0abc12...`, `hop 10.0.1.21` |
| Narrow down | `hop -p 'acme-*' -e prod`, `hop -r us-east-1 tools` |
| Inventory | `hop ls`, `hop ls shop`, `hop ls --all`, `hop ls --json` |
| One-off command | `hop run tools uptime` |
| Port forwarding | `hop tunnel tools 3000`, `hop tunnel tools 5432 15432 --host db.internal` |
| Copy a small file | `hop push ./app.conf tools /etc/app.conf` |
| SSO login | `hop login`, `hop login --status`, `hop login acme` |
| Check your setup | `hop doctor` |
| Find forgotten regions | `hop scan`, `hop scan --write` |

Global flags: `-p/--profile` (glob patterns accepted), `-r/--region`, `-e/--env`
(`prod` or `test`), `--refresh` (ignore the cache).

### How search works

- hop acts right away only on an unambiguous match: one instance, running, whose name,
  ID or IP is exactly what you typed, or whose name contains every word you typed as
  a whole word (`hop shop app` for `acme-p-shop-app`).
- An IP address or an instance ID must match exactly: `10.0.1.2` never picks
  `10.0.1.21`.
- Anything else asks first: the picker when several instances match (the same name in
  two accounts, an Auto Scaling group), a confirmation for a single partial match or a
  typo. For `run` and `push`, the confirmation defaults to no.
- Without a terminal, hop never guesses: it lists the candidates and stops.
- Partial matches are checked against fresh data before hop asks, so an instance
  launched minutes ago is found.
- Stopped instances are shown as stopped, not as "not found".
- Instances you used in the last seven days come first.

The environment (`prod`, `test`) comes from an `Environment`, `Env` or `Stage` tag when
there is one, otherwise from words in the instance or profile name (`acme-p-tools`,
`acme-shop-test`).

### Before opening a shell

1. **Credentials.** With an expired SSO token, hop offers to open the portal, then
   carries on.
2. **Session logging.** hop reads the `SSM-SessionManagerRunShell` document of the
   account in that region. It shows where the session is logged (CloudWatch Logs or
   S3), or warns and asks when it is not.
3. **SSM agent.** hop warns when the agent was offline at the last inventory.

A shell never opens without a terminal. In scripts, use `hop run`.

### What `run` and `push` leave behind

`hop run` and `hop push` go through SSM Run Command, not through a session:

- the session logging settings (and `transcript: require`) do not apply to them;
- the command, its parameters and its output stay in the SSM command history of the
  account (about 30 days) and in CloudTrail. For `push`, that includes the file
  content, base64-encoded. Do not push secrets this way.

## Profiles, regions and SSO

Each profile is queried **in its own region**: the one in `~/.aws/config`, otherwise
`default_region` from hop's config, otherwise the region of `[default]`, otherwise
`AWS_REGION` or `AWS_DEFAULT_REGION`, otherwise `us-east-1`. Add more regions per profile with `extra_regions`, or let `hop scan`
propose them.

An unreachable account does not hide the others: its last known instances stay
visible, and hop prints the reason and the command that fixes it.

SSO profiles are grouped **by token**: all the profiles behind one portal log in
together. When a profile is defined both with SSO (in `config`) and with keys (in
`credentials`), hop follows the AWS CLI and uses SSO, where the Go SDK alone would
pick the keys. `hop doctor` points these out.

An instance visible through two profiles (two roles on the same account) is listed
once.

## Configuration

Optional, in `~/.config/hop/config.yaml` (`%AppData%\hop\config.yaml` on Windows):

```yaml
# Profiles to use and to skip (glob patterns).
include: ["acme-*", globex]
exclude: ["*_AdministratorAccess"]

# Regions queried in addition to each profile's own. "hop scan" finds them.
extra_regions:
  acme-prod: [us-east-1]

# warn (default): warn and ask. require: refuse. off: do not check.
transcript: warn

cache_ttl: 15m

# Labels shown next to the profile, and searchable: "hop ACME tools".
labels:
  acme-prod: ACME
```

hop also writes `~/.cache/hop/inventory.json` (instances, no credentials) and
`~/.local/state/hop/recents.json` (recently used instances); on Windows both live in
`%LocalAppData%\hop`. The XDG variables are honoured when set.

## IAM permissions

| Action | Used by |
|---|---|
| `ec2:DescribeInstances` | inventory |
| `ssm:DescribeInstanceInformation` | agent status |
| `ssm:GetDocument` | session logging check |
| `ssm:StartSession`, `ssm:TerminateSession` | shell, tunnel |
| `ssm:SendCommand`, `ssm:GetCommandInvocation` | `hop run`, `hop push` |
| `ec2:DescribeRegions` | `hop scan` |

## Contributing

Issues and pull requests are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Please
report security issues privately, as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
