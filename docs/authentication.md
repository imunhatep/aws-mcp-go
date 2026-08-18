# Authentication

The server uses the standard AWS credential chain, so it works with anything
`aws-sdk-go-v2` understands. Three mutually exclusive modes.

## Local mode (default) — AWS profile / SSO / env

Single account, using whatever credentials the default chain resolves
(`~/.aws/config` SSO sessions, `AWS_PROFILE`, static env vars, IMDS, …):

```sh
aws sso login --profile my-sso-profile
AWS_PROFILE=my-sso-profile ./bin/aws-mcp serve
```

## Multi-profile mode — one account per profile

Serves several AWS shared-config profiles from a single process, each as its own
account. Useful when the accounts are reachable as separate profiles (SSO,
static keys, `credential_process`) rather than through assumable roles:

```sh
aws sso login --sso-session my-session      # one login covers every profile on that session
./bin/aws-mcp serve --profiles dev,prod
```

Every tool then spans all profiles: `list_accounts` reports each account,
`list_resources` / `count_resources` fan out across them, and the Cost Explorer
tools return per-account groups. To run this in a container, see
[Running in a container](container.md).

Notes:

- Each profile is checked before any AWS call (see below) and its identity then
  resolved with one STS call, so a missing, undefined or expired profile aborts
  the server with the profile named in the error. Region clients are still
  created lazily on first use.
- Only the named profile's credentials are used — ambient `AWS_PROFILE` /
  `AWS_ACCESS_KEY_ID` env vars do not override them.
- If two profiles point at the same account, the duplicate `(account, region)`
  pair is dropped so results are not counted twice.
- `--profiles` cannot be combined with `--assume-role` / `--assume-role-arns`.

## Assume-role mode — cross-account

Assumes IAM roles in other accounts, chaining off the base credentials' STS.

- **Auto-discover** the assumable roles from the current IAM role's attached
  policies (`sts:AssumeRole` resources):

  ```sh
  ./bin/aws-mcp serve --assume-role
  ```

- **Explicit** roles (comma-separated; `accountID=roleArn` or a bare role ARN):

  ```sh
  ./bin/aws-mcp serve --assume-role-arns 'arn:aws:iam::111111111111:role/reader,arn:aws:iam::222222222222:role/reader'
  ```

  Providing `--assume-role-arns` implies assume-role mode and overrides
  auto-discovery.

## Startup credential checks

Before any AWS call, each profile's shared config is parsed and checked, so
credential problems are reported with the fix instead of surfacing later as an
opaque SDK error on the first tool call. The checks are pure config/file reads —
no AWS request is made:

| Condition | Result |
|-----------|--------|
| Profile not defined in `~/.aws/config` | abort, naming the profile |
| SSO profile with no cached token | abort with `run: aws sso login --sso-session <name>` |
| SSO token expired | abort, showing the expiry and the same login command |
| SSO token cache unreadable / no `expiresAt` | abort, pointing at the cache file |
| SSO token cache directory not writable | warn — refresh will fail later (see the [container notes](container.md)) |
| SSO token expiring within 30 minutes | warn, with the remaining validity |
| Legacy SSO profile (`sso_start_url`, no `sso_session`) | warn — the SDK cannot refresh it |
| `credential_process` profile | warn — needs that binary on PATH, unavailable in the distroless image |

A healthy start logs the token lifetime, so it is visible up front how long the
server can run unattended:

```
INF [mcpserver.PreflightProfile] sso token cached profile=dev sso_session=my-session token_valid_for=7h58m0s
INF [ProfilePool] profile resolved profile=dev account=111111111111 arn=arn:aws:sts::111111111111:assumed-role/…
```

Errors that do slip through to the SDK are translated the same way — an expired
session, an undefined profile, an unrunnable `credential_process` and expired
static credentials each get a message naming the profile and the fix.

Startup also resolves the base credentials' STS caller identity and logs the
account, ARN, user ID and region. In assume-role mode this reports the base
principal that role assumption chains off of.
