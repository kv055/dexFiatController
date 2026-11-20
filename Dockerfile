FROM golang:1.27-alpine AS builder
ARG GOPROXY=https://proxy.golang.org|https://goproxy.io|direct
ENV GOPROXY=${GOPROXY}

RUN apk add --no-cache git

WORKDIR /build/dexFiatController

# commons/ is a workspace dependency via `replace commons => ../commons`.
# The build context is the repo root, so copy it to the parent of this module.
COPY commons/ ../commons/
COPY dexFiatController/go.mod dexFiatController/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    sh -ec 'for attempt in 1 2 3 4 5; do go mod download && exit 0; echo "go mod download attempt $attempt failed, retrying..."; sleep $((attempt * 10)); done; exit 1'

COPY dexFiatController/ .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /dexFiatController .

FROM alpine:3.24

# Scraping (SCRAPING_ENABLED) is off by default; Chromium is only needed when
# re-enabling browser automation for PayPal/Wise/Revolut connectors.
# RUN apk add --no-cache chromium nss freetype harfbuzz ca-certificates ttf-freefont zstd
# ENV CHROME_PATH=/usr/bin/chromium-browser
# ENV HEADLESS=true
RUN apk add --no-cache ca-certificates wget

ENV PUSHPUSH_ADDR=:8080

# pushpush data dir (rules.json + notifications.jsonl persisted here).
RUN mkdir -p /data
VOLUME ["/data"]

EXPOSE 8080

COPY --from=builder /dexFiatController /usr/local/bin/dexFiatController

ENTRYPOINT ["dexFiatController"]
