# Write tools (`--read-write`)

By default aws-mcp is read-only. No tool that can change AWS is registered,
and every tool it does register carries `readOnlyHint: true` (`aws_sso_login`
is the one exception: it writes the local SSO token cache, never AWS).
Read-only is enforced by absence, not by refusal. A tool the server never
offered cannot be called, whatever an agent is told.

```sh
aws-mcp serve --read-write            # or MCP_READ_WRITE=true
```

With the flag, the server also registers:

| Tool | API | Annotations |
|------|-----|-------------|
| `create_resource` | Cloud Control `CreateResource` | write |
| `update_resource` | Cloud Control `UpdateResource` (RFC 6902 JSON Patch) | write, destructive |
| `delete_resource` | Cloud Control `DeleteResource` | write, destructive, idempotent |
| `tag_resources` | Resource Groups Tagging `TagResources` | write, destructive (overwrites values), idempotent |
| `untag_resources` | Resource Groups Tagging `UntagResources` | write, destructive, idempotent |
| `cancel_resource_request` | Cloud Control `CancelResourceRequest` | write, destructive, idempotent |
| `get_resource_request` | Cloud Control `GetResourceRequestStatus` | read-only |
| `list_resource_requests` | Cloud Control `ListResourceRequests` | read-only |

The server's initialize instructions change with the mode, so the agent is told
up front that writes are live. Startup logs `read-write mode` at warn level.

The write tools use whatever identity the server runs with. In
[credential passthrough](authentication.md#credential-passthrough--the-client-forwards-its-own-credentials)
mode, that is the identity the calling user forwarded. Every write is logged at
info level (`[mcpserver.write] submitting AWS write`, with the tool, account,
region, type and target) before it is sent.

## Safety rules

- **Every write names its target.** `account_id` and `region` are required on
  every write-mode tool, even when the server reaches a single account. The
  client comes from `GetAccountClients`, so no other account is contacted. The
  server also checks that the client really is that account and region before
  sending anything. A pool that returns zero or several clients is refused.
- **Validation happens before AWS.** Account ID shape, region, resource type,
  JSON documents, patch operations, ARNs, tag keys and limits are all checked
  first. A malformed call costs nothing and cannot half-apply.
- **ARNs must match the target.** `tag_resources` / `untag_resources` reject an
  ARN whose account or region segment differs from `account_id` / `region`.
  ARNs with an empty region segment (S3, IAM) are accepted. A call is capped at
  200 ARNs and 50 tags, and `aws:`-prefixed keys are refused.
- **An accepted request is never reported as an error.** Cloud Control writes
  are asynchronous. Once AWS has accepted one, a wait that runs out or a status
  poll that fails means "not known yet", not "did not happen". So the result is
  `done: false` with the request token, plus a warning not to resubmit. An
  error there would invite a retry, and a retried create creates twice.

## Resource writes (Cloud Control)

Resource writes reach the same types, by the same CloudFormation names and
primary identifiers, as `list_resources_fallback`. Read a resource with that
tool (`view=detail`), then change it with these.

| Argument | Notes |
|----------|-------|
| `resource_type` | CloudFormation form, e.g. `AWS::Logs::LogGroup`. Normalized like the fallback, and the result warns when the spelling changed |
| `identifier` | Primary identifier, the fallback's `id`. Composite identifiers are joined with `\|` |
| `desired_state` | `create_resource`: a JSON object of properties, as a string or as an object |
| `patch_document` | `update_resource`: an RFC 6902 patch array. A single operation object is wrapped |
| `wait_seconds` | Default 120, max 900; `0` returns on acceptance. Status is polled every 2–15s |

Result: `{account_id, region, request, done, properties, warnings}`.

- **Rejected up front** (validation, AccessDenied): an error result saying
  nothing was changed.
- **`SUCCESS`**: for create and update, `properties` is read back with
  `GetResource`, so it shows what AWS applied, defaults included.
- **`FAILED` / `CANCEL_COMPLETE`**: an error result carrying `error_code`
  (`AlreadyExists`, `NotFound`, …) and `status_message`.
- **`PENDING` / `IN_PROGRESS`** with `done: false`: follow
  `request.request_token` with `get_resource_request`.

Properties the type's schema marks `createOnlyProperties` cannot be patched.
Cloud Control rejects such an update instead of replacing the resource.

## Tag writes (Resource Groups Tagging)

Tags are written by ARN, synchronously. awslib batches 20 ARNs per call. The
result is `{requested, succeeded, failed:[{arn, error_code, error_message,
status_code}], error, warnings}`:

- **Partial failure.** AWS reports per-resource failures inside an HTTP 200, so
  `failed` must be read. A partial failure is a normal result. It becomes an
  error only when nothing succeeded.
- **Stopped part-way.** A batch error after earlier batches landed is an error
  result. It warns that those batches may already be applied.

## Caching

A Cloud Control write evicts every cached Cloud Control list for its account
and region: once when AWS accepts it, and again after the wait. So
`list_resources_fallback` shows the change on its next call. The typed
repositories behind `list_resources` / `count_resources` cache under keys a
write cannot name. They may serve the previous state until `--cache-ttl`
expires, and the write result says so. Tag writes evict nothing. Use
`--no-cache` for a deployment that mostly writes.

## IAM permissions

The identity needs, beyond its read permissions:

- **Resource writes:** `cloudformation:CreateResource`, `UpdateResource`,
  `DeleteResource`, `GetResource`, `GetResourceRequestStatus`,
  `ListResourceRequests` and `CancelResourceRequest`. Cloud Control actions are
  authorized under the `cloudformation:` prefix. It also needs the underlying
  service's own write permissions (`logs:CreateLogGroup`, …), because the
  handler runs as the caller.
- **Tag writes:** `tag:TagResources` / `tag:UntagResources`, plus the
  per-service tagging permission (`ec2:CreateTags`, `s3:PutBucketTagging`, …).

The flag decides which tools exist. IAM decides what they can do. For a
production pool, run read-write instances with a role scoped to what they are
meant to change, or keep production accounts on a read-only instance.
