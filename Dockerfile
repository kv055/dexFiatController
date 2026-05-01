FROM golang:1.25-alpine AS builder
ARG GOPROXY=https://goproxy.io,https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

RUN apk add --no-cache git chromium

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /dexFiatController .

FROM alpine:3.19

RUN apk add --no-cache chromium nss freetype harfbuzz ca-certificates ttf-freefont

ENV CHROME_PATH=/usr/bin/chromium-browser
ENV HEADLESS=true

COPY --from=builder /dexFiatController /usr/local/bin/dexFiatController

ENTRYPOINT ["dexFiatController"]
