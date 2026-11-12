<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo-dark.svg">
    <img alt="hop" src="docs/logo-light.svg" width="320">
  </picture>
</h1>

<p align="center">
  <strong>Reach any EC2 instance in any of your AWS accounts with one command.</strong>
</p>

<p align="center">
  <a href="https://github.com/morganseznec/hop/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/morganseznec/hop/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/morganseznec/hop/actions/workflows/docker.yml"><img alt="Docker" src="https://github.com/morganseznec/hop/actions/workflows/docker.yml/badge.svg"></a>
  <a href="https://github.com/morganseznec/hop/actions/workflows/codeql.yml"><img alt="CodeQL" src="https://github.com/morganseznec/hop/actions/workflows/codeql.yml/badge.svg"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/morganseznec/hop"><img alt="OpenSSF Scorecard" src="https://api.scorecard.dev/projects/github.com/morganseznec/hop/badge"></a>
  <a href="https://pkg.go.dev/github.com/morganseznec/hop"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/morganseznec/hop.svg"></a>
  <a href="https://github.com/morganseznec/hop/releases"><img alt="Release" src="https://img.shields.io/github/v/release/morganseznec/hop?sort=semver"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
  <a href="https://ko-fi.com/B0F429R66U"><img alt="Support on Ko-fi" src="https://img.shields.io/badge/Ko--fi-support-FF5E5B?logo=ko-fi&amp;logoColor=white"></a>
</p>

<p align="center">
  <img alt="hop ls lists 26 instances from 9 AWS profiles in 6 regions; hop shop app finds two matches and asks which one; hop 10.90.2.73 opens a shell on an instance of another account" src="docs/demo.gif">
  <br>
  <sub>A fictional fleet, recorded with <code>vhs docs/demo/demo.tape</code>.</sub>
</p>

hop finds your instances across every AWS profile on your machine, lets you pick one
by name, and opens an AWS Systems Manager Session Manager shell. No SSH keys, no
bastion, no instance IDs to copy. macOS, Linux and Windows; amd64 and arm64.

```console
$ hop shop app
hop: session LOGGED on acme-p-shop-app (i-0abc1234def567890, acme-prod, eu-west-1) to /acme/ssm/sessions. Everything that follows is recorded.

Starting session with SessionId: alice-0a1b2c3d4e5f67890
sh-5.2$
```

That's it. hop searched every profile in `~/.aws/config` and `~/.aws/credentials`,
each in its own region, found `acme-p-shop-app` in `acme-prod`, checked that your SSO
token is valid and that the account logs sessions, and connected.

`hop ls` shows everything it can reach:

```console
$ hop ls
  NAME             PROFILE    REGION     ENV   IP          AGENT        LOGGING
  acme-p-shop-app  acme-prod  eu-west-1  prod  10.0.1.21   online       yes
  acme-p-tools     acme-prod  eu-west-1  prod  10.0.1.40   online       yes
  acme-p-batch     acme-prod  us-east-1  prod  10.8.2.15   lost 3h ago  yes
  acme-t-api       acme-test  eu-west-1  test  10.20.1.30  online       NO
4 instances, 2 profiles, regions eu-west-1, us-east-1 (cached 4m ago, --refresh to query again)
```

(Accounts, names and addresses in this README are fictional.)

## Quick start

```bash
brew install --cask morganseznec/tap/hop-cli   # macOS; see Install for Linux, Windows and Docker
aws configure sso                              # only if you have no AWS profile yet
hop doctor                                     # plugin, SSO tokens, keys, profiles
hop ls                                         # every instance hop can see
hop shop                                       # connect by a word of its name
```

A partial or fuzzy match asks before connecting. The instances must meet the
[instance requirements](#instance-requirements), and your role needs the
[IAM permissions](#iam-permissions) listed below.

## Features

- **Every account, one command.** Each profile is queried in its own region (plus any
  `extra_regions`), in parallel, and cached for 15 minutes. `hop scan` finds the
  instances hiding in other regions. Hybrid nodes (`mi-...`) are listed too.
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
- **More than shells.** `ssh`, `scp`, `rsync`, `git` and editors' remote mode reach
  any instance as `tools.hop` after `hop init ssh`. `hop run` for one-off commands
  (exit code preserved), `hop tunnel` for port forwarding (also to hosts behind the
  instance), `hop push` and `hop pull` to copy files with a SHA-256 check and no SSH
  key (`pull` of any size, `push` of any size with a relay bucket).
- **S3 across accounts.** `hop s3 ls` lists the buckets of every profile, each with
  its region; `hop s3 pull` and `hop s3 push` copy objects without naming the account,
  in parallel parts for large files, and never replace an object without asking.
- **A health check for your AWS setup.** `hop doctor` spots expired tokens, revoked or
  long-lived keys, profiles defined twice, unlogged accounts and more, each with the
  command that fixes it.

## Instance requirements

hop goes through Systems Manager, so each instance must be a managed node:

- the **SSM agent** installed and online (the AGENT column of `hop ls`). Port
  forwarding to another host (`hop tunnel --host`) needs agent 3.1.1374.0 or later;
- an **instance role** with the `AmazonSSMManagedInstanceCore` managed policy, or
  Default Host Management Configuration turned on for the account and region;
- **outbound HTTPS** to the `ssm`, `ssmmessages` and `ec2messages` endpoints of the
  region, or VPC interface endpoints for them.

On your machine, hop needs `session-manager-plugin` 1.2.497.0 or later: `hop doctor`
checks it.

## Install

hop needs the AWS
[Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html).
The [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)
is only used to sign in to SSO portals: by `hop login`, and when hop offers to sign in
again during a command.

### Homebrew (macOS)

```bash
brew install --cask morganseznec/tap/hop-cli
```

The cask is called `hop-cli` and installs the `hop` command, plus
`session-manager-plugin`. Homebrew's main repository has an unrelated `hop` cask, so
never use `brew install hop` or `brew upgrade --cask hop`. Once installed with the
full name, the short name works: `brew upgrade --cask hop-cli` (or a plain
`brew upgrade`). Earlier installs of `morganseznec/tap/hop` move to `hop-cli` on the
next `brew upgrade`.

### Linux packages

The [releases](https://github.com/morganseznec/hop/releases) carry `.deb`, `.rpm`,
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
docker run --rm -it --user "$(id -u):$(id -g)" -v ~/.aws:/home/hop/.aws:ro \
  -v hop-cache:/tmp/hop ghcr.io/morganseznec/hop ls
```

The image (linux/amd64 and linux/arm64) holds hop and a `session-manager-plugin`
built from source, on Alpine, as a non-root user, without a package manager.

- `--user` runs hop as you, so it can read your `~/.aws` files (they are `0600`).
- The `hop-cache` volume keeps the inventory and the recent instances between runs
  (the image keeps them under `/tmp/hop`); without it, every run queries every
  account again. Needs v0.1.5 or later: older images created the volume owned by root.
- `~/.aws` is mounted read-only: sign in to SSO on the host. The image has no AWS CLI,
  so neither `hop login` nor the sign-in hop offers on an expired token can work in
  the container. Drop `:ro` only if you want the container to save refreshed SSO
  tokens there.
- hop's own configuration is read from `/home/hop/.config/hop/config.yaml`: add
  `-v ~/.config/hop:/home/hop/.config/hop:ro` to use yours.
- The image has no Google Cloud CLI either: the experimental GCP support needs hop
  installed on the host.
- It also runs with `--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges`.
- Port forwarding needs `--network host` (Linux only), since the plugin listens on the
  container's localhost.

<details>
<summary><strong>Verify what you downloaded</strong></summary>

Release archives and images are built by GitHub Actions, with signed provenance. Check
the exact workflow and tag that built them, not just the repository: any workflow of
the repository could sign something.

```bash
VERSION=0.1.2
gh attestation verify hop_${VERSION}_linux_amd64.tar.gz --repo morganseznec/hop \
  --signer-workflow morganseznec/hop/.github/workflows/release.yml \
  --source-ref refs/tags/v${VERSION}
gh attestation verify oci://ghcr.io/morganseznec/hop:${VERSION} --repo morganseznec/hop \
  --signer-workflow morganseznec/hop/.github/workflows/docker.yml \
  --source-ref refs/tags/v${VERSION}
```

From v0.1.2, each release also carries a Sigstore signature of `checksums.txt` and a
SLSA level 3 provenance (`hop.intoto.jsonl`):

```bash
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/morganseznec/hop/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt
slsa-verifier verify-artifact hop_${VERSION}_linux_amd64.tar.gz \
  --provenance-path hop.intoto.jsonl \
  --source-uri github.com/morganseznec/hop --source-tag v${VERSION}
```

Images are also signed with Sigstore. Once verified, run the image by the digest that
`cosign` prints (`ghcr.io/morganseznec/hop@sha256:...`) rather than by tag:

```bash
cosign verify ghcr.io/morganseznec/hop:${VERSION} \
  --certificate-identity "https://github.com/morganseznec/hop/.github/workflows/docker.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The Linux packages are not signed with a repository key (hence `--allow-untrusted`
for apk): check them with `gh attestation verify` instead. The macOS binary is not
notarized, so the Homebrew cask clears its quarantine flag after install.

</details>

### Shell completion

zsh, in `~/.zshrc` after `compinit`:

```zsh
eval "$(hop init zsh)"
```

bash, in `~/.bashrc` (with the `bash-completion` package when it is installed, without
it otherwise):

```bash
eval "$(hop init bash)"
```

fish, in `~/.config/fish/config.fish`:

```fish
hop init fish | source
```

Used to typing another name? `--alias ssm` also defines `ssm` as an alias, with the
same completion (`hop init zsh --alias ssm`). For PowerShell, see
`hop completion powershell --help`.

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
| ssh, scp, rsync | `hop init ssh >> ~/.ssh/config` once, then `ssh ec2-user@tools.hop` |
| Upload a file | `hop push ./app.conf tools /etc/app.conf`, `hop push ./release.tar.gz web /opt/app/release.tar.gz` |
| Download a file | `hop pull tools /var/log/app.log`, `hop pull tools /etc/app.conf conf/` |
| S3 buckets and objects | `hop s3 ls`, `hop s3 ls -lh logs/2026/`, `hop s3 pull logs/2026/app.log`, `hop s3 push dump.sql backups/db/` |
| SSO login | `hop login`, `hop login --status`, `hop login acme` |
| Check your setup | `hop doctor` |
| Find forgotten regions | `hop scan`, `hop scan --write` |

Global flags: `-p/--profile` (glob patterns accepted), `-r/--region`, `-e/--env`
(`prod` or `test`; `staging`, `dev`, `qa`, `uat` and the other words listed below
select `test`), `--refresh` (ignore the cache).

`hop ls --json` prints one object per instance, with how fresh its data is
(`fetched_at`, `stale`, `error`). Unreachable accounts are also reported on stderr, as
with `hop ls`. The [reference](docs/reference.md) lists every JSON field, the exit
codes, the limits, the environment variables and where hop keeps its files.

### How search works

- hop acts right away only on an unambiguous match: one instance, running, whose name,
  ID or IP is exactly what you typed, or whose name contains every word you typed as
  a whole word (`hop shop app` for `acme-p-shop-app`).
- An IP address or an instance ID must match exactly: `10.0.1.2` never picks
  `10.0.1.21`. Private IPs repeat across accounts, so with several profiles in scope
  `run` and `push` ask before acting on an IP (or refuse without a terminal: narrow
  with `-p`).
- Hybrid nodes (`mi-...`) report their own name and IP: only their ID is trusted
  without a question.
- Anything else asks first: the picker when several instances match (the same name in
  two accounts, an Auto Scaling group), a confirmation for a single partial match or a
  typo. For `run` and `push`, the confirmation defaults to no.
- Without a terminal, hop never guesses: it lists the candidates and stops.
- When a search finds nothing, only a partial match or only stopped instances, hop
  first queries again the accounts whose data is more than a minute old, so an
  instance launched minutes ago is found. A word search that opens the picker shows
  the cache as is.
- Before acting on an instance whose data is more than a minute old, hop looks that
  one instance up again in its account. If it is gone, stopped, or no longer matches
  what you typed, hop says what changed and searches once more. If the lookup fails,
  a shell or a tunnel goes ahead, while `run` and `push` ask first (and refuse without
  a terminal).
- Stopped instances are shown as stopped, not as "not found".
- Instances you used in the last seven days come first.

The environment (`prod`, `test`) comes from an `Environment`, `Env` or `Stage` tag when
there is one, otherwise from words in the instance or profile name (`acme-p-tools`,
`acme-shop-test`). There are only two groups: `prod`, `prd`, `production` and `p` mean
`prod`; `test`, `tst`, `t`, `dev`, `staging`, `stg`, `preprod`, `qa`, `uat` and `recette`
all mean `test`. `-e` accepts any of these words, so `-e staging` lists every non-production
instance, and refuses any other.

### Before opening a shell

1. **Credentials.** With an expired SSO token, hop offers to open the portal (with the
   AWS CLI), then carries on.
2. **Session logging.** hop reads the `SSM-SessionManagerRunShell` document of the
   account in that region. It shows where the session is logged (CloudWatch Logs or
   S3), or warns and asks when it is not. With `transcript: require`, the document is
   read again right before the session opens, rather than taken from the cache, and the
   session is refused when it is not logged or cannot be checked.
3. **SSM agent.** hop warns when the agent was offline at the last inventory.

A shell never opens without a terminal. In scripts, use `hop run`.

### What `run` and `push` leave behind

`hop run` and `hop push` go through SSM Run Command (`AWS-RunShellScript`), not
through a session:

- they run as root, on Linux and macOS instances only;
- the session logging settings (and `transcript: require`) do not apply to them;
- the command, its parameters and its output stay in the SSM command history of the
  account (about 30 days). For a `push` of up to 64 KB, that includes the file
  content, base64-encoded: do not push secrets this way. Through a
  [relay bucket](#uploading-a-large-file) (a larger file, or `--relay`), the history
  keeps a download URL that has expired, not the file;
- CloudTrail records who sent each command, to which instance and with which
  document, but not the command itself: it shows the parameters as
  `HIDDEN_DUE_TO_SECURITY_REASONS`.

`hop run` prints the output once the command ends (24,000 characters per stream at
most, where SSM cuts it) and waits 2 minutes by default (`--timeout`). A command the instance has
not picked up when hop stops waiting is canceled.

### Downloading a file

`hop pull` goes through a Session Manager session that runs a single command
(`AWS-StartNonInteractiveCommand`), so it needs no SSH key and has no size limit:

```bash
hop pull tools /var/log/app.log                  # to ./app.log
hop pull tools /etc/nginx/nginx.conf conf/       # to conf/nginx.conf
hop pull db /var/backups/db.sql.gz /tmp/prod.sql.gz
```

- The file is read as the session user (`ssm-user` by default), or with `sudo -n`
  when that user cannot read it. Linux and macOS instances only.
- hop writes it under a temporary name, then renames it once the SHA-256 matches on
  both sides: an interrupted download leaves nothing behind. A new file is readable
  by you only; an existing one is replaced and keeps its mode.
- While it streams, the SSM agent keeps the output on the instance's disk, in base64
  (a third more than the file): for a large file, check the free space there.
- CloudTrail records the `StartSession` call. When the account logs sessions, the log
  may keep a copy of the file, base64-encoded. Like `hop tunnel`, it opens no shell:
  `transcript: require` does not apply.
- Your role needs `ssm:StartSession` on the `AWS-StartNonInteractiveCommand` document
  (see [IAM permissions](#iam-permissions)).

### Uploading a large file

SSM Run Command takes 97 KB per command, so `hop push` carries a file of up to 64 KB
in the command itself. Past that, it goes through a **relay bucket**, with the same
command:

```console
$ hop push ./release.tar.gz web /opt/app/release.tar.gz
hop: ./release.tar.gz -> acme-p-web:/opt/app/release.tar.gz, 412.6 MB through s3://acme-tools-relay
hop: uploaded: acme-p-web is fetching it (up to 8m52s)
hop: copied, SHA-256 matches on both sides
```

1. hop uploads the file to the bucket under a random key, in parallel parts.
2. hop signs a URL that downloads that one object, and sends the instance a short
   script: it fetches the URL with `curl` or `wget` into a private directory next to
   the destination, compares the SHA-256 with the one of the file sent, and only then
   copies it over the destination. A download that fails leaves the file in place
   untouched.
3. hop compares the SHA-256 of the final file, and deletes the object.

Name the bucket in hop's configuration, by profile or pattern:

```yaml
relay_buckets:
  "acme-*": acme-tools-relay/hop     # a bucket, or a bucket and a key prefix
  globex: globex-relay
```

What it takes, and where it stops:

- **hop never creates the bucket.** It must exist, and a profile without a relay
  bucket keeps the 64 KB limit, with a message that says so. hop sends the file to
  the bucket you name: check the name, a mistyped one can be somebody else's. Someone allowed to
  create buckets sets it up once; those who push only need rights on its objects.
- **One bucket can serve every account.** The instance downloads with the signed URL,
  so the bucket may live in another account or region than the instance, as long as
  each profile that pushes may write to it.
- **Your role** needs `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject` and
  `s3:AbortMultipartUpload` on the bucket (or on the prefix). hop tries the write and
  the read before it asks the instance anything, and names the one that is missing.
- **The instance** needs no S3 permission: only `curl` (or GNU `wget`; the one of
  BusyBox cannot take the URL from stdin), a way to reach S3 (a VPC endpoint for S3,
  or a route out, that the endpoint and bucket policies do not close), and room for
  the file twice next to the destination while it is copied.
- **The object goes** once the copy is checked, or has failed. It stays in two cases,
  and hop says so: your role may not delete it, or hop stopped waiting (Ctrl-C, or
  the time allowed) while the instance was still fetching it. A lifecycle rule that
  expires objects after a day, and aborts incomplete multipart uploads, covers both.
- **`--relay`** sends a small file the same way, to keep its content out of the SSM
  command history: `hop push --relay ./api.key tools /etc/app/api.key`.

The signed URL is valid for the time hop waits plus five minutes, and no longer than
the credentials that signed it. It reaches the instance inside the script, never as a
command argument, so the process list of the instance does not show it.

### S3 buckets

`hop s3` does for buckets what `hop ls` does for instances: one list across every
profile, and no account or region to give.

```console
$ hop s3 ls
  NAME            PROFILE    REGION     ENV   CREATED
  acme-p-backups  acme-prod  eu-west-1  prod  2024-03-11
  acme-p-logs     acme-prod  us-east-1  prod  2023-09-02
  acme-t-scratch  acme-test  eu-west-1  test  2026-05-20
3 buckets, 2 profiles (cached 4m ago, --refresh to query again)

$ hop s3 ls -lh logs/2026/
    SIZE  MODIFIED          CLASS     NAME
       -  -                 -         11/
  1.2 MB  2026-11-07 14:02  STANDARD  app.log
   340 B  2026-11-09 08:30  STANDARD  readme.txt
2 objects, 1 prefix, 1.2 MB in s3://acme-p-logs/2026/ (acme-prod, us-east-1)

$ hop s3 pull logs/2026/app.log
hop: s3://acme-p-logs/2026/app.log -> app.log (acme-prod, us-east-1)
hop: 1.2 MB received, CRC64NVME matches on both sides
```

- **Buckets.** `hop s3 ls` takes one `ListBuckets` call per profile, cached like the
  instances, and shows each bucket in the region it lives in. Words filter the list
  (`hop s3 ls logs`), `-r` keeps the buckets of some regions, `-e` an environment,
  read from the name of the bucket or of the profile. Listing instances never calls
  S3.
- **Objects.** An argument with a `/` names a place in a bucket: `acme-p-logs/` is
  the top of the bucket, `acme-p-logs/2026/` what is under that prefix, grouped at the
  next `/` like directories. The options are
  those of `ls` and combine (`-alh`): `-l` for size, date and storage class, `-h` for
  sizes in KB, MB and GB, `-a` for the names that start with a dot (left out
  otherwise, and the footer says how many), `-R` for every object below, `-t` and
  `-S` to sort. `-h` on its own stays the help, and reversing is `--reverse`, since
  `-r` is `--region`. `--json` is for scripts.
- **Which bucket.** The bucket part is searched like an instance name, among the
  buckets listed. The exact name, or whole words of it (`logs/` for `acme-p-logs`),
  is used directly; a looser match asks first, and without a terminal hop lists the
  candidates and stops. After `s3://`, it is an exact name, as everywhere else in AWS
  (`s3://acme-p-logs/2026/`): a word or a typo finds nothing.
- **Buckets of other accounts.** S3 lists the buckets an account owns, not those it
  may use. Name such a bucket exactly, after `s3://`, with the profile that reaches it:
  `hop s3 ls -p acme-prod s3://partner-drop/in/`. hop reads from it; before uploading
  to it, hop asks, and refuses without a terminal, since a mistyped name can be
  somebody else's bucket. A search never tries a word as the name of an unlisted
  bucket.
- **Download.** `hop s3 pull <bucket>/<key> [local-path]` fetches a large object as
  ranges of 8 MB, five at a time, into a temporary file renamed once it is complete.
  hop compares the size, and the checksum S3 holds for the whole object when there is
  one; otherwise it says that only the size was checked (an object sent without a
  checksum, or in parts by a tool that kept only a checksum of its parts).
- **Upload.** `hop s3 push <local-file> <bucket>[/<key>]` sends a file of 16 MB or
  more as a multipart upload, in parts of 8 MB, five at a time, and aborts it when it
  fails or is interrupted, so no part stays in the bucket. A key that ends with `/`
  keeps the name of the file. hop never replaces an object without asking: it asks
  when the key exists, refuses without a terminal unless `--force` is given, and S3
  itself refuses the write if the object appeared meanwhile. S3 checks a CRC32 of
  each request and returns the one of the whole object, which hop compares with the
  file's.
- **On the other commands.** `s3://` and the exact name tell a bucket from an
  instance: `hop ls -lh s3://acme-p-logs/2026/`,
  `hop pull s3://acme-p-logs/2026/app.log`,
  `hop push ./dump.sql.gz s3://acme-p-backups/db/`.
- **What it leaves behind.** CloudTrail records the `ListBuckets` call. It records
  reads and writes of objects only in accounts that log S3 data events, which hop
  cannot check: the session logging settings (and `transcript: require`) do not apply
  to S3. `hop s3` never deletes an object.

Your role needs the S3 actions listed in [IAM permissions](#iam-permissions). `hop s3`
is AWS only: it does not list Google Cloud Storage buckets.

Was your first search word `s3` (`hop s3 gateway`)? `s3` is now a command, like `ls`
or `run`: put the word elsewhere (`hop gateway s3`) or write `hop -- s3 gateway`.

### SSH, scp and rsync

`hop init ssh` prints a block for `~/.ssh/config` that sends every host ending in
`.hop` through Session Manager, with `hop ssh-proxy` as its `ProxyCommand`:

```bash
hop init ssh >> ~/.ssh/config

ssh ec2-user@tools.hop
scp ./dump.sql ec2-user@tools.hop:/tmp/
rsync -a ./site/ ec2-user@acme-p-web.hop:/srv/site/
ssh -L 5432:db.internal:5432 ec2-user@tools.hop
```

Anything that speaks SSH works the same way: `git`, `sftp`, Ansible, or the remote
mode of VS Code and JetBrains editors. Before `.hop` goes what hop searches for: a
name, an instance ID or a private IP (`ec2-user@10.0.1.40.hop`). ssh gives hop no
terminal to ask from, so only an unambiguous match connects. To narrow the search for
every host, add `-p`, `-r` or `-e` to the `ProxyCommand` line
(`hop ssh-proxy -p 'acme-*' -- %h %p`).

The block runs hop by its full path, since editors start ssh with a short `PATH`, and
hop also looks for `session-manager-plugin` where its installers put it. The instance
needs `sshd` running and your public key in the target user's
`~/.ssh/authorized_keys`, and your role needs `ssm:StartSession` on the
`AWS-StartSSHSession` document (see [IAM permissions](#iam-permissions)).

Session Manager does not log what goes through SSH, nor through a tunnel: with
`transcript: require`, `hop ssh-proxy` refuses. Without the ssh setup, a tunnel to
port 22 does the same job by hand:

```bash
hop tunnel tools 22    # hop: localhost:8022 -> acme-p-tools:22 (Ctrl-C to close)

# in another terminal
ssh -p 8022 -o HostKeyAlias=acme-p-tools ec2-user@localhost
```

## Profiles, regions and SSO

hop reads the profiles of both `~/.aws/config` and `~/.aws/credentials`, or of the
files named by `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE`. It ignores
`AWS_PROFILE`: it queries every profile that has credentials, and `-p` narrows that
down (`-p acme-prod`, `-p 'acme-*'`).

Each profile is queried **in its own region**: the one in `~/.aws/config`, otherwise
`default_region` from hop's config, otherwise the region of `[default]`, otherwise
`AWS_REGION` or `AWS_DEFAULT_REGION`, otherwise `us-east-1`. Add more regions per
profile with `extra_regions`, or let `hop scan` find them: it queries every region
enabled on each account and proposes those that have instances. Hybrid nodes
(`mi-...`) registered in an account and region are listed with its EC2 instances.

An unreachable account does not hide the others: its last known instances stay
visible, and hop prints the reason and the command that fixes it.

SSO profiles are grouped **by token**: all the profiles behind one portal log in
together. When a profile is defined both with SSO (in `config`) and with keys (in
`credentials`), hop follows the AWS CLI and uses SSO, where the Go SDK alone would
pick the keys. `hop doctor` points these out.

An instance visible through two profiles (two roles on the same account) is listed
once.

hop does not prompt for MFA codes yet: a profile that assumes a role with `mfa_serial`
(directly or through its `source_profile`) is skipped and reported, by `hop ls` and
`hop doctor`. Use a profile without MFA for these accounts, or get the credentials from
[aws-vault](https://github.com/99designs/aws-vault) or another `credential_process`.

## GCP (experimental)

hop can also list the Compute Engine instances of your GCP projects and open a shell on
them through [IAP](https://cloud.google.com/iap/docs/using-tcp-forwarding), with no
external IP and no bastion. It is a first step, not yet at the level of AWS:

| | AWS | GCP (experimental) |
|---|---|---|
| Search across accounts or projects | yes | yes |
| Shell | Session Manager | `gcloud compute ssh --tunnel-through-iap` |
| Session logging check | yes | no: GCP does not record shell sessions |
| `run`, `tunnel`, `push`, `pull`, ssh by name | yes | not yet: hop prints the gcloud command instead |

Turn it on in hop's configuration:

```yaml
# Every project gcloud's account can see, or a list; glob patterns allowed.
gcp_projects: ["*"]
```

hop needs the [Google Cloud CLI](https://cloud.google.com/sdk/docs/install) and uses
its active account (`gcloud auth login`). It lists the instances with
`compute.instances.list` (and `"*"` lists the projects you can see), and checks one
with `compute.instances.get` before a shell when the inventory is more than a minute
old. A project where Compute Engine is off, or whose instances you may not list, is
skipped. The shell needs what `gcloud compute ssh --tunnel-through-iap` needs: the
IAP-secured Tunnel User role, a firewall rule that lets IAP (`35.235.240.0/20`) reach
port 22, and OS Login or project SSH keys.

GCP instances show up as `gcp:<project>`, in their zone: `hop ls -p gcp` lists only
them, `hop ls -p 'gcp:shop-*'` those of some projects. With `transcript: require`, hop
refuses GCP shells, since nothing records them.

Do you use hop on GCP, or would you on Azure? Say what you need in
[the GCP and Azure discussion](https://github.com/morganseznec/hop/discussions/19):
that is what decides how far this goes.

## Configuration

Optional, in `~/.config/hop/config.yaml` (`%AppData%\hop\config.yaml` on Windows,
`$XDG_CONFIG_HOME/hop/config.yaml` when that is set):

```yaml
# Profiles to use and to skip (glob patterns).
include: ["acme-*", globex]
exclude: ["*_AdministratorAccess"]

# Regions queried in addition to each profile's own. "hop scan" finds them.
extra_regions:
  acme-prod: [us-east-1]

# Region of the profiles that set none, before [default] and AWS_REGION.
default_region: eu-west-1

# warn (default): warn and ask. require: refuse. off: do not check.
transcript: warn

cache_ttl: 15m

# Labels shown next to the profile, and searchable: "hop ACME tools".
labels:
  acme-prod: ACME

# GCP projects to list as well (experimental, see GCP above).
gcp_projects: ["*"]

# Buckets that carry files too large for SSM to an instance (see "Uploading a large
# file"): by profile or pattern, a bucket or a bucket and a key prefix.
relay_buckets:
  "acme-*": acme-tools-relay/hop
```

`hop scan --write` rewrites this file to add regions: comments are not kept, and the
previous version stays next to it as `config.yaml.bak`.

hop also writes `~/.cache/hop/inventory.json` (instances and bucket names, no
credentials) and
`~/.local/state/hop/recents.json` (recently used instances); on Windows both live in
`%LocalAppData%\hop`. The XDG variables are honoured when set.

## IAM permissions

| Action | Used by |
|---|---|
| `ec2:DescribeInstances` | inventory |
| `ssm:DescribeInstanceInformation` | agent status, hybrid nodes |
| `ssm:GetDocument` | session logging check |
| `ssm:StartSession` | shell, tunnel, ssh, `hop pull` |
| `ssm:TerminateSession`, `ssm:ResumeSession`, `ssmmessages:OpenDataChannel` | shell, tunnel, ssh, `hop pull` (your own sessions, also used by `session-manager-plugin`) |
| `kms:GenerateDataKey` | shell, when the account encrypts sessions with KMS |
| `ssm:SendCommand`, `ssm:GetCommandInvocation` | `hop run`, `hop push` |
| `ssm:CancelCommand` | `hop run`, `hop push` (cancels a command the instance never picked up) |
| `ec2:DescribeRegions` | `hop scan` |
| `s3:ListAllMyBuckets` | `hop s3 ls` (the buckets of each account) |
| `s3:ListBucket` | `hop s3 ls <bucket>/`, finding a bucket of another account, and telling a missing object from a refused one |
| `s3:GetObject` | `hop s3 pull`, and the look `hop s3 push` takes before replacing an object |
| `s3:PutObject`, `s3:AbortMultipartUpload` | `hop s3 push` |
| `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject`, `s3:AbortMultipartUpload` on the relay bucket | `hop push` of more than 64 KB, or with `--relay` |
| `kms:Decrypt`, `kms:GenerateDataKey` | `hop s3 pull` and `hop s3 push` on a bucket encrypted with a KMS key |

As a policy for your role (or permission set):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Inventory",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeRegions",
        "ssm:DescribeInstanceInformation"
      ],
      "Resource": "*"
    },
    {
      "Sid": "SessionLoggingCheck",
      "Effect": "Allow",
      "Action": "ssm:GetDocument",
      "Resource": "arn:aws:ssm:*:*:document/SSM-SessionManagerRunShell"
    },
    {
      "Sid": "ShellAndTunnel",
      "Effect": "Allow",
      "Action": "ssm:StartSession",
      "Resource": [
        "arn:aws:ec2:*:*:instance/*",
        "arn:aws:ssm:*:*:managed-instance/*",
        "arn:aws:ssm:*:*:document/SSM-SessionManagerRunShell",
        "arn:aws:ssm:*:*:document/AWS-StartPortForwardingSession",
        "arn:aws:ssm:*:*:document/AWS-StartPortForwardingSessionToRemoteHost",
        "arn:aws:ssm:*:*:document/AWS-StartSSHSession",
        "arn:aws:ssm:*:*:document/AWS-StartNonInteractiveCommand"
      ]
    },
    {
      "Sid": "OwnSessions",
      "Effect": "Allow",
      "Action": [
        "ssm:TerminateSession",
        "ssm:ResumeSession",
        "ssmmessages:OpenDataChannel"
      ],
      "Resource": "arn:aws:ssm:*:*:session/${aws:userid}-*"
    },
    {
      "Sid": "RunAndPush",
      "Effect": "Allow",
      "Action": "ssm:SendCommand",
      "Resource": [
        "arn:aws:ec2:*:*:instance/*",
        "arn:aws:ssm:*:*:managed-instance/*",
        "arn:aws:ssm:*:*:document/AWS-RunShellScript"
      ]
    },
    {
      "Sid": "FollowCommands",
      "Effect": "Allow",
      "Action": [
        "ssm:GetCommandInvocation",
        "ssm:CancelCommand"
      ],
      "Resource": "*"
    },
    {
      "Sid": "KMSEncryptedSessions",
      "Effect": "Allow",
      "Action": "kms:GenerateDataKey",
      "Resource": "arn:aws:kms:*:*:key/*"
    },
    {
      "Sid": "S3Buckets",
      "Effect": "Allow",
      "Action": "s3:ListAllMyBuckets",
      "Resource": "*"
    },
    {
      "Sid": "S3Objects",
      "Effect": "Allow",
      "Action": [
        "s3:ListBucket",
        "s3:GetObject",
        "s3:PutObject",
        "s3:AbortMultipartUpload"
      ],
      "Resource": [
        "arn:aws:s3:::acme-*",
        "arn:aws:s3:::acme-*/*"
      ]
    },
    {
      "Sid": "RelayBucket",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:GetObject",
        "s3:DeleteObject",
        "s3:AbortMultipartUpload"
      ],
      "Resource": "arn:aws:s3:::acme-tools-relay/hop/*"
    }
  ]
}
```

The session statements follow the AWS sample policies for Session Manager. Every
resource can be narrowed: instances to some accounts and regions, or to tags with an
`ssm:resourceTag/...` condition, and the KMS key to the one in your session
preferences. Drop what you do not use: `RunAndPush` and `FollowCommands` without
`run` and `push`, the `AWS-StartNonInteractiveCommand` document without `pull`,
`KMSEncryptedSessions` when sessions are not KMS-encrypted,
`ec2:DescribeRegions` without `hop scan`, `S3Buckets` and `S3Objects` without
`hop s3`, `RelayBucket` without a relay bucket. In AWS GovCloud (US) or China, ARNs
start with `arn:aws-us-gov:` or `arn:aws-cn:`.

`S3Objects` gives access to data, not to machines: name the buckets hop may read
(`acme-*` above is an example), keep `s3:PutObject` and `s3:AbortMultipartUpload` for
the ones you upload to, and add `kms:Decrypt` and `kms:GenerateDataKey` on the key of
a bucket encrypted with KMS. `hop s3` needs no `s3:DeleteObject`: it never deletes.
The only object hop deletes is the temporary copy it wrote itself to a relay bucket,
which is why `RelayBucket` names that bucket, and here a prefix of it, and nothing
else.

The **instance role** needs more than `AmazonSSMManagedInstanceCore` when the account
logs sessions, since the agent writes the log itself: `logs:CreateLogStream`,
`logs:PutLogEvents`, `logs:DescribeLogGroups` and `logs:DescribeLogStreams` for
CloudWatch Logs, or `s3:PutObject` on the bucket and `s3:GetEncryptionConfiguration`
for S3, plus `kms:Decrypt` on the session key when sessions are KMS-encrypted.

## How it compares

hop is not the only Session Manager client. What the others do well, as of October
2026:

| Tool | Good at | Next to hop |
|---|---|---|
| `aws ssm start-session` | The reference: every document and parameter. | One instance ID, profile and region at a time; port forwarding takes JSON parameters. |
| [aws-gate](https://github.com/xen0l/aws-gate) | SSH with ephemeral keys pushed through EC2 Instance Connect; host aliases in a config file. | One profile and region per host, set up by hand. Last release in 2023. |
| [aws-ssm-tools](https://github.com/mludvig/aws-ssm-tools) | ECS sessions, SSH, and `ssm-tunnel`, an IP link to the instance's VPC. | One profile per call (`--profile`). |
| [taws](https://github.com/huseyinbabal/taws) | A full terminal UI for AWS, which can also open an SSM shell. | Browses one profile and region at a time. |
| [Systems Manager unified console](https://docs.aws.amazon.com/systems-manager/latest/userguide/systems-manager-unified-console.html) | The nodes of every account of an AWS Organization, in the browser. | Needs Organizations and a delegated administrator; no terminal. |
| Teleport, StrongDM | Access platforms: policies, approvals, recordings. | A server and agents to deploy. |

hop is a client-side tool built around the whole fleet:

- one inventory of every profile on your machine, queried in parallel and cached,
  even across separate AWS Organizations, searchable by name, words, ID or IP;
- one login per SSO portal, with expired tokens caught before connecting;
- a session logging check before each shell, and `transcript: require` to refuse
  unlogged sessions;
- never acting on a fuzzy match without a question, nor on any loose match without a
  terminal;
- `ssh`, `scp` and `rsync` by instance name through `hop init ssh`;
- `run` keeps the remote exit code and cancels commands the instance never picked up;
  `push` and `pull` compare the SHA-256 on both sides, and `push` carries large files
  through a bucket without giving the instance any S3 permission;
- the S3 buckets of every account in one list, and objects copied by bucket name,
  without replacing one unasked;
- `hop doctor` for the local AWS setup.

What hop does not do (yet): ECS or EKS sessions, ephemeral SSH keys, a VPN-like
tunnel, a command on many instances at once, or what `aws s3 sync` and `aws s3 rm`
do (whole directories, deletions). Issues and pull requests are welcome.

## Contributing

Issues and pull requests are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Please
report security issues privately, as described in [SECURITY.md](SECURITY.md). What
changed in each version is in [CHANGELOG.md](CHANGELOG.md).

## Support

hop is free and maintained on my own time. If it saves you a few logins a day, you can
buy me a coffee:

[![ko-fi](https://ko-fi.com/img/githubbutton_sm.svg)](https://ko-fi.com/B0F429R66U)

## License

[MIT](LICENSE)
