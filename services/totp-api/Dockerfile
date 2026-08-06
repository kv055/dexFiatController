# TOTP API is disabled for now (see commented totp-api service in
# docker-compose.yml). Re-enable this Dockerfile together with that service
# and the scraping TOTP_* env vars when browser-automation 2FA is needed again.
#
# FROM golang:1.22-alpine AS builder
# WORKDIR /app
# COPY go.mod go.sum ./
# RUN go mod download
# COPY . .
# RUN CGO_ENABLED=0 go build -o /totp-api .
#
# FROM alpine:3.19
# RUN apk add --no-cache ca-certificates
# COPY --from=builder /totp-api /usr/local/bin/totp-api
# EXPOSE 8080
# ENTRYPOINT ["totp-api"]
