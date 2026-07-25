# Builder must be >= the `go` directive in go.mod. Official golang images pin
# GOTOOLCHAIN=local, so an older builder cannot silently fall back to its own
# older stdlib — keep this tag in step with go.mod when that directive moves.
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app/bin/app ./cmd/app
RUN CGO_ENABLED=0 go build -o /app/bin/migrate ./cmd/migrate

FROM alpine:3.22
# busybox already provides wget for the healthcheck, so only certs are needed.
RUN apk --no-cache add ca-certificates \
    && adduser -D -u 10001 app
WORKDIR /app
COPY --from=builder /app/bin/app .
COPY --from=builder /app/bin/migrate .
COPY --from=builder /app/db ./db
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8080}/health" || exit 1
# Run migrations then start app (avoids "no such table: ride_requests" on fresh deploy)
CMD ["sh", "-c", "./migrate -up && exec ./app"]
