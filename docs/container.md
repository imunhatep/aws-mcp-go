# Running in a container

The runtime image is distroless (`gcr.io/distroless/static-debian12:nonroot`) —
no shell, no package manager, running as uid 65532. It sets
`HOME=/home/nonroot`, which is where the AWS credential chain looks, and
`MCP_ADDR=0.0.0.0:3040`.

## Getting the image

Tagged releases publish a `linux/amd64,linux/arm64` manifest to GHCR:

```sh
podman pull ghcr.io/imunhatep/aws-mcp-go:latest
```

Or build locally. The image compiles from the vendored dependency tree, so
`make image` runs `go mod vendor` first:

```sh
make image                          # -> ghcr.io/imunhatep/aws-mcp-go:latest
make image VERSION=v0.1.0           # -> ghcr.io/imunhatep/aws-mcp-go:v0.1.0
make image-multiarch VERSION=v0.1.0 # linux/amd64,linux/arm64 manifest
```

Override `IMAGE=registry/name` to tag for a different registry.

## Multi-account with AWS SSO (recommended)

Serve several accounts from one container using native AWS SSO (`sso_session`
profiles) and `--profiles`. Nothing AWS-related runs inside the container: the
SDK reads the token the host wrote and refreshes it on its own, so no shell, no
`aws` CLI and no credential helper are needed in the image.

**1. Define the profiles on the host.** All profiles that share an
`[sso-session]` block share a single token, so one login covers all of them:

```ini
# ~/.aws/config
[sso-session my-session]
sso_start_url = https://d-1234567890.awsapps.com/start
sso_region = eu-central-1
sso_registration_scopes = sso:account:access

[profile dev]
sso_session = my-session
sso_account_id = 111111111111
sso_role_name = readonly
region = eu-central-1

[profile prod]
sso_session = my-session
sso_account_id = 222222222222
sso_role_name = readonly
region = eu-central-1
```

**2. Log in once on the host** — the only step that ever needs a browser:

```sh
aws sso login --sso-session my-session
```

**3. Run the container** with the config and the token cache mounted:

```sh
podman run --rm -d \
  --name aws-mcp \
  -v ~/.aws/config:/home/nonroot/.aws/config:ro \
  -v ~/.aws/sso/cache:/home/nonroot/.aws/sso/cache:rw \
  -p 127.0.0.1:3040:3040 \
  ghcr.io/imunhatep/aws-mcp-go:latest \
  serve --profiles dev,prod
```

**4. Check the startup log.** Each profile's token lifetime is reported before the
server listens; the identities themselves resolve immediately afterwards, in the
background, since nothing about credentials may hold up the listener:

```sh
podman logs aws-mcp
```
```
INF [serve] using aws shared-config profiles profiles=["dev","prod"]
INF [mcpserver.PreflightProfile] sso token cached profile=dev sso_session=my-session token_valid_for=7h58m0s
INF [mcpserver.ServeHTTP] starting MCP streamable-HTTP server addr=0.0.0.0:3040 endpoint=/mcp
INF [ProfilePool] profile resolved profile=dev account=111111111111 arn=arn:aws:sts::111111111111:assumed-role/…
INF [ProfilePool] profile resolved profile=prod account=222222222222 arn=arn:aws:sts::222222222222:assumed-role/…
```

Then `list_accounts` returns both account IDs, and every resource and cost tool
spans them.

Five things worth knowing:

- **Mount the token cache read-write.** On refresh the SDK writes a temp file
  into the cache directory and renames it over the token, so a `:ro` mount works
  until the token expires and then fails every call. The same mount is where the
  server writes the token when it runs an SSO login itself, so read-only also
  disables that. Startup warns when the directory is not writable.
- **Mount `config` and `sso/cache` separately**, not all of `~/.aws`, so the
  container never sees `~/.aws/credentials`.
- **File ownership.** The image runs as uid 65532; if that user cannot write the
  cache mount, add `--userns=keep-id:uid=65532,gid=65532` (or, locally,
  `--user 0`).
- **Re-login needs no restart.** When the SSO session finally expires, run
  `aws sso login --sso-session my-session` on the host again — the running
  container re-reads the token file on its next refresh and carries on.
- **The container can run the login itself.** With `--sso-auto-login` (the
  default), a tool call that hits an expired session starts the device flow
  in-process and returns the verification URL and user code to the calling agent,
  which relays them to you; approving in a browser on any machine completes it and
  the token lands in the mounted cache. No `aws` CLI, shell or browser is needed
  in the image — only egress to `oidc.<sso_region>.amazonaws.com`. Browser opening
  is skipped automatically inside a container.

Profiles spread across *different* `sso_session` blocks work too; each session
needs its own `aws sso login`, and all their tokens live in the same mounted
cache directory.

Single-account SSO is the same command without `--profiles` and with
`-e AWS_PROFILE=dev`.

## Credential passthrough — nothing AWS-related in the container

With `--credentials-passthrough` the container holds no credentials and mounts
no `~/.aws` at all. The MCP client resolves credentials on its own machine and
forwards them with every request in the `X-Aws-Credentials` header; the server
uses those and nothing else. The mechanism, expiry handling and wire format are in
[authentication.md](authentication.md#credential-passthrough--the-client-forwards-its-own-credentials).

**1. Run the container** with no volumes and no AWS environment:

```sh
podman run --rm -d \
  --name aws-mcp \
  -p 127.0.0.1:3040:3040 \
  ghcr.io/imunhatep/aws-mcp-go:latest \
  serve --credentials-passthrough
```

`-e MCP_CREDENTIALS_PASSTHROUGH=true` with the image's default `serve` command
does the same.

**2. Log in on the client machine**, with the profiles defined there as in the
SSO section above:

```sh
aws sso login --sso-session my-session
```

**3. Register the server with a `headersHelper`.** The helper runs on the client
machine, not in the server's container, so the client needs the `aws-mcp`
binary — take it from a release tarball (`aws-mcp_<version>_<os>_<arch>.tar.gz`)
or `make build`:

```sh
claude mcp add-json --scope user aws '{
  "type": "http",
  "url": "http://127.0.0.1:3040/mcp",
  "headersHelper": "/usr/local/bin/aws-mcp credentials-headers --profiles dev,prod"
}'
```

**No binary on the client?** The image can run the helper too. It reads the
client's config and SSO cache through the same mounts as the SSO setup above,
prints the header and exits:

```sh
claude mcp add-json --scope user aws '{
  "type": "http",
  "url": "http://127.0.0.1:3040/mcp",
  "headersHelper": "podman run --rm -v $HOME/.aws/config:/home/nonroot/.aws/config:ro -v $HOME/.aws/sso/cache:/home/nonroot/.aws/sso/cache:rw ghcr.io/imunhatep/aws-mcp-go:latest credentials-headers --profiles dev,prod"
}'
```

`$HOME` is expanded by the shell Claude Code runs the helper through. Pull the
image beforehand: Claude Code kills a helper after 10 seconds, and a first-run
pull inside that budget fails the connection. Once pulled, a helper run takes
well under a second. Only the default `--source sdk` works this way — the image
has no `aws` binary for `--source aws-cli`. The cache is mounted read-write
because the SDK refreshes the SSO token in place, and the file-ownership note
above applies to it.

**4. Check the startup log:**

```
INF [serve] credential passthrough: using only the AWS credentials each request forwards header=X-Aws-Credentials
WRN [serve] credential passthrough over plain HTTP on every interface: forwarded AWS credentials cross the network in cleartext; bind to 127.0.0.1 or put a TLS-terminating proxy in front addr=0.0.0.0:3040
INF [mcpserver.ServeHTTP] starting MCP streamable-HTTP server addr=0.0.0.0:3040 credentials_passthrough=true endpoint=/mcp
```

Four things worth knowing:

- **The plain-HTTP warning is expected here.** The image listens on `0.0.0.0`
  inside the container; publishing the port on `127.0.0.1` is what keeps it off
  the network. When the container serves other machines, put a TLS-terminating
  proxy in front, because the header carries live credentials.
- **Logins happen on the client, never in the container.** `--sso-auto-login`,
  `--sso-open-browser` and the other `--sso-*` flags have no effect, and
  `aws_sso_login` only explains what to run: the server cannot write the client's
  SSO cache. After `aws sso login` on the client, reconnect the server
  (`/mcp` → aws → *Reconnect*). Expired *role* credentials need nothing — the
  server answers with a 401 and Claude Code re-runs the helper on its own.
- **One container can serve several people**, each with their own credentials.
  The resource cache is keyed by account, not by caller, so add `--no-cache` if
  two callers with different roles in the same account must not share listings.
  Otherwise the cache can be persisted as described below.
- **`AWS_*` variables passed to the container are ignored.** `AWS_PROFILE` is
  unset at startup (with a warning) and the server's shared config files are not
  read, so a stray `-e` or mount cannot become a fallback identity.

## Static or assumed-role credentials

Credentials from the current shell, in cross-account mode:

```sh
podman run --rm -p 127.0.0.1:3040:3040 \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN -e AWS_REGION \
  ghcr.io/imunhatep/aws-mcp-go:latest serve --assume-role
```

## `credential_process` profiles do not work in the image

The image has **no shell**, so a mounted profile that resolves credentials
through `credential_process` (aws-sso-cli, aws-vault, …) fails with
`error in credential_process: exec: "sh": executable file not found` on the first
tool call that needs it. Startup warns as soon as it sees such a profile.

Converting those profiles to native `sso_session` is the durable fix. As a
stopgap, resolve them on the host and pass the result as environment variables:

```sh
# bash/zsh
eval "$(aws configure export-credentials --format env)"
```
```fish
# fish
aws configure export-credentials --format env-no-export \
  | while read -l l; set -x (string split -m1 -- = $l); end
```

Then run the container with `-e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY
-e AWS_SESSION_TOKEN` as above. These are short-lived; re-export when they
expire.

## Persisting the resource cache

By default the on-disk cache lands in the container's `/tmp` and is lost when it
exits. To keep it across restarts, mount a volume and point `MCP_CACHE_DIR` at
it — `:U` chowns the volume to the image's non-root user, which otherwise cannot
write to it:

```sh
podman run --rm -d --name aws-mcp \
  -e MCP_CACHE_DIR=/cache -e MCP_CACHE_TTL=6h \
  -v ~/.aws/config:/home/nonroot/.aws/config:ro \
  -v ~/.aws/sso/cache:/home/nonroot/.aws/sso/cache:rw \
  -v aws-mcp-cache:/cache:U \
  -p 127.0.0.1:3040:3040 ghcr.io/imunhatep/aws-mcp-go:latest \
  serve --profiles dev,prod
```

## Troubleshooting

The preflight checks and the failing tool's own error name the cause; these are
the ones specific to running in a container:

| Symptom in `podman logs` | Cause and fix |
|--------------------------|---------------|
| `has no cached SSO token (…); run: aws sso login --sso-session …` | The cache mount is missing or points at the wrong path, or no login has happened. Check `-v ~/.aws/sso/cache:/home/nonroot/.aws/sso/cache` |
| `has an expired SSO token that cannot be refreshed (…)` | The refresh token is gone too. Re-run `aws sso login` on the host, or let the server do it: with `--sso-auto-login` the failing tool result carries a verification URL and code. Either way no restart is needed |
| `sso token cache is not writable; refresh will fail` | The cache is mounted `:ro`, or uid 65532 cannot write it — mount `:rw` and add `--userns=keep-id:uid=65532,gid=65532` |
| `is not defined in ~/.aws/config` | The config mount is missing, or the profile only exists in a file that was not mounted |
| `credential_process could not be executed … no shell` | A `credential_process` profile in a distroless image — convert it to `sso_session` |
| `legacy SSO profile … cannot refresh this token` | Profile uses `sso_start_url` directly; move it under an `[sso-session]` block so refresh works |
| Tool result: `this request carries no AWS credentials: the server runs with --credentials-passthrough …` | Passthrough server, but the client was registered without a `headersHelper` — re-register with `add-json` as above, then reconnect |
| The client fails to connect at all, passthrough with a containerized helper | The helper did not finish within 10 seconds, usually an image pull on its first run. `podman pull` the image, then reconnect |
