FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/raw-writer ./cmd/raw-writer

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/mirlan-irokez/rawbbit"
LABEL org.opencontainers.image.description="Rawbbit raw writer"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

COPY --from=build /out/raw-writer /usr/local/bin/raw-writer

CMD ["/usr/local/bin/raw-writer"]
