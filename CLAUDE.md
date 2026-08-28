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

`awslib` is a plain tagged dependency (`github.com/imunhatep/awslib v0.9.0` in `require`), resolved from the module proxy — there is **no `replace` directive** and no local checkout is needed to build. To try an unreleased `awslib` change, add one temporarily (`go mod edit -replace github.com/imunhatep/awslib=../pkgs/awslib && go mod tidy && go mod vendor`) and drop it before committing; CI builds the committed `go.mod` as-is and a stray `replace` would fail there.

The repo vendors its dependencies (`vendor/`), so run `go mod tidy && go mod vendor` after any dependency change or the build fails with "inconsistent vendoring". `vendor/` is gitignored — it is a local build input, not committed state.

### CI / releases (`.github/workflows`)

`ci.yml` (push to `master`/`main`, PRs, manual) runs a gofmt check over `cmd internal pkg`, `go vet`, `go test -race`, `make build`, and a container build that is not pushed. `release.yml` (on `v*` tags) builds the `darwin,linux` × `amd64,arm64` tarballs plus `checksums.txt` for a GitHub release, and pushes a `linux/amd64,linux/arm64` image to `ghcr.io/${{ github.repository }}` with `GITHUB_TOKEN` (semver + `latest` tags via `docker/metadata-action`).

Two things to know about both workflows:

- **They build the committed `go.mod` verbatim** — no `go mod edit`/`go mod tidy` fixups on the runner, so the module graph CI resolves is the one in the tree. Consequence: an awslib change must be tagged, published, *and* bumped in `require` here before a release tag will pick it up.
- **The `Containerfile` builds with `-mod=vendor`** and `vendor/` is gitignored, so the image jobs run `go mod vendor` before `docker/build-push-action`. `.dockerignore` deliberately does *not* exclude `vendor/`.

## Architecture

Request flow, outermost to innermost:

- `cmd/aws-mcp/main.go` → `internal.NewApp()` (root `urfave/cli/v3` command, global `--verbose`/`zerolog` setup) → attaches `command.ServeCommand` and `command.VersionCommand`.
- `internal/command/serve.go` is the composition root for `serve`: parses flags into `internal/config.Config`, builds the SSO login manager, builds the client pool (lazily — see below), builds the cache, then `mcpserver.NewServer(...).WithAuth(...)` + `ServeHTTP` (streamable-HTTP transport, endpoint `/mcp`).
- `internal/mcpserver` is the core. `Server` holds a `ClientPool`, an optional `*cache.DataCache`, and the `mcp-go` server. `registerTools()` (in `tools.go`) declares the resource tools (`list_resource_types`, `list_regions`, `list_accounts`, `list_resources`, `count_resources`) then calls `registerCostTools()` (in `cost_tools.go`) for the Cost Explorer tools, `registerAuthTools()` (in `auth_tools.go`) for `aws_auth_status` / `aws_sso_login`, and `registerSavingsPlansTools()` (in `savingsplans_tools.go`) for the Savings Plans tools; handlers translate MCP calls into awslib pipeline runs. Query mechanics (fetch, filter, paginate, aggregate) live in `query.go`, the cost equivalents in `cost.go` and the Savings Plans ones in `savingsplans.go`.

### The ClientPool abstraction (why the auth modes are transparent)

`mcpserver.ClientPool` (server.go) is a **local interface** with just `GetClients` + `ListAccountIDs`. Three implementations satisfy it, and `buildClientPool` in `serve.go` picks one:

- **Local mode** (default) → `provider.NewClientPool` — single account from the default AWS credential chain (SSO, `AWS_PROFILE`, env, IMDS), wrapped in `mcpserver.LazyPool`.
- **Multi-profile mode** (`--profiles`) → `mcpserver.NewProfilePool` (`profilepool.go`, this repo's own implementation) — one account per named AWS shared-config profile.
- **Assume-role mode** (`--assume-role` or `--assume-role-arns`) → `v3.NewClientPool` — cross-account, either from explicit ARNs (`parseRoleArns`, accepts `accountID=roleArn` or a bare ARN) or auto-discovered from the current IAM role's policies. Also wrapped in `LazyPool`, since discovery itself needs working credentials.

The modes are mutually exclusive — `config.Validate` rejects `--profiles` combined with either assume-role flag. Only the local and assume-role branches build the default `v3.ClientBuilder` and run `logCallerIdentity`; profile mode never touches the default credential chain.

**`ProfilePool` specifics.** One `v3.ClientBuilder` + `provider.ClientPool` per profile. It deliberately does *not* use `v3.DefaultAwsClientProviders`: that helper folds ambient `AWS_ACCESS_KEY_ID` / `AWS_PROFILE` env credentials into the config, and a static credentials provider beats `WithSharedConfigProfile`, which would collapse every profile onto the same identity. So it builds the retry options plus `WithSharedConfigProfile` itself. Invariants worth keeping:

- **Preflight before AWS** — `PreflightProfile` (`credcheck.go`) parses the profile's shared config first, so an undefined profile, a missing/unrefreshable/malformed SSO token or an unrunnable `credential_process` is reported with an actionable message before a client is built.
- **Lazy identity, lazy regions** — `NewProfilePool` makes no AWS call at all. The account ID comes from `sso_account_id` in the shared config, so `ListAccountIDs` and account scoping answer for free; `resolve` does one STS `GetCallerIdentity` per profile on first use, and region clients are created on demand by the inner pools. A failed profile keeps its error for `profileRetryInterval` (5s) and is then retried, which is what makes a re-login take effect without a restart.
- **Partial failure is not failure** — one unusable profile does not fail a query across the others, but if *no* profile authenticates, or every client build fails, `GetClients` errors instead of returning an empty slice. `TestProfilePoolGetClientsErrorsWhenEveryProfileFails` pins this: "nobody is logged in" must not render as "no resources".
- **Dedupe by `(accountID, region)`** in `GetClients` — two profiles can point at the same account, and `fetchResources` fans out over every client without deduplicating, so duplicates would double rows and inflate counts.

The rest of the server never branches on auth mode — it only sees the interface.

**Account scoping is a client-selection concern, not a filter.** `ClientPool` has
`GetAccountClients(accountID, regions...)` alongside `GetClients(regions...)`, and
`Server.poolClients` (`server.go`) picks between them from the `account_id`
argument. This has to happen before any client exists: building one assumes the
target account's role (`v3.ClientPool`) or exercises that profile's credentials
(`ProfilePool`), so an `account_id` applied to the resulting *rows* would already
have issued API calls against every other account and merely hidden their output.
On a pool spanning a development and a production account that difference is the
entire value of the argument. Two rules follow:

- **An unreachable account is an error, never an empty result** — in all three
  pools. Answering "no resources" for an account the server cannot even reach is
  the silent-wrong-answer failure this codebase keeps having to design against.
- **Every new fetch path must go through `poolClients`**, not `s.pool` directly.
  `TestFetchPathsScopeByAccount` pins both existing paths with a spy pool that
  fails if the unscoped method is reached.

### list_resources / count_resources data path

Both handlers share `Server.fetchResources` (`query.go`): resolve regions (one, or every enabled one — see *Region narrowing* below) → `pool.GetClients(regions...)` → `proxy.NewRepoProxyPool(ctx, clients).WithCache(dc)` → `resources.NewProvider(rt, proxyPool.List(rt)...).Run()`, then `Read()` for the rows and `Failures()` for the proxies that could not be queried. The awslib proxy/provider does the parallel fan-out and caching; this repo drives it, then filters/projects/paginates/aggregates the resulting `[]service.ResourceInterface` in-memory.

`handleListResources`: parse `view` + `resourceFilter` + cursor/limit → for each resource compute `summaryAttributes` once, apply `filter.matches`, build a `resourceDTO` at the requested view → `paginate` (stable sort by arn/id/name, offset/limit) → return a `listResult` envelope `{items,count,total,next_cursor,queried,warnings}`.

`handleCountResources`: parse `group_by` dims + filter → `aggregate` groups the (filtered) resources into `countBucket`s sorted by descending count → `countResult` envelope, with the same `queried`/`warnings`. This is the cheap path for "how many / break down by" questions — it never serializes rows.

**Both envelopes carry `queried` and `warnings`, and that is not decoration.** A `(account, region)` pair that errored or timed out contributes no rows, so without the qualification "this account holds none" and "we could not look" are the same answer — the silent-wrong-answer failure this codebase keeps designing against. `typedFetch.warnings()` names the unreachable pairs *with their reason* (`describeFailures`, shared with the fallback path in `query.go`), because "not enabled for this account" and "timed out" call for different responses. A count needs it most: a single number carries no hint that a region is missing. Any new fetch path must plumb `reader.Failures()` through — reading only `Read()` is exactly the bug this replaced.

**Views** (`resourceDTO`): `viewID` (default, thin — identity + `State` lifted from attributes) → `viewSummary` (+ tags + curated `attributes`) → `viewDetail` (+ `raw`). `summaryAttributes` (`attributes.go`) type-switches on the concrete awslib entity (values, not pointers — `proxy` boxes them via `cast[T]`) and pulls the most relevant fields for common types (EC2, RDS, ELB, ECS, EKS, Lambda, DynamoDB, S3, Route53, Secrets Manager, EFS, SQS, SNS, IAM, ASG, CloudFront); unrecognized types get a nil map. `viewDetail` sets `raw` to `json.Marshal(r)` — since each entity embeds the raw AWS SDK struct (`ec2.Instance` embeds `types.Instance`), this yields every field generically, with no per-service code.

Notes:
- Some SDK structs and `AbstractResource` both declare a `Type` field, so a bare `e.Type` in the switch is ambiguous — qualify it (`e.LoadBalancer.Type`, `e.ResourceRecordSet.Type`).
- `state`/`tag`/`attribute` filters and `state`/`attr:`/`tag:` group-by dims all read through the curated attributes (`resourceState`, `dimValue`), so adding an entity to `summaryAttributes` automatically makes it filterable/groupable.
- The `default` branch of the `summaryAttributes` switch is not just `return nil`: a resource that implements `attributeProvider` (`GetAttributes() map[string]any` — awslib's Cloud Control entities do) gets its own property bag adapted by `genericAttributes`. Combined with the point above, that one case is what gives the fallback tool the state/tag/attribute filters and every group-by dimension for free.

### list_resources_fallback (`fallback.go`)

The last-resort lister for types with no awslib repository, answered from the **Cloud Control API** rather than AWS Config. Config was evaluated and rejected: it needs a Configuration Recorder enabled per account and region (absent ⇒ an empty result, not an error), bills per configuration item whether or not anyone queries, would be a net-new SDK dependency (only `configservice/types` is vendored, for the `ResourceType` constants), and does not even model most of the types awslib had to hand-write. Cloud Control needs no enablement, costs nothing extra, returns the full property bag, and was already vendored.

`handleListResourcesFallback` reuses the typed path wholesale: `ResolveFallbackResourceType` → `fetchFallbackResources` → `dedupeFallback` → the same `summaryAttributes`/`filter.matches`/`buildResourceDTO`/`paginate` chain. The only new machinery is `proxy.NewGenericRepoProxyPool` in awslib, whose `GenericRepoProxy` satisfies `RepoProxyInterface`, so `resources.NewProvider` fans out and caches exactly as it does for `list_resources`.

Four things worth keeping:

- **Type resolution is deliberately looser than `ResolveResourceType`** and must stay that way — being limited to the allowlist is the thing this tool exists to escape. It canonicalizes via the URL form and a case-insensitive sweep of the ~530-entry Config vocabulary, then **passes an unknown but well-formed name through verbatim**: Cloud Control's registry is larger than that vocabulary and its `TypeName` is case-sensitive, so there is nothing to canonicalize against. `resourceTypePattern` is only a shape check, so a malformed argument costs no API calls.
- **`dedupeFallback` is not optional.** `RepoProxyPool.List` collapses global types to one proxy, but it drives that off `cfg.ResourceTypeListGlobal()` — a curated list that by definition cannot cover an arbitrary fallback type. Without dedup, a global type asked for across all regions returns every row once per region and inflates counts. The key is the ARN when there is one (a global resource's ARN has an empty region segment, so it is identical from every region), else account+region+id, which never merges genuinely distinct resources.
- **`detailed` is tied to `view=detail`**, not exposed as its own argument, because it costs one `GetResource` per resource — some types' LIST handler returns identifiers only (S3 buckets), others return full properties (EC2 instances).
- **The envelope carries `queried` and `warnings`.** A generic backend has more ways to answer incompletely — a missing per-account IAM grant for the underlying service means that pair contributes no rows — so an under-reported list would otherwise be indistinguishable from a short one. `queried` tells "asked 12 accounts, found nothing" from "asked 1", and the per-proxy errors themselves come from `reader.Failures()` (`resources.ProxyFailure`). The typed path now carries the same fields; `queryScope` and `describeFailures` live in `query.go` and are shared by both.

### Cost Explorer data path

Cost Explorer is a billing API, not an inventory one, so it bypasses the resource proxy/provider pipeline entirely. `Server.costRepositories` (`cost.go`) asks the pool for clients in `ptypes.DefaultAwsRegion` only (Cost Explorer is global, served from us-east-1 — the client's region decides only which credentials sign) and wraps each in `costexplorer.NewCostExplorerRepository(...)`, plus `.WithCache(dc)` when caching is on. The local `costRepository` interface is what the handlers see, so the plain and cached repositories are interchangeable.

**Multi-account.** There is no cross-account fan-out primitive here: each account's repository is queried in turn and the results merged in `costAccumulator`. When more than one account answered, every group carries an extra `account_id` key and the response gets a `note` — a pool holding both a payer and its members double counts, and the note says so.

**Argument parsing** (`cost.go`) is deliberately forgiving, since the caller is a language model: periods are named (`last_30_days`, `mtd`, and the generic `last_<n>_days` / `last_<n>_months` via `relativePeriodPattern`) or explicit dates; dimensions accept any casing plus the `costDimensionAliases` (`account` → `LINKED_ACCOUNT`); metrics fold across spellings (`foldMetricName`); `filters` decodes from a JSON array, a JSON string or a single unwrapped object.

**Validation happens before AWS is touched.** Cost Explorer bills per request, so `handleGetCostAndUsage` parses everything and calls `CostQuery.Validate()` (awslib's — it catches the match options `GetCostAndUsage` rejects, and the 2-grouping cap) *before* `costRepositories`, so a bad query is not sent once per account. `TestCostToolsValidateBeforeCallingAWS` pins this with a nil ClientPool that would panic if a handler reached for it.

**Result shaping.** `buildCostResult` returns groups aggregated over the whole window (ranked by the first metric, truncated to `limit`) *and* the same data split by period. Two details worth keeping: with a `group_by` set the API leaves `ResultByTime.Total` empty, so totals are summed from the groups instead; and tag/cost-category group keys come back as `<key>$<value>`, which `costAccumulator.groupKeys` unwraps (empty value → `(not set)`).

### Savings Plans data path (`savingsplans.go`, `savingsplans_tools.go`)

`list_savings_plans` (inventory) and `list_savings_plan_rates` (offering rates) come from awslib's `service/savingsplans`. Neither is a resource — a purchased plan is a contract, a rate is a price, and nothing in that package implements `service.ResourceInterface` or is reachable through `FindAll` — so, like Cost Explorer, they bypass the proxy/provider pipeline entirely. Upstream's own notes are in awslib's `docs/savingsplans.md`.

The API is **partition-global** (`savingsplans.amazonaws.com`): the client's region decides only which credentials sign. Everything else follows from that:

- **`savingsPlansRepositories(accountID, cached)`** asks `poolClients` — not `s.pool` — for `DefaultAwsRegion` clients only, one per account. Going through `poolClients` is the rule from the account-scoping section above, and `TestSavingsPlansScopeByAccount` pins it. (The older `costRepositories` still filters after `GetClients`; new fetch paths do not.)
- **Inventory fans out, rates do not.** Plans differ per account so every account is queried and each row carries its `account_id`; rates are public prices identical whichever account asks, so exactly one repository is used — fanning out would return the same price list N times.
- **Inventory is deliberately uncached** (`cached=false`). It answers "what are we committed to right now", and the 6h resource-cache window answers a different question. Rates are cached.
- **`region` is a filter, never an endpoint.** For inventory it matches the plan's own `Region`, which only EC2 Instance plans carry — so a region filter excludes Compute plans, and that is correct, not a bug. For rates it goes into `OfferingRatesQuery.Region`.

Four things worth keeping:

- **`state` defaults to `active`.** `DescribeSavingsPlans` keeps returning retired, queued and payment-failed plans, so an unfiltered inventory overstates coverage — the one filter AWS does apply is the one that matters most.
- **`region` + (`instance_type` | `instance_family`) are required for rates**, checked before any AWS call. awslib deliberately offers no unfiltered `ListOfferingRatesAll` because the price list is thousands of pages; this is that guard at the tool boundary.
- **`product` resolves the product type and the rate service code together** (`spProducts`). The API takes them separately and a mismatched pair returns *nothing* rather than erroring. Note Fargate rates are published under `AmazonECS`, with `AmazonEKS` as a separate `fargate-eks` lookup.
- **Lookup-table keys must be pre-normalized.** `normalizeSPToken` strips spaces, hyphens and underscores, so a key spelled `"ec2-instance"` in `planTypeAliases` is unreachable — a bug a map hides rather than reports. `spProducts` keeps readable keys (they double as the error message's vocabulary) and is therefore matched by normalizing both sides, not by direct indexing.

The summary in the inventory envelope sums per currency and never across, counts unparseable amounts in `amounts_unparsed` instead of treating them as zero, and covers every matching plan rather than the truncated page — a response that caps rows must still state the real commitment.

### Resource-type registry — keep in sync with awslib

`SupportedResourceTypes()` in `resource.go` is a hand-maintained allowlist that **must mirror awslib's `proxy.RepoProxy.FindAll` dispatch switch**. Types in awslib's registry but not wired into `FindAll` (e.g. Athena, CloudTrail) are deliberately omitted so callers never hit a "resource type not supported" error. When awslib adds/removes a `FindAll` case, update this list. `ResolveResourceType` accepts both canonical (`AWS::EC2::Instance`, case-insensitive) and URL (`aws_ec2_instance`) forms.

**Drift here is silent and expensive.** A dispatchable type that is missing from the allowlist does not error — it falls through to `list_resources_fallback`, which answers from Cloud Control with the provider's raw property bag and no curated attributes. That is how EIPs were being served region-by-region from a LIST handler that returns only `public_ip` + `allocation_id`, while awslib had a full `DescribeAddresses`-backed entity all along. Adding a type therefore takes **two** files, not one: `resource.go` for the allowlist and `attributes.go` for a `summaryAttributes` case, because a type with no case gets a nil attribute map and loses `state`, the `attr:`/`state` filters and every group-by dimension. `vpc_resources_test.go` pins the EC2/VPC set (Vpc, Subnet, SecurityGroup, VPCEndpoint, RouteTable, EIP) as supported, regional, and attributed.

Elastic IPs have no lifecycle state in `DescribeAddresses`, so `summaryAttributes` derives one (`associated`/`unassociated`) from `IsAssociated()`. That is deliberate: "which EIPs are idle and billing for nothing" is the question asked of this type, and deriving the state is what makes it answerable through the same `state` filter and `group_by=state` as every other type. `instance_id`/`network_interface_id` identify what an address fronts — an EIP on a NAT gateway or NLB shows only the ENI, since awslib has no NAT/ENI repository to resolve it further.

CloudFront is present only as its SaaS Manager types — `AWS::CloudFront::DistributionTenantSummary` and `AWS::CloudFront::ConnectionGroup`. The full `AWS::CloudFront::DistributionTenant` is a per-identifier Get, not a list, so it stays out of the allowlist; classic **distributions are not listable at all** because awslib has no `ListDistributions` — exposing them needs an upstream repository method, resource type and `FindAll` case first. Both CloudFront types are in `cfg.ResourceTypeListGlobal()`, and awslib's `RepoProxyPool.List` already collapses global types to one proxy per account, so "all regions" does not fan out or duplicate rows.

### Region narrowing (`regions.go`)

`resolveRegions("")` returns awslib's static list of every AWS region. Sweeping all of them across many accounts is what made a first request time out: each `(account, region)` pair costs a client build with an STS round trip, and a region the account never opted into pays that only to be rejected. `Server.narrowRegions` asks each account once — `Ec2Repository.ListRegionsAll()` — and drops the regions no reachable account has enabled.

Four rules hold this together:

- **`ListRegionsAll`, not `ListRegionsOptIn`.** `DescribeRegions` without `AllRegions` already returns exactly the regions enabled for the account. `ListRegionsOptIn` filters to `opt-in-status=opted-in`, which excludes every standard region (they are `opt-in-not-required`) and would narrow a sweep to almost nothing.
- **Union across accounts, never intersection.** A region enabled in one account of many still has to be swept or its resources go unreported. Per-account precision would be tighter, but the pool takes one region list for the whole fan-out, and a client that fails for an account lacking the region is what `v3.FailureCache` already makes cheap.
- **Fail open, always.** If the lookup fails for even one account, the full list is used and the response says so. Uncertainty costs time, never correctness: narrowing on a partial answer produces an authoritative-looking empty result, which is the failure this feature exists to remove. `us-east-1` is kept unconditionally — it needs no opt-in and serves the global endpoints.
- **Narrowing is never silent, and never overrides an explicit `region`.** What was skipped, and why, goes into the same `warnings` array as the unreachable pairs. A caller that cannot see the narrowing cannot tell it from a smaller estate.

The result is cached per account by `regionCache` on the TTL passed to `Server.WithRegionTTL` (`serve.go` gives it the same value as the client-failure cache, since both record something a deliberate act changes). This is not a substitute for `v3.FailureCache`: that makes a repeat of a known-bad pair cheap, this avoids asking at all — which is what the first sweep after a restart needs.
### Credentials are never fatal (`lazypool.go`, `ssologin.go`, `auth_tools.go`)

An MCP server that exits when credentials are unusable hands the calling agent a refused connection — no explanation, no recovery. This used to happen: `buildClientPool` resolved credentials during `serve`, so an expired SSO token reached `main` and `os.Exit(1)`. Three pieces prevent it, and the property they defend is *always answer, never exit*:

- **`LazyPool`** wraps the local and assume-role pools: nothing touches AWS until the first tool call, failures are returned to that caller, and construction is retried after `lazyRetryInterval` (5s) — so a re-login lands without a restart. `buildClientPool` now errors only for operator mistakes that cannot fix themselves (malformed role ARN, duplicate profile). `serve.run` warms the pool in a background goroutine purely for the startup log.
- **`SSOLoginManager`** runs the RFC 8628 device-authorization flow in-process with `ssooidc` (`RegisterClient` → `StartDeviceAuthorization` → poll `CreateToken`), then writes the token to `ssocreds.StandardCachedTokenFilepath` in the CLI's JSON shape, mode 0600, temp file + rename. Four things to keep: one flow per `sso_session` (25 profiles on one session ⇒ one code, not 25); the cached OIDC registration is reused because re-registering invalidates the refresh token beside it; `Explain` only starts a flow when `IsCredentialFailure(err)` — a disabled region or a missing IAM grant must not send anyone to a browser; and the device code stays unexported so it cannot reach a tool result.
- **`aws_auth_status` / `aws_sso_login`** are the agent's way out. Both work with a nil pool and read only local files, which is the point: they have to answer *while* AWS is unusable.

`--sso-auto-login` (default true) governs only the automatic start from a failing tool call; `aws_sso_login` works regardless. `--sso-open-browser` is a host convenience, skipped when `inContainer()`.

### Credential preflight (`credcheck.go`)

AWS credential failures surface from the SDK as long, causeless strings, and they surface on a *tool call* rather than at startup. `credcheck.go` closes both gaps, using only local file reads:

- `InspectProfile` reads a profile via `awsconfig.LoadSharedConfigProfile`. That function does **not** honour `AWS_CONFIG_FILE`/`AWS_SHARED_CREDENTIALS_FILE` on its own (unlike `LoadDefaultConfig`), so the overrides are read from `awsconfig.NewEnvConfig()` and passed in — otherwise the preflight would inspect different files than the credential chain does.
- `CheckSSOToken` locates the cached token with the SDK's own `ssocreds.StandardCachedTokenFilepath` (sha1 of the `sso_session` name, or of the start URL for legacy profiles) and reads only `expiresAt` out of it — the file holds live credentials.
- The **writability check is on the directory, not the file**: `ssocreds.storeCachedToken` writes `<token>.tmp-<nanos>` alongside the token and renames it, so a read-only `~/.aws/sso/cache` mount fails at the first refresh, hours after a healthy-looking startup.
- `PreflightProfile` decides warn vs fail (the table is in `docs/authentication.md`) — "fail" means *this profile is unusable now*, never *the process exits*. One deliberate downgrade: an expired access token whose cache still holds a refresh token and client registration (`SSOTokenStatus.Refreshable`) is a warning, because the SDK renews it on first use — treating it as fatal was demanding a browser approval nobody needed.
- `ExplainCredentialError` wraps SDK errors that still get through, and `SSOLoginManager.Explain` layers the login instructions on top. Both are reached from `ProfilePool.resolve`/`explain` and from `logCallerIdentity` in `serve.go`, so all three auth modes get the same treatment.

Native `sso_session` profiles need no code beyond this — the SDK resolves and refreshes them. `credential_process` profiles cannot work in the distroless image (no shell), which is why they are warned about explicitly.

### Cache

`cache.go`: `NewCache` builds an awslib `DataCache` with a bigcache in-memory handler plus, when `--cache-dir` is set, a file handler (both share the TTL, default 6h). `--no-cache` makes `buildCache` return `nil`, and `NewServer`/`WithCache` both accept nil to run cache-free.

## Conventions

- **Errors:** use this module's own `pkg/errors` (dependency-free, adds stack traces via `WithStack`/`Errorf`), not the stdlib `errors` or `pkg/errors` upstream.
- **Logging:** `zerolog`, message prefix convention `[pkg.Func] ...`.
- **Tools return errors as results:** handlers return `mcp.NewToolResultErrorFromErr/Errorf(...)` with a `nil` Go error for user-facing failures (bad input, AWS init failure) — reserve a non-nil Go error for transport-level faults.
- Go 1.26 toolchain (`go.mod`); `docs/development.md` notes a hard Go 1.25+ floor from the `mcp-go` dependency.

## Testing

`internal/mcpserver/server_test.go` spins up the server behind an in-process streamable-HTTP test transport (`server.NewTestStreamableHTTPServer`) and drives it with a real `mcp-go` client. Tests that only exercise AWS-independent tools (`list_resource_types`, `list_regions`, `list_cost_dimensions`) pass a `nil` ClientPool — follow that pattern to test tool wiring without AWS credentials. Argument-rejection tests use the same nil pool as a tripwire: if validation ever moved after the pool access the test would panic instead of failing.

`cost_test.go` covers the cost helpers against a fixed clock (`now`, a mid-month date) so relative periods do not drift with the calendar, and feeds synthetic `CostAndUsage` values through `buildCostResult` rather than mocking AWS.
