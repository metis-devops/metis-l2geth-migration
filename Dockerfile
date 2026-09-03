# syntax=docker/dockerfile:1
FROM golang:1.27.1-alpine AS build
WORKDIR /app
RUN apk add --no-cache ca-certificates git
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o ./bin/ ./cmd/...

FROM alpine:latest
COPY --from=build /app/bin/l2state /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/l2state"]
