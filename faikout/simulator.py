#!/usr/bin/env python3
"""Faikout device simulator — a full emulator of one or more Faikout units.

This emulates real Faikout hardware over MQTT so you can develop and test
controllers without physical devices.  Each simulated unit:

- Publishes periodic status JSON to ``state/{hostname}/status`` (retained).
- Publishes HA-format state to ``state/{hostname}`` (retained).
- Subscribes to ``command/{hostname}/#`` and ``setting/{hostname}/#``.
- Accepts all commands a real Faikout does (power, mode, temp, fan, swing,
  econo, powerful, comfort, streamer, sensor, led, quiet, demand, auto mode).
- Handles Home Assistant MQTT command topics (mode, fan, swing, preset, power).
- Runs a thermal simulation that makes temperatures drift realistically.

Usage::

    # Single unit with default name
    python simulator.py

    # Three named units, custom broker
    python simulator.py --broker 192.168.1.10 LivingRoom Bedroom Kitchen

    # Fast reporting for testing
    python simulator.py --reporting 2 TestAC1 TestAC2 TestAC3

Design decisions
----------------

Asyncio with gmqtt
    The previous paho-mqtt version used one thread per simulated unit plus
    the paho network thread.  This version uses a single asyncio event loop.
    Each unit's physics/publish cycle is an ``async def`` coroutine managed
    by ``asyncio.gather()``.  This eliminates all threading, all locks, and
    the silent message-delivery stalls we saw with paho-mqtt v2.

Single MQTT client for all units
    All simulated units share one gmqtt client.  This is simpler and uses
    fewer broker connections.  The downside is MQTT only allows one LWT per
    client, so only the first unit gets a proper offline LWT.  A real Faikout
    device is one unit per ESP32 with its own connection.  For testing purposes
    this trade-off is fine.

Thermal simulation
    Room temperature drifts toward outdoor temperature (thermal leakage) and
    is pushed by AC heating/cooling.  The model is intentionally simple — a
    first-order system with tunable constants — rather than a physically
    accurate building simulation.  It produces believable temperature curves
    that exercise the controller's state tracking.

Realistic initial state
    Each unit starts with randomised room temperature, outdoor temperature,
    humidity, energy counters, and a random Daikin model string.  This tests
    that the controller handles variety rather than assuming fixed values.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import random
import signal
import sys
from typing import Optional

import gmqtt

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(name)-12s] %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger("sim")

# ---------------------------------------------------------------------------
# Physics constants for the thermal simulation
# ---------------------------------------------------------------------------
AMBIENT_OUTSIDE = 32.0       # outdoor temperature (°C)
THERMAL_DRIFT = 0.003        # how fast room temp drifts toward outside per tick
COOLING_POWER = 0.04         # degrees per tick when actively cooling
HEATING_POWER = 0.04         # degrees per tick when actively heating
DRY_COOLING = 0.01           # mild cooling in dry mode
TICK_INTERVAL = 2.0          # simulation tick (seconds)
REPORT_INTERVAL = 10.0       # default status publish interval (seconds)

# ---------------------------------------------------------------------------
# Valid enums
# ---------------------------------------------------------------------------
VALID_MODES = {"F", "H", "C", "A", "D"}
VALID_FANS = {"A", "1", "2", "3", "4", "5", "Q"}


class SimulatedUnit:
    """Simulates one Faikout device.

    This class owns only the AC state and thermal simulation logic.
    It does NOT own the MQTT client — that's passed in by the runner,
    so multiple units can share one connection.
    """

    def __init__(self, hostname: str, client: gmqtt.Client):
        self.hostname = hostname
        self.client = client
        self.log = logging.getLogger(f"sim.{hostname}")

        # --- AC state ---
        self.power = False
        self.mode = "A"        # Auto
        self.temp = 24.0       # set point
        self.fan = "A"
        self.swingh = False
        self.swingv = False
        self.econo = False
        self.powerful = False
        self.comfort = False
        self.streamer = False
        self.sensor = False
        self.led = True
        self.quiet = False
        self.demand = 100

        # --- simulated sensors ---
        self.home_temp = 25.0 + random.uniform(-2, 2)
        self.outside_temp = AMBIENT_OUTSIDE + random.uniform(-3, 3)
        self.inlet_temp = self.home_temp
        self.liquid_temp = self.home_temp - 5
        self.humidity = 55.0 + random.uniform(-10, 10)
        self.fan_rpm = 0
        self.comp_freq = 0
        self.consumption = 0
        self.wh_total = random.randint(10000, 500000)
        self.wh_heating = random.randint(5000, 200000)
        self.wh_cooling = random.randint(5000, 200000)
        self.model = f"FTXM{random.choice([25, 35, 50, 60, 71])}R"

        # --- auto mode ---
        self.env: float | None = None
        self.target: list | float | None = None
        self.auto_control = False

        # --- faikout auto internals ---
        self._active_heating = False
        self._active_cooling = False

        # --- settings ---
        self.settings: dict = {
            "reporting": REPORT_INTERVAL,
            "livestatus": False,
            "ha.enable": True,
        }

    # -------------------------------------------------------------------
    # MQTT subscriptions
    # -------------------------------------------------------------------

    def subscribe(self):
        """Subscribe to command and setting topics for this unit."""
        self.client.subscribe(f"command/{self.hostname}/#")
        self.client.subscribe(f"command/{self.hostname}")
        self.client.subscribe(f"setting/{self.hostname}/#")
        self.client.subscribe(f"setting/{self.hostname}")

    # -------------------------------------------------------------------
    # Command handling
    # -------------------------------------------------------------------

    def handle_message(self, topic: str, payload: str):
        """Dispatch an incoming MQTT message to the right handler."""
        parts = topic.split("/")
        if len(parts) < 2:
            return

        prefix = parts[0]
        if parts[1] != self.hostname:
            return

        suffix = "/".join(parts[2:]) if len(parts) > 2 else ""

        if prefix == "command":
            self._handle_command(suffix, payload)
        elif prefix == "setting":
            self._handle_setting(suffix, payload)

    def _handle_command(self, suffix: str, payload: str):
        """Handle a command/{hostname}/... message."""
        if suffix == "":
            # JSON control command
            try:
                data = json.loads(payload) if payload else {}
            except json.JSONDecodeError:
                self.log.warning("Bad JSON control: %s", payload[:200])
                return
            self._apply_control(data)
            return

        # Simple commands
        cmd = suffix.lower()
        if cmd == "on":
            self.power = True
            self.log.info("Power ON")
        elif cmd == "off":
            self.power = False
            self.log.info("Power OFF")
        elif cmd == "heat":
            self.mode = "H"
        elif cmd == "cool":
            self.mode = "C"
        elif cmd == "auto":
            self.mode = "A"
        elif cmd == "fan":
            self.mode = "F"
        elif cmd == "dry":
            self.mode = "D"
        elif cmd == "low":
            self.fan = "1"
        elif cmd == "medium":
            self.fan = "3"
        elif cmd == "high":
            self.fan = "5"
        elif cmd == "temp" and payload:
            try:
                t = float(payload)
                self.temp = max(16.0, min(32.0, t))
                self.log.info("Temp → %.1f", self.temp)
            except ValueError:
                pass
        elif cmd == "status":
            self._publish_status()
        elif cmd == "mode":
            self._handle_ha_mode(payload)
        elif cmd == "swing":
            self._handle_ha_swing(payload)
        elif cmd == "power":
            self.power = payload.upper() in ("ON", "TRUE", "1")
        elif cmd == "preset":
            self._handle_preset(payload)
        else:
            self.log.debug("Unknown command: %s %s", suffix, payload)

    def _apply_control(self, data: dict):
        """Apply a JSON control payload."""
        if "power" in data:
            self.power = bool(data["power"])
        if "mode" in data and data["mode"] in VALID_MODES:
            self.mode = data["mode"]
        if "temp" in data:
            try:
                self.temp = max(16.0, min(32.0, float(data["temp"])))
            except (ValueError, TypeError):
                pass
        if "fan" in data and str(data["fan"]) in VALID_FANS:
            self.fan = str(data["fan"])
        if "swingh" in data:
            self.swingh = bool(data["swingh"])
        if "swingv" in data:
            self.swingv = bool(data["swingv"])
        if "econo" in data:
            self.econo = bool(data["econo"])
        if "powerful" in data:
            self.powerful = bool(data["powerful"])
        if "comfort" in data:
            self.comfort = bool(data["comfort"])
        if "streamer" in data:
            self.streamer = bool(data["streamer"])
        if "sensor" in data:
            self.sensor = bool(data["sensor"])
        if "led" in data:
            self.led = bool(data["led"])
        if "quiet" in data:
            self.quiet = bool(data["quiet"])
        if "demand" in data:
            try:
                self.demand = max(30, min(100, int(data["demand"])))
            except (ValueError, TypeError):
                pass

        # Faikout auto fields
        if "env" in data:
            try:
                self.env = float(data["env"])
            except (ValueError, TypeError):
                pass
        if "target" in data:
            self.target = data["target"]
            self.auto_control = True

        self.log.info(
            "Control: power=%s mode=%s temp=%.1f fan=%s",
            self.power, self.mode, self.temp, self.fan,
        )

    def _handle_ha_mode(self, payload: str):
        """Handle HA mode command."""
        ha_map = {
            "off": (False, None),
            "heat": (True, "H"),
            "cool": (True, "C"),
            "heat_cool": (True, "A"),
            "dry": (True, "D"),
            "fan_only": (True, "F"),
        }
        mode_lower = payload.strip().lower()
        if mode_lower in ha_map:
            power, mode = ha_map[mode_lower]
            self.power = power
            if mode:
                self.mode = mode

    def _handle_ha_fan(self, payload: str):
        """Handle HA fan mode command."""
        fan_map = {
            "auto": "A", "low": "1", "lowmedium": "2", "lowMedium": "2",
            "medium": "3", "mediumhigh": "4", "mediumHigh": "4",
            "high": "5", "night": "Q", "quiet": "Q",
        }
        fan = fan_map.get(payload.strip(), fan_map.get(payload.strip().lower()))
        if fan:
            self.fan = fan

    def _handle_ha_swing(self, payload: str):
        """Handle HA swing mode command."""
        s = payload.strip()
        if s == "off":
            self.swingh = False
            self.swingv = False
        elif s == "H":
            self.swingh = True
            self.swingv = False
        elif s == "V":
            self.swingh = False
            self.swingv = True
        elif s == "H+V":
            self.swingh = True
            self.swingv = True
        elif s == "on":
            self.swingh = True
            self.swingv = True

    def _handle_preset(self, payload: str):
        """Handle HA preset mode command."""
        p = payload.strip().lower()
        if p == "eco":
            self.econo = True
            self.powerful = False
        elif p == "boost":
            self.powerful = True
            self.econo = False
        elif p == "home":
            self.econo = False
            self.powerful = False

    def _handle_setting(self, suffix: str, payload: str):
        """Handle a setting/{hostname}/... message."""
        if suffix == "" and not payload:
            # Return all settings
            self.client.publish(
                f"setting/{self.hostname}",
                json.dumps(self.settings),
            )
            return
        if suffix == "" and payload:
            try:
                data = json.loads(payload)
                self.settings.update(data)
                self.log.info("Settings updated: %s", data)
            except json.JSONDecodeError:
                pass
            return
        if suffix and payload:
            try:
                val = json.loads(payload)
            except json.JSONDecodeError:
                val = payload
            self.settings[suffix] = val
            self.log.info("Setting %s = %s", suffix, val)

    # -------------------------------------------------------------------
    # Thermal simulation
    # -------------------------------------------------------------------

    def _tick(self):
        """One simulation step: update temperatures and sensor values."""
        # Drift toward outside temp
        diff = self.outside_temp - self.home_temp
        self.home_temp += diff * THERMAL_DRIFT

        # Apply AC effect
        if self.power:
            effective_mode = self.mode
            # Auto mode: pick heat or cool based on set point vs room temp
            if effective_mode == "A":
                if self.home_temp < self.temp - 0.5:
                    effective_mode = "H"
                    self._active_heating = True
                    self._active_cooling = False
                elif self.home_temp > self.temp + 0.5:
                    effective_mode = "C"
                    self._active_cooling = True
                    self._active_heating = False
                else:
                    self._active_heating = False
                    self._active_cooling = False

            fan_mult = self._fan_multiplier()
            power_mult = 1.5 if self.powerful else (0.6 if self.econo else 1.0)

            if effective_mode == "C":
                rate = COOLING_POWER * fan_mult * power_mult
                if self.home_temp > self.temp:
                    self.home_temp -= rate
                    self._active_cooling = True
                    self._active_heating = False
                else:
                    self._active_cooling = False
                self.comp_freq = (
                    int(30 + (self.home_temp - self.temp) * 10)
                    if self._active_cooling else 0
                )
                self.comp_freq = max(0, min(120, self.comp_freq))

            elif effective_mode == "H":
                rate = HEATING_POWER * fan_mult * power_mult
                if self.home_temp < self.temp:
                    self.home_temp += rate
                    self._active_heating = True
                    self._active_cooling = False
                else:
                    self._active_heating = False
                self.comp_freq = (
                    int(30 + (self.temp - self.home_temp) * 10)
                    if self._active_heating else 0
                )
                self.comp_freq = max(0, min(120, self.comp_freq))

            elif effective_mode == "D":
                self.home_temp -= DRY_COOLING
                self.humidity = max(30, self.humidity - 0.3)
                self._active_cooling = True
                self._active_heating = False
                self.comp_freq = 20

            elif effective_mode == "F":
                self.comp_freq = 0
                self._active_cooling = False
                self._active_heating = False

            # Fan RPM
            fan_rpms = {
                "Q": 350, "1": 500, "2": 700, "3": 900,
                "4": 1100, "5": 1300, "A": 800,
            }
            self.fan_rpm = fan_rpms.get(self.fan, 800)
            if self.quiet:
                self.fan_rpm = min(self.fan_rpm, 400)

            # Power consumption
            base_w = 30  # fan only
            self.consumption = base_w + self.comp_freq * 8
            if self.powerful:
                self.consumption = int(self.consumption * 1.5)
            elif self.econo:
                self.consumption = int(self.consumption * 0.6)

            # Accumulate energy
            wh_tick = self.consumption * (TICK_INTERVAL / 3600)
            self.wh_total += int(wh_tick)
            if self._active_heating:
                self.wh_heating += int(wh_tick)
            elif self._active_cooling:
                self.wh_cooling += int(wh_tick)

        else:
            # Off
            self.fan_rpm = 0
            self.comp_freq = 0
            self.consumption = 0
            self._active_cooling = False
            self._active_heating = False

        # Inlet/liquid follow home temp with lag
        self.inlet_temp += (self.home_temp - self.inlet_temp) * 0.1
        target_liquid = (
            self.home_temp - 8 if self._active_cooling
            else self.home_temp + 3
        )
        self.liquid_temp += (target_liquid - self.liquid_temp) * 0.05

        # Humidity drift
        self.humidity += random.uniform(-0.2, 0.2)
        self.humidity = max(25, min(85, self.humidity))

        # Outside temp slow drift
        self.outside_temp += random.uniform(-0.05, 0.05)

    def _fan_multiplier(self) -> float:
        """Map fan setting to a cooling/heating rate multiplier."""
        return {
            "Q": 0.5, "1": 0.6, "2": 0.75, "3": 0.9,
            "4": 1.0, "5": 1.1, "A": 0.9,
        }.get(self.fan, 0.9)

    # -------------------------------------------------------------------
    # Status publishing
    # -------------------------------------------------------------------

    def _build_status(self) -> dict:
        """Build the full status JSON (published on state/{hostname}/status)."""
        status = {
            "online": True,
            "model": self.model,
            "power": self.power,
            "mode": self.mode,
            "temp": round(self.temp, 1),
            "fan": self.fan,
            "swingh": self.swingh,
            "swingv": self.swingv,
            "econo": self.econo,
            "powerful": self.powerful,
            "comfort": self.comfort,
            "streamer": self.streamer,
            "sensor": self.sensor,
            "led": self.led,
            "quiet": self.quiet,
            "demand": self.demand,
            "home": round(self.home_temp, 1),
            "outside": round(self.outside_temp, 1),
            "inlet": round(self.inlet_temp, 1),
            "liquid": round(self.liquid_temp, 1),
            "hum": round(self.humidity, 1),
            "fanrpm": self.fan_rpm,
            "comp": self.comp_freq,
            "consumption": self.consumption,
            "heat": self._active_heating,
            "slave": False,
            "antifreeze": False,
            "control": self.auto_control,
            "Whoutside": self.wh_total,
            "Whheating": self.wh_heating,
            "Whcooling": self.wh_cooling,
        }
        if self.env is not None:
            status["env"] = self.env
        if self.target is not None:
            status["target"] = self.target
        return status

    def _build_ha_status(self) -> dict:
        """Build the HA-format state (published on state/{hostname})."""
        mode_map = {
            "F": "fan_only", "H": "heat", "C": "cool",
            "A": "heat_cool", "D": "dry",
        }
        fan_map = {
            "A": "auto", "1": "low", "2": "lowMedium",
            "3": "medium", "4": "mediumHigh", "5": "high", "Q": "night",
        }

        ha_mode = "off" if not self.power else mode_map.get(self.mode, "off")

        # Determine swing
        if self.swingh and self.swingv:
            swing = "H+V"
        elif self.swingh:
            swing = "H"
        elif self.swingv:
            swing = "V"
        else:
            swing = "off"

        # Determine action
        if not self.power:
            action = "off"
        elif self._active_heating:
            action = "heating"
        elif self._active_cooling:
            action = "cooling"
        elif self.mode == "D":
            action = "drying"
        elif self.mode == "F":
            action = "fan"
        else:
            action = "idle"

        # Determine preset
        if self.econo:
            preset = "eco"
        elif self.powerful:
            preset = "boost"
        else:
            preset = "home"

        return {
            "mode": ha_mode,
            "fan": fan_map.get(self.fan, "auto"),
            "swing": swing,
            "temp": round(self.home_temp, 1),
            "hum": round(self.humidity, 1),
            "target": round(self.temp, 1),
            "action": action,
            "preset": preset,
            "power": self.power,
            "streamer": self.streamer,
            "sensor": self.sensor,
            "powerful": self.powerful,
            "comfort": self.comfort,
            "quiet": self.quiet,
            "econo": self.econo,
        }

    def _publish_status(self):
        """Publish both full-status and HA-format state to MQTT."""
        status = self._build_status()
        ha_status = self._build_ha_status()

        self.client.publish(
            f"state/{self.hostname}/status",
            json.dumps(status),
            retain=True,
        )
        self.client.publish(
            f"state/{self.hostname}",
            json.dumps(ha_status),
            retain=True,
        )

    # -------------------------------------------------------------------
    # Main loop (async)
    # -------------------------------------------------------------------

    async def run(self, stop_event: asyncio.Event):
        """Run the unit's simulation loop until stop_event is set.

        This is the async replacement for the old threading approach.
        Each tick runs the physics simulation and, at the configured
        reporting interval, publishes status.
        """
        self.subscribe()
        self._publish_status()
        self.log.info("Started (model=%s, home=%.1f°C)", self.model, self.home_temp)

        last_report = asyncio.get_event_loop().time()
        while not stop_event.is_set():
            await asyncio.sleep(TICK_INTERVAL)
            self._tick()
            now = asyncio.get_event_loop().time()
            interval = self.settings.get("reporting", REPORT_INTERVAL)
            if now - last_report >= interval:
                self._publish_status()
                last_report = now

    def stop(self):
        """Publish offline LWT."""
        self.client.publish(
            f"state/{self.hostname}", "false", retain=True
        )


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

async def run_simulator(
    hostnames: list[str],
    broker: str = "localhost",
    port: int = 1883,
    reporting: float = REPORT_INTERVAL,
):
    """Run the simulator as an async coroutine.

    This is the programmatic entry point, useful for embedding the simulator
    in tests or other async applications.
    """
    client = gmqtt.Client(
        f"faikout-sim-{random.randint(1000, 9999)}",
        will_message=gmqtt.Message(
            f"state/{hostnames[0]}", "false", retain=True
        ) if hostnames else None,
    )

    units: list[SimulatedUnit] = []
    for name in hostnames:
        u = SimulatedUnit(name, client)
        u.settings["reporting"] = reporting
        units.append(u)

    connected = asyncio.Event()

    def on_connect(client, flags, rc, properties):
        log.info("Connected to %s:%d", broker, port)
        connected.set()

    def on_message(client, topic, payload, qos, properties):
        payload_str = payload.decode("utf-8", errors="replace") if payload else ""
        for u in units:
            u.handle_message(topic, payload_str)
        return 0

    client.on_connect = on_connect
    client.on_message = on_message

    log.info("Starting simulator with units: %s", ", ".join(hostnames))
    await client.connect(broker, port, version=4)
    await asyncio.sleep(0.1)  # let on_connect fire

    stop_event = asyncio.Event()

    # Install signal handlers for clean shutdown.
    loop = asyncio.get_event_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stop_event.set)
        except NotImplementedError:
            pass  # Windows doesn't support add_signal_handler

    # Run all unit loops concurrently.
    try:
        await asyncio.gather(*(u.run(stop_event) for u in units))
    except asyncio.CancelledError:
        pass
    finally:
        log.info("Shutting down...")
        for u in units:
            u.stop()
        await client.disconnect()


def main():
    """CLI entry point."""
    parser = argparse.ArgumentParser(description="Faikout device simulator")
    parser.add_argument(
        "hostnames", nargs="*", default=["SimAC1"],
        help="Hostnames for simulated units (default: SimAC1)",
    )
    parser.add_argument(
        "--broker", "-b", default="localhost", help="MQTT broker",
    )
    parser.add_argument(
        "--port", "-p", type=int, default=1883, help="MQTT port",
    )
    parser.add_argument(
        "--reporting", "-r", type=float, default=10.0,
        help="Status reporting interval in seconds",
    )
    args = parser.parse_args()

    asyncio.run(run_simulator(
        hostnames=args.hostnames,
        broker=args.broker,
        port=args.port,
        reporting=args.reporting,
    ))


if __name__ == "__main__":
    main()
