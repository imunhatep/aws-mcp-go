---
name: using-aws-mcp
description: Use whenever a question is about AWS resources, inventory or spend and the aws MCP server (aws-mcp) is connected — "how many / list / which X do we have", "what does Y cost", "forecast", "savings plans", "why can't you see account Z". Covers choosing between list_resources, count_resources and list_resources_fallback (the Cloud Control fallback that serves almost every type NOT in list_resource_types — NAT gateways, ENIs, KMS keys, IAM roles, ECR, ElastiCache, MSK, OpenSearch, Kinesis, Step Functions, API Gateway, Transit Gateway…), reading the queried/warnings envelope, account scoping, Cost Explorer query recipes, forecasts, Savings Plans inventory and rates, and auth troubleshooting in every authentication mode.
---

# Using the aws MCP server

`aws-mcp` is a **read-only** MCP server: AWS inventory across every reachable
account and region, plus Cost Explorer and Savings Plans. It fans out on the
server, caches for 6h by default, and returns answers rather than raw API dumps.
No tool writes anything.

This guide ships inside the server binary. The `aws_usage_guide` tool returns
it, whole or one section at a time (`topic`). It lives in the aws-mcp repository
at `skills/using-aws-mcp/SKILL.md`, and changes with the tools it describes.

## Core rules

- **"Not in `list_resource_types`" never means "unsupported".** It means "use
  `list_resources_fallback`". Never tell the user a type cannot be listed until
  the fallback has actually returned an error for it (see
  [the fallback](#list_resources_fallback--everything-else)).
- **An empty result is not "none exist" until you've read `queried` and
  `warnings`.** See [Reading results](#reading-results).
- **Scope with `account_id`** whenever the user names an account or environment.
  Only that account is contacted, which keeps queries away from production.
- **On anything that looks like an auth error, call `aws_auth_status`** before
  drawing conclusions. See [Auth and troubleshooting](#auth-and-troubleshooting).

## Tool map

| Question shape | Tool |
|---|---|
| What can this server reach? | `list_accounts`, `list_regions`, `list_resource_types` |
| "How many…", "break down by…" | `count_resources` (tiny aggregates, no rows) |
| "List / show me / which…" (supported type) | `list_resources` |
| Same, for a type **not** in `list_resource_types` | `list_resources_fallback` |
| "What did we spend on…" | `get_cost_and_usage` |
| "What will we spend…" | `get_cost_forecast` |
| Exact strings for a cost filter | `list_cost_dimension_values` (vocabulary: `list_cost_dimensions`) |
| "What are we committed to / what expires" | `list_savings_plans` |
| "What would a commitment cost" | `list_savings_plan_rates` |
| Auth error, missing account | `aws_auth_status`, then `aws_sso_login` |
| This guide | `aws_usage_guide` |

If your client defers tool schemas (Claude Code's `ToolSearch`), load the ones
you need first, e.g. `select:mcp__aws__list_resources,mcp__aws__list_resources_fallback`.
The prefix is `mcp__<server name>__`.

## Choosing the resource tool

```
list_resource_types  ──► type listed? ──yes──► "how many / by X"? ──yes──► count_resources
        │                                              └──no───► list_resources
        └──no──► list_resources_fallback  (CloudFormation name, e.g. AWS::EC2::NatGateway)
```

`list_resource_types` holds roughly 40 curated types:

- **EC2:** instances, volumes, snapshots, VPCs, subnets, security groups, route
  tables, VPC endpoints and EIPs.
- **RDS:** instances and snapshots.
- **Others:** ELBv2, ECS clusters and services, EKS, Lambda, DynamoDB, S3,
  Route53 zones/records/domains, Secrets Manager, EFS, SQS, SNS, IAM users, ASGs,
  CloudWatch log groups, Glue, EMR / EMR Serverless, Batch, and CloudFront
  distribution tenants and connection groups.

Call `list_resource_types` rather than trusting this list. Both forms work for
these types: `AWS::EC2::Instance` and `aws_ec2_instance`. Asking
`list_resources` for any other type returns an error that names the fallback.

### `list_resources` / `count_resources`

Shared arguments: `resource_type` (required), `region`, `account_id`, and the
filters `state`, `tag` and `attribute`.

- **`region`**: omit it to sweep every region the accounts have enabled. Regions
  no account opted into are skipped and reported in `warnings`. Pass a region
  whenever the question allows; it is much faster. Global types ignore it.
- **`account_id`** *scopes the fan-out*: only that account's credentials are
  used and no other account is contacted. An unreachable account is an
  **error**, not an empty result.
- **Filters** run on the server and are case-insensitive:
  - `state=running`
  - `tag=environment=preprod` (`Key=Value`)
  - `attribute=instance_type=m5.2xlarge` (curated attributes)
- **`view`** (`list_resources` only):
  - `id` (default): identity plus state.
  - `summary`: adds `tags` and curated `attributes` such as `instance_type`,
    `engine` and `dns_name`.
  - `detail`: adds `raw`, the full SDK struct. Use it only with a region and
    filters.
- **Paging**: `limit` (default 50, max 1000) and `cursor`. Pass back
  `next_cursor` until it is absent.
- **`group_by`** (`count_resources` only): a comma-separated list of `type`,
  `state`, `region`, `account_id`, `tag:<key>` or `attr:<key>`. The default is
  `state`. Examples: `"state,attr:instance_type"`, `"account_id,region"`,
  `"tag:team"`.

Recipes:

```jsonc
// How many running instances per type in one account?
count_resources {"resource_type":"AWS::EC2::Instance","account_id":"<id>","state":"running","group_by":"attr:instance_type"}

// Idle Elastic IPs (EIP state is derived: associated / unassociated)
list_resources {"resource_type":"AWS::EC2::EIP","state":"unassociated","view":"summary"}

// Unattached EBS volumes by region and account
count_resources {"resource_type":"AWS::EC2::Volume","state":"available","group_by":"region,account_id"}

// Postgres RDS instances with their class and endpoint
list_resources {"resource_type":"AWS::RDS::DBInstance","attribute":"engine=postgres","view":"summary"}

// EKS clusters and versions, everywhere
list_resources {"resource_type":"AWS::EKS::Cluster","view":"summary"}
```

To find the attribute keys a type offers, run `list_resources` once with
`view=summary` and `limit=3`, then filter or group on what you see.

### `list_resources_fallback` — everything else

This serves almost any `AWS::Service::Resource` type through the **Cloud Control
API**. It needs no per-type support and no AWS Config recorder. It takes the
same arguments, views, filters and paging as `list_resources`, and returns the
same envelope.

Commonly asked types that are **not** curated, all of which go here:

| Asked for | `resource_type` |
|---|---|
| NAT gateways | `AWS::EC2::NatGateway` |
| Network interfaces (ENIs) | `AWS::EC2::NetworkInterface` |
| Transit gateways / attachments | `AWS::EC2::TransitGateway`, `AWS::EC2::TransitGatewayAttachment` |
| Internet gateways | `AWS::EC2::InternetGateway` |
| Launch templates | `AWS::EC2::LaunchTemplate` |
| KMS keys | `AWS::KMS::Key` |
| IAM roles / managed policies | `AWS::IAM::Role`, `AWS::IAM::ManagedPolicy` |
| ECR repositories | `AWS::ECR::Repository` |
| ElastiCache | `AWS::ElastiCache::ReplicationGroup`, `AWS::ElastiCache::ServerlessCache` |
| MSK clusters | `AWS::MSK::Cluster` |
| OpenSearch domains | `AWS::OpenSearchService::Domain` |
| Kinesis / Firehose | `AWS::Kinesis::Stream`, `AWS::KinesisFirehose::DeliveryStream` |
| Step Functions | `AWS::StepFunctions::StateMachine` |
| API Gateway | `AWS::ApiGateway::RestApi`, `AWS::ApiGatewayV2::Api` |
| CloudFront distributions (classic) | `AWS::CloudFront::Distribution` (global, so pass `region: us-east-1`) |
| WAFv2 / ACM / SSM params | `AWS::WAFv2::WebACL`, `AWS::CertificateManager::Certificate`, `AWS::SSM::Parameter` |

These names come from the CloudFormation resource reference and are not
guaranteed. If a name is wrong or the type is not listable, the tool returns an
error. That error is the signal; don't guess around it.

Rules for the fallback:

- **Use exact CloudFormation casing** for anything outside the curated vocabulary
  (`AWS::Kinesis::Stream`, not `aws::kinesis::stream`). Cloud Control's type
  name is case-sensitive.
- **Pass `region` for global types** (IAM, CloudFront). Otherwise every region
  is queried and the duplicates are collapsed: still correct, but slow.
- **An error means "not listable this way"**, not "none exist". Two kinds of
  type cannot be listed:
  - types with only a `READ` handler;
  - nested types that need a parent identifier (`AWS::ApiGateway::Method`,
    `AWS::EKS::Nodegroup`).

  Report it as that.
- **`view=detail` costs one `GetResource` call per resource.** Always pair it
  with `region`, `account_id` and filters. For some types, LIST returns only
  identifiers (S3 works this way), so `summary` may be sparse and `detail` is
  the only way to get properties.
- **Attributes and tags work differently here.** Attribute keys are the
  resource's own properties in snake_case (`engine_version=8.0`); run
  `view=summary` first to see them. `tag` works only for types with a
  CloudFormation-style `Tags` list.
- **Rows are thinner.** There is no `created_at`, and an `arn` only if the type
  exposes one.
- **If `warnings` says the type has dedicated support, switch to
  `list_resources`.**

## Reading results

Every resource envelope is `{items|buckets, count, total, next_cursor, queried, warnings}`.

- **`queried`** says how many accounts and regions actually answered. "Asked 12
  accounts, found nothing" and "asked 1" are different answers.
- **`warnings`** names each unreachable `(account, region)` pair *with its
  reason*: timeout, region not enabled, missing IAM grant, or expired
  credentials. It also lists regions skipped by narrowing, duplicates collapsed
  and type normalization.
- **Always surface warnings that affect the answer.** A count especially has no
  other hint that a region was missing.
- **Say so when an answer is partial**, e.g. "23 instances across 4 of 5
  accounts; `prod-eu` timed out". Never present it as complete.

## Cost Explorer

Cost Explorer **bills per request** (about $0.01), and every call runs once per
reachable account, so plan the query before sending it. Answers are cached for
6h. Arguments are validated before AWS is called, so a malformed query costs
nothing.

### `get_cost_and_usage`

- **Window**: `period`, or `start` + `end`.
  - Named periods: `today`, `yesterday`, `last_7_days`, `last_30_days`,
    `last_<n>_days`, `this_month` / `mtd`, `last_month`, `last_3_months`,
    `last_<n>_months`, `this_year` / `ytd`, `last_year`.
  - `last_*` periods cover whole elapsed units and exclude today. `this_*` and
    `today` include today's still-partial costs.
  - Explicit dates are `YYYY-MM-DD`: `start` inclusive, `end` exclusive.
- **`granularity`**: `MONTHLY` (default), `DAILY`, or `HOURLY` (last 14 days
  only, and only if hourly data is enabled).
- **`metrics`**: the first metric ranks the groups.
  - `UnblendedCost` is the default, and is what gets invoiced.
  - `AmortizedCost` spreads RI and Savings Plan fees over the period.
  - Also available: `NetUnblendedCost`, `NetAmortizedCost`, `UsageQuantity`.
- **`group_by`**: at most **2** of `SERVICE`, `LINKED_ACCOUNT` (alias
  `account`), `REGION`, `INSTANCE_TYPE`, `USAGE_TYPE`, `OPERATION`,
  `RECORD_TYPE`, `PURCHASE_TYPE`, `TAG:<key>`, `COST_CATEGORY:<name>`.
- **`filters`**: rows are AND-ed, and the values within a row are OR-ed.
  ```json
  [{"key":"SERVICE","values":["Amazon Elastic Compute Cloud - Compute"]},
   {"type":"tag","key":"environment","values":["prod"]},
   {"key":"RECORD_TYPE","values":["Credit","Refund"],"exclude":true}]
  ```
  `{"type":"tag","key":"team","absent":true}` matches untagged spend;
  `"present":true` matches any value.
- **Values must be exact, and service names are never what you'd guess**:
  `Amazon Elastic Compute Cloud - Compute`, `EC2 - Other` (EBS, NAT, data
  transfer), `Amazon Simple Storage Service`, `Amazon Relational Database
  Service`. Get them first from
  `list_cost_dimension_values {"dimension":"SERVICE","search":"Elastic"}`. The
  same applies to `USAGE_TYPE` and `REGION`. For `LINKED_ACCOUNT`, the
  attributes carry account names.
- **Response**:
  - `total`.
  - `groups`, aggregated over the whole window and ranked: use them for "what
    costs most".
  - `periods`, per-period totals: use them for trends. `include_periods: true`
    breaks each period down by group.
  - `limit` caps the groups (default 25), and `groups_truncated` says how many
    were dropped.
- **Multi-account**: by default every reachable account is queried and the
  results are merged. Groups then gain an `account_id` key, and a `note` warns
  of double counting when the pool holds a payer *and* its members. Inside an
  organization, prefer `account_id: <payer>` with
  `group_by: ["LINKED_ACCOUNT"]`.
- **Tag keys**: tag group keys come back unwrapped, and an empty tag value shows
  as `(not set)`, meaning untagged spend. If that share is large, say so: it
  limits what can be attributed.

Recipes:

```jsonc
// Top services last month
get_cost_and_usage {"period":"last_month","group_by":["SERVICE"],"limit":10}

// Daily EC2 trend this month, by usage type
get_cost_and_usage {"period":"mtd","granularity":"DAILY","group_by":["USAGE_TYPE"],
  "filters":[{"key":"SERVICE","values":["Amazon Elastic Compute Cloud - Compute"]}]}

// Spend by team tag and service, last 3 months, excluding credits
get_cost_and_usage {"period":"last_3_months","group_by":["TAG:team","SERVICE"],
  "filters":[{"key":"RECORD_TYPE","values":["Credit","Refund"],"exclude":true}]}

// Per-account breakdown from the payer
get_cost_and_usage {"period":"last_30_days","account_id":"<payer>","group_by":["LINKED_ACCOUNT"]}
```

### `get_cost_forecast`

- **`period`**: `this_month` (the rest of the month; the default),
  `next_month`, `next_<n>_days` or `next_<n>_months`. Alternatively `start` /
  `end`, where `start` is no later than today.
- **`metric`**: exactly one (`UNBLENDED_COST` by default).
- **No grouping, and dimension filters only**: tag and cost-category rows are
  rejected.
- **`prediction_interval_level`** (51–99) adds bounds.
- **Month-end estimate**: `mtd` actuals from `get_cost_and_usage` plus the
  `this_month` forecast. State both numbers.

## Savings Plans

- **`list_savings_plans`**: purchased plans with their commitment, term,
  payment option and time left.
  - Defaults to `state=active`; use `state=all` to include retired and queued
    plans.
  - Always read live, never from cache.
  - `region` matches only EC2 Instance plans, so a region filter excludes
    Compute plans. That is correct.
  - `expiring_within_days: 60` answers "what expires soon".
  - The `summary` totals the hourly commitment per currency across *every*
    matching plan, not just the returned page.
- **`list_savings_plan_rates`**: published prices, the same whichever account
  asks.
  - Needs `region` plus `instance_type` or `instance_family`, checked before any
    AWS call.
  - Defaults to EC2, Linux/UNIX, shared tenancy.
  - Omit `plan_type`, `payment_option` and `term` to compare them all side by
    side.
  - `product` covers other services: `fargate`, `fargate-eks`, `lambda`, `rds`,
    `sagemaker`, …
- **Coverage and utilization**: combine the plans with `get_cost_and_usage`
  grouped by `PURCHASE_TYPE`, or filtered on
  `RECORD_TYPE=SavingsPlanCoveredUsage`.

## Auth and troubleshooting

Tools never answer "no resources" when the real problem is credentials: they
return an error naming the profile and the fix. On any auth-looking error, call
**`aws_auth_status`**. It makes no AWS calls. Its `mode` decides the fix:

| `mode` | Credentials come from | Fix for an expired session |
|---|---|---|
| `default-chain` / `profiles` | The server's own `~/.aws` and SSO cache | `aws_sso_login` returns a verification URL and code. Relay them to the user **verbatim**; a human must approve. The server polls and picks the token up itself, so retry afterwards — no restart. |
| `passthrough` | Forwarded by the MCP client's `headersHelper` (`aws-mcp credentials-headers`) on every request | See below. |

Passthrough mode:

- **Expired role credentials refresh automatically.** The server answers 401,
  and Claude Code re-runs the helper and retries once. You will usually never
  see it.
- **A profile with an `error` and a `login_hint`** means its SSO session itself
  has expired. Ask the user to:
  1. run the hint on their machine (e.g. `aws sso login --sso-session corp`;
     in Claude Code, `! <command>`);
  2. reconnect the server (`/mcp` → server → *Reconnect*);
  3. retry.

  `aws_sso_login` cannot help in this mode, and says so.
- **"this request carries no AWS credentials"** means the client registration
  has no `headersHelper`. Point the user at `docs/authentication.md` in the
  aws-mcp repository.

Other failure shapes:

- **"account X is not reachable"**: that `account_id` is not in the pool. Check
  `list_accounts`. Don't retry without `account_id`: that would query *every*
  account.
- **AccessDenied / not authorized for one service**: the role lacks that read
  permission, and logging in again will not help. Report the account and
  service.
- **Slow first call, or timeouts on an all-region sweep**: pass `region`, and
  `account_id` if possible. The first sweep after a restart builds clients and
  learns which regions are enabled; later calls are cached.
- **Fallback error on a type**: check the CloudFormation spelling and casing,
  and whether the type needs a parent id. Then report it as not listable through
  Cloud Control.
