# Development

Requires **Go 1.25+** (a floor introduced by the `github.com/mark3labs/mcp-go`
dependency); the module targets the Go 1.26 toolchain.

## The awslib dependency

This module consumes `awslib` as a normal tagged dependency
(`github.com/imunhatep/awslib v0.5.0`), resolved from the module proxy — no local
checkout or `replace` directive is required to build. To test an unreleased
`awslib` change, add a `replace` locally (`go mod edit -replace
github.com/imunhatep/awslib=../pkgs/awslib`) and drop it again before committing.

Dependencies are vendored — run `make tidy` (`go mod tidy && go mod vendor`)
after any dependency change, or the build fails with "inconsistent vendoring".
The container build reads `vendor/` too.

## Make targets

| Target | What it does |
|---|---|
| `make build` | Build `bin/aws-mcp` with version/commit ldflags |
| `make test` | `go test ./...` |
| `make tidy` | `go mod tidy && go mod vendor` |
| `make run` | Build, then `serve` |
| `make image` | Vendor, then build `$(IMAGE):$(VERSION)` (defaults to `ghcr.io/imunhatep/aws-mcp-go:latest`) with podman |
| `make image-multiarch` | Same as a `linux/amd64,linux/arm64` manifest |

## CI & releases

Two GitHub Actions workflows live in `.github/workflows`:

| Workflow | Trigger | What it produces |
|---|---|---|
| `ci.yml` | push to `master`/`main`, PRs, manual | gofmt/vet gates, `go test -race`, `make build`, and a container build (not pushed) |
| `release.yml` | push of a `v*` tag | `darwin,linux` × `amd64,arm64` binary tarballs + `checksums.txt` on a GitHub release, and a multi-arch image pushed to `ghcr.io/imunhatep/aws-mcp-go` |

Release image tags come from `docker/metadata-action`: the full version,
`major.minor`, `major`, plus `latest` for tags without a prerelease suffix.

Both workflows build the committed `go.mod` as-is, so `awslib` has to be
**tagged and pushed** — and the new version committed in `require` — before a
release tag here can build against it. The image jobs run `go mod vendor` first,
because `vendor/` is gitignored but the `Containerfile` builds with
`-mod=vendor`.

## Conventions

- **zerolog** for logging, message prefix `[pkg.Func] …`
- **urfave/cli v3** for the CLI
- **`pkg/errors`** (this module's own dependency-free package) for error wrapping
