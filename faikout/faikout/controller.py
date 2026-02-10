"""Central controller: connects to MQTT, discovers and manages Faikout units.

Design decisions
----------------

Single MQTT client
    One paho-mqtt client handles all units.  Topic wildcards (`state/+/status`)
    let us discover units without knowing hostnames upfront.  This keeps broker
    connections to a minimum and makes the controller easy to embed in a larger
    app (one connection, one background thread).

Thread safety
    Unit access is guarded by a single lock.  The MQTT callback thread updates
    unit state; user code reads it from the main thread.  FaikoutUnit fields
    are simple scalars so read–tearing isn't a practical concern, but the lock
    protects the _units dict itself (discovery can add entries at any time).

Callback model
    Three hooks — on_discovered, on_updated, on_offline — cover the lifecycle.
    Callbacks fire on the MQTT thread, so they should be fast.  For web apps,
    the pattern is: callback pushes to a queue / sets a flag, web handler reads
    from it.

Why not asyncio?
    paho-mqtt's async support is limited.  A future version could swap to
    aiomqtt, but the threading model works well today and is simpler to
    integrate with frameworks that may or may not be async (Flask, Django,
    FastAPI).  The controller's start(blocking=False) returns immediately,
    so callers can use whatever concurrency model they like.
"""

from __future__ import annotations

import json
import logging
import threading
from typing import Any, Callable, Optional

import paho.mqtt.client as mqtt

from .enums import Mode, Fan
from .unit import FaikoutUnit

log = logging.getLogger("faikout")

# Type aliases for event callbacks.
UnitCallback = Callable[[FaikoutUnit], None]
UnitChangeCallback = Callable[[FaikoutUnit, list[str]], None]


class FaikoutController:
    """Manages discovery and control of Faikout units over a single MQTT broker.

    Quick start::

        ctrl = FaikoutController("mqtt.local")
        ctrl.start()  # connects, subscribes, discovers in background

        import time; time.sleep(3)  # wait for retained status messages

        for unit in ctrl:
            print(unit)

        ac = ctrl["GuestAC"]
        ac.turn_on()
        ac.set_mode(Mode.COOL)
        ac.set_temp(22.5)

        ctrl.stop()

    The controller is also usable as a context manager::

        with FaikoutController("mqtt.local") as ctrl:
            ctrl.start()
            ...
    """

    def __init__(
        self,
        broker: str = "localhost",
        port: int = 1883,
        username: Optional[str] = None,
        password: Optional[str] = None,
        client_id: str = "",
    ):
        self._broker = broker
        self._port = port
        self._units: dict[str, FaikoutUnit] = {}  # hostname → unit
        self._lock = threading.Lock()

        # Event callbacks (lists to allow multiple subscribers).
        self._on_unit_discovered: list[UnitCallback] = []
        self._on_unit_updated: list[UnitChangeCallback] = []
        self._on_unit_offline: list[UnitCallback] = []

        # Paho MQTT v2 client.
        self._client = mqtt.Client(
            callback_api_version=mqtt.CallbackAPIVersion.VERSION2,
            client_id=client_id or f"faikout-ctrl-{id(self):x}",
        )
        if username:
            self._client.username_pw_set(username, password)
        self._client.on_connect = self._on_connect
        self._client.on_disconnect = self._on_disconnect
        self._client.on_message = self._on_message
        # LWT so other systems know we've gone away.
        self._client.will_set("faikout-controller/status", "offline", retain=True)

    # ------------------------------------------------------------------ #
    # Event registration                                                  #
    #                                                                     #
    # Decorators return the callback unchanged so they can be used with   #
    # or without @-syntax:                                                #
    #   @ctrl.on_discovered                                               #
    #   def handler(unit): ...                                            #
    #                                                                     #
    #   ctrl.on_discovered(some_function)                                 #
    # ------------------------------------------------------------------ #

    def on_discovered(self, cb: UnitCallback) -> UnitCallback:
        """Register callback for when a new unit is first seen."""
        self._on_unit_discovered.append(cb)
        return cb

    def on_updated(self, cb: UnitChangeCallback) -> UnitChangeCallback:
        """Register callback for when a unit's state changes.

        Callback receives ``(unit, list_of_changed_field_names)``.
        Only fires when at least one field actually changed value.
        """
        self._on_unit_updated.append(cb)
        return cb

    def on_offline(self, cb: UnitCallback) -> UnitCallback:
        """Register callback for when a unit goes offline (MQTT LWT)."""
        self._on_unit_offline.append(cb)
        return cb

    # ------------------------------------------------------------------ #
    # Unit access — dict-like interface                                    #
    #                                                                     #
    # The controller behaves like a read-only dict keyed by hostname.     #
    # This makes it natural to use in web handlers:                       #
    #   unit = ctrl["BedroomAC"]                                          #
    #   if "BedroomAC" in ctrl: ...                                       #
    #   for unit in ctrl: ...                                             #
    # ------------------------------------------------------------------ #

    @property
    def units(self) -> list[FaikoutUnit]:
        """Snapshot of all discovered units (list copy for thread safety)."""
        with self._lock:
            return list(self._units.values())

    @property
    def hostnames(self) -> list[str]:
        """Hostnames of all discovered units."""
        with self._lock:
            return list(self._units.keys())

    def __getitem__(self, hostname: str) -> FaikoutUnit:
        with self._lock:
            return self._units[hostname]

    def __contains__(self, hostname: str) -> bool:
        with self._lock:
            return hostname in self._units

    def __len__(self) -> int:
        with self._lock:
            return len(self._units)

    def __iter__(self):
        return iter(self.units)

    def get(self, hostname: str) -> Optional[FaikoutUnit]:
        """Get a unit by hostname, or None if not yet discovered."""
        with self._lock:
            return self._units.get(hostname)

    # ------------------------------------------------------------------ #
    # Lifecycle                                                           #
    # ------------------------------------------------------------------ #

    def start(self, *, blocking: bool = False):
        """Connect to the broker and begin listening.

        Args:
            blocking: If True, blocks the calling thread forever (useful for
                scripts).  If False (default), runs the MQTT loop in a
                background thread and returns immediately.
        """
        self._client.connect(self._broker, self._port)
        if blocking:
            self._client.loop_forever()
        else:
            self._client.loop_start()

    def stop(self):
        """Disconnect and stop the background MQTT loop."""
        self._client.publish("faikout-controller/status", "offline", retain=True)
        self._client.disconnect()
        self._client.loop_stop()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.stop()

    @property
    def connected(self) -> bool:
        """True if the MQTT client is currently connected."""
        return self._client.is_connected()

    # ------------------------------------------------------------------ #
    # Publishing                                                          #
    # ------------------------------------------------------------------ #

    def _publish(self, topic: str, payload: str = ""):
        """Internal publish used by FaikoutUnit command methods."""
        self._client.publish(topic, payload)

    def publish_raw(self, topic: str, payload: str = "", retain: bool = False):
        """Publish an arbitrary MQTT message (escape hatch for advanced use)."""
        self._client.publish(topic, payload, retain=retain)

    # ------------------------------------------------------------------ #
    # MQTT callbacks (called on the paho network thread)                  #
    # ------------------------------------------------------------------ #

    def _on_disconnect(self, client, userdata, flags, rc, properties=None):
        log.warning("Disconnected from MQTT broker (rc=%s), will auto-reconnect", rc)

    def _on_connect(self, client, userdata, flags, rc, properties=None):
        log.info("Connected to MQTT broker %s:%d", self._broker, self._port)

        # Subscribe to status topics for auto-discovery.
        # `state/+/status` matches any Faikout unit's periodic status JSON.
        client.subscribe("state/+/status")

        # `state/+` catches the LWT message ("false" when a unit disconnects).
        # It also catches the HA-format state JSON, which we ignore (we use
        # the more detailed state/+/status instead).
        client.subscribe("state/+")

        # Error/event subscriptions for completeness — not currently processed
        # but available for future use.
        client.subscribe("error/+/#")
        client.subscribe("event/+/#")

        client.publish("faikout-controller/status", "online", retain=True)

    def _on_message(self, client, userdata, msg: mqtt.MQTTMessage):
        topic = msg.topic
        payload_raw = (
            msg.payload.decode("utf-8", errors="replace") if msg.payload else ""
        )

        parts = topic.split("/")

        # state/{hostname}/status — full status JSON from a Faikout unit.
        # This is the primary data source for discovery and state tracking.
        if len(parts) == 3 and parts[0] == "state" and parts[2] == "status":
            hostname = parts[1]
            try:
                data = json.loads(payload_raw)
            except json.JSONDecodeError:
                log.warning("Bad JSON from %s: %s", hostname, payload_raw[:200])
                return
            self._handle_status(hostname, data)

        # state/{hostname} — either LWT ("false") or HA-format state JSON.
        # We only act on the LWT; the HA JSON is redundant with /status.
        elif len(parts) == 2 and parts[0] == "state":
            hostname = parts[1]
            if payload_raw.lower() in ("false", ""):
                self._handle_offline(hostname)

    def _handle_status(self, hostname: str, data: dict):
        """Process a status message: create or update the unit."""
        is_new = False
        with self._lock:
            if hostname not in self._units:
                unit = FaikoutUnit(hostname=hostname)
                unit._publish = self._publish
                self._units[hostname] = unit
                is_new = True
            else:
                unit = self._units[hostname]

        changed = unit.update_from_status(data)

        if is_new:
            log.info("Discovered unit: %s", hostname)
            for cb in self._on_unit_discovered:
                try:
                    cb(unit)
                except Exception:
                    log.exception("Error in on_discovered callback")

        if changed:
            for cb in self._on_unit_updated:
                try:
                    cb(unit, changed)
                except Exception:
                    log.exception("Error in on_updated callback")

    def _handle_offline(self, hostname: str):
        """Process an offline LWT for a unit."""
        with self._lock:
            unit = self._units.get(hostname)
        if unit:
            unit.online = False
            log.info("Unit offline: %s", hostname)
            for cb in self._on_unit_offline:
                try:
                    cb(unit)
                except Exception:
                    log.exception("Error in on_offline callback")
