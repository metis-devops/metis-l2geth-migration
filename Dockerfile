# syntax=docker/dockerfile:1
FROM golang:1.27.0-alpine AS build
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
    -ldflags="-s -w" \
    -o ./bin/ ./cmd/...

FROM alpine:latest
COPY --from=build /app/bin/l2state /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/l2state"]
