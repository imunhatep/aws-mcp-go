# Savings Plans

Two tools, answering two different questions:

| Tool | Question |
|------|----------|
| `list_savings_plans` | What have we already committed to? (inventory) |
| `list_savings_plan_rates` | What would a commitment cost? (published prices) |

Neither is a resource listing and neither is Cost Explorer. A purchased plan is a
contract, a rate is a price, so both bypass the resource proxy pipeline and are
answered from awslib's `service/savingsplans` directly. For what is actually being
*spent*, use [`get_cost_and_usage`](cost-explorer.md) — filtering on
`PURCHASE_TYPE` separates Savings Plan coverage from on-demand.

## The API is global, and region is a filter

Every Savings Plans client resolves to `savingsplans.amazonaws.com` whatever
region it was built for. Two consequences the tools rely on:

- **Inventory is account-wide.** One request per account returns every plan; asking
  per region would multiply requests without finding a single extra plan. The
  `region` field on a plan is the region an *EC2 Instance* plan is locked to, not
  the region it was fetched from. Compute plans carry no region at all — which is
  why `region` as a filter excludes them.
- **Rates take the region as a query parameter.** Any reachable account can price
  any region, so `list_savings_plan_rates` queries exactly one account. Fanning out
  would return the same public price list once per account.

## `list_savings_plans` — what we hold

```
list_savings_plans(plan_type="ec2", expiring_within_days=90)
```

| Argument | Effect |
|---|---|
| `plan_type` | `EC2Instance` (accepts `ec2`), `Compute`, `SageMaker`, `Database` |
| `state` | Comma-separated states, or `all`. **Defaults to `active`** |
| `region` | EC2 Instance plans locked to this region (excludes Compute plans) |
| `instance_family` | EC2 Instance plans committed to this family, e.g. `m5` |
| `expiring_within_days` | Plans whose commitment ends within N days |
| `account_id` | Scope to one account — its credentials only, no other account called |
| `limit` | Rows to return (default 200, max 1000) |

Only `state` is filtered by AWS; `DescribeSavingsPlans` accepts no other filters,
so the rest are matched against the fields each plan carries.

The response is `{items, count, total, truncated, summary, accounts_queried,
warnings, note}`:

- Each row carries the plan's identity, `hourly_commitment` + `currency`, `term`
  (`1yr`/`3yr` rather than a second count), payment option, upfront and recurring
  amounts, start/end, and `days_remaining` computed from the end date.
- `summary` totals the hourly commitment and upfront payment **per currency**,
  counts plans by type/state/region, and names the plan expiring next. It covers
  every matching plan, not just the returned page, so a truncated response still
  states the real commitment. Amounts that do not parse are counted in
  `amounts_unparsed` rather than silently treated as zero, and currencies are never
  added together.
- `accounts_queried` and `warnings` exist so a short answer is not mistaken for a
  complete one: an account whose query failed is named in `warnings` and the other
  accounts still answer. If *no* account could be queried, the tool errors.

Two defaults worth knowing:

- **`state` defaults to `active`.** A retired or payment-failed plan stays listed by
  the API forever, so an unfiltered inventory reads as far more coverage than the
  account actually has.
- **Inventory is not cached**, unlike almost everything else this server returns.
  It answers "what are we committed to right now", and the default 6h cache window
  would answer a different question. Rates are cached — published prices change
  rarely.

## `list_savings_plan_rates` — what it would cost

```
list_savings_plan_rates(region="eu-central-1", instance_family="m5", term="3yr")
```

`region` is required, and so is `instance_type` **or** `instance_family`. This is
not politeness: the price list holds every instance type at every term, payment
option, OS and tenancy, and an unfiltered lookup is a multi-thousand-page sweep.
Both checks happen before any AWS call.

| Argument | Default |
|---|---|
| `region` | *required* |
| `instance_type` / `instance_family` | one of the two *required* |
| `product` | `ec2`. Also: `fargate`, `fargate-eks`, `lambda`, `sagemaker`, `rds`, `dynamodb`, `elasticache`, `opensearch`, `docdb`/`documentdb`, `neptune`, `timestream` |
| `plan_type` | both `EC2Instance` and `Compute` — the comparison worth making |
| `payment_option` | all three (`No Upfront`, `Partial Upfront`, `All Upfront`) |
| `term` | both `1yr` and `3yr` |
| `product_description` | `Linux/UNIX` — pass `all` for every OS. EC2 only |
| `tenancy` | `shared` — pass `all` for every tenancy. EC2 only |
| `account_id` | any reachable account; this only picks whose credentials sign |
| `limit` | 200 (max 1000) |

`product` sets both the product type and the rate service code, which the API
takes separately — a mismatched pair returns nothing rather than an error, so the
two are always resolved together. `product_description` and `tenancy` are EC2
properties and are omitted for other products, where sending them would filter
every rate away.

`Linux/UNIX` + `shared` are defaulted because without them every lookup returns
the same rate once per OS and tenancy combination. The response echoes the
effective `query`, defaults included, so a narrow result is never mistaken for a
narrow market.

Arguments are forgiving about spelling: `ec2` for `EC2Instance`, `no-upfront` /
`no_upfront` for `No Upfront`, `1 year` / `12months` for `1yr`.

## Term filtering costs a client-side pass

The commitment length lives on each rate's parent offering and the API offers no
filter for it, so awslib drops the other terms after fetching. A `term` argument
therefore narrows the *response*, not the request — the pages are fetched either
way.

## Permissions

`savingsplans:DescribeSavingsPlans` for the inventory,
`savingsplans:DescribeSavingsPlansOfferingRates` for the rates, in every account
queried. A missing grant is reported per account in `warnings` rather than
silently shortening the list.
