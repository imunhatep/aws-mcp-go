# Configuration

## Commands

| Command | Description |
|---------|-------------|
| `serve` | Run the MCP server (streamable HTTP, served at `/mcp`) |
| `version` | Print version and commit |

## `serve` flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--addr` | `MCP_ADDR` | `:3040` | Listen address (MCP served at `/mcp`) |
| `--cache-ttl` | `MCP_CACHE_TTL` | `6h` | TTL for cached resource listings (e.g. `6h`, `30m`) |
| `--cache-dir` | `MCP_CACHE_DIR` | OS temp dir | On-disk cache directory; empty = in-memory only |
| `--no-cache` | | `false` | Disable caching entirely |
| `--profiles` | `MCP_AWS_PROFILES` | | Comma-separated AWS shared-config profiles to serve, one account each (excludes the assume-role flags) |
| `--assume-role` | | `false` | Auto-discover assumable roles from the current IAM role |
| `--assume-role-arns` | `MCP_ASSUME_ROLE_ARNS` | | Explicit assumable role ARNs (implies assume-role mode) |
| `--verbose` / `-v` | `AWS_MCP_VERBOSE`, `LOG_LEVEL` | `3` | Log verbosity: `0`=fatal … `5`=trace (global flag) |

The authentication flags are covered in [authentication.md](authentication.md).

## Endpoint

`serve` listens on `--addr` and serves one path:

- `/mcp` — MCP streamable-HTTP endpoint (POST for JSON-RPC requests, GET for the
  SSE stream). Connect your client here.

## Caching

Results are cached for `--cache-ttl` (default **6 hours**) in an in-memory
bigcache, plus an on-disk layer when `--cache-dir` is set (it defaults to the OS
temp dir). `--no-cache` disables both. The cache covers resource listings and
Cost Explorer answers alike — relevant because Cost Explorer bills per request.

To keep the on-disk cache across container restarts, see
[container.md](container.md#persisting-the-resource-cache).

## Client configuration

Claude Code:

```sh
claude mcp add --transport http aws http://127.0.0.1:3040/mcp
```

Generic MCP client config:

```json
{
  "mcpServers": {
    "aws": {
      "url": "http://localhost:3040/mcp"
    }
  }
}
```
