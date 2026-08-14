# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

An MCP server (streamable-HTTP transport, served at `/mcp`) that lists AWS resources across accounts and regions and queries AWS Cost Explorer. It is a thin server layer over [`awslib`](https://github.com/imunhatep/awslib), which does the actual AWS resource fetching and cost querying. This repo owns the MCP wiring, CLI, credential/assume-role setup, and caching policy — not the per-service AWS logic.

## Commands

```sh
go build -o aws-mcp ./cmd/aws-mcp        # build
./aws-mcp serve                          # run the streamable-HTTP server (default addr :3040, MCP at /mcp)
./aws-mcp version                        # print version + commit

go test ./...                            # all tests
go test ./internal/mcpserver -run TestResolveResourceType   # single test
go mod tidy                              # after changing dependencies
```

Version/commit are injected at build time via `-ldflags` into `internal/version`.

### awslib dependency

`awslib` is a plain tagged dependency (`github.com/imunhatep/awslib v0.5.0` in `require`), resolved from the module proxy — there is **no `replace` directive** and no local checkout is needed to build. To try an unreleased `awslib` change, add one temporarily (`go mod edit -replace github.com/imunhatep/awslib=../pkgs/awslib && go mod tidy && go mod vendor`) and drop it before committing; CI builds the committed `go.mod` as-is and a stray `replace` would fail there.

The repo vendors its dependencies (`vendor/`), so run `go mod tidy && go mod vendor` after any dependency change or the build fails with "inconsistent vendoring". `vendor/` is gitignored — it is a local build input, not committed state.

### CI / releases (`.github/workflows`)

`ci.yml` (push to `master`/`main`, PRs, manual) runs a gofmt check over `cmd internal pkg`, `go vet`, `go test -race`, `make build`, and a container build that is not pushed. `release.yml` (on `v*` tags) builds the `darwin,linux` × `amd64,arm64` tarballs plus `checksums.txt` for a GitHub release, and pushes a `linux/amd64,linux/arm64` image to `ghcr.io/${{ github.repository }}` with `GITHUB_TOKEN` (semver + `latest` tags via `docker/metadata-action`).

Two things to know about both workflows:

- **They build the committed `go.mod` verbatim** — no `go mod edit`/`go mod tidy` fixups on the runner, so the module graph CI resolves is the one in the tree. Consequence: an awslib change must be tagged, published, *and* bumped in `require` here before a release tag will pick it up.
- **The `Containerfile` builds with `-mod=vendor`** and `vendor/` is gitignored, so the image jobs run `go mod vendor` before `docker/build-push-action`. `.dockerignore` deliberately does *not* exclude `vendor/`.

## Architecture

Request flow, outermost to innermost:

- `cmd/aws-mcp/main.go` → `internal.NewApp()` (root `urfave/cli/v3` command, global `--verbose`/`zerolog` setup) → attaches `command.ServeCommand` and `command.VersionCommand`.
- `internal/command/serve.go` is the composition root for `serve`: parses flags into `internal/config.Config`, builds the client pool, builds the cache, then `mcpserver.NewServer(...)` + `ServeHTTP` (streamable-HTTP transport, endpoint `/mcp`).
- `internal/mcpserver` is the core. `Server` holds a `ClientPool`, an optional `*cache.DataCache`, and the `mcp-go` server. `registerTools()` (in `tools.go`) declares the resource tools (`list_resource_types`, `list_regions`, `list_accounts`, `list_resources`, `count_resources`) then calls `registerCostTools()` (in `cost_tools.go`) for the Cost Explorer tools; handlers translate MCP calls into awslib pipeline runs. Query mechanics (fetch, filter, paginate, aggregate) live in `query.go`, the cost equivalents in `cost.go`.

### The ClientPool abstraction (why two auth modes are transparent)

`mcpserver.ClientPool` (server.go) is a **local interface** with just `GetClients` + `ListAccountIDs`. Two awslib implementations satisfy it, and `buildClientPool` in `serve.go` picks one:

- **Local mode** (default) → `provider.NewClientPool` — single account from the default AWS credential chain (SSO, `AWS_PROFILE`, env, IMDS).
- **Assume-role mode** (`--assume-role` or `--assume-role-arns`) → `v3.NewClientPool` — cross-account, either from explicit ARNs (`parseRoleArns`, accepts `accountID=roleArn` or a bare ARN) or auto-discovered from the current IAM role's policies.

The rest of the server never branches on auth mode — it only sees the interface.

### list_resources / count_resources data path

Both handlers share `Server.fetchResources` (`query.go`): resolve regions (one, or all via `ptypes.GetAwsRegionList()`) → `pool.GetClients(regions...)` → `proxy.NewRepoProxyPool(ctx, clients).WithCache(dc)` → `resources.NewProvider(rt, proxyPool.List(rt)...).Run().Read()`. The awslib proxy/provider does the parallel fan-out and caching; this repo drives it, then filters/projects/paginates/aggregates the resulting `[]service.ResourceInterface` in-memory.

`handleListResources`: parse `view` + `resourceFilter` + cursor/limit → for each resource compute `summaryAttributes` once, apply `filter.matches`, build a `resourceDTO` at the requested view → `paginate` (stable sort by arn/id/name, offset/limit) → return a `listResult` envelope `{items,count,total,next_cursor}`.

`handleCountResources`: parse `group_by` dims + filter → `aggregate` groups the (filtered) resources into `countBucket`s sorted by descending count → `countResult` envelope. This is the cheap path for "how many / break down by" questions — it never serializes rows.

**Views** (`resourceDTO`): `viewID` (default, thin — identity + `State` lifted from attributes) → `viewSummary` (+ tags + curated `attributes`) → `viewDetail` (+ `raw`). `summaryAttributes` (`attributes.go`) type-switches on the concrete awslib entity (values, not pointers — `proxy` boxes them via `cast[T]`) and pulls the most relevant fields for common types (EC2, RDS, ELB, ECS, EKS, Lambda, DynamoDB, S3, Route53, Secrets Manager, EFS, SQS, SNS, IAM, ASG); unrecognized types get a nil map. `viewDetail` sets `raw` to `json.Marshal(r)` — since each entity embeds the raw AWS SDK struct (`ec2.Instance` embeds `types.Instance`), this yields every field generically, with no per-service code.

Notes:
- Some SDK structs and `AbstractResource` both declare a `Type` field, so a bare `e.Type` in the switch is ambiguous — qualify it (`e.LoadBalancer.Type`, `e.ResourceRecordSet.Type`).
- `state`/`tag`/`attribute` filters and `state`/`attr:`/`tag:` group-by dims all read through the curated attributes (`resourceState`, `dimValue`), so adding an entity to `summaryAttributes` automatically makes it filterable/groupable.

### Cost Explorer data path

Cost Explorer is a billing API, not an inventory one, so it bypasses the resource proxy/provider pipeline entirely. `Server.costRepositories` (`cost.go`) asks the pool for clients in `ptypes.DefaultAwsRegion` only (Cost Explorer is global, served from us-east-1 — the client's region decides only which credentials sign) and wraps each in `costexplorer.NewCostExplorerRepository(...)`, plus `.WithCache(dc)` when caching is on. The local `costRepository` interface is what the handlers see, so the plain and cached repositories are interchangeable.

**Multi-account.** There is no cross-account fan-out primitive here: each account's repository is queried in turn and the results merged in `costAccumulator`. When more than one account answered, every group carries an extra `account_id` key and the response gets a `note` — a pool holding both a payer and its members double counts, and the note says so.

**Argument parsing** (`cost.go`) is deliberately forgiving, since the caller is a language model: periods are named (`last_30_days`, `mtd`, and the generic `last_<n>_days` / `last_<n>_months` via `relativePeriodPattern`) or explicit dates; dimensions accept any casing plus the `costDimensionAliases` (`account` → `LINKED_ACCOUNT`); metrics fold across spellings (`foldMetricName`); `filters` decodes from a JSON array, a JSON string or a single unwrapped object.

**Validation happens before AWS is touched.** Cost Explorer bills per request, so `handleGetCostAndUsage` parses everything and calls `CostQuery.Validate()` (awslib's — it catches the match options `GetCostAndUsage` rejects, and the 2-grouping cap) *before* `costRepositories`, so a bad query is not sent once per account. `TestCostToolsValidateBeforeCallingAWS` pins this with a nil ClientPool that would panic if a handler reached for it.

**Result shaping.** `buildCostResult` returns groups aggregated over the whole window (ranked by the first metric, truncated to `limit`) *and* the same data split by period. Two details worth keeping: with a `group_by` set the API leaves `ResultByTime.Total` empty, so totals are summed from the groups instead; and tag/cost-category group keys come back as `<key>$<value>`, which `costAccumulator.groupKeys` unwraps (empty value → `(not set)`).

### Resource-type registry — keep in sync with awslib

`SupportedResourceTypes()` in `resource.go` is a hand-maintained allowlist that **must mirror awslib's `proxy.RepoProxy.FindAll` dispatch switch**. Types in awslib's registry but not wired into `FindAll` (e.g. Athena, CloudTrail) are deliberately omitted so callers never hit a "resource type not supported" error. When awslib adds/removes a `FindAll` case, update this list. `ResolveResourceType` accepts both canonical (`AWS::EC2::Instance`, case-insensitive) and URL (`aws_ec2_instance`) forms.

### Cache

`cache.go`: `NewCache` builds an awslib `DataCache` with a bigcache in-memory handler plus, when `--cache-dir` is set, a file handler (both share the TTL, default 6h). `--no-cache` makes `buildCache` return `nil`, and `NewServer`/`WithCache` both accept nil to run cache-free.

## Conventions

- **Errors:** use this module's own `pkg/errors` (dependency-free, adds stack traces via `WithStack`/`Errorf`), not the stdlib `errors` or `pkg/errors` upstream.
- **Logging:** `zerolog`, message prefix convention `[pkg.Func] ...`.
- **Tools return errors as results:** handlers return `mcp.NewToolResultErrorFromErr/Errorf(...)` with a `nil` Go error for user-facing failures (bad input, AWS init failure) — reserve a non-nil Go error for transport-level faults.
- Go 1.26 toolchain (`go.mod`); the README notes a hard Go 1.25+ floor from the `mcp-go` dependency.

## Testing

`internal/mcpserver/server_test.go` spins up the server behind an in-process streamable-HTTP test transport (`server.NewTestStreamableHTTPServer`) and drives it with a real `mcp-go` client. Tests that only exercise AWS-independent tools (`list_resource_types`, `list_regions`, `list_cost_dimensions`) pass a `nil` ClientPool — follow that pattern to test tool wiring without AWS credentials. Argument-rejection tests use the same nil pool as a tripwire: if validation ever moved after the pool access the test would panic instead of failing.

`cost_test.go` covers the cost helpers against a fixed clock (`now`, a mid-month date) so relative periods do not drift with the calendar, and feeds synthetic `CostAndUsage` values through `buildCostResult` rather than mocking AWS.
