# syntax=docker/dockerfile:1
FROM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
ARG TOOL_VERSION=container-devel
WORKDIR /app
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath \
      -ldflags="-s -w -X=github.com/metis-devops/metis-l2geth-migration/internal/version.buildToolVersion=${TOOL_VERSION}" \
      -o ./bin/ ./cmd/...

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
COPY --from=build /app/bin/l2state /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/l2state"]
