FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# timetzdata embeds the time zones, so TZ works in the distroless image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags timetzdata -ldflags="-s -w" -o /bot ./cmd/bot

# distroless static: no shell, CA certificates included, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /bot /app/bot
USER nonroot:nonroot
ENTRYPOINT ["/app/bot", "-config", "/app/config.toml"]
