# Homescreen — Agent Guide

Smart home control panel. Go backend + MQTT + SSE.

## Architecture

Stateless Go server on port 8000. MQTT broker is the source of truth.

```
Browser ←—SSE—→ Go server ←—MQTT—→ Mosquitto broker
Browser —POST→ Go server —publish→ Mosquitto broker
```

The server subscribes to all relevant MQTT topics on startup, keeps an in-memory cache of latest values, and pushes changes to browsers via SSE. It does not persist any state to disk.

The app is an installable PWA with offline support. Static assets (fonts, icons, CSS) are vendored and embedded in the binary. A service worker pre-caches the offline skeleton page and static assets.

## Key files

| File | Responsibility |
|---|---|
| `main.go` | Entry point. Loads config, connects MQTT, starts HTTP server. Embeds `templates/` and `static/` via `go:embed`. Serves `/static/` from embedded FS and `/sw.js` from root path (for service worker scope). |
| `config.go` | `Config`, `ZoneConfig`, `HeatingRoom`, `LightConfig` types. Loads config from `~/.config/homescreen/config.yaml`, `/usr/local/etc/homescreen.yaml`, or `/etc/homescreen.yaml` (first found). `HeatingTopics()` builds the 3 MQTT topics for a heating unit. `LightConfig` has `Name` and `Entities []string` (entity names under zigbee2mqtt). `MQTTConfig` has `TopicPrefix string` for the zigbee2mqtt topic prefix. |
| `mqtt.go` | `MQTTClient` — connects to broker, subscribes to topics from config, maintains `cache map[string]string`, calls `onChange` callback on every message. |
| `sse.go` | `SSEBroadcaster` — manages set of `chan string` clients, broadcasts JSON to all. Drops messages for slow clients rather than blocking. Sends heartbeat comments every 15s to keep connections alive through proxies/mobile. |
| `handlers.go` | `App` struct holds Config+MQTT+Broadcaster+Template. `PageData` includes config + `InitialState` (JSON snapshot of all device state, embedded in HTML so the page renders correctly on first paint without waiting for SSE). Routes: `GET /` (template), `GET /api/events` (SSE), POST endpoints for heating/lights. `TopicToEvent()` maps MQTT topic+value to JSON SSE event. |
| `templates/index.html` | Go `text/template` (NOT `html/template` — the latter breaks on complex JS in `<script>` tags). Receives `PageData` (config + initial state JSON). All zone/room/light HTML is generated from config. JS handles SSE, POST calls, zone aggregation. |
| `static/sw.js` | Service worker. Pre-caches offline page + static assets. Intercepts navigation requests — serves offline skeleton if server returns 5xx or network fails. |
| `static/offline.html` | Standalone offline skeleton page with shimmer placeholder UI matching the real layout. Shows spinner overlay, auto-retries via page reload every 5s. |
| `static/manifest.json` | PWA manifest. App name "Home Control", standalone display, theme color `#f4a942`. |
| `static/inter.css` | Vendored Inter font CSS (weights 400/500/600/700). |
| `static/lucide.css` | Vendored Lucide icon font CSS. |
| `static/fonts/` | Vendored font files: Inter TTFs + Lucide woff2/woff/ttf. |
| `static/icons/` | PWA icons: `icon-192.png`, `icon-512.png` (house icon on orange). |
| `homescreen.service` | systemd unit. Runs on port 8000, after mosquitto. |

## Config

Lives at `~/.config/homescreen/config.yaml` (searched first), `/usr/local/etc/homescreen.yaml` (FreeBSD), or `/etc/homescreen.yaml` (system). Defines MQTT broker address and zone/room/light mappings. The HTML template renders from this config, so it's the single source of truth for what rooms exist.

## Secret zones

Zones can be marked `secret: true` in config. Secret zones are hidden from the UI by default. Users can reveal them by quickly tapping the "Lights" or "Heating" header (column header on desktop, tab button on mobile) 13 times in rapid succession (<500ms between taps). This toggles a `secret` flag in localStorage, and the page adds/removes a `secret-mode` class on the body. Secret zones have a `secret-zone` CSS class that's hidden unless `body.secret-mode` is present.

## MQTT topic patterns

### Light state request on startup

zigbee2mqtt doesn't retain light state messages by default. After subscribing to all topics, the server publishes `{"state":""}` to `{topic_prefix}/{entity}/get` for every light entity. zigbee2mqtt responds by publishing the device's current state to the state topic, which populates the cache before any browser connects. Mains-powered devices respond immediately; battery/sleepy devices may not respond until their next check-in.

Heating rooms use a `unit_id` to construct three topics:
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetHeatingCoolingState` — power (0/1)
- `HomeKit/{unit_id}_Thermostat/Thermostat/TargetTemperature` — temp (e.g. "21.0")
- `HomeKit/{unit_id}_IndoorQuiet/Switch/On` — quiet (0/1)

Lights use zigbee2mqtt topics. Each light has a list of entities. For each entity:
- Subscribe: `{topic_prefix}/{entity}` — state payloads are JSON, e.g. `{"state":"ON","brightness":254}`
- Subscribe: `{topic_prefix}/{entity}/availability` — availability payloads are JSON `{"state":"online"}` / `{"state":"offline"}` or plain strings `online` / `offline`
- Publish: `{topic_prefix}/{entity}/set` — command payloads are `{"state":"ON"}`, `{"state":"OFF"}`, or `{"brightness":128}`

**Unavailable entities** are treated as OFF for display purposes. If an entity's availability topic reports `offline`, it is skipped in the any-on logic, brightness aggregation, and brightness_on calculation. Entities with no availability message in the cache are assumed available (default). Commands are still sent to unavailable entities (they take effect when the device reconnects).

A light group with multiple entities uses **any-on logic**: the group shows ON if any entity is ON, and OFF only when all entities are OFF. Toggling a light publishes to ALL entities in the group.

**Brightness** is optional per-entity (not per-light group). Not all entities in a group may support it — only those whose zigbee2mqtt payload includes a `brightness` field (0-254) will receive brightness commands. The UI shows a brightness slider when any entity in the group reports brightness. For groups, the displayed brightness is the max across entities that support it. Setting brightness publishes `{"brightness":N}` only to entities that have previously reported brightness in their cached state **and are currently ON** — dragging the slider never turns on an OFF light. When turning a light ON via the power toggle, the frontend includes the current slider value as `"brightness"` in the request; the backend sends `{"state":"ON","brightness":N}` to brightness-capable entities so they start at the right level. The slider is colored grey when all brightness-capable entities are OFF, and orange when at least one is ON.

Config format uses `entities: [entity1, entity2]` (not a single `topic:` field).

All heating publishes are retained (QoS 1). Light publishes use zigbee2mqtt conventions.

## Zone aggregation

Done in frontend JS, not the server:
- **Zone temperature** = `Math.max()` of all **powered-on** rooms' `target_temp` (falls back to all rooms if none are on)
- **Zone quiet** = all **powered-on** rooms must have `quiet === true` (falls back to all rooms if none are on)
- Setting zone temp/quiet publishes to ALL rooms via the backend
- When setting zone temp, also publishes to any powered-off rooms so they start at the correct setpoint when turned on
- Room power toggles are individual

## Heating power-on logic

When turning a room ON via `handleRoomPower`, the backend publishes the cached target temperature first, then the power-on message. This ensures the unit starts at the correct setpoint rather than whatever stale value it had.

## API

```
POST /api/heating/zone/{zone}/temperature  {"value": 21}      → publishes to all rooms
POST /api/heating/zone/{zone}/quiet        {"value": true}    → publishes to all rooms
POST /api/heating/room/{zone}/{room}/power {"value": true}    → single room
POST /api/light/{zone}/{name}/power        {"value": true}    → single light
POST /api/light/{zone}/{name}/power        {"value": true, "brightness": 128} → with initial brightness
POST /api/light/{zone}/{name}/brightness   {"value": 128}     → single light (0-254), only ON entities
GET  /api/events                           SSE stream
```

SSE sends JSON per line:
- Heating: `data: {"type":"heating","zone":"...","room":"...","power":true,"target_temp":21.0,"quiet":false}`
- Light (no brightness): `data: {"type":"light","zone":"...","name":"...","on":true}`
- Light (with brightness): `data: {"type":"light","zone":"...","name":"...","on":true,"brightness":200,"brightness_on":true}`

`brightness_on` indicates whether any brightness-capable entity in the group is ON (used to grey out the slider when all dimmable lights are off). Both `brightness` and `brightness_on` are omitted when no entity reports brightness.

On connect, sends a full snapshot (one event per room + light), then live deltas. Heartbeat comments (`: heartbeat`) sent every 15s.

## Offline / PWA behavior

1. Browser registers service worker from `/sw.js` (served from root for full scope)
2. SW pre-caches offline skeleton page + all static assets (fonts, CSS)
3. On SSE disconnect, a spinner overlay appears after **3.5 seconds**
4. After **3 more seconds** (6.5s total), the page reloads — the SW intercepts the navigation and serves the offline skeleton if the server is down (5xx or network error)
5. The offline skeleton page auto-reloads every 5s until the server recovers
6. When the server is back, the full page loads with fresh state

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

49 tests across five files. Integration/e2e tests clean up retained MQTT messages after themselves.

## Build and deploy

```bash
go build -o homescreen .
sudo systemctl restart homescreen
```

The binary embeds `templates/index.html` and the entire `static/` directory via `go:embed`, so you must rebuild after template or static asset changes.

## Important gotchas

- **Use `text/template`, not `html/template`**: The HTML template contains complex JavaScript with template literals (backtick strings). Go's `html/template` contextual escaper corrupts the `<script>` content, silently truncating the output. `text/template` works correctly. This is safe because all template data comes from our own config, not user input.
- **MQTT client ID conflicts**: The server generates a unique client ID using a nanosecond timestamp (`homescreen-{UnixNano}`), so multiple instances can run simultaneously. Tests also generate unique IDs. You may still see "connection lost: EOF" in test logs if clients disconnect — that's expected.
- **Retained messages**: All publishes are retained. Tests must clean up retained messages to avoid polluting subsequent test runs. The `clearRetained()` helper publishes empty payloads.
- **Debounce on temp slider**: The frontend debounces temperature changes (300ms) to avoid flooding MQTT while dragging. The slider doesn't update from SSE while the user is actively dragging (`:active` pseudo-class check).
- **SSE heartbeats**: The server sends `: heartbeat\n\n` every 15 seconds. Without this, proxies (Caddy, exe.dev proxy) and mobile Safari kill idle TCP connections after ~30s.
- **Service worker scope**: `sw.js` is served from `/sw.js` (not `/static/sw.js`) so it has root scope and can intercept navigation requests to `/`.
- **5xx as offline**: The service worker treats HTTP 5xx responses (e.g. Caddy 502 when backend is down) the same as network failures — serves the offline skeleton page.

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

## Workflow

Always `git push` after finishing work if all tests pass.
