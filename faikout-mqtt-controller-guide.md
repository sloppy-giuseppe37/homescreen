# Faikout MQTT Controller Implementation Guide

A reference for implementing an MQTT-based controller for the **Faikout** (formerly Faikin) ESP32 module by [revk](https://github.com/revk/ESP32-Faikout). Faikout replaces Daikin's cloud-based WiFi modules with local control via web UI, MQTT, and Home Assistant.

---

## Table of Contents

1. [Overview](#overview)
2. [MQTT Topic Structure](#mqtt-topic-structure)
3. [Commands](#commands)
4. [Status Messages](#status-messages)
5. [The `control` Command (JSON)](#the-control-command-json)
6. [Settings](#settings)
7. [Faikout Auto Mode](#faikout-auto-mode)
8. [Home Assistant Integration](#home-assistant-integration)
9. [Enum Reference](#enum-reference)
10. [Practical Examples](#practical-examples)

---

## Overview

Faikout is an ESP32-based board that plugs into a Daikin air conditioner's S21/X50A connector and communicates over the Daikin serial protocol. It exposes control via:

- **Local web UI** at `http://{hostname}.local`
- **MQTT** for programmatic control and monitoring
- **Home Assistant** auto-discovery over MQTT

The device is identified by its **hostname** (e.g. `GuestAC`), which you set during initial WiFi configuration. All MQTT topics use this hostname.

**Source repo:** <https://github.com/revk/ESP32-Faikout>

---

## MQTT Topic Structure

Topics follow the pattern: `{prefix}/{hostname}[/{suffix}]`

The device uses the [RevK ESP32 library](https://github.com/revk/ESP32-RevK) conventions. Default prefixes:

| Prefix | Direction | Retained | Purpose |
|-----------|-----------|----------|---------|
| `command`  | → device  | No       | Send commands to the device |
| `setting`  | → device  | No       | Read/write persistent settings |
| `state`    | ← device  | Yes      | Current state (published periodically + on change) |
| `event`    | ← device  | No       | One-off event notifications |
| `info`     | ← device  | No       | Informational messages |
| `error`    | ← device  | No       | Error reports |

### Key topics your controller should use

| Topic | Description |
|-------|-------------|
| `state/{hostname}` | Device online status. Payload `false` = offline (LWT). |
| `state/{hostname}/status` | JSON: full AC state (published every `reporting` seconds, default 60) |
| `command/{hostname}` | Send the `control` JSON command here (no suffix) |
| `command/{hostname}/{cmd}` | Simple commands like `on`, `off`, `heat`, `cool`, `temp`, etc. |
| `setting/{hostname}` | Send JSON to set multiple settings; empty payload → returns current settings |
| `setting/{hostname}/{key}` | Set a single setting; payload is the value |
| `Faikout/{hostname}` | Periodic logging messages (for database storage via `faikoutlog`) |

### Subscribing

A controller should subscribe to:
```
state/{hostname}/#
error/{hostname}/#
event/{hostname}/#
```

---

## Commands

Send to `command/{hostname}/{command}`. Payload is the argument (if any).

### Simple Commands

| Command | Payload | Description |
|---------|---------|-------------|
| `on` | *(none)* | Power on |
| `off` | *(none)* | Power off |
| `heat` | *(none)* | Set mode to Heat |
| `cool` | *(none)* | Set mode to Cool |
| `auto` | *(none)* | Set mode to Auto |
| `fan` | *(none)* | Set mode to Fan |
| `dry` | *(none)* | Set mode to Dry |
| `low` | *(none)* | Set fan to low |
| `medium` | *(none)* | Set fan to medium |
| `high` | *(none)* | Set fan to high |
| `temp` | `22.5` | Set target temperature (°C) |
| `status` | *(none)* | Force an immediate status report |
| `send` | `D62000` | Send raw S21 message (advanced/debug) |

### The `control` Command (JSON)

Send to `command/{hostname}` (no suffix). Payload is a JSON object.

This is the **primary method** for programmatic control — it lets you set multiple parameters atomically.

```json
{
  "power": true,
  "mode": "C",
  "temp": 22.5,
  "fan": "A",
  "swingv": true,
  "swingh": false
}
```

All fields are optional — include only what you want to change.

#### Writable Control Fields

| Field | Type | Values | Description |
|-------|------|--------|-------------|
| `power` | boolean | `true`/`false` | AC power on/off |
| `mode` | string | `H`, `C`, `A`, `D`, `F` | Heat, Cool, Auto, Dry, Fan |
| `temp` | number | 16.0–32.0 (typically, 0.5° steps on S21) | Target temperature °C |
| `fan` | string | `A`, `1`–`5`, `Q` | Auto, speed 1–5, Quiet |
| `swingh` | boolean | `true`/`false` | Horizontal louvre swing |
| `swingv` | boolean | `true`/`false` | Vertical louvre swing |
| `econo` | boolean | `true`/`false` | Economy mode |
| `powerful` | boolean | `true`/`false` | Powerful/boost mode |
| `comfort` | boolean | `true`/`false` | Comfort mode |
| `streamer` | boolean | `true`/`false` | Streamer (air purifier) |
| `sensor` | boolean | `true`/`false` | Intelligent eye sensor |
| `led` | boolean | `true`/`false` | LED indicator on unit |
| `quiet` | boolean | `true`/`false` | Quiet mode |
| `demand` | integer | 30–100 | Demand control percentage |

#### Faikout Auto Control Fields

These fields engage or configure the Faikout's built-in auto mode:

| Field | Type | Description |
|-------|------|-------------|
| `env` | number | External/reference temperature (must be sent regularly, within `tcontrol` seconds) |
| `target` | number or [min, max] | Target temperature. Single number or `[min, max]` array. Engages Faikout auto. |
| `margin` | number | When `target` is a single number: min = target - margin/2, max = target + margin/2 |
| `autor` | number | Auto range setting (0 = off). Persisted. |
| `autot` | number | Auto target temperature. Persisted. |
| `autob` | string | BLE sensor device name. Persisted. |
| `auto0` | string | Auto off time `HH:MM` (`00:00` = disabled). Persisted. |
| `auto1` | string | Auto on time `HH:MM` (`00:00` = disabled). Persisted. |

> **Important:** When using remote control (sending `env` + `target`), the `control` command must be sent **repeatedly** (at least every `tcontrol` seconds, default 600s) or the device reverts to local/non-auto mode. Include `env` in every message.

---

## Status Messages

Published to `state/{hostname}/status` as retained JSON.

### Read-Only Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `online` | boolean | AC unit connected and communicating |
| `control` | boolean | Device is under external/automatic control |
| `heat` | boolean | Currently in heating mode |
| `slave` | boolean | Not master for heat/cool (cannot change mode) |
| `antifreeze` | boolean | Antifreeze mode active |
| `model` | string | AC model name (if known, max 20 chars) |
| `home` | number | Room temperature measured by AC (°C) |
| `outside` | number | Outdoor unit temperature (°C) |
| `inlet` | number | Inlet/heat exchanger temperature (°C) |
| `liquid` | number | Liquid coolant temperature (°C) |
| `fanrpm` | integer | Fan speed in RPM |
| `comp` | integer | Compressor frequency (Hz) |
| `anglev` | integer | Vertical louver angle |
| `hum` | number | Indoor humidity (%) |
| `Whoutside` | integer | Lifetime energy usage, outside unit (Wh) |
| `Whheating` | integer | Lifetime heating energy (Wh) |
| `Whcooling` | integer | Lifetime cooling energy (Wh) |
| `consumption` | integer | Current power consumption (W) |
| `flap` | boolean | Flap status |

### Writable Fields in Status

All writable control fields (`power`, `mode`, `temp`, `fan`, `swingh`, `swingv`, `econo`, `powerful`, `comfort`, `streamer`, `sensor`, `led`, `quiet`, `demand`) are also included in the status JSON, reflecting the current state of the AC.

The `env` and `target` fields may also appear when Faikout auto mode is active.

### Logging Messages (`Faikout/{hostname}`)

Sent periodically (typically every minute) for time-series logging. Values that remained constant during the period are sent as-is. Values that changed:

- **Numeric:** reported as `[min, average, max]` array
- **Boolean:** reported as `0.0` to `1.0` (fraction of time `true`)
- **Enum:** reported as current value

The `fixstatus` setting forces the array/fraction format even for constant values.

---

## Settings

Send to `setting/{hostname}` with a JSON payload, or `setting/{hostname}/{key}` with a plain value.

Sending an empty payload to `setting/{hostname}` returns all current settings as JSON.

### Key Settings for Controller Developers

#### MQTT & Connectivity

| Setting | Default | Description |
|---------|---------|-------------|
| `hostname` | — | Device hostname (used in all topics) |
| `mqtthost` | — | MQTT broker hostname/IP |
| `mqttuser` | — | MQTT username |
| `mqttpass` | — | MQTT password |

#### Reporting

| Setting | Default | Description |
|---------|---------|-------------|
| `reporting` | `60` | Status reporting interval (seconds) |
| `livestatus` | `false` | Report `state/` on any change (not just periodic) |
| `fixstatus` | `false` | Always use min/avg/max format in logging |

#### Temperature Limits

| Setting | Default | Description |
|---------|---------|-------------|
| `t.min` | `16` | Minimum settable temperature (°C) |
| `t.coolmin` | `16` | Minimum temperature for cooling |
| `t.max` | `32` | Maximum settable temperature (°C) |
| `t.heatmax` | `32` | Maximum temperature for heating |

#### Feature Disables (`no.*`)

Boolean settings to disable features the AC doesn't support or you don't want exposed:

| Setting | Description |
|---------|-------------|
| `no.demand` | Disable demand control |
| `no.auto` | Disable auto mode |
| `no.econo` | Disable economy mode |
| `no.swingv` | Disable vertical swing |
| `no.swingh` | Disable horizontal swing |
| `no.comfort` | Disable comfort mode |
| `no.streamer` | Disable streamer |
| `no.powerful` | Disable powerful mode |
| `no.sensor` | Disable sensor |
| `no.quiet` | Disable quiet mode |
| `no.led` | Disable LED control |

#### Auto Mode Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `auto.e` | `true` | Enable auto mode logic |
| `auto.t` | — | Target temperature |
| `auto.r` | — | Margin/range (0 = auto off) |
| `auto.p` | `false` | Auto power on/off |
| `auto.0` | `0000` | Auto off time (HHMM, 0000 = disabled) |
| `auto.1` | `0000` | Auto on time (HHMM, 0000 = disabled) |
| `auto.b` | — | BLE sensor ID for reference temp |
| `auto.fmax` | `5` | Max fan speed in auto |
| `auto.ptemp` | `0.5` | Degrees outside range to trigger auto power |
| `auto.topic` | — | MQTT topic to subscribe for reference temp |
| `auto.payload` | — | JSON field name in that topic for temperature |
| `thermostat` | `false` | Thermostat mode (hysteresis-based) |

#### Temperature Control Tuning

| Setting | Default | Description |
|---------|---------|-------------|
| `switchtemp` | `0.5` | Temperature adjustment for heat↔cool switching |
| `pushtemp` | `0.1` | Temperature nudge to avoid hovering at min/max |
| `cool.over` | `6` | Degrees to overshoot set point when cooling "on" |
| `cool.back` | `6` | Degrees to pull back when cooling "off" |
| `heat.over` | `6` | Degrees to overshoot when heating "on" |
| `heat.back` | `6` | Degrees to pull back when heating "off" |
| `t.predicts` | `30` | Prediction sample interval (seconds) |
| `t.predictt` | `120` | Prediction look-ahead periods |
| `t.sample` | `900` | Auto mode sampling period (seconds) |
| `t.control` | `600` | Timeout for remote control messages (seconds) |

#### Home Assistant

| Setting | Default | Description |
|---------|---------|-------------|
| `ha.enable` | `true` | Enable HA auto-discovery |
| `ha.switches` | `false` | Expose boolean controls as separate switches |
| `ha.fanrpm` | `false` | Expose fan RPM sensor |
| `ha.comprpm` | `false` | Expose compressor frequency sensor |
| `ha.1c` | `false` | Use 1°C steps instead of 0.5°C |

#### Protocol Selection

| Setting | Default | Description |
|---------|---------|-------------|
| `no.s21` | `false` | Disable S21 protocol |
| `no.x50a` | `false` | Disable X50A protocol |
| `no.cnwired` | `true` | Disable CN_WIRED protocol |

---

## Faikout Auto Mode

Faikout includes a sophisticated auto-control layer that sits above the AC's built-in modes. It's the recommended way to implement external temperature control.

### How It Works

1. You send a `control` command with `target` (desired temp range) and `env` (current room temperature from your sensor)
2. Faikout uses **predictive control** — it samples temperature every `t.predicts` seconds and looks ahead `t.predictt` periods
3. It "turns on" heating/cooling by setting the AC's set point far above/below current temp (±`heat.over`/`cool.over`)
4. It "turns off" by setting the set point to let the AC idle (±`heat.back`/`cool.back`)
5. The goal is to keep temperature within the `[min, max]` range, not to hit a specific target

### Using Remote Control

For external sensor integration, send periodic `control` commands:

```json
{
  "env": 21.3,
  "target": [20.0, 22.0]
}
```

Or with a single target + margin:
```json
{
  "env": 21.3,
  "target": 21.0,
  "margin": 2.0
}
```

This must be sent at least every `t.control` seconds (default 600 = 10 minutes).

### Using MQTT Topic as Reference

Alternatively, configure the device to subscribe to a temperature topic:
```
setting/{hostname} {"auto.topic": "sensors/bedroom/temperature", "auto.payload": "temperature"}
```

The device will read the `temperature` field from JSON payloads on that topic.

### Thermostat Mode

Set `thermostat` to `true` for hysteresis-based control:
- **Heating:** aims for `max`, lets temp fall to `min` before turning back on
- **Cooling:** aims for `min`, lets temp rise to `max` before turning back on
- Disables `pushtemp`/`switchtemp` adjustments

---

## Home Assistant Integration

When `ha.enable` is `true` (default), the device publishes MQTT auto-discovery configs.

### Discovery Topics

```
homeassistant/climate/{device_id}/config
homeassistant/sensor/{device_id}{tag}/config
homeassistant/switch/{device_id}{tag}/config
homeassistant/select/{device_id}demand/config
```

Where `{device_id}` is typically `{hostname}-{MAC}` (the RevK device ID).

### Climate Entity

**Command topics** (HA publishes to):

| Topic | Payload |
|-------|---------|
| `command/{hostname}/temp` | Temperature number (e.g. `22.5`) |
| `command/{hostname}/mode` | `off`, `heat`, `cool`, `heat_cool`, `dry`, `fan_only` |
| `command/{hostname}/fan` | `auto`, `low`, `lowMedium`, `medium`, `mediumHigh`, `high`, `night` |
| `command/{hostname}/swing` | `off`, `H`, `V`, `H+V`, `on`, `C` |
| `command/{hostname}/power` | `ON` / `OFF` |
| `command/{hostname}/preset` | `eco`, `boost`, `home` |

**State topic:** `state/{hostname}` — single JSON with all values.

**HA mode ↔ internal mode mapping:**

| HA Mode | Internal Code | Meaning |
|---------|---------------|----|
| `off` | *(power off)* | Power off |
| `heat` | `H` | Heat |
| `cool` | `C` | Cool |
| `heat_cool` | `A` | Auto |
| `dry` | `D` | Dry |
| `fan_only` | `F` | Fan only |

**HA fan mode ↔ internal fan mapping (5-level, typical for S21):**

| HA Fan Mode | Internal Code |
|-------------|---------------|
| `auto` | `A` |
| `low` | `1` |
| `lowMedium` | `2` |
| `medium` | `3` |
| `mediumHigh` | `4` |
| `high` | `5` |
| `night` / `quiet` | `Q` |

### Sensor Entities

| Entity | device_class | Unit | Notes |
|--------|-------------|------|-------|
| Outside temp | `temperature` | °C | |
| Inlet temp | `temperature` | °C | |
| Liquid temp | `temperature` | °C | |
| AC-Home temp | `temperature` | °C | AC's own measurement |
| AC-Target temp | `temperature` | °C | Current set point |
| Humidity | `humidity` | % | |
| Compressor | `frequency` | Hz | If `ha.comprpm` enabled |
| Fan | `frequency` | Hz/rpm | If `ha.fanrpm` enabled |
| Lifetime energy (outside) | `energy` | kWh | `total_increasing` |
| Lifetime heating energy | `energy` | kWh | `total_increasing` |
| Lifetime cooling energy | `energy` | kWh | `total_increasing` |
| Power consumption | `power` | W | Current draw |
| BLE Temp/Humidity/Battery | various | various | If BLE sensor connected |

### Switch Entities (if `ha.switches` enabled)

`power`, `streamer`, `sensor`, `powerful`, `comfort`, `quiet`, `econo`, `autoe`

### Select Entity

`demand` — options: 30, 35, 40, ... 100 (step 5)

---

## Enum Reference

### Mode (`mode` field)

| Code | Name | Internal Index |
|------|------|----------------|
| `F` | Fan | 0 |
| `H` | Heat | 1 |
| `C` | Cool | 2 |
| `A` | Auto | 3 |
| `D` | Dry | 7 |

### Fan Speed (`fan` field)

| Code | Name | Internal Index |
|------|------|----------------|
| `A` | Auto | 0 |
| `1` | Speed 1 (low) | 1 |
| `2` | Speed 2 | 2 |
| `3` | Speed 3 (medium) | 3 |
| `4` | Speed 4 | 4 |
| `5` | Speed 5 (high) | 5 |
| `Q` | Quiet/Night | 6 |

> **Note:** Some units (CN_WIRED) only support 3 levels: `1` (low), `3` (medium), `5` (high) + `A` (auto) + `Q` (quiet).

### Fan Type Setting (`fantype`)

| Value | Description |
|-------|-------------|
| 0 | Default (auto-detect) |
| 1 | 5-level + auto |
| 2 | 3-level |
| 3 | 3-level + auto |

---

## Practical Examples

All examples use `mosquitto_pub`/`mosquitto_sub` with hostname `GuestAC`.

### Monitor AC State

```bash
mosquitto_sub -t 'state/GuestAC/#' -v
```

### Turn On, Set to Cool at 22°C, Fan Auto

```bash
mosquitto_pub -t 'command/GuestAC' -m '{"power":true,"mode":"C","temp":22,"fan":"A"}'
```

### Simple Power On

```bash
mosquitto_pub -t 'command/GuestAC/on' -m ''
```

### Set Temperature Only

```bash
mosquitto_pub -t 'command/GuestAC/temp' -m '23.5'
```

### Enable Faikout Auto with External Sensor

```bash
# Send repeatedly (at least every 10 minutes)
mosquitto_pub -t 'command/GuestAC' -m '{"env":21.3,"target":[20,22]}'
```

### Configure Auto Mode via Settings

```bash
mosquitto_pub -t 'setting/GuestAC' -m '{"auto.e":true,"auto.t":21,"auto.r":2}'
```

### Read All Current Settings

```bash
mosquitto_pub -t 'setting/GuestAC' -m ''
mosquitto_sub -t 'setting/GuestAC' -C 1
```

### Enable Live Status Updates

```bash
mosquitto_pub -t 'setting/GuestAC/livestatus' -m '1'
```

### Force Status Report

```bash
mosquitto_pub -t 'command/GuestAC/status' -m ''
```

---

## Quick-Start Checklist for Controller Implementation

1. **Connect to MQTT broker** that the Faikout is configured to use
2. **Subscribe to** `state/{hostname}/#` for status updates
3. **Send commands** to `command/{hostname}` with JSON payload (preferred) or `command/{hostname}/{cmd}` for simple actions
4. **Parse status JSON** from `state/{hostname}/status` — contains all readable + writable fields
5. **Handle the `state/{hostname}` topic** — payload `false` means device went offline (MQTT LWT)
6. **For auto mode:** send `control` with `env` + `target` at least every 600 seconds
7. **Enable `livestatus`** if you need real-time state tracking (otherwise updates come every 60s)
8. **Check `no.*` settings** to know which features the specific AC unit supports

---

*Generated from [ESP32-Faikout](https://github.com/revk/ESP32-Faikout) source code and documentation (Feb 2026). The S21 protocol is reverse-engineered; not all fields are available on all Daikin models.*
