FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /orcaterm2api ./cmd/server

FROM alpine:3.20
RUN adduser -D -u 10001 app && apk add --no-cache ca-certificates tzdata
USER app
WORKDIR /app
COPY --from=builder /orcaterm2api /app/orcaterm2api
EXPOSE 8317
ENTRYPOINT ["/app/orcaterm2api", "-c", "/app/config.yaml"]
