# syntax=docker/dockerfile:1.7
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# /app/data must exist in the image owned by nonroot: a fresh named volume
# inherits the ownership of the image directory it is mounted over, and
# distroless has no shell to chown it afterwards.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/bot ./cmd/bot \
    && mkdir -p /app/data

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build --chown=65532:65532 /app /app
USER nonroot:nonroot
# CWD is the mounted volume. Every writable path in the bot is relative
# (bot.log, bot_state.json, appointments.db, config.json), so they all land
# here and survive a redeploy.
WORKDIR /app/data
ENTRYPOINT ["/app/bot"]
