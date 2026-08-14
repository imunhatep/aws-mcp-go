# syntax=docker/dockerfile:1

# Multi-arch build: pass --platform (e.g. linux/amd64,linux/arm64) via buildx/podman.
# BuildKit injects BUILDPLATFORM/TARGETOS/TARGETARCH so the Go toolchain runs
# natively on the build host and cross-compiles for the target arch.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none

WORKDIR /src

# awslib is consumed through a `replace` directive pointing outside the build
# context, so dependencies come from the vendor/ tree instead of the module
# cache — no `go mod download`, and `-mod=vendor` below. Run `go mod vendor`
# before building (`make image` does it for you).
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=vendor -trimpath \
    -ldflags "-s -w \
    -X github.com/imunhatep/aws-mcp-go/internal/version.Version=${VERSION} \
    -X github.com/imunhatep/aws-mcp-go/internal/version.Commit=${COMMIT}" \
    -o /out/aws-mcp ./cmd/aws-mcp

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/aws-mcp /usr/local/bin/aws-mcp

# Listen on all interfaces so the server is reachable from outside the container.
ENV MCP_ADDR=0.0.0.0:3040
# The AWS credential chain resolves ~/.aws through $HOME; distroless does not
# set it, so mounting the host config at /home/nonroot/.aws is enough.
ENV HOME=/home/nonroot
EXPOSE 3040

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/aws-mcp"]
CMD ["serve"]
