# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build

WORKDIR /src

# Download dependencies first so Docker caches this layer separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /app main.go

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM alpine:3.21

# ca-certificates needed for outbound TLS if ever required.
RUN apk --no-cache add ca-certificates wget

WORKDIR /app

COPY --from=build /app /app/cache-proxy

# Respect Bunny Magic Container CPU share limit.
ENV GOMAXPROCS=8

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/health || exit 1

CMD ["/app/cache-proxy"]
