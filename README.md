> [!CAUTION]
> This repository is 100% AI-generated slop. It should not be used by anyone, for any purpose, ever. You have been warned.

# Home Control

A smart home control panel that talks to devices over MQTT. It runs as a Go web server that serves a mobile-friendly UI and keeps every open browser tab in sync via Server-Sent Events (SSE).

The MQTT broker is the source of truth — the server holds no persistent state.

Installable as a PWA with offline support.

## How it works

```
┌──────────┐  SSE (live updates)   ┌──────────┐  subscribe   ┌───────────┐
│  Browser  │◄─────────────────────│ Go server │◄────────────│   MQTT    │
│          │  POST (user actions)  │  :8000   │  publish     │  broker   │
│          │─────────────────────►│          │────────────►│ (mosquitto)│
└──────────┘                      └──────────┘             └───────────┘
```

1. You open the page and the browser connects to `/api/events` (SSE)
2. The server sends the current state of every room as a snapshot
3. When you flip a toggle or drag a slider, the browser POSTs to the API
4. The server publishes the change to MQTT
5. The broker delivers it back to the server (and to any other subscribers)
6. The server pushes the update to all connected browsers via SSE

This means if someone changes the temperature from their phone, your tablet updates too. If an external system changes a value directly in MQTT, all browsers see it.

The server sends SSE heartbeat comments every 15 seconds to keep connections alive through proxies and mobile networks.

## Configuration

Config is searched in order:
1. `~/.config/homescreen/config.yaml` (user-level, for dev/desktop)
2. `/usr/local/etc/homescreen.yaml` (FreeBSD pkg convention)
3. `/etc/homescreen.yaml` (system-level, for Docker/servers)

```yaml
mqtt:
  broker: "tcp://localhost:1883"
  topic_prefix: zigbee2mqtt        # zigbee2mqtt prefix for light topics

zones:
  - name: Upstairs
    heating:
      - name: Bedroom
        unit_id: BedroomFaikin     # used to build MQTT topics
      - name: Guest Room
        unit_id: GuestFaikin
    lights:
      - name: Bedroom
        entities:                  # zigbee2mqtt entity names
          - BedroomLight
      - name: "Kids' Room — Lamp"
        entities:
          - KidsLamp

  - name: Downstairs
    heating:
      - name: Kitchen
        unit_id: KitchenFaikin
    lights:
      - name: Kitchen
        entities:
          - KitchenLight1
          - KitchenLight2          # multiple entities = light group

  - name: Secret Lair
    secret: true                   # hidden unless user enables secret mode
    heating:
      - name: Bunker
        unit_id: BunkerFaikin
```

**Zones** can optionally be marked `secret: true`. Secret zones are hidden from the UI by default. Users can reveal them by quickly tapping the "Lights" or "Heating" header 13 times in rapid succession. This toggles a flag in localStorage that persists across page loads.

**Heating** rooms use a `unit_id` to construct three MQTT topics per room:
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetHeatingCoolingState` — on/off (0/1)
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetTemperature` — target temp (e.g. "21.0")
- `HomeKit/{unit_id}_IndoorQuiet/Switch/On` — quiet mode (0/1)

**Lights** use zigbee2mqtt entities. Each light has a `name` and a list of `entities`. For each entity, the server:
- Subscribes to `{topic_prefix}/{entity}` for state (JSON payloads like `{"state":"ON"}`)
- Publishes to `{topic_prefix}/{entity}/set` for commands (JSON payloads like `{"state":"ON"}` or `{"state":"OFF"}`)

A light with multiple entities acts as a group: it shows **ON** if **any** entity is ON, and **OFF** only when **all** entities are OFF. Toggling a light publishes the command to **all** entities in the group.

The HTML is generated from this config, so adding a room or light is just a YAML edit and a restart.

## Zone logic

The UI has one temperature slider and one quiet toggle per zone, but each room is independent in MQTT.

- **Temperature displayed** = highest target temp across all **powered-on** rooms in the zone (falls back to all rooms if none are on)
- **Quiet shown as on** = only if every **powered-on** room in the zone has quiet on (falls back to all rooms if none are on)
- **Changing temp** publishes to all rooms in the zone (including powered-off rooms, so they have the correct setpoint when turned on)
- **Changing quiet** publishes to all rooms in the zone
- **On/off toggles** are per-room (not zone-wide)
- **Turning a room on** publishes the target temperature first, then the power-on message, so the unit starts at the correct setpoint

## PWA & offline support

The app is installable as a PWA (manifest + service worker + icons).

**Offline behavior:**
1. Service worker pre-caches the offline skeleton page and all static assets
2. If SSE disconnects, a full-page spinner overlay appears after 3.5 seconds
3. After 3 more seconds (6.5s total), the page reloads
4. The service worker intercepts the navigation — if the server is down (network error or 5xx), it serves an offline skeleton page with shimmer placeholder UI
5. The offline page auto-reloads every 5 seconds until the server recovers
6. When the server is back, the full page loads with fresh state from MQTT

The service worker also treats reverse proxy errors (e.g. Caddy returning 502) as offline.

## Running

### Prerequisites

- Go 1.24+
- Mosquitto (or any MQTT broker)

### Build and run

```bash
go build -o homescreen .
./homescreen
```

The binary embeds `templates/index.html` and the entire `static/` directory via `go:embed`, so you must rebuild after template or static asset changes.

The server starts on port 8000. Open http://localhost:8000.

### As a systemd service

```bash
sudo cp homescreen.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable homescreen
sudo systemctl start homescreen
```

Manage with `systemctl status homescreen`, `journalctl -u homescreen -f`.

## Testing

```bash
go test -v ./...
```

Requires mosquitto running on localhost:1883. To skip MQTT tests:

```bash
SKIP_INTEGRATION=1 go test -v ./...
```

There are 41 tests across five files:

| Level | File | What it tests |
|---|---|---|
| **Unit** | `config_test.go` | Config parsing, MQTT topic generation |
| **Unit** | `sse_test.go` | Broadcaster add/remove, broadcast, slow client handling |
| **Handler** | `handlers_test.go` | HTTP routing, error responses, template rendering, event building |
| **Integration** | `integration_test.go` | Full MQTT round-trip: publish → broker → cache |
| **E2E** | `e2e_test.go` | Complete flow: HTTP POST → MQTT → SSE → verify. Multi-client sync, external MQTT changes, reconnect snapshots |

Integration/e2e tests clean up retained MQTT messages after themselves.

## API

All POST endpoints accept `{"value": ...}` and return 204 on success.

| Endpoint | Value | Effect |
|---|---|---|
| `POST /api/heating/zone/{zone}/temperature` | number (e.g. `21`) | Sets target temp for all rooms in zone |
| `POST /api/heating/zone/{zone}/quiet` | boolean | Sets quiet mode for all rooms in zone |
| `POST /api/heating/room/{zone}/{room}/power` | boolean | Turns one room's heating on/off |
| `POST /api/light/{zone}/{name}/power` | boolean | Turns one light on/off |
| `GET /api/events` | — | SSE stream of state updates |

### SSE events

```json
{"type":"heating", "zone":"Upstairs", "room":"Bedroom", "power":true, "target_temp":21.0, "quiet":false}
{"type":"light",   "zone":"Upstairs", "name":"Bedroom",  "on":true}
```

On connect, the server sends a full snapshot (one event per room + per light), then streams live deltas. Heartbeat comments (`: heartbeat`) are sent every 15 seconds.

## Project structure

```
main.go                  Entry point — wires config, MQTT, SSE, HTTP; embeds templates + static
config.go                YAML config types and loader
mqtt.go                  MQTT client, subscriptions, cache, publish
sse.go                   SSE broadcaster with heartbeats (manages connected browser clients)
handlers.go              HTTP handlers (page with initial state snapshot, API, SSE endpoint)
templates/index.html     Go template — the full UI (HTML + CSS + JS)
static/sw.js             Service worker (offline fallback, asset caching)
static/offline.html      Offline skeleton page with shimmer placeholders
static/manifest.json     PWA manifest
static/inter.css         Vendored Inter font CSS
static/lucide.css        Vendored Lucide icon font CSS
static/fonts/            Vendored font files (Inter TTFs, Lucide woff2/woff/ttf)
static/icons/            PWA icons (192×192, 512×512)
homescreen.service       systemd unit file
```
