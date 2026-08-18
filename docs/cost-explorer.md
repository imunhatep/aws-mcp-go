# Cost Explorer tools

| Tool | Arguments | Description |
|------|-----------|-------------|
| `get_cost_and_usage` | `period`, `start`, `end`, `granularity`, `metrics`, `group_by`, `filters`, `account_id`, `limit`, `include_periods` | Actual spend/usage, filtered and grouped |
| `get_cost_forecast` | `period`, `start`, `end`, `granularity`, `metric`, `filters`, `account_id`, `prediction_interval_level` | Forecast future spend |
| `list_cost_dimension_values` | `dimension` (required), `period`, `start`, `end`, `search`, `filters`, `context`, `account_id`, `limit` | Values a cost dimension actually took |
| `list_cost_dimensions` | — | The cost query vocabulary (dimensions, metrics, periods, filter shape) |

`get_cost_and_usage` is the console's cost report as a tool: a time window, a
granularity, cost metrics, filter rows and up to **2** groupings. Call
`list_cost_dimensions` for the full vocabulary and `list_cost_dimension_values`
to discover the exact strings a filter needs — AWS service names (`Amazon
Elastic Compute Cloud - Compute`) rarely match what a caller would guess.

## Time window

Either a named `period` or an explicit `start`/`end` pair (`YYYY-MM-DD`, end
exclusive). Named periods beginning `last_` cover whole elapsed units and stop at
today, so their numbers are stable; `this_month` (`mtd`), `this_year` (`ytd`) and
`today` run through today, whose costs are still partial.

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

## Grouping

`group_by` takes up to two entries: a dimension (`SERVICE`, `LINKED_ACCOUNT`,
`REGION`, `INSTANCE_TYPE`, `USAGE_TYPE`, `RECORD_TYPE`, `PURCHASE_TYPE`,
`OPERATION`, …, case-insensitive with aliases like `account` and `charge_type`),
`TAG:<key>` or `COST_CATEGORY:<key>`. Grouped tag values come back unwrapped from
the API's `<key>$<value>` encoding, with untagged spend labelled `(not set)`.

## Filtering

`filters` is a list of rows, AND-ed together, with the values inside a row
OR-ed — the console's filter panel:

```jsonc
{ "type": "dimension" | "tag" | "cost_category",  // optional; inferred from key
  "key": "SERVICE",                               // dimension, tag key or category
  "values": ["…"],
  "match_options": ["EQUALS"],                    // EQUALS / CASE_SENSITIVE, ABSENT on tags
  "exclude": false,                               // the console's "Exclude" toggle
  "absent": false,                                // key not carried at all
  "present": false }                              // key carried, any value
```

## Response

`groups` aggregates the whole window ranked by spend (for "what costs the most"),
`periods` splits the same data by time (for trends), and `total` sums it. `limit`
caps the group list (default 25) with `group_count` / `groups_truncated`
reporting what was dropped; `include_periods` adds the per-period group
breakdown, which is off by default to keep responses small.

```jsonc
// Top RDS spenders by team tag last month, excluding credits and refunds
{ "period": "last_month", "group_by": ["TAG:Team"], "limit": 10,
  "filters": [{ "key": "SERVICE", "values": ["Amazon Relational Database Service"] },
              { "key": "RECORD_TYPE", "values": ["Credit", "Refund"], "exclude": true }] }

// Daily EC2 spend per region over the last 30 days, with the per-period breakdown
{ "period": "last_30_days", "granularity": "DAILY", "include_periods": true,
  "group_by": ["SERVICE", "REGION"] }
```

## Multiple accounts

By default every reachable account is queried and the results summed, with each
group attributed by `account_id` and a `note` on the response. If the pool holds
both a management (payer) account and its members their costs overlap — pass
`account_id` to scope to one, or `group_by: ["LINKED_ACCOUNT"]` to break an
organization's spend down from the payer.

> Cost Explorer bills **$0.01 per request**. Arguments are validated before any
> call is made, and answers are cached like resource listings.
