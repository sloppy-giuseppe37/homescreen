"""Enums matching the Faikout MQTT protocol.

These mirror the single-character codes used in Faikout's MQTT JSON payloads.
Using str enums means they serialize naturally to/from JSON while providing
type safety and IDE autocompletion at the call site.

The Mode enum also provides .ha / .from_ha() helpers for converting between
Faikout's internal codes ("H", "C", "A", "D", "F") and Home Assistant's mode
strings ("heat", "cool", "heat_cool", "dry", "fan_only").  These are useful
if you're building a bridge between Faikout and HA-compatible systems.
"""

from enum import Enum


class Mode(str, Enum):
    """AC operating mode."""
    FAN = "F"
    HEAT = "H"
    COOL = "C"
    AUTO = "A"
    DRY = "D"

    # HA mode string → internal code
    _HA_MAP = None  # populated below

    @classmethod
    def from_ha(cls, ha_mode: str) -> "Mode":
        return _HA_TO_MODE[ha_mode]

    @property
    def ha(self) -> str:
        return _MODE_TO_HA[self]


_HA_TO_MODE = {
    "heat": Mode.HEAT,
    "cool": Mode.COOL,
    "heat_cool": Mode.AUTO,
    "dry": Mode.DRY,
    "fan_only": Mode.FAN,
}
_MODE_TO_HA = {v: k for k, v in _HA_TO_MODE.items()}


class Fan(str, Enum):
    """Fan speed."""
    AUTO = "A"
    SPEED_1 = "1"
    SPEED_2 = "2"
    SPEED_3 = "3"
    SPEED_4 = "4"
    SPEED_5 = "5"
    QUIET = "Q"


class SwingMode(str, Enum):
    """Swing mode for HA integration."""
    OFF = "off"
    HORIZONTAL = "H"
    VERTICAL = "V"
    BOTH = "H+V"
    ON = "on"
    COMFORT = "C"


class Preset(str, Enum):
    """Preset modes."""
    ECO = "eco"
    BOOST = "boost"
    HOME = "home"
