# Dockerfile — Minimal container for the Homescreen web app.
#
# Multi-stage build:
#   Stage 1: Compile the Go binary (uses the full Go SDK image)
#   Stage 2: Copy just the binary into a scratch image (~10MB total)
#
# The MQTT broker (Mosquitto) is NOT included — it runs separately.
# Point the config's mqtt.broker at the broker's address.
#
# Build:
#   docker build -t homescreen .
#
# Run:
#   docker run -p 8000:8000 -v /path/to/config.yaml:/etc/homescreen.yaml homescreen
#
# The config file is mounted at /etc/homescreen.yaml (the system-level
# search path). See config.go for details.

# ── Stage 1: Build ──
FROM golang:1.24-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a static binary
COPY . .
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -o /homescreen .

# ── Stage 2: Runtime ──
# scratch = empty image, nothing but our binary.
# We add ca-certificates in case the MQTT broker uses TLS.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /homescreen /homescreen

EXPOSE 8000
ENTRYPOINT ["/homescreen"]
