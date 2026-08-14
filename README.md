# aws-mcp

An [MCP](https://modelcontextprotocol.io) server (streamable-HTTP transport) that
lists AWS resources across accounts and regions and queries AWS Cost Explorer,
built on top of [`awslib`](https://github.com/imunhatep/awslib)'s
resource-fetching and cost pipelines. Results are cached (default **6 hours**).

`awslib` remains a dedicated AWS library; this repository is only the MCP server
that consumes it.

## Build & run

```sh
make build
./bin/aws-mcp serve --addr :3040
```

The `serve` command listens on `--addr` and serves the MCP endpoint over the
streamable-HTTP transport at:

- `/mcp` — MCP streamable-HTTP endpoint (POST for JSON-RPC requests, GET for the SSE stream). Connect your client here.

### Container

The image compiles from the vendored dependency tree, so `make image` runs
`go mod vendor` first — the local `awslib` checkout is needed on the host, not
inside the image.

```sh
make image                  # -> ghcr.io/imunhatep/aws-mcp-go:dev
make image VERSION=v0.1.0   # -> ghcr.io/imunhatep/aws-mcp-go:v0.1.0
```

Tagged releases publish a `linux/amd64,linux/arm64` manifest to GHCR (see
[CI & releases](#ci--releases)), so a prebuilt image can be pulled instead:

```sh
podman pull ghcr.io/imunhatep/aws-mcp-go:latest
```

Run it with the host's AWS config and SSO cache mounted read-only (the image
sets `HOME=/home/nonroot`, so that path is where the credential chain looks):

```sh
podman run --rm -d \
  --name aws-mcp \
  -e AWS_PROFILE \
  -e AWS_REGION \
  -v ~/.aws:/home/nonroot/.aws:ro \
  -p 127.0.0.1:3040:3040 ghcr.io/imunhatep/aws-mcp-go:dev
```

Static or role credentials from the current shell instead, in cross-account
mode:

```sh
podman run --rm -p 127.0.0.1:3040:3040 \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN -e AWS_REGION \
  ghcr.io/imunhatep/aws-mcp-go:dev serve --assume-role
```

> The runtime image is distroless and has **no shell**, so a mounted profile
> that resolves credentials through `credential_process` fails at startup with
> `error in credential_process: exec: "sh": executable file not found`. SSO and
> static profiles read from `~/.aws` directly and work as mounted; for
> `credential_process` profiles, resolve them on the host and hand the result
> to the container as environment variables:
>
> ```sh
> # bash/zsh
> eval "$(aws configure export-credentials --format env)"
> ```
> ```fish
> # fish
> aws configure export-credentials --format env-no-export \
>   | while read -l l; set -x (string split -m1 -- = $l); end
> ```
>
> then run the container with `-e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY
> -e AWS_SESSION_TOKEN` as above. These are short-lived; re-export when they
> expire.

By default the on-disk cache lands in the container's `/tmp` and is lost when it
exits. To keep it across restarts, mount a volume and point `MCP_CACHE_DIR` at
it — `:U` chowns the volume to the image's non-root user, which otherwise cannot
write to it:

```sh
podman run --rm -d --name aws-mcp \
  -e AWS_PROFILE -e MCP_CACHE_DIR=/cache -e MCP_CACHE_TTL=6h \
  -v ~/.aws:/home/nonroot/.aws:ro \
  -v aws-mcp-cache:/cache:U \
  -p 127.0.0.1:3040:3040 ghcr.io/imunhatep/aws-mcp-go:dev
```

Multi-arch manifest (override `IMAGE` to tag for a different registry):

```sh
make image-multiarch VERSION=v0.1.0
```

### Claude Code

```sh
claude mcp add --transport http aws http://127.0.0.1:3040/mcp
```

## Commands

| Command | Description |
|---------|-------------|
| `serve` | Run the MCP server (streamable HTTP, served at `/mcp`) |
| `version` | Print version and commit |

## Authentication

The server uses the standard AWS credential chain, so it works with anything
`aws-sdk-go-v2` understands. Two modes:

### Local mode (default) — AWS profile / SSO / env

Single account, using whatever credentials the default chain resolves
(`~/.aws/config` SSO sessions, `AWS_PROFILE`, static env vars, IMDS, …):

```sh
aws sso login --profile my-sso-profile
AWS_PROFILE=my-sso-profile ./bin/aws-mcp serve
```

### Assume-role mode — cross-account

Assumes IAM roles in other accounts, chaining off the base credentials' STS.

- **Auto-discover** the assumable roles from the current IAM role's attached policies (`sts:AssumeRole` resources):

  ```sh
  ./bin/aws-mcp serve --assume-role
  ```

- **Explicit** roles (comma-separated; `accountID=roleArn` or a bare role ARN):

  ```sh
  ./bin/aws-mcp serve --assume-role-arns 'arn:aws:iam::111111111111:role/reader,arn:aws:iam::222222222222:role/reader'
  ```

  Providing `--assume-role-arns` implies assume-role mode and overrides auto-discovery.

## Flags (`serve`)

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--addr` | `MCP_ADDR` | `:3040` | Listen address (MCP served at `/mcp`) |
| `--cache-ttl` | `MCP_CACHE_TTL` | `6h` | TTL for cached resource listings (e.g. `6h`, `30m`) |
| `--cache-dir` | `MCP_CACHE_DIR` | OS temp dir | On-disk cache directory; empty = in-memory only |
| `--no-cache` | | `false` | Disable caching entirely |
| `--assume-role` | | `false` | Auto-discover assumable roles from the current IAM role |
| `--assume-role-arns` | `MCP_ASSUME_ROLE_ARNS` | | Explicit assumable role ARNs (implies assume-role mode) |
| `--verbose` / `-v` | `AWS_MCP_VERBOSE`, `LOG_LEVEL` | `3` | Log verbosity: `0`=fatal … `5`=trace (global flag) |

## Tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_resource_types` | — | Supported resource types (canonical + URL form, global flag) |
| `list_regions` | — | Known AWS regions with descriptions |
| `list_accounts` | — | Account IDs the server can reach |
| `list_resources` | `resource_type` (required), `region`, `account_id`, `view`, `state`, `tag`, `attribute`, `limit`, `cursor` | List resources of a type across accounts/regions (paginated) |
| `count_resources` | `resource_type` (required), `group_by`, `region`, `account_id`, `state`, `tag`, `attribute` | Aggregate resources into group counts |
| `get_cost_and_usage` | `period`, `start`, `end`, `granularity`, `metrics`, `group_by`, `filters`, `account_id`, `limit`, `include_periods` | Actual spend/usage, filtered and grouped |
| `get_cost_forecast` | `period`, `start`, `end`, `granularity`, `metric`, `filters`, `account_id`, `prediction_interval_level` | Forecast future spend |
| `list_cost_dimension_values` | `dimension` (required), `period`, `start`, `end`, `search`, `filters`, `context`, `account_id`, `limit` | Values a cost dimension actually took |
| `list_cost_dimensions` | — | The cost query vocabulary (dimensions, metrics, periods, filter shape) |

`resource_type` accepts either the canonical form (`AWS::EC2::Instance`) or the
URL form (`aws_ec2_instance`). When `region` is omitted, all known regions are
queried (global resource types are fetched from a single region automatically).

### `list_resources` — views, filters, pagination

The response is a paginated envelope: `{ "items": [...], "count": N, "total": M,
"next_cursor": "…" }`. `count` is the rows on this page, `total` the count after
filtering, and `next_cursor` is present only when more rows remain (pass it back
as `cursor` for the next page). `limit` defaults to 50 (max 1000).

`view` controls how much each row carries, trading size for richness:

| `view` | Fields per row |
|--------|----------------|
| `id` (default) | `account_id`, `region`, `type`, `arn`, `id`, `name`, `state`, `created_at` |
| `summary` | + `tags` and a curated `attributes` object |
| `detail` | + `raw`, the full provider-native entity (every field) |

The curated `attributes` surface the most relevant provider-native fields per
type — an EC2 instance yields `instance_type`, `instance_family`, `state`,
`private_ip`; RDS yields `engine`, `instance_class`, `status`, `endpoint`; a load
balancer yields `lb_type`, `scheme`, `dns_name`, `state`. Curated types include
EC2 (instance/volume/snapshot/vpc), RDS (instance/snapshot), ELBv2, ECS
(cluster/service), EKS, Lambda, DynamoDB, S3, Route53, Secrets Manager, EFS, SQS,
SNS, IAM users and Auto Scaling groups.

Filters narrow results server-side (so payload scales with the answer, not the
inventory): `state` (case-insensitive lifecycle match, e.g. `running`), `tag`
(`Key=Value`), and `attribute` (`key=value` against a curated attribute, e.g.
`instance_type=m5.2xlarge`).

```jsonc
// list_resources: running m5.2xlarge instances, thin view
{ "resource_type": "AWS::EC2::Instance", "region": "eu-central-1",
  "state": "running", "attribute": "instance_type=m5.2xlarge" }
```

### `count_resources` — aggregates without listing

For "how many / break down by" questions, `count_resources` returns
`{ "total": N, "group_by": [...], "buckets": [{ "group": {...}, "count": N }] }`
(sorted by descending count) instead of every row — kilobytes instead of
megabytes. `group_by` is a comma-separated list of `type`, `state`, `region`,
`account_id`, `tag:<key>` or `attr:<key>` (defaults to `state`). It accepts the
same `state`/`tag`/`attribute` filters.

```jsonc
// count_resources: running instances grouped by type
{ "resource_type": "AWS::EC2::Instance", "region": "eu-central-1",
  "state": "running", "group_by": "attr:instance_type" }
```

## Cost Explorer

`get_cost_and_usage` is the console's cost report as a tool: a time window, a
granularity, cost metrics, filter rows and up to **2** groupings. Call
`list_cost_dimensions` for the full vocabulary and `list_cost_dimension_values`
to discover the exact strings a filter needs — AWS service names (`Amazon
Elastic Compute Cloud - Compute`) rarely match what a caller would guess.

**Time window.** Either a named `period` or an explicit `start`/`end` pair
(`YYYY-MM-DD`, end exclusive). Named periods beginning `last_` cover whole
elapsed units and stop at today, so their numbers are stable; `this_month`
(`mtd`), `this_year` (`ytd`) and `today` run through today, whose costs are still
partial.

| | |
|---|---|
| Days | `today`, `yesterday`, `last_7_days`, `last_30_days`, `last_<n>_days` |
| Months | `this_month` / `mtd`, `last_month`, `last_3_months`, `last_<n>_months` |
| Years | `this_year` / `ytd`, `last_year` |
| Forecast | `this_month`, `next_month`, `next_<n>_days`, `next_<n>_months` |

`granularity` defaults to `MONTHLY` (`DAILY`, `HOURLY`); `metrics` defaults to
`UnblendedCost` (also `AmortizedCost`, `BlendedCost`, `NetUnblendedCost`,
`NetAmortizedCost`, `UsageQuantity`, `NormalizedUsageAmount`). The first metric
ranks the groups.

**Grouping.** `group_by` takes up to two entries: a dimension (`SERVICE`,
`LINKED_ACCOUNT`, `REGION`, `INSTANCE_TYPE`, `USAGE_TYPE`, `RECORD_TYPE`,
`PURCHASE_TYPE`, `OPERATION`, …, case-insensitive with aliases like `account`
and `charge_type`), `TAG:<key>` or `COST_CATEGORY:<key>`. Grouped tag values come
back unwrapped from the API's `<key>$<value>` encoding, with untagged spend
labelled `(not set)`.

**Filtering.** `filters` is a list of rows, AND-ed together, with the values
inside a row OR-ed — the console's filter panel:

```jsonc
{ "type": "dimension" | "tag" | "cost_category",  // optional; inferred from key
  "key": "SERVICE",                               // dimension, tag key or category
  "values": ["…"],
  "match_options": ["EQUALS"],                    // EQUALS / CASE_SENSITIVE, ABSENT on tags
  "exclude": false,                               // the console's "Exclude" toggle
  "absent": false,                                // key not carried at all
  "present": false }                              // key carried, any value
```

**Response.** `groups` aggregates the whole window ranked by spend (for "what
costs the most"), `periods` splits the same data by time (for trends), and
`total` sums it. `limit` caps the group list (default 25) with `group_count` /
`groups_truncated` reporting what was dropped; `include_periods` adds the
per-period group breakdown, which is off by default to keep responses small.

```jsonc
// Top RDS spenders by team tag last month, excluding credits and refunds
{ "period": "last_month", "group_by": ["TAG:Team"], "limit": 10,
  "filters": [{ "key": "SERVICE", "values": ["Amazon Relational Database Service"] },
              { "key": "RECORD_TYPE", "values": ["Credit", "Refund"], "exclude": true }] }

// Daily EC2 spend per region over the last 30 days, with the per-period breakdown
{ "period": "last_30_days", "granularity": "DAILY", "include_periods": true,
  "group_by": ["SERVICE", "REGION"] }
```

By default every reachable account is queried and the results summed, with each
group attributed by `account_id` and a `note` on the response. If the pool holds
both a management (payer) account and its members their costs overlap — pass
`account_id` to scope to one, or `group_by: ["LINKED_ACCOUNT"]` to break an
organization's spend down from the payer.

> Cost Explorer bills **$0.01 per request**. Arguments are validated before any
> call is made, and answers are cached like resource listings.

## Example MCP client config

```json
{
  "mcpServers": {
    "aws": {
      "url": "http://localhost:3040/mcp"
    }
  }
}
```

## Notes

- On startup `serve` resolves the base credentials' STS caller identity and logs
  the account, ARN, user ID and region. If the credential chain is missing,
  expired or invalid, startup aborts immediately with a clear error rather than
  failing on the first tool call. In assume-role mode this reports the base
  principal that role assumption chains off of.
- Requires **Go 1.25+** (a floor introduced by the `github.com/mark3labs/mcp-go` dependency).
- On the first `list_resources` call without a `region`, the server initializes
  a client per region (one STS `GetCallerIdentity` each), which is slower; clients
  and results are cached afterwards. Pass a specific `region` for fast, targeted queries.

## Development

This module consumes `awslib` as a normal tagged dependency
(`github.com/imunhatep/awslib v0.5.0`), resolved from the module proxy — no
local checkout or `replace` directive is required to build. To test an unreleased
`awslib` change, add a `replace` locally (`go mod edit -replace
github.com/imunhatep/awslib=../pkgs/awslib`) and drop it again before committing.

Dependencies are vendored — run `make tidy` (`go mod tidy && go mod vendor`)
after any dependency change, or the build fails with "inconsistent vendoring".
The container build reads `vendor/` too.

| Target | What it does |
|---|---|
| `make build` | Build `bin/aws-mcp` with version/commit ldflags |
| `make test` | `go test ./...` |
| `make tidy` | `go mod tidy && go mod vendor` |
| `make run` | Build, then `serve` |
| `make image` | Vendor, then build `$(IMAGE):$(VERSION)` (defaults to `ghcr.io/imunhatep/aws-mcp-go:dev`) with podman |
| `make image-multiarch` | Same as a `linux/amd64,linux/arm64` manifest |

### CI & releases

Two GitHub Actions workflows live in `.github/workflows`:

| Workflow | Trigger | What it produces |
|---|---|---|
| `ci.yml` | push to `master`/`main`, PRs, manual | gofmt/vet gates, `go test -race`, `make build`, and a container build (not pushed) |
| `release.yml` | push of a `v*` tag | `darwin,linux` × `amd64,arm64` binary tarballs + `checksums.txt` on a GitHub release, and a multi-arch image pushed to `ghcr.io/imunhatep/aws-mcp-go` |

Release image tags come from `docker/metadata-action`: the full version, `major.minor`,
`major`, plus `latest` for tags without a prerelease suffix.

Both workflows build the committed `go.mod` as-is, so `awslib` has to be
**tagged and pushed** — and the new version committed in `require` — before a
release tag here can build against it. The image jobs run `go mod vendor` first,
because `vendor/` is gitignored but the `Containerfile` builds with `-mod=vendor`.

- **zerolog** for logging
- **urfave/cli v3** for the CLI
- **`pkg/errors`** (this module's own dependency-free package) for error wrapping
