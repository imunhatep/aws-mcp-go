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
`list_resources` / `count_resources` / `list_resources_fallback` fan out across
them, and the Cost Explorer tools return per-account groups. To run this in a
container, see [Running in a container](container.md).

Pass `account_id` to confine a query to one account. It scopes the fan-out, not
the output: only that account's profile credentials are used and only that
account is called, so a query against a development account issues no API calls
against a production one in the same pool. An account no profile points at is an
error rather than an empty result.

One caveat specific to `list_resources_fallback`: a per-account permission gap is
invisible in the rows. It needs `cloudcontrol:ListResources` plus the target
service's own read permission in *every* account it queries, and an account that
denies either is logged and skipped rather than failing the call — so the result
is quietly short. Its `queried` field reports how many accounts were actually
asked, and its `warnings` field says so explicitly.

Notes:

- Each profile is checked before any AWS call (see below), and its credentials
  are exercised on **first use** rather than at startup: one STS call per profile,
  when a tool first needs it. Region clients are created lazily too.
- A profile that cannot authenticate does not stop the server or the other
  profiles — it is reported to the caller of the tool that needed it, and retried
  on the next call (see [Credential failures never stop the server](#credential-failures-never-stop-the-server)).
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

## Credential failures never stop the server

An MCP server that exits when AWS credentials are unusable is worse than useless:
the calling agent gets a refused connection, which tells it nothing, and cannot
recover on its own. So nothing about credentials is fatal here.

- **Startup never touches AWS.** Each profile's config is inspected locally, then
  the pool is built on first use. `serve` fails only on operator errors that
  cannot fix themselves — a malformed role ARN, a duplicated profile, a missing
  listen address.
- **Failures come back as tool results.** A tool that needs an unauthenticated
  profile returns an error naming the profile and the command that fixes it, and
  the server keeps serving. `aws_auth_status` reports the same state without
  touching AWS at all.
- **Recovery needs no restart.** A failed profile is retried a few seconds later,
  and the SDK re-reads `~/.aws/sso/cache` when a token expires — so an
  `aws sso login` in another terminal (or the flow below) takes effect on the next
  question the agent asks.
- **An empty answer is never used to hide a failure.** If no profile could be
  authenticated, or every client failed to build, the tool errors instead of
  reporting zero resources. "Nobody is logged in" and "that account is empty"
  must not look the same.

## Automatic SSO login (device flow)

When an SSO session has expired *beyond refresh*, someone has to approve a
browser prompt — there is no way around that. What the server can do is start
that prompt itself and hand the URL and code to the agent, which is exactly what
it does:

```
list_resources → error: aws profile "dev": the SSO session has expired and could
not be refreshed; run: aws sso login --sso-session my-session | aws sso login
started for session "my-session": ask the user to open
https://device.sso.eu-central-1.amazonaws.com/?user_code=ABCD-EFGH and confirm
code ABCD-EFGH (valid for 9m52s). The server is polling and caches the token
itself — retry this call once the user has approved; no restart is needed
```

This is the RFC 8628 device-authorization flow — the same one `aws sso login`
runs — performed in-process with `ssooidc`. Consequences worth knowing:

- **No AWS CLI, no shell, no browser is needed on the server**, which is what
  makes it work in the distroless image. The human approving it needs a browser;
  the server does not.
- **The token is written to the SDK's own cache** (`~/.aws/sso/cache/<sha1>.json`,
  mode `0600`, temp file + rename) in the same JSON shape the CLI writes, so the
  credential chain and the `aws` CLI both pick it up. The cache directory must be
  writable — see the [container notes](container.md).
- **One flow per `sso_session`**, reused while it is valid. Twenty-five profiles
  on one SSO session produce one URL and one code, not twenty-five.
- **The existing OIDC client registration is reused** when it has not expired.
  Re-registering would invalidate the refresh token cached beside it, turning
  every expiry into a full re-approval.
- **Only credential failures trigger it.** A region an account has not enabled, a
  missing IAM grant or a throttle is reported as itself; sending someone to a
  browser for those would be noise.

Two tools drive it directly:

| Tool | Purpose |
|------|---------|
| `aws_auth_status` | Per-profile mechanism, account, usability, SSO token expiry and refreshability, plus any login in progress. Reads local files only, so it answers while AWS is unusable. |
| `aws_sso_login` | Starts (or picks up) a login for a profile and returns the verification URL and user code. Works even with `--sso-auto-login=false`. |

Flags:

| Flag | Env | Default | Effect |
|------|-----|---------|--------|
| `--sso-auto-login` | `MCP_SSO_AUTO_LOGIN` | `true` | A failing tool call starts a device flow itself and includes the URL and code in its error. |
| `--sso-open-browser` | `MCP_SSO_OPEN_BROWSER` | `true` | Also opens that URL locally (`open` / `xdg-open`). Best-effort, and skipped inside a container or without a display. |

## Preflight credential checks

Before any AWS call, each profile's shared config is parsed and checked, so
credential problems are reported with the fix instead of surfacing later as an
opaque SDK error. The checks are pure config/file reads — no AWS request is made.
None of them abort the server; "fail" below means the profile is unusable until
it is fixed, and every tool that needs it says so:

| Condition | Result |
|-----------|--------|
| Profile not defined in `~/.aws/config` | fail, naming the profile |
| SSO profile with no cached token | fail with `run: aws sso login --sso-session <name>`, and a device flow when auto-login is on |
| SSO access token expired, refresh token present | warn — the SDK refreshes it on first use, no human needed |
| SSO token expired and *not* refreshable | fail, showing the expiry, plus the login command and device flow |
| SSO token cache unreadable / no `expiresAt` | fail, pointing at the cache file |
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

Shortly after startup the base credentials' STS caller identity is resolved in the
background and logged with the account, ARN, user ID and region. In assume-role
mode this reports the base principal that role assumption chains off of; if it
fails, the server logs the reason and carries on.
