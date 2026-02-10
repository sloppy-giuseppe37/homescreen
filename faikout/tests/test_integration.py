#!/usr/bin/env python3
"""Integration test for the Faikout MQTT controller library.

Architecture
------------
This test spins up the simulator as a **subprocess** and the controller
in-process.  This is a deliberate design choice:

1.  **Process isolation avoids MQTT library quirks.**  When paho-mqtt ran
    the sim and controller in one process, the paho network thread would
    silently stop delivering messages after sustained activity.  Separate
    processes eliminate any shared-state threading issues entirely.  With
    the gmqtt migration, the controller runs on its own asyncio event loop
    in a background thread (via ``run_in_thread()``), but the sim is still
    a subprocess — because that's how real deployments work anyway.

2.  **Mirrors real deployment.**  In production, the Faikout device (or
    this simulator standing in for it) is a separate process/machine from
    the controller.  Testing with separate processes exercises the full
    MQTT pub/sub path through the broker.

3.  **Retained messages from prior runs can confuse discovery.**  The test
    clears retained messages before starting to ensure deterministic results.

The test flow:
    1.  Clear retained MQTT messages from any prior run.
    2.  Start the simulator subprocess with 3 named units.
    3.  Wait until all 3 units are publishing status (verified via
        ``mosquitto_sub``).
    4.  Start the controller with ``run_in_thread()`` (background asyncio
        loop).
    5.  Wait for the controller to discover all 3 units.
    6.  Run through a series of command → verify-state → assert checks.
    7.  Tear everything down.

Each test step is numbered and prints ✓/✗ so you can see exactly where
a failure occurs without needing a full test framework.

Prerequisites
-------------
-   Mosquitto running on localhost:1883 with anonymous access.
-   The ``faikout`` package importable (``uv pip install -e .``).
-   ``mosquitto_sub`` and ``mosquitto_pub`` CLI tools available.
"""

import json
import os
import signal
import subprocess
import sys
import time

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
BROKER = "localhost"
PORT = 1883
SIM_HOSTS = ["TestAC1", "TestAC2", "TestAC3"]
SIM_REPORT_INTERVAL = 2.0  # seconds — fast for testing

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

failed = 0
passed = 0


def check(label: str, condition: bool) -> bool:
    """Print a pass/fail line and track results."""
    global failed, passed
    if condition:
        print(f"   \u2713 {label}")
        passed += 1
    else:
        print(f"   \u2717 {label}")
        failed += 1
    return condition


def wait_for(predicate, *, timeout: float = 10.0, poll: float = 0.2) -> bool:
    """Poll ``predicate()`` until it returns True or timeout expires."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(poll)
    return False


def clear_retained():
    """Remove retained MQTT messages left by prior test runs.

    We publish an empty (null) retained message to each known topic.
    This is the standard MQTT way to delete a retained message.
    """
    for h in SIM_HOSTS:
        for suffix in ["/status", ""]:
            subprocess.run(
                ["mosquitto_pub", "-t", f"state/{h}{suffix}", "-n", "-r"],
                timeout=5, capture_output=True,
            )
    # Also clear the controller's LWT topic.
    subprocess.run(
        ["mosquitto_pub", "-t", "faikout-controller/status", "-n", "-r"],
        timeout=5, capture_output=True,
    )
    print("   Retained messages cleared")


def wait_for_sim_ready():
    """Block until every simulated unit has published at least one status.

    Uses ``mosquitto_sub -C 1`` to wait for a single message on each unit's
    status topic.  This ensures the sim subprocess is fully connected and
    publishing before we start the controller.
    """
    for host in SIM_HOSTS:
        result = subprocess.run(
            [
                "mosquitto_sub",
                "-t", f"state/{host}/status",
                "-C", "1",   # exit after 1 message
                "-W", "15",  # timeout 15s
            ],
            timeout=20, capture_output=True, text=True,
        )
        if result.returncode != 0:
            print(f"   \u2717 Timed out waiting for {host}")
            sys.exit(1)
    print("   \u2713 All simulators publishing")


# ---------------------------------------------------------------------------
# Main test
# ---------------------------------------------------------------------------

def main():
    global failed, passed

    # Add parent dir to path so we can import faikout.
    sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    from faikout import FaikoutController, Mode, Fan

    print("="*70)
    print(f"  Faikout Integration Test (gmqtt)")
    print(f"  broker={BROKER}:{PORT}  sim_hosts={SIM_HOSTS}  "
          f"report_interval={SIM_REPORT_INTERVAL}s")
    print("="*70)

    # Track discovered / updated units for assertions.
    discovered: list[str] = []
    changes_log: list[tuple] = []

    # ------------------------------------------------------------------ #
    # Step 0: Clear retained messages
    # ------------------------------------------------------------------ #
    print("\n[0] Clearing retained messages...")
    clear_retained()

    # ------------------------------------------------------------------ #
    # Step 1: Start simulator subprocess
    # ------------------------------------------------------------------ #
    print("\n[1] Starting simulator subprocess...")
    sim_proc = subprocess.Popen(
        [
            sys.executable, "-u", "simulator.py",
            "--broker", BROKER,
            "--port", str(PORT),
            "--reporting", str(SIM_REPORT_INTERVAL),
        ] + SIM_HOSTS,
        cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    print(f"   Simulator PID: {sim_proc.pid}")
    wait_for_sim_ready()

    # ------------------------------------------------------------------ #
    # Step 2: Start controller, wait for discovery
    # ------------------------------------------------------------------ #
    print("\n[2] Starting controller, waiting for auto-discovery...")
    ctrl = FaikoutController(BROKER, PORT, client_id="test-controller")

    @ctrl.on_discovered
    def _on_disc(unit):
        discovered.append(unit.hostname)

    @ctrl.on_updated
    def _on_upd(unit, changed):
        changes_log.append((unit.hostname, changed))

    ctrl.run_in_thread()

    ok = wait_for(lambda: len(ctrl) >= len(SIM_HOSTS), timeout=15)
    check(f"All {len(SIM_HOSTS)} units discovered", ok)
    check("Discovered via callback", set(discovered) == set(SIM_HOSTS))

    # Grab a reference to the first unit for subsequent tests.
    ac1 = ctrl.get("TestAC1")
    if not check("TestAC1 accessible", ac1 is not None):
        print("FATAL: Cannot continue without TestAC1")
        sim_proc.terminate()
        ctrl.shutdown()
        sys.exit(1)

    # ------------------------------------------------------------------ #
    # Step 3: Test unit access patterns
    # ------------------------------------------------------------------ #
    print("\n[3] Testing unit access...")
    check("ctrl['TestAC1'] works", ctrl["TestAC1"].hostname == "TestAC1")
    check("Unit is online", ac1.online is True)
    check("Model populated", ac1.model is not None and ac1.model.startswith("FTXM"))
    check("'TestAC1' in ctrl", "TestAC1" in ctrl)
    check("'NonExistent' not in ctrl", "NonExistent" not in ctrl)
    check(f"len(ctrl) >= {len(SIM_HOSTS)}", len(ctrl) >= len(SIM_HOSTS))
    names = sorted(u.hostname for u in ctrl)
    check(f"Iterable ({', '.join(names)})", set(names) == set(SIM_HOSTS))

    # ------------------------------------------------------------------ #
    # Step 4: Power on
    # ------------------------------------------------------------------ #
    print("\n[4] Power on...")
    ac1.turn_on()
    ok = wait_for(lambda: ac1.power is True, timeout=8)
    check("power = True", ok)

    # ------------------------------------------------------------------ #
    # Step 5: Mode + temp
    # ------------------------------------------------------------------ #
    print("\n[5] Mode + temp...")
    ac1.set_mode(Mode.COOL)
    ac1.set_temp(21.0)
    ok = wait_for(lambda: ac1.mode == Mode.COOL and ac1.temp == 21.0, timeout=8)
    check("mode = COOL, temp = 21.0", ok)

    # ------------------------------------------------------------------ #
    # Step 6: Fan speed
    # ------------------------------------------------------------------ #
    print("\n[6] Fan speed...")
    ac1.set_fan(Fan.SPEED_3)
    ok = wait_for(lambda: ac1.fan == Fan.SPEED_3, timeout=8)
    check("fan = SPEED_3", ok)

    # ------------------------------------------------------------------ #
    # Step 7: Boolean controls
    # ------------------------------------------------------------------ #
    print("\n[7] Boolean controls...")
    ac1.set_swing(horizontal=True, vertical=True)
    ok = wait_for(lambda: ac1.swingh is True and ac1.swingv is True, timeout=8)
    check("swingh=True, swingv=True", ok)

    ac1.set_econo(True)
    ok = wait_for(lambda: ac1.econo is True, timeout=8)
    check("econo=True", ok)

    ac1.set_econo(False)
    ok = wait_for(lambda: ac1.econo is False, timeout=8)
    check("econo=False (toggle back)", ok)

    # ------------------------------------------------------------------ #
    # Step 8: Auto mode (external env + target)
    # ------------------------------------------------------------------ #
    print("\n[8] Auto mode with external sensor...")
    ac1.set_auto_target(env=23.5, target_min=22.0, target_max=24.0)
    ok = wait_for(lambda: ac1.env == 23.5 and ac1.target == [22.0, 24.0], timeout=8)
    check("env=23.5, target=[22.0, 24.0]", ok)

    # ------------------------------------------------------------------ #
    # Step 9: Sensor data populated
    # ------------------------------------------------------------------ #
    print("\n[9] Checking sensor data...")
    check(f"home = {ac1.home}", ac1.home is not None)
    check(f"outside = {ac1.outside}", ac1.outside is not None)
    check(f"inlet = {ac1.inlet}", ac1.inlet is not None)
    check(f"liquid = {ac1.liquid}", ac1.liquid is not None)
    check(f"hum = {ac1.hum}", ac1.hum is not None)
    check(f"fanrpm = {ac1.fanrpm}", ac1.fanrpm is not None and ac1.fanrpm > 0)
    check(f"comp = {ac1.comp}", ac1.comp is not None)
    check(f"consumption = {ac1.consumption}", ac1.consumption is not None)

    # ------------------------------------------------------------------ #
    # Step 10: Continuous state refresh
    #
    # Verify the controller keeps receiving status updates over time.
    # We record last_seen, wait for several reporting intervals, and
    # check that last_seen has advanced — proving the MQTT message flow
    # is still alive.
    # ------------------------------------------------------------------ #
    print("\n[10] Continuous state refresh...")
    ts_before = ac1.last_seen
    time.sleep(SIM_REPORT_INTERVAL * 3 + 1)
    ts_after = ac1.last_seen
    delta = ts_after - ts_before
    check(
        f"last_seen advanced by {delta:.1f}s (expect >= {SIM_REPORT_INTERVAL:.0f}s)",
        delta >= SIM_REPORT_INTERVAL,
    )

    # ------------------------------------------------------------------ #
    # Step 11: Power off
    # ------------------------------------------------------------------ #
    print("\n[11] Power off...")
    ac1.turn_off()
    ok = wait_for(lambda: ac1.power is False, timeout=8)
    check("power = False", ok)

    # ------------------------------------------------------------------ #
    # Step 12: Multi-unit control
    # ------------------------------------------------------------------ #
    print("\n[12] Multi-unit control...")
    ac2 = ctrl.get("TestAC2")
    if ac2:
        ac2.turn_on()
        ac2.set_mode(Mode.HEAT)
        ac2.set_temp(26.0)
        ok = wait_for(
            lambda: ac2.power is True and ac2.mode == Mode.HEAT and ac2.temp == 26.0,
            timeout=8,
        )
        check("TestAC2: power=True, mode=HEAT, temp=26.0", ok)
    else:
        check("TestAC2 available", False)

    # ------------------------------------------------------------------ #
    # Step 13: Settings
    # ------------------------------------------------------------------ #
    print("\n[13] Settings...")
    ac1.set_setting("livestatus", "true")
    ac1.set_settings(reporting=5, ha_enable=True)
    # Settings don't appear in status JSON, so we just verify no crash.
    time.sleep(1)
    check("Settings commands sent without error", True)

    # ------------------------------------------------------------------ #
    # Teardown
    # ------------------------------------------------------------------ #
    print("\n" + "-"*70)
    print("Tearing down...")
    ctrl.shutdown()
    sim_proc.send_signal(signal.SIGTERM)
    try:
        sim_proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        sim_proc.kill()
    print(f"   Controller stopped, simulator exited (rc={sim_proc.returncode})")

    # Clear retained messages so we don't pollute the next run.
    clear_retained()

    # ------------------------------------------------------------------ #
    # Summary
    # ------------------------------------------------------------------ #
    total = passed + failed
    print("\n" + "="*70)
    if failed == 0:
        print(f"  ALL {total} CHECKS PASSED \u2713")
    else:
        print(f"  {passed}/{total} passed, {failed} FAILED \u2717")
    print("="*70)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
