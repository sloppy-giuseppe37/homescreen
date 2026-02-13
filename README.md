> [!CAUTION]
> This repository is 100% AI-generated slop. It should not be used by anyone, for any purpose, ever. You have been warned.

# Home Control

A smart home control panel that talks to devices over MQTT. It runs as a Go web server that serves a mobile-friendly UI and keeps every open browser tab in sync via Server-Sent Events (SSE).

The MQTT broker is the source of truth — the server holds no persistent state.

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

## Configuration

All config lives in `~/.config/homescreen/config.yaml`.

```yaml
mqtt:
  broker: "tcp://localhost:1883"

zones:
  - name: Upstairs
    heating:
      - name: Bedroom
        unit_id: BedroomFaikin     # used to build MQTT topics
      - name: Guest Room
        unit_id: GuestFaikin
    lights:
      - name: Bedroom
        topic: HomeKit/BedroomLight/Lightbulb/On   # literal MQTT topic
      - name: "Kids' Room — Lamp"
        topic: HomeKit/KidsLamp/Lightbulb/On

  - name: Downstairs
    heating:
      - name: Kitchen
        unit_id: KitchenFaikin
    lights:
      - name: Kitchen
        topic: HomeKit/KitchenLight/Lightbulb/On
```

**Heating** rooms use a `unit_id` to construct three MQTT topics per room:
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetHeatingCoolingState` — on/off (0/1)
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetTemperature` — target temp (e.g. "21.0")
- `HomeKit/{unit_id}_IndoorQuiet/Switch/On` — quiet mode (0/1)

**Lights** use an arbitrary `topic` directly — no naming convention assumed.

The HTML is generated from this config, so adding a room or light is just a YAML edit and a restart.

## Zone logic

The UI has one temperature slider and one quiet toggle per zone, but each room is independent in MQTT.

- **Temperature displayed** = highest target temp across all rooms in the zone
- **Quiet shown as on** = only if every room in the zone has quiet on
- **Changing temp** publishes to all rooms in the zone
- **Changing quiet** publishes to all rooms in the zone
- **On/off toggles** are per-room (not zone-wide)

## Running

### Prerequisites

- Go 1.24+
- Mosquitto (or any MQTT broker)

### Build and run

```bash
go build -o homescreen .
./homescreen
```

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

There are 30 tests across four levels:

| Level | What it tests |
|---|---|
| **Unit** | Config parsing, MQTT topic generation, SSE broadcaster |
| **Handler** | HTTP routing, error responses, template rendering, event building |
| **Integration** | Full MQTT round-trip: publish → broker → cache |
| **E2E** | Complete flow: HTTP POST → MQTT → SSE → verify. Multi-client sync, external MQTT changes, reconnect snapshots |

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

## Project structure

```
main.go              Entry point — wires config, MQTT, SSE, HTTP together
config.go            YAML config types and loader
mqtt.go              MQTT client, subscriptions, cache, publish
sse.go               SSE broadcaster (manages connected browser clients)
handlers.go          HTTP handlers (page, API, SSE endpoint)
templates/index.html Go template — the full UI (HTML + CSS + JS)
homescreen.service   systemd unit file
```
