"""Central controller: connects to MQTT, discovers and manages Faikout units.

Design decisions
----------------

Single MQTT client
    One gmqtt client handles all units.  Topic wildcards (``state/+/status``)
    let us discover units without knowing hostnames upfront.  This keeps broker
    connections to a minimum and makes the controller easy to embed in a larger
    app (one connection, one event loop).

Asyncio-first, sync-friendly
    The core is async: ``await ctrl.start()`` / ``await ctrl.stop()``.  For
    callers that aren't async (scripts, Flask, Django), ``run_in_thread()``
    spins up an asyncio event loop in a background thread and returns a
    synchronous handle.  This replaces paho-mqtt's ``loop_start()`` approach
    with something that works identically from the caller's perspective.

No threading locks needed
    gmqtt runs on a single asyncio event loop.  Message handling, publishing,
    and user callbacks all happen on that loop — no concurrent access, no race
    conditions.  The ``_units`` dict is only mutated inside ``_handle_status``
    which is only called from ``on_message``.  When using ``run_in_thread()``,
    the background thread owns the loop and user reads from the main thread
    are safe because Python dict reads are atomic for our use case (simple
    attribute lookups on dataclass instances).

Callback model
    Three hooks — on_discovered, on_updated, on_offline — cover the lifecycle.
    Callbacks can be sync or async; we detect and handle either.  They fire on
    the asyncio event loop, so they should be fast (or schedule work elsewhere).

Why gmqtt over paho-mqtt?
    paho-mqtt v2 exhibited silent message-delivery stalls in long-running
    sessions with multiple clients.  gmqtt is a clean, independent MQTT
    implementation built on asyncio — no background threads, no mysterious
    stalls.  It's also a natural fit for async web frameworks (FastAPI,
    aiohttp) while remaining usable from sync code via ``run_in_thread()``.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import logging
import threading
from typing import Any, Callable, Optional, Union

import gmqtt

from .enums import Mode, Fan
from .unit import FaikoutUnit

log = logging.getLogger("faikout")

# Type aliases for event callbacks (can be sync or async).
UnitCallback = Callable[[FaikoutUnit], Any]
UnitChangeCallback = Callable[[FaikoutUnit, list[str]], Any]


def _run_callback(cb: Callable, *args):
    """Invoke a callback, handling both sync and async transparently."""
    try:
        result = cb(*args)
        if inspect.isawaitable(result):
            asyncio.ensure_future(result)
    except Exception:
        log.exception("Error in callback %s", cb)


class FaikoutController:
    """Manages discovery and control of Faikout units over a single MQTT broker.

    Async usage::

        ctrl = FaikoutController("mqtt.local")
        await ctrl.start()

        await asyncio.sleep(3)  # wait for retained status messages

        for unit in ctrl:
            print(unit)

        ac = ctrl["GuestAC"]
        ac.turn_on()
        ac.set_mode(Mode.COOL)
        ac.set_temp(22.5)

        await ctrl.stop()

    Sync usage (runs event loop in a background thread)::

        ctrl = FaikoutController("mqtt.local")
        ctrl.run_in_thread()

        import time; time.sleep(3)
        ac = ctrl["GuestAC"]
        ac.turn_on()

        ctrl.shutdown()  # stops the background thread

    Context manager (sync)::

        with FaikoutController("mqtt.local") as ctrl:
            ctrl.run_in_thread()
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
        self._username = username
        self._password = password
        self._units: dict[str, FaikoutUnit] = {}  # hostname → unit

        # Event callbacks (lists to allow multiple subscribers).
        self._on_unit_discovered: list[UnitCallback] = []
        self._on_unit_updated: list[UnitChangeCallback] = []
        self._on_unit_offline: list[UnitCallback] = []

        # gmqtt client — created once, connected in start().
        cid = client_id or f"faikout-ctrl-{id(self):x}"
        self._client = gmqtt.Client(
            cid,
            will_message=gmqtt.Message(
                "faikout-controller/status", "offline", retain=True
            ),
        )
        if username:
            self._client.set_auth_credentials(username, password)

        self._client.on_connect = self._on_connect
        self._client.on_message = self._on_message
        self._client.on_disconnect = self._on_disconnect

        # Background thread support for sync callers.
        self._bg_loop: Optional[asyncio.AbstractEventLoop] = None
        self._bg_thread: Optional[threading.Thread] = None
        self._bg_stop: Optional[asyncio.Event] = None

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
        """Snapshot of all discovered units."""
        return list(self._units.values())

    @property
    def hostnames(self) -> list[str]:
        """Hostnames of all discovered units."""
        return list(self._units.keys())

    def __getitem__(self, hostname: str) -> FaikoutUnit:
        return self._units[hostname]

    def __contains__(self, hostname: str) -> bool:
        return hostname in self._units

    def __len__(self) -> int:
        return len(self._units)

    def __iter__(self):
        return iter(self.units)

    def get(self, hostname: str) -> Optional[FaikoutUnit]:
        """Get a unit by hostname, or None if not yet discovered."""
        return self._units.get(hostname)

    # ------------------------------------------------------------------ #
    # Async lifecycle                                                     #
    # ------------------------------------------------------------------ #

    async def start(self):
        """Connect to the broker and begin listening (async)."""
        await self._client.connect(self._broker, self._port, version=4)

    async def stop(self):
        """Disconnect cleanly (async)."""
        self._client.publish(
            "faikout-controller/status", "offline", retain=True
        )
        await self._client.disconnect()

    # ------------------------------------------------------------------ #
    # Sync lifecycle — background thread                                  #
    #                                                                     #
    # For non-async callers: run_in_thread() starts an asyncio event loop #
    # in a daemon thread.  shutdown() stops it.  This gives the same UX   #
    # as paho-mqtt's loop_start()/loop_stop().                            #
    # ------------------------------------------------------------------ #

    def run_in_thread(self):
        """Start the controller in a background thread (sync convenience).

        Returns immediately.  The MQTT connection and message processing run
        on a private asyncio event loop in a daemon thread.  Call
        ``shutdown()`` to stop it.
        """
        ready = threading.Event()

        def _thread_main():
            loop = asyncio.new_event_loop()
            asyncio.set_event_loop(loop)
            self._bg_loop = loop
            self._bg_stop = asyncio.Event()
            loop.run_until_complete(self._run_bg(ready))

        self._bg_thread = threading.Thread(
            target=_thread_main, daemon=True, name="faikout-ctrl"
        )
        self._bg_thread.start()
        ready.wait(timeout=10)

    async def _run_bg(self, ready: threading.Event):
        """Entry point for the background asyncio loop."""
        await self.start()
        ready.set()
        await self._bg_stop.wait()
        await self.stop()

    def shutdown(self):
        """Stop the background thread started by ``run_in_thread()``."""
        if self._bg_stop and self._bg_loop:
            self._bg_loop.call_soon_threadsafe(self._bg_stop.set)
        if self._bg_thread:
            self._bg_thread.join(timeout=5)
            self._bg_thread = None

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.shutdown()

    @property
    def connected(self) -> bool:
        """True if the MQTT client is currently connected."""
        return self._client.is_connected

    # ------------------------------------------------------------------ #
    # Publishing                                                          #
    # ------------------------------------------------------------------ #

    def _publish(self, topic: str, payload: str = ""):
        """Internal publish used by FaikoutUnit command methods.

        gmqtt.Client.publish() is synchronous (fire-and-forget for QoS 0)
        so this can be called from any thread safely.
        """
        self._client.publish(topic, payload)

    def publish_raw(
        self, topic: str, payload: str = "", retain: bool = False
    ):
        """Publish an arbitrary MQTT message (escape hatch for advanced use)."""
        self._client.publish(topic, payload, retain=retain)

    # ------------------------------------------------------------------ #
    # MQTT callbacks (called on the asyncio event loop)                   #
    # ------------------------------------------------------------------ #

    def _on_connect(self, client, flags, rc, properties):
        """Called when the broker accepts our connection.

        We (re-)subscribe here so subscriptions survive reconnections —
        gmqtt may auto-reconnect after a network blip.
        """
        log.info("Connected to MQTT broker %s:%d", self._broker, self._port)

        # state/+/status — periodic status JSON from each Faikout unit.
        # This is the primary data source for discovery and state tracking.
        client.subscribe("state/+/status")

        # state/+ — catches the LWT ("false" when a unit disconnects).
        # Also catches the HA-format state JSON, which we ignore (we use
        # the more detailed state/+/status instead).
        client.subscribe("state/+")

        # Error/event subscriptions for completeness.
        client.subscribe("error/+/#")
        client.subscribe("event/+/#")

        client.publish("faikout-controller/status", "online", retain=True)

    def _on_disconnect(self, client, packet, exc=None):
        """Called on broker disconnect.  gmqtt auto-reconnects by default."""
        log.warning("Disconnected from MQTT broker")

    def _on_message(self, client, topic: str, payload: bytes, qos, properties):
        """Dispatch incoming messages to the appropriate handler.

        gmqtt delivers ``payload`` as bytes.  The return value tells gmqtt
        whether to ACK (relevant for QoS > 0); we always return 0 (ACK).
        """
        payload_str = payload.decode("utf-8", errors="replace") if payload else ""
        parts = topic.split("/")

        # state/{hostname}/status — full status JSON from a Faikout unit.
        if len(parts) == 3 and parts[0] == "state" and parts[2] == "status":
            hostname = parts[1]
            try:
                data = json.loads(payload_str)
            except json.JSONDecodeError:
                log.warning("Bad JSON from %s: %s", hostname, payload_str[:200])
                return 0
            self._handle_status(hostname, data)

        # state/{hostname} — either LWT ("false") or HA-format state JSON.
        # We only act on the LWT; the HA JSON is redundant with /status.
        elif len(parts) == 2 and parts[0] == "state":
            hostname = parts[1]
            if payload_str.lower() in ("false", ""):
                self._handle_offline(hostname)

        return 0

    def _handle_status(self, hostname: str, data: dict):
        """Process a status message: create or update the unit."""
        is_new = hostname not in self._units
        if is_new:
            unit = FaikoutUnit(hostname=hostname)
            unit._publish = self._publish
            self._units[hostname] = unit
        else:
            unit = self._units[hostname]

        changed = unit.update_from_status(data)

        if is_new:
            log.info("Discovered unit: %s", hostname)
            for cb in self._on_unit_discovered:
                _run_callback(cb, unit)

        if changed:
            for cb in self._on_unit_updated:
                _run_callback(cb, unit, changed)

    def _handle_offline(self, hostname: str):
        """Process an offline LWT for a unit."""
        unit = self._units.get(hostname)
        if unit:
            unit.online = False
            log.info("Unit offline: %s", hostname)
            for cb in self._on_unit_offline:
                _run_callback(cb, unit)
