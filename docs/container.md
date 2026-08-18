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
sso_role_name = prodops-readonly
region = eu-central-1

[profile prod]
sso_session = my-session
sso_account_id = 222222222222
sso_role_name = prodops-readonly
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

**4. Check the startup log.** Each profile is resolved and its token lifetime
reported before the server listens:

```sh
podman logs aws-mcp
```
```
INF [serve] using aws shared-config profiles profiles=["dev","prod"]
INF [mcpserver.PreflightProfile] sso token cached profile=dev sso_session=my-session token_valid_for=7h58m0s
INF [ProfilePool] profile resolved profile=dev account=111111111111 arn=arn:aws:sts::111111111111:assumed-role/…
INF [ProfilePool] profile resolved profile=prod account=222222222222 arn=arn:aws:sts::222222222222:assumed-role/…
INF [mcpserver.ServeHTTP] starting MCP streamable-HTTP server addr=0.0.0.0:3040 endpoint=/mcp
```

Then `list_accounts` returns both account IDs, and every resource and cost tool
spans them.

Four things worth knowing:

- **Mount the token cache read-write.** On refresh the SDK writes a temp file
  into the cache directory and renames it over the token, so a `:ro` mount works
  until the token expires and then fails every call. Startup warns when the
  directory is not writable.
- **Mount `config` and `sso/cache` separately**, not all of `~/.aws`, so the
  container never sees `~/.aws/credentials`.
- **File ownership.** The image runs as uid 65532; if that user cannot write the
  cache mount, add `--userns=keep-id:uid=65532,gid=65532` (or, locally,
  `--user 0`).
- **Re-login needs no restart.** When the SSO session finally expires, run
  `aws sso login --sso-session my-session` on the host again — the running
  container re-reads the token file on its next refresh and carries on.

Profiles spread across *different* `sso_session` blocks work too; each session
needs its own `aws sso login`, and all their tokens live in the same mounted
cache directory.

Single-account SSO is the same command without `--profiles` and with
`-e AWS_PROFILE=dev`.

## Static or assumed-role credentials

Credentials from the current shell, in cross-account mode:

```sh
podman run --rm -p 127.0.0.1:3040:3040 \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN -e AWS_REGION \
  ghcr.io/imunhatep/aws-mcp-go:latest serve --assume-role
```

## `credential_process` profiles do not work in the image

The image has **no shell**, so a mounted profile that resolves credentials
through `credential_process` (aws-sso-cli, aws-vault, …) fails at startup with
`error in credential_process: exec: "sh": executable file not found`. Startup
warns as soon as it sees such a profile.

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

The startup checks name the cause; these are the ones specific to running in a
container:

| Symptom in `podman logs` | Cause and fix |
|--------------------------|---------------|
| `has no cached SSO token (…); run: aws sso login --sso-session …` | The cache mount is missing or points at the wrong path, or no login has happened. Check `-v ~/.aws/sso/cache:/home/nonroot/.aws/sso/cache` |
| `has an expired SSO token (expired …)` | Re-run `aws sso login` on the host; no need to rebuild or restart anything else |
| `sso token cache is not writable; refresh will fail` | The cache is mounted `:ro`, or uid 65532 cannot write it — mount `:rw` and add `--userns=keep-id:uid=65532,gid=65532` |
| `is not defined in ~/.aws/config` | The config mount is missing, or the profile only exists in a file that was not mounted |
| `credential_process could not be executed … no shell` | A `credential_process` profile in a distroless image — convert it to `sso_session` |
| `legacy SSO profile … cannot refresh this token` | Profile uses `sso_start_url` directly; move it under an `[sso-session]` block so refresh works |
