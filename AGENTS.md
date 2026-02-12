# Homescreen — Agent Guide

Smart home control panel. Go backend + MQTT + SSE.

## Architecture

Stateless Go server on port 8000. MQTT broker is the source of truth.

```
Browser ←—SSE—→ Go server ←—MQTT—→ Mosquitto broker
Browser —POST→ Go server —publish→ Mosquitto broker
```

The server subscribes to all relevant MQTT topics on startup, keeps an in-memory cache of latest values, and pushes changes to browsers via SSE. It does not persist any state to disk.

## Key files

| File | Responsibility |
|---|---|
| `main.go` | Entry point. Loads config, connects MQTT, starts HTTP server. |
| `config.go` | `Config`, `ZoneConfig`, `HeatingRoom`, `LightConfig` types. Loads `~/.config/homescreen/config.yaml`. `HeatingTopics()` builds the 3 MQTT topics for a heating unit. |
| `mqtt.go` | `MQTTClient` — connects to broker, subscribes to topics from config, maintains `cache map[string]string`, calls `onChange` callback on every message. |
| `sse.go` | `SSEBroadcaster` — manages set of `chan string` clients, broadcasts JSON to all. Drops messages for slow clients rather than blocking. |
| `handlers.go` | `App` struct holds Config+MQTT+Broadcaster+Template. Routes: `GET /` (template), `GET /api/events` (SSE), POST endpoints for heating/lights. `TopicToEvent()` maps MQTT topic+value to JSON SSE event. |
| `templates/index.html` | Go `text/template` (NOT `html/template` — the latter breaks on complex JS in `<script>` tags). Receives `*Config` as data. All zone/room/light HTML is generated from config. JS handles SSE, POST calls, zone aggregation. |
| `homescreen.service` | systemd unit. Runs on port 8000, after mosquitto. |

## Config

Lives at `~/.config/homescreen/config.yaml`. Defines MQTT broker address and zone/room/light mappings. The HTML template renders from this config, so it's the single source of truth for what rooms exist.

## MQTT topic patterns

Heating rooms use a `unit_id` to construct three topics:
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetHeatingCoolingState` — power (0/1)
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetTemperature` — temp (e.g. "21.0")
- `HomeKit/{unit_id}_IndoorQuiet/Switch/On` — quiet (0/1)

Lights have an arbitrary topic per light (configured in YAML). Value is 0/1.

All publishes are retained (QoS 1).

## Zone aggregation

Done in frontend JS, not the server:
- **Zone temperature** = `Math.max()` of all rooms' `target_temp`
- **Zone quiet** = all rooms must have `quiet === true`
- Setting zone temp/quiet publishes to ALL rooms via the backend
- Room power toggles are individual

## API

```
POST /api/heating/zone/{zone}/temperature  {"value": 21}      → publishes to all rooms
POST /api/heating/zone/{zone}/quiet        {"value": true}    → publishes to all rooms
POST /api/heating/room/{zone}/{room}/power {"value": true}    → single room
POST /api/light/{zone}/{name}/power        {"value": true}    → single light
GET  /api/events                           SSE stream
```

SSE sends JSON per line: `data: {"type":"heating","zone":"...","room":"...","power":true,"target_temp":21.0,"quiet":false}`

On connect, sends a full snapshot (one event per room + light), then live deltas.

## Tests

```bash
go test -v ./...                    # all tests (needs mosquitto on localhost:1883)
SKIP_INTEGRATION=1 go test -v ./... # unit + handler tests only
```

Test files:
- `config_test.go` — config parsing, topic generation
- `sse_test.go` — broadcaster add/remove, broadcast, slow client handling
- `handlers_test.go` — HTTP handlers with fake MQTT (no broker needed)
- `integration_test.go` — real MQTT round-trips
- `e2e_test.go` — full HTTP+MQTT+SSE flows, multi-client sync, external changes

Integration/e2e tests clean up retained MQTT messages after themselves.

## Build and deploy

```bash
go build -o homescreen .
sudo systemctl restart homescreen
```

The binary embeds `templates/index.html` via `go:embed`, so you must rebuild after template changes.

## Important gotchas

- **Use `text/template`, not `html/template`**: The HTML template contains complex JavaScript with template literals (backtick strings). Go's `html/template` contextual escaper corrupts the `<script>` content, silently truncating the output. `text/template` works correctly. This is safe because all template data comes from our own config, not user input.
- **MQTT client ID conflicts**: Each test creates its own MQTT client. Mosquitto disconnects existing clients when a new client connects with the same ID. The paho library uses auto-reconnect to handle this, but you'll see "connection lost: EOF" in test logs — that's expected.
- **Retained messages**: All publishes are retained. Tests must clean up retained messages to avoid polluting subsequent test runs. The `clearRetained()` helper publishes empty payloads.
- **Debounce on temp slider**: The frontend debounces temperature changes (300ms) to avoid flooding MQTT while dragging. The slider doesn't update from SSE while the user is actively dragging (`:active` pseudo-class check).

## Adding new device types

To add a new device type (e.g. blinds):
1. Add a new config struct in `config.go` (like `LightConfig`)
2. Add it to `ZoneConfig`
3. Subscribe to its topics in `mqtt.go` (via `allTopics()`)
4. Add a `buildBlindEvent()` and new SSE event type in `handlers.go`
5. Add API endpoint(s) in `handlers.go` + `SetupRoutes()`
6. Add UI section in `templates/index.html`
7. Add tests

## Services

| Service | Port | Managed by |
|---|---|---|
| mosquitto | 1883 (localhost only) | `systemctl status mosquitto` |
| homescreen | 8000 | `systemctl status homescreen` |
