"""Represents a single discovered Faikout unit and its live state.

Design decisions
----------------

Dataclass, not dict
    A dataclass gives us named, typed fields with IDE autocompletion and clear
    documentation of what each value means.  The raw JSON is still available
    via the ``_raw`` field for anything we don't explicitly model.

Command methods on the unit
    ``ac.turn_on()`` is more natural than ``controller.turn_on("GuestAC")``.
    The unit holds a ``_publish`` callback injected by the controller, keeping
    the unit decoupled from the controller's internals while still allowing
    direct command dispatch.

No local optimistic state updates
    When you call ``ac.turn_on()``, the ``power`` field does NOT immediately
    flip to True.  It only changes when the next status message arrives from
    the device confirming the change.  This avoids state divergence — the unit
    object always reflects what the device actually reported.

Enum fields
    ``mode`` and ``fan`` are stored as proper enums (Mode, Fan) rather than
    raw strings.  This catches typos at the call site and makes switch/match
    statements exhaustive.  The update_from_status method handles the
    string→enum conversion.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Optional

from .enums import Mode, Fan


@dataclass
class FaikoutUnit:
    """A single Faikout-controlled AC unit.

    Attributes reflect the latest state received over MQTT.  Use the command
    methods (``turn_on``, ``set_temp``, etc.) to control the unit — don't
    mutate fields directly.
    """

    hostname: str

    # ---- writable controls (can be set via commands) ----
    power: bool = False
    mode: Optional[Mode] = None
    temp: Optional[float] = None          # target set point (°C)
    fan: Optional[Fan] = None
    swingh: bool = False                  # horizontal louvre swing
    swingv: bool = False                  # vertical louvre swing
    econo: bool = False
    powerful: bool = False
    comfort: bool = False
    streamer: bool = False                # air purifier
    sensor: bool = False                  # intelligent eye
    led: bool = False
    quiet: bool = False
    demand: Optional[int] = None          # demand control (30–100%)

    # ---- read-only sensors (populated from status messages) ----
    online: bool = False                  # AC unit responding on serial bus
    home: Optional[float] = None          # room temp measured by AC (°C)
    outside: Optional[float] = None       # outdoor unit temp (°C)
    inlet: Optional[float] = None         # heat exchanger temp (°C)
    liquid: Optional[float] = None        # coolant line temp (°C)
    hum: Optional[float] = None           # indoor humidity (%)
    fanrpm: Optional[int] = None          # fan speed (RPM)
    comp: Optional[int] = None            # compressor frequency (Hz)
    consumption: Optional[int] = None     # current power draw (W)
    model: Optional[str] = None           # AC model string, e.g. "FTXM35R"
    heat: bool = False                    # currently in heating action
    slave: bool = False                   # not master for heat/cool
    antifreeze: bool = False              # antifreeze mode active
    control: bool = False                 # under external/auto control

    # ---- Faikout auto mode fields ----
    env: Optional[float] = None           # external reference temp (°C)
    target: Any = None                    # float or [min, max]
    autor: Optional[float] = None         # auto range setting
    autot: Optional[float] = None         # auto target temp

    # ---- bookkeeping ----
    last_seen: float = field(default_factory=time.time)
    _raw: dict = field(default_factory=dict, repr=False)

    # ---- internal (injected by controller) ----
    _publish: Optional[Callable] = field(default=None, repr=False)

    # ------------------------------------------------------------------ #
    # State updates from MQTT                                             #
    # ------------------------------------------------------------------ #

    def update_from_status(self, payload: dict) -> list[str]:
        """Apply a status JSON dict to this unit's fields.

        Returns a list of field names whose values actually changed.
        Always updates ``last_seen`` and ``_raw`` regardless.
        """
        changed: list[str] = []
        self._raw = payload
        self.last_seen = time.time()

        for key, value in payload.items():
            # Enum fields need special handling.
            if key == "mode" and value is not None:
                try:
                    new = Mode(value)
                except ValueError:
                    continue  # unknown mode code, skip
                if self.mode != new:
                    self.mode = new
                    changed.append("mode")

            elif key == "fan" and value is not None:
                try:
                    new = Fan(value)
                except ValueError:
                    continue
                if self.fan != new:
                    self.fan = new
                    changed.append("fan")

            # All other known fields: set if changed.
            elif hasattr(self, key) and not key.startswith("_"):
                old = getattr(self, key)
                if old != value:
                    setattr(self, key, value)
                    changed.append(key)

        return changed

    @property
    def is_stale(self) -> bool:
        """True if no status received in the last 120 seconds."""
        return (time.time() - self.last_seen) > 120

    # ------------------------------------------------------------------ #
    # Command methods                                                     #
    #                                                                     #
    # Each method publishes an MQTT message and returns immediately.       #
    # The actual state change happens when the device processes the        #
    # command and publishes updated status.  This means there's always a   #
    # delay (typically 1–3 seconds) between calling a method and seeing    #
    # the corresponding field update.                                     #
    # ------------------------------------------------------------------ #

    def _cmd(self, suffix: str, payload: str = ""):
        """Publish to ``command/{hostname}/{suffix}``."""
        if self._publish is None:
            raise RuntimeError("Unit not attached to a controller")
        self._publish(f"command/{self.hostname}/{suffix}", payload)

    def _control(self, **fields):
        """Publish a JSON control payload to ``command/{hostname}``."""
        if self._publish is None:
            raise RuntimeError("Unit not attached to a controller")
        self._publish(f"command/{self.hostname}", json.dumps(fields))

    def turn_on(self):
        """Power on the AC unit."""
        self._cmd("on")

    def turn_off(self):
        """Power off the AC unit."""
        self._cmd("off")

    def set_mode(self, mode: Mode):
        """Set operating mode (HEAT, COOL, AUTO, DRY, FAN)."""
        self._control(mode=mode.value)

    def set_temp(self, temp: float):
        """Set target temperature in °C (typically 16.0–32.0, 0.5° steps)."""
        self._cmd("temp", str(temp))

    def set_fan(self, fan: Fan):
        """Set fan speed (AUTO, SPEED_1–SPEED_5, QUIET)."""
        self._control(fan=fan.value)

    def set_swing(
        self,
        *,
        horizontal: Optional[bool] = None,
        vertical: Optional[bool] = None,
    ):
        """Set louvre swing modes.  Pass only the axes you want to change."""
        fields: dict[str, bool] = {}
        if horizontal is not None:
            fields["swingh"] = horizontal
        if vertical is not None:
            fields["swingv"] = vertical
        if fields:
            self._control(**fields)

    def set_econo(self, on: bool):
        """Enable or disable economy mode."""
        self._control(econo=on)

    def set_powerful(self, on: bool):
        """Enable or disable powerful/boost mode."""
        self._control(powerful=on)

    def set_comfort(self, on: bool):
        """Enable or disable comfort mode."""
        self._control(comfort=on)

    def set_streamer(self, on: bool):
        """Enable or disable the streamer (air purifier)."""
        self._control(streamer=on)

    def set_quiet(self, on: bool):
        """Enable or disable quiet mode."""
        self._control(quiet=on)

    def set_demand(self, pct: int):
        """Set demand control percentage (clamped to 30–100)."""
        self._control(demand=max(30, min(100, pct)))

    def set_auto_target(
        self, env: float, target_min: float, target_max: float
    ):
        """Engage Faikout auto mode with an external sensor reading.

        Args:
            env: Current room temperature from your external sensor (°C).
            target_min: Lower bound of desired temperature range.
            target_max: Upper bound of desired temperature range.

        This must be called **repeatedly** (at least every 600s, configurable
        via the ``t.control`` setting) or the device reverts to local mode.
        Include ``env`` in every call.
        """
        self._control(env=env, target=[target_min, target_max])

    def request_status(self):
        """Ask the device to publish a status update immediately."""
        self._cmd("status")

    # ------------------------------------------------------------------ #
    # Settings                                                            #
    #                                                                     #
    # Settings are persistent config stored in the device's flash.        #
    # Changes survive reboots.  Use sparingly — for runtime control,      #
    # prefer the command methods above.                                   #
    # ------------------------------------------------------------------ #

    def set_setting(self, key: str, value: Any):
        """Set a single persistent setting on the device."""
        if self._publish is None:
            raise RuntimeError("Unit not attached to a controller")
        self._publish(f"setting/{self.hostname}/{key}", str(value))

    def set_settings(self, **settings):
        """Set multiple persistent settings at once (JSON payload)."""
        if self._publish is None:
            raise RuntimeError("Unit not attached to a controller")
        self._publish(f"setting/{self.hostname}", json.dumps(settings))

    # ------------------------------------------------------------------ #
    # Display                                                             #
    # ------------------------------------------------------------------ #

    def __str__(self) -> str:
        mode_str = self.mode.value if self.mode else "?"
        fan_str = self.fan.value if self.fan else "?"
        state = "ON" if self.power else "OFF"
        temp_str = f"{self.temp}\u00b0C" if self.temp is not None else "?"
        home_str = f"{self.home}\u00b0C" if self.home is not None else "?"
        return (
            f"{self.hostname}: {state} mode={mode_str} temp={temp_str} "
            f"fan={fan_str} home={home_str}"
        )
