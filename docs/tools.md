# Resource tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_resource_types` | — | Supported resource types (canonical + URL form, global flag) |
| `list_regions` | — | Known AWS regions with descriptions |
| `list_accounts` | — | Account IDs the server can reach |
| `list_resources` | `resource_type` (required), `region`, `account_id`, `view`, `state`, `tag`, `attribute`, `limit`, `cursor` | List resources of a type across accounts/regions (paginated) |
| `count_resources` | `resource_type` (required), `group_by`, `region`, `account_id`, `state`, `tag`, `attribute` | Aggregate resources into group counts |
| `list_resources_fallback` | `resource_type` (required), `region`, `account_id`, `view`, `state`, `tag`, `attribute`, `limit`, `cursor` | Last resort for types `list_resources` does not support, via the Cloud Control API |

The Cost Explorer tools are documented separately in
[cost-explorer.md](cost-explorer.md).

`resource_type` accepts either the canonical form (`AWS::EC2::Instance`) or the
URL form (`aws_ec2_instance`). When `region` is omitted, all known regions are
queried (global resource types are fetched from a single region automatically).

`account_id` **scopes** the query rather than filtering its output. Only that
account's credentials are used and only that account is called — no API calls are
issued against the others. This matters when one server reaches several accounts:
passing the development account's ID means production is never contacted. An
account the server cannot reach is an error, not an empty result, so a typo does
not masquerade as "nothing there". Call `list_accounts` for the reachable IDs.

## `list_resources` — views, filters, pagination

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
SNS, IAM users, Auto Scaling groups and CloudFront (distribution tenants /
connection groups).

Filters narrow results server-side (so payload scales with the answer, not the
inventory): `state` (case-insensitive lifecycle match, e.g. `running`), `tag`
(`Key=Value`), and `attribute` (`key=value` against a curated attribute, e.g.
`instance_type=m5.2xlarge`).

```jsonc
// list_resources: running m5.2xlarge instances, thin view
{ "resource_type": "AWS::EC2::Instance", "region": "eu-central-1",
  "state": "running", "attribute": "instance_type=m5.2xlarge" }
```

## `count_resources` — aggregates without listing

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

## `list_resources_fallback` — types with no dedicated support

`list_resources` only covers the resource types listed by `list_resource_types`
(currently 34). `list_resources_fallback` answers for almost anything else by
going through the **AWS Cloud Control API**, which can enumerate any
`AWS::Service::Resource` type whose CloudFormation registry entry implements a
`LIST` handler — no per-type support needed on this side.

**Prefer `list_resources` whenever the type appears in `list_resource_types`.**
The fallback is less good on purpose:

| | `list_resources` | `list_resources_fallback` |
|---|---|---|
| Coverage | 34 curated types | Almost any type with a `LIST` handler |
| `attributes` | Curated, hand-picked per type | The resource's own top-level properties, snake_cased |
| `arn` | Always | Only when the type exposes one as a property |
| `created_at` | Usually | Never — Cloud Control reports none |
| Cost of `view=detail` | Free | One extra AWS call **per resource** |

Everything else behaves the same: identical `view` levels, the same
`state`/`tag`/`attribute` filters, the same pagination envelope. Two additions:

- **`queried`** — `{ "source": "cloudcontrol", "accounts": N, "regions": M,
  "detailed": bool }`, so an empty result is interpretable. "Asked 12 accounts,
  found nothing" is a different answer from "asked 1".
- **`warnings`** — a list qualifying the answer. **Read it.** It reports
  collapsed duplicates, type-name normalization, whether you paid for the
  per-resource detail calls, and the fact that a resource is missing entirely if
  that account's credentials lack the underlying service's read permission. It
  also says so outright when the type you asked for *does* have dedicated
  support and you should be calling `list_resources` instead.

```jsonc
// list_resources_fallback: Kinesis streams, a type list_resources has no support for
{ "resource_type": "AWS::Kinesis::Stream", "region": "eu-central-1" }
```

Three things to know before relying on it:

- **Type names are case-sensitive for unknown types.** Well-known types are
  normalized for you (`aws::ec2::instance` and `aws_ec2_instance` both work), but
  anything outside that vocabulary is passed to AWS exactly as you spell it, so
  it needs the real CloudFormation casing (`AWS::Kinesis::Stream`).
- **Some types return an error, not an empty list** — those implementing only
  `READ`, and nested types needing a parent identifier
  (e.g. `AWS::ApiGateway::Method`). An error here means "not listable this way",
  not "none exist".
- **Pass `region` for global types.** Without it the type is fetched once per
  region; duplicates are collapsed automatically and reported in `warnings`, but
  a single region is faster and cheaper.

Its permissions are also different: Cloud Control does not bypass IAM, so the
credentials still need the underlying service's own read permission for the
target type (a broad `ReadOnlyAccess`-style policy covers this).

## Performance

On the first `list_resources` call without a `region`, the server initializes a
client per region (one STS `GetCallerIdentity` each), which is slower; clients
and results are cached afterwards. Pass a specific `region` for fast, targeted
queries, and prefer `count_resources` when the question is "how many".
