# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/rawbbit-mcp-server ./cmd/rawbbit-mcp-server

# The image runs as root like the previous python:3.12-slim image: the
# protected MCP_DATASETS_FILE is mounted with mode 0600 and owned by the
# deploy user, which a non-root container user could not read.
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/rawbbit-mcp-server /usr/local/bin/rawbbit-mcp-server

EXPOSE 8000

ENTRYPOINT ["/usr/local/bin/rawbbit-mcp-server"]
