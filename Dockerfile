# syntax=docker/dockerfile:1

ARG GO_VERSION=1.25.4

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/opensqt_market_maker .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 -s /sbin/nologin opensqt \
    && mkdir -p /app/log \
    && chown -R opensqt:opensqt /app

WORKDIR /app

COPY --from=builder --chown=opensqt:opensqt /out/opensqt_market_maker /usr/local/bin/opensqt_market_maker
COPY --chown=opensqt:opensqt config.example.yaml /app/config.example.yaml

ARG VERSION=dev
ARG SOURCE_REPO=https://github.com/zuoban/opensqt_market_maker

LABEL org.opencontainers.image.title="OpenSQT Market Maker" \
      org.opencontainers.image.description="High-frequency crypto market maker" \
      org.opencontainers.image.source="${SOURCE_REPO}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

USER opensqt

EXPOSE 8787

VOLUME ["/app/log"]

STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/opensqt_market_maker"]
