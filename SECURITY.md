# Security policy

hop opens shells on real servers, so security reports are welcome and handled first.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting
instead: **Security** tab of this repository, then **Report a vulnerability**.

Include what you found, how to reproduce it, and the version (`hop --version`).
You should get an answer within a week.

## Scope

In scope:

- hop connecting, running a command or copying a file somewhere the user did not
  choose (for example a fuzzy match acting without confirmation);
- credentials, tokens or command output written to disk or logs by hop;
- the session logging check reporting a session as logged when it is not.

Out of scope: the security of AWS Session Manager itself, the
`session-manager-plugin`, or the IAM permissions of your accounts.

## What hop stores

- `~/.cache/hop/inventory.json`: instance names, IDs, private IPs, regions and the
  session logging settings of each account. No credentials.
- `~/.local/state/hop/recents.json`: which instances you used, and when.
- `~/.config/hop/config.yaml`: only what you write there.

hop never reads or copies the values of your AWS keys. It hands the profile name to
the AWS SDK and to `session-manager-plugin`, like the AWS CLI does.
