# syntax=docker/dockerfile:1
# Rawbbit ingestion runner (image name kept: rawbbit-dbt-runner). dbt-core,
# dbt-clickhouse and Python are no longer used: /app/bin/dbt-job is a static
# Go binary scheduled by Supercronic.

FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/dbt-job ./cmd/dbt-job

FROM --platform=$BUILDPLATFORM alpine:3.20 AS supercronic
ARG TARGETARCH=amd64
ARG SUPERCRONIC_VERSION=v0.2.48
RUN apk add --no-cache curl \
    && case "${TARGETARCH}" in \
         amd64) supercronic_sha256="88c1b66b94c486f972fdd1a4d1f901e3e75ff04f749cddd60c5db573e3a33c6c" ;; \
         arm64) supercronic_sha256="50ae8755e04fa72812d0a1bc47a112a856811cc91cce7b6c875c378a850788bc" ;; \
         *) echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/aptible/supercronic/releases/download/${SUPERCRONIC_VERSION}/supercronic-linux-${TARGETARCH}" \
       -o /supercronic \
    && echo "${supercronic_sha256}  /supercronic" | sha256sum -c - \
    && chmod 0755 /supercronic

# Supercronic runs crontab commands through /bin/sh, and the deployment
# helper uses `cat` inside the runner, so the final stage keeps a minimal
# busybox shell (no bash, git, curl or Python).
FROM alpine:3.20
ARG DBT_UID=1000
ARG DBT_GID=1000
ENV TZ=UTC
RUN apk add --no-cache ca-certificates \
    && addgroup -g "${DBT_GID}" dbt \
    && adduser -D -u "${DBT_UID}" -G dbt -s /sbin/nologin dbt \
    && mkdir -p /app/bin /app/runtime \
    && chown -R dbt:dbt /app
COPY --from=supercronic /supercronic /usr/local/bin/supercronic
COPY --from=build /out/dbt-job /app/bin/dbt-job
# `dbt debug|parse|build` remain available for deployment and maintenance.
RUN ln -s /app/bin/dbt-job /usr/local/bin/dbt
COPY crontab /app/crontab
WORKDIR /app
USER dbt

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD ["supercronic", "-test", "/app/crontab"]

CMD ["supercronic", "-passthrough-logs", "/app/crontab"]
