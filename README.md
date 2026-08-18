# aws-mcp

An [MCP](https://modelcontextprotocol.io) server that lets an AI assistant answer
questions about your AWS estate — inventory across every account and region, and
what it all costs — without handing it a shell or write access.

```
"How many running m5 instances do we have in prod?"
"Which team tag spent the most on RDS last month?"
"List the EKS clusters across all accounts with their versions."
```

**Why it exists.** Pointing an assistant at the `aws` CLI is slow, expensive in
tokens and unbounded in blast radius. This server instead exposes a small set of
read-only tools that do the fan-out server-side and return *answers*:
`count_resources` aggregates without shipping rows, filters and views cut
payloads to what was asked for, everything is cached (default 6h), and Cost
Explorer queries are validated before they are billed. It is read-only by
construction — no tool mutates anything.

It is a thin MCP layer over [`awslib`](https://github.com/imunhatep/awslib),
which does the actual AWS work.

## Quick start

```sh
make build
aws sso login --sso-session my-session
./bin/aws-mcp serve --profiles dev,prod
```

Or with a container — mount the AWS config and SSO token cache, no credentials
baked in:

```sh
podman run --rm -d --name aws-mcp \
  -v ~/.aws/config:/home/nonroot/.aws/config:ro \
  -v ~/.aws/sso/cache:/home/nonroot/.aws/sso/cache:rw \
  -p 127.0.0.1:3040:3040 \
  ghcr.io/imunhatep/aws-mcp-go:latest serve --profiles dev,prod
```

Then point your client at it:

```sh
claude mcp add --transport http aws http://127.0.0.1:3040/mcp
```

Startup validates every profile before serving, so a missing or expired SSO
login fails immediately with the command that fixes it — not on the first
question you ask.

## What it exposes

| Tool | Answers |
|------|---------|
| `list_resources` | "Show me the …" — paginated rows, three levels of detail, server-side filters |
| `count_resources` | "How many / break down by …" — grouped counts, no rows shipped |
| `list_accounts`, `list_regions`, `list_resource_types` | What this server can reach |
| `get_cost_and_usage` | "What did we spend on …" — grouped, filtered, multi-account |
| `get_cost_forecast` | "What will we spend …" |
| `list_cost_dimensions`, `list_cost_dimension_values` | The cost vocabulary, and the exact strings filters need |

Coverage spans EC2, RDS, ELB, ECS, EKS, Lambda, DynamoDB, S3, Route53, Secrets
Manager, EFS, SQS, SNS, IAM, Auto Scaling and CloudFront — call
`list_resource_types` for the current list.

## Documentation

| Guide | Contents |
|-------|----------|
| [Authentication](docs/authentication.md) | Local, multi-profile and assume-role modes; the startup credential checks |
| [Running in a container](docs/container.md) | Multi-account AWS SSO with podman, image builds, cache volumes, troubleshooting |
| [Resource tools](docs/tools.md) | `list_resources` views/filters/pagination, `count_resources` aggregation |
| [Cost Explorer](docs/cost-explorer.md) | Periods, groupings, filter shape, response layout, per-request billing |
| [Configuration](docs/configuration.md) | Commands, flags and environment variables, caching, client config |
| [Development](docs/development.md) | Building, the awslib dependency, make targets, CI and releases |

## Requirements

Go 1.25+ to build, and AWS credentials the standard chain can resolve — an SSO
session, a profile, environment variables or an instance role. Read-only IAM
permissions are enough (`ce:GetCostAndUsage` and friends for the cost tools).
