# Resource tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `list_resource_types` | — | Supported resource types (canonical + URL form, global flag) |
| `list_regions` | — | Known AWS regions with descriptions |
| `list_accounts` | — | Account IDs the server can reach |
| `list_resources` | `resource_type` (required), `region`, `account_id`, `view`, `state`, `tag`, `attribute`, `limit`, `cursor` | List resources of a type across accounts/regions (paginated) |
| `count_resources` | `resource_type` (required), `group_by`, `region`, `account_id`, `state`, `tag`, `attribute` | Aggregate resources into group counts |

The Cost Explorer tools are documented separately in
[cost-explorer.md](cost-explorer.md).

`resource_type` accepts either the canonical form (`AWS::EC2::Instance`) or the
URL form (`aws_ec2_instance`). When `region` is omitted, all known regions are
queried (global resource types are fetched from a single region automatically).

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

## Performance

On the first `list_resources` call without a `region`, the server initializes a
client per region (one STS `GetCallerIdentity` each), which is slower; clients
and results are cached afterwards. Pass a specific `region` for fast, targeted
queries, and prefer `count_resources` when the question is "how many".
