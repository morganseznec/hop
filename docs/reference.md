# hop reference

Details that the [README](../README.md) leaves out: exit codes, the JSON output of
`hop ls` and `hop s3 ls`, limits, environment variables, the columns of `hop ls` and
`hop s3 ls`, and where hop keeps its files. Accounts, names and addresses below are fictional.

## Exit codes

| Code | When |
|---|---|
| 0 | Success. |
| exit code of the remote command | `hop run`, when the command fails on the instance with an exit code above 0. |
| exit code of `session-manager-plugin` | A shell, a tunnel or `hop ssh-proxy`: hop returns what the plugin returns when the session ends. |
| exit code of `gcloud compute ssh` | A shell on a GCP instance (experimental). |
| 130 | Interrupted with Ctrl-C, or canceled: Esc in the picker, or "no" to a question. |
| 1 | Any other error, printed on stderr as `hop: ...`. |

`hop run` also returns 1 when the command ends without an exit code (canceled or
timed out on the instance), or when hop stops waiting (`--timeout`). Ctrl-C while
`hop run` waits says whether the command still runs on the instance.

## JSON output of `hop ls`

`hop ls --json` prints a JSON array, `[]` when nothing matches, with one object per
instance. The problem report goes to stderr, as with `hop ls`, so a script can tell
that some accounts were not reached.

```json
[
  {
    "id": "i-0abc1234def567890",
    "name": "acme-p-shop-app",
    "profile": "acme-prod",
    "region": "eu-west-1",
    "private_ip": "10.0.1.21",
    "state": "running",
    "env": "prod",
    "platform": "Amazon Linux",
    "agent": "Online",
    "last_ping": "2026-10-30T09:14:12Z",
    "transcript": {
      "checked": true,
      "enabled": true,
      "log_group": "/acme/ssm/sessions",
      "encrypted": true
    },
    "label": "ACME",
    "fetched_at": "2026-10-30T09:15:00Z",
    "stale": false
  }
]
```

Fields marked "optional" are left out when empty.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Instance ID (`i-...`), or `mi-...` for a hybrid node. |
| `name` | string | `Name` tag, or the ID when there is none. A hybrid node reports its own name. |
| `profile` | string | AWS profile the instance was found through. |
| `region` | string | Region it was found in. |
| `private_ip` | string, optional | Private IP address. |
| `state` | string | EC2 state: `pending`, `running`, `stopping` or `stopped`. Hybrid nodes are always `running`. |
| `env` | string, optional | `prod` or `test`, from a tag or from the names (see the README). |
| `platform` | string, optional | Platform reported by EC2 (`Linux/UNIX`, `Windows`...) or, when it has one, by the SSM agent (`Amazon Linux`, `Ubuntu`...). |
| `agent` | string, optional | SSM agent status: `Online`, `ConnectionLost` or `Inactive`. Absent when no agent is registered, or when the agent states could not be read. |
| `last_ping` | string, optional | Last contact of the agent, RFC 3339. |
| `also_via` | array of strings, optional | Other profiles that see the same instance (two roles on one account). |
| `transcript` | object | Session logging of the account in that region, as last read (see below). |
| `label` | string, optional | Label of the profile, from `labels:` in hop's configuration. |
| `fetched_at` | string, optional | Last successful query of this profile and region: RFC 3339, UTC, whole seconds. |
| `stale` | boolean | `true` when the last query of this profile and region failed, or is older than `cache_ttl`, or never succeeded. |
| `error` | string, optional | Why the last query failed, starting with the cause when hop knows it (`SSO token expired: ...`). |

`transcript` holds:

| Field | Type | Meaning |
|---|---|---|
| `checked` | boolean | The `SSM-SessionManagerRunShell` document could be read (or does not exist, which means no logging). |
| `enabled` | boolean | Shells are logged, to CloudWatch Logs or S3. |
| `log_group` | string, optional | CloudWatch Logs group. |
| `bucket` | string, optional | S3 bucket. |
| `encrypted` | boolean, optional | The log, or the session, is encrypted with KMS. |
| `err` | string, optional | Why the document could not be read. |

## JSON output of `hop s3 ls`

`hop s3 ls --json` prints a JSON array, `[]` when nothing matches. As with `hop ls`,
the accounts that could not be reached are reported on stderr.

Without a place, or with words, there is one object per bucket:

```json
[
  {
    "name": "acme-p-logs",
    "profile": "acme-prod",
    "region": "us-east-1",
    "env": "prod",
    "created": "2023-09-02T10:21:44Z",
    "label": "ACME",
    "fetched_at": "2026-11-12T09:15:00Z",
    "stale": false
  }
]
```

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Bucket name. |
| `profile` | string | AWS profile the bucket was listed through. |
| `region` | string, optional | Region the bucket lives in, whatever the region of the profile. Absent when the endpoint does not say (some S3-compatible stores). |
| `env` | string, optional | `prod` or `test`, from the name of the bucket or of the profile. |
| `created` | string, optional | Creation date as S3 reports it, RFC 3339. S3 may move it when the bucket changes, for example when its policy is edited. |
| `also_via` | array of strings, optional | Other profiles that list the same bucket (two roles on one account). |
| `label` | string, optional | Label of the profile, from `labels:` in hop's configuration. |
| `fetched_at` | string, optional | Last successful `ListBuckets` of this profile: RFC 3339, UTC, whole seconds. |
| `stale` | boolean | `true` when the last call of this profile failed, or is older than `cache_ttl`, or never succeeded. |
| `error` | string, optional | Why the last call failed, starting with the cause when hop knows it. |

With a place (`hop s3 ls --json acme-p-logs/2026/`), there is one object per entry of
the listing, in the order shown:

```json
[
  {
    "key": "2026/11/",
    "size": 0,
    "prefix": true
  },
  {
    "key": "2026/app.log",
    "size": 1204311,
    "modified": "2026-11-07T14:02:11Z",
    "storage_class": "STANDARD"
  }
]
```

| Field | Type | Meaning |
|---|---|---|
| `key` | string | The whole key, whatever the listing shows, exactly as S3 holds it: a key may contain any character. |
| `size` | number | Size in bytes; 0 for a prefix. |
| `modified` | string, optional | Last modification, RFC 3339. Absent for a prefix. |
| `storage_class` | string, optional | `STANDARD`, `STANDARD_IA`, `GLACIER`... Absent for a prefix. |
| `prefix` | boolean, optional | `true` for a group of keys that share what comes before the next `/`. Absent with `-R`, which lists every object. |

`-a`, `-t`, `-S` and `--reverse` apply to the JSON output too: without `-a`, the
entries whose name starts with a dot are left out, and stderr says how many.

## Limits and timings

| What | Value |
|---|---|
| `hop run` output | 24,000 characters per stream (stdout, stderr): SSM cuts the rest, and hop says so. |
| `hop run --timeout` | 2 minutes by default. Above one hour, it also raises the command's execution timeout, up to 48 hours (`AWS-RunShellScript` stops a command after one hour by default). |
| A command not delivered | Canceled when hop stops waiting (timeout, Ctrl-C), so it never runs later. A command already running goes on. |
| `hop push` | To an absolute path, written as root. Up to 64 KB (64,000 bytes), in the command itself: SSM Run Command takes 97 KB per command with its document, and base64 adds a third. hop waits up to 2 minutes, then compares the SHA-256 on both sides. |
| `hop push` through a relay bucket | Past 64 KB, or with `--relay`, when `relay_buckets` names a bucket for the profile (its own name first, else the longest pattern that matches). No size limit: the upload follows the rules of `hop s3 push` below. hop waits for the instance 2 minutes plus one second per MB, 12 hours at most; the signed URL is valid for that long plus 5 minutes. The instance downloads next to the destination, so it needs twice the size there while the file is copied. The temporary object is deleted afterwards, unless the instance may still be fetching it when hop stops waiting. |
| `hop pull` | No size limit, from an absolute path, read as the session user or with `sudo -n`. The file is written under a temporary name next to its destination, then renamed once the SHA-256 matches on both sides. The SSM agent keeps the output on the instance's disk while it streams, in base64. |
| `hop s3 ls` | The buckets of a profile are cached like its instances (`cache_ttl`), and read again when a name finds nothing in a list more than one minute old. Objects are never cached: a listing takes one call per 1,000 keys, and holds the whole listing in memory before printing it. |
| `hop s3 pull` | No size limit. Ranges of 8 MB, five at a time, each tried up to three times when the connection drops, all from the same version of the object. The file is written under a temporary name next to its destination, then renamed once the size matches and, when S3 holds a checksum of the whole object (SHA-256, CRC-64/NVME, CRC-32C or CRC-32), once that matches too. An archived object must be restored first. |
| `hop s3 push` | One request up to 16 MB, a multipart upload from there: parts of 8 MB, five at a time, larger parts past 80 GB (S3 takes 10,000 parts). A failed or interrupted upload is aborted, Ctrl-C included (the abort gets 10 seconds). An existing object is replaced only after a yes, or with `--force`. The content type is set from the file extension. |
| Inventory cache | 15 minutes (`cache_ttl`). A profile and region whose query failed is retried after one minute. |
| Freshness before acting | An instance whose data is more than one minute old is looked up again before hop acts on it. |
| Inventory queries | 16 profile/region pairs at a time, 25 seconds each at most. |
| GCP inventory (experimental) | 16 projects at a time, 25 seconds in all. A GCP instance listed more than one minute ago is read again (15 seconds at most) before a shell. |
| `transcript: require` | The logging settings are read again before each shell, within 10 seconds, or the shell is refused. |
| Recent instances | Listed first for 7 days, forgotten after 90 days. |
| `hop tunnel` | A remote port below 1024 is opened 8000 above on your machine (22 becomes 8022), or on the next free port. |
| `hop ssh-proxy` | The host ssh gives, without `.hop`, is searched like `hop <host>`, but without a terminal: only an unambiguous match connects. Port 22 unless ssh gives another. stdout carries only the SSH stream, and stderr only warnings and errors. |

## Environment variables

| Variable | Effect |
|---|---|
| `NO_COLOR` | Any non-empty value turns colors off. |
| `XDG_CONFIG_HOME` | hop's configuration goes to `$XDG_CONFIG_HOME/hop/config.yaml`, on every system. |
| `XDG_CACHE_HOME` | The inventory cache goes to `$XDG_CACHE_HOME/hop/`. |
| `XDG_STATE_HOME` | The recent instances go to `$XDG_STATE_HOME/hop/`. |
| `AWS_CONFIG_FILE` | AWS config file to read instead of `~/.aws/config`. |
| `AWS_SHARED_CREDENTIALS_FILE` | AWS credentials file to read instead of `~/.aws/credentials`. |
| `AWS_REGION`, then `AWS_DEFAULT_REGION` | Region of the profiles that set none, after `default_region` in hop's configuration and the region of `[default]`. |
| `AWS_PROFILE` | Ignored: hop queries every profile with credentials. Use `-p` to narrow the scope. |

The AWS SDK reads its own variables too (for example `HTTPS_PROXY`,
`AWS_ENDPOINT_URL_SSM` or `AWS_ENDPOINT_URL_S3`), and hop hands the SSM endpoint it
resolved to `session-manager-plugin`. With GCP on, gcloud reads its own as well (for example
`CLOUDSDK_CONFIG` or `CLOUDSDK_CORE_ACCOUNT`): hop uses whatever account gcloud uses.

## `hop ls` columns

| Column | Values |
|---|---|
| `NAME` | `Name` tag, or the instance ID. |
| `PROFILE` | Profile, followed by its label in parentheses when `labels:` sets one. `gcp:<project>` for a GCP instance (experimental). |
| `REGION` | Region the instance was found in; the zone for a GCP instance. |
| `ENV` | `prod`, `test`, or empty. |
| `IP` | Private IP address. |
| `AGENT` | `online`: the SSM agent answers. `lost 3h ago`: connection lost, with its last contact. `inactive` (or another status, in lower case): registered but not reachable. `none`: the agent states were read, and none is registered for this instance. `?`: the agent states could not be read (permissions, timeout), so hop does not know. With `--all`, a stopped instance shows its state here instead. `-` for a GCP instance, reached through IAP. |
| `LOGGING` | `yes`: the account logs shells in that region (CloudWatch Logs or S3). `NO`: it does not, or has no Session Manager preferences at all. `?`: not checked, because the `SSM-SessionManagerRunShell` document could not be read (permissions, network) or that region was never queried. `-` for a GCP instance: GCP does not record shell sessions. |

The footer, on stderr, counts instances, profiles and regions, and says how old the
cache is.

## `hop s3 ls` columns

The list of buckets:

| Column | Values |
|---|---|
| `NAME` | Bucket name. |
| `PROFILE` | Profile the bucket was listed through, followed by its label when `labels:` sets one. |
| `REGION` | Region the bucket lives in. |
| `ENV` | `prod`, `test`, or empty. |
| `CREATED` | Creation date as S3 reports it, in the time zone of your machine. S3 may move it when the bucket changes, for example when its policy is edited: do not read it as the age of the bucket. |

The objects of a bucket, with `-l` (without it, names only, one per line):

| Column | Values |
|---|---|
| `SIZE` | Bytes, or KB, MB and GB (powers of 1,000) with `-h`. `-` for a prefix. |
| `MODIFIED` | Last modification, in the time zone of your machine. `-` for a prefix. |
| `CLASS` | Storage class. `-` for a prefix. |
| `NAME` | What follows the last `/` of the place listed, with a final `/` for a prefix; the whole key with `-R`. Control characters, which a key may hold, show as `?`. |

The footer, on stderr, counts objects and prefixes, adds up the sizes, names the
bucket with its profile and region, and says how many names `-a` would add.

## File locations

| File | Linux, macOS | Windows |
|---|---|---|
| hop's configuration | `~/.config/hop/config.yaml` | `%AppData%\hop\config.yaml` |
| Previous configuration, after `hop scan --write` | `~/.config/hop/config.yaml.bak` | `%AppData%\hop\config.yaml.bak` |
| Inventory cache (instances and buckets, no credentials) | `~/.cache/hop/inventory.json` | `%LocalAppData%\hop\inventory.json` |
| Lock files of the cache | `~/.cache/hop/refresh.lock`, `save.lock` | `%LocalAppData%\hop\` |
| Recent instances | `~/.local/state/hop/recents.json` | `%LocalAppData%\hop\recents.json` |
| AWS profiles (read only) | `~/.aws/config`, `~/.aws/credentials` | `%UserProfile%\.aws\config`, `credentials` |
| SSO tokens (read only, written by `aws sso login`) | `~/.aws/sso/cache/` | `%UserProfile%\.aws\sso\cache\` |

The `XDG_*` and `AWS_*_FILE` variables above move these files. hop creates its
folders readable by you only (`0700`) and its files `0600`. In the Docker image, the
cache and the recent instances live under `/tmp/hop`, and the configuration in
`/home/hop/.config/hop/config.yaml`.
