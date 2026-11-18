# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/collector-api ./cmd/collector-api

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/mirlan-irokez/rawbbit"
LABEL org.opencontainers.image.description="Rawbbit collector API"
LABEL org.opencontainers.image.licenses="Apache-2.0"

# An optional .env file is read from the working directory, as before.
WORKDIR /app

COPY --from=build /out/collector-api /usr/local/bin/collector-api

EXPOSE 8080

# Listens on 0.0.0.0:${PORT:-8080}. SIGTERM finishes in-flight requests,
# drains NATS and exits with the signal, like the previous uvicorn image.
ENTRYPOINT ["/usr/local/bin/collector-api"]
