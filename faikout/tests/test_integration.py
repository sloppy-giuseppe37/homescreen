#!/usr/bin/env python3
"""Integration test for the Faikout MQTT controller library.

Architecture
------------
This test spins up a real MQTT broker (mosquitto, assumed running on localhost:1883),
launches the Faikout simulator as a **separate process**, and then exercises the
FaikoutController API against it.

Why a subprocess for the simulator?
    paho-mqtt v2's `loop_start()` creates a background thread per client. When two
    clients (sim + controller) share a process and both have active subscriptions and
    high-frequency publishes, the controller's network loop thread silently stops
    dispatching `on_message` callbacks after ~30s of sustained activity. This appears
    to be a threading/GIL interaction in paho-mqtt — the broker still has messages,
    the client reports `is_connected() == True`, but callbacks stop firing.

    Running the simulator out-of-process avoids this entirely, and also better
    reflects real-world usage where Faikout hardware is a separate network device.

Test flow
---------
1.  Clear retained MQTT messages from prior runs (stale retained state on
    `state/{hostname}/status` would cause false discovery of dead units).
2.  Launch simulator subprocess with 3 virtual AC units, fast reporting (2s).
3.  Wait for the simulator to be publishing (verified by subscribing to a
    status topic with mosquitto_sub).
4.  Create a FaikoutController connected to the same broker.
5.  Run test steps: discovery, commands, state verification, sensor checks.
6.  Tear down: stop controller, kill simulator, clear retained messages.

Each test step uses `wait_for()` — a polling helper — rather than fixed sleeps.
This makes the test resilient to timing jitter while keeping total runtime low.
The polling interval (0.2s) is well under the sim's 2s reporting cycle.

All assertions are soft (collected into an errors list) so that every test step
runs regardless of earlier failures, giving a complete picture on failure.
"""

import json
import os
import signal
import subprocess
import sys
import time
import threading

# Ensure the package and simulator are importable from the repo root.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from faikout import FaikoutController, FaikoutUnit, Mode, Fan

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

BROKER = "localhost"
PORT = 1883

# Simulated unit hostnames.  Three is enough to test multi-unit discovery and
# independent control without making the test slow.
SIM_HOSTS = ["TestAC1", "TestAC2", "TestAC3"]

# How often the simulator publishes status (seconds).  2s is fast enough that
# the test completes quickly, but slow enough that MQTT isn't flooded.
SIM_REPORT_INTERVAL = 2.0

# Maximum time to wait for any single condition (seconds).  Must be comfortably
# longer than one full sim reporting cycle.
WAIT_TIMEOUT = 10


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def wait_for(condition, timeout=WAIT_TIMEOUT, interval=0.2):
    """Poll *condition()* until truthy or *timeout* seconds elapse.

    Returns True if the condition was met, False on timeout.

    We poll rather than use threading.Event because the state we're checking
    lives on FaikoutUnit objects that are updated by the MQTT callback thread.
    Polling with a short interval is simple and avoids coupling the test to
    the library's internal threading model.
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        if condition():
            return True
        time.sleep(interval)
    return False


def clear_retained():
    """Null-publish retained messages from prior test runs.

    MQTT retained messages persist on the broker until explicitly cleared.
    If stale `state/{host}/status` messages exist from a previous sim run,
    the controller would "discover" phantom units that never send updates.
    """
    for host in SIM_HOSTS:
        subprocess.run(
            ["mosquitto_pub", "-t", f"state/{host}/status", "-n", "-r"],
            capture_output=True,
        )
        subprocess.run(
            ["mosquitto_pub", "-t", f"state/{host}", "-n", "-r"],
            capture_output=True,
        )
    subprocess.run(
        ["mosquitto_pub", "-t", "faikout-controller/status", "-n", "-r"],
        capture_output=True,
    )


def start_simulator() -> subprocess.Popen:
    """Launch the simulator as a child process.

    Returns the Popen handle.  The sim is started with --reporting set to
    SIM_REPORT_INTERVAL so status messages arrive frequently enough for the
    test's wait_for() polls to converge quickly.

    We redirect stderr to PIPE so sim log lines don't interleave with test
    output, but they're still available for debugging via proc.stderr.
    """
    sim_script = os.path.join(os.path.dirname(__file__), "..", "simulator.py")
    proc = subprocess.Popen(
        [
            sys.executable, "-u", sim_script,
            "--broker", BROKER,
            "--port", str(PORT),
            "--reporting", str(SIM_REPORT_INTERVAL),
            *SIM_HOSTS,
        ],
        stderr=subprocess.PIPE,
        stdout=subprocess.PIPE,
    )
    return proc


def wait_for_sim_ready(hosts: list[str], timeout=15):
    """Block until the simulator is publishing status for all hosts.

    Uses mosquitto_sub with -C (count) and -W (timeout) to receive exactly
    one message from each host's status topic.  This confirms the sim is
    connected, subscribed, and actively publishing before the controller
    starts — eliminating race conditions in test setup.
    """
    for host in hosts:
        result = subprocess.run(
            ["mosquitto_sub", "-t", f"state/{host}/status", "-C", "1", "-W", str(timeout)],
            capture_output=True, text=True, timeout=timeout + 2,
        )
        if result.returncode != 0 or not result.stdout.strip():
            raise RuntimeError(f"Simulator not publishing for {host} within {timeout}s")


def stop_simulator(proc: subprocess.Popen):
    """Gracefully stop the simulator subprocess."""
    if proc.poll() is None:
        proc.send_signal(signal.SIGINT)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

def test_full_flow():
    print("\n" + "=" * 70)
    print("  Faikout Integration Test")
    print("  broker=%s:%d  sim_hosts=%s  report_interval=%.1fs" % (
        BROKER, PORT, SIM_HOSTS, SIM_REPORT_INTERVAL))
    print("=" * 70)

    errors: list[str] = []

    def check(name: str, condition: bool, fail_detail: str = ""):
        """Record a pass/fail.  All checks run regardless of prior failures."""
        if condition:
            print(f"   \u2713 {name}")
        else:
            msg = f"{name}: {fail_detail}" if fail_detail else name
            errors.append(msg)
            print(f"   \u2717 {msg}")

    # ------------------------------------------------------------------
    # Setup
    # ------------------------------------------------------------------

    print("\n[setup] Clearing retained messages and starting simulator...")
    clear_retained()

    sim_proc = start_simulator()
    try:
        wait_for_sim_ready(SIM_HOSTS)
    except Exception as e:
        print(f"   \u2717 Simulator failed to start: {e}")
        stop_simulator(sim_proc)
        return False
    print("   \u2713 Simulator running, all hosts publishing")

    # ------------------------------------------------------------------
    # [1] Auto-discovery
    #
    # The controller subscribes to `state/+/status` on connect.  The broker
    # delivers retained messages immediately, so discovery should complete
    # within a second or two of ctrl.start().
    # ------------------------------------------------------------------

    print("\n[1] Auto-discovery")
    discovered_hosts: list[str] = []

    ctrl = FaikoutController(BROKER, PORT)

    @ctrl.on_discovered
    def _on_disc(unit: FaikoutUnit):
        discovered_hosts.append(unit.hostname)

    ctrl.start()

    ok = wait_for(lambda: all(h in ctrl for h in SIM_HOSTS))
    check("All 3 sim units discovered", ok,
          f"expected {SIM_HOSTS}, got {ctrl.hostnames}")

    # Verify each discovered unit has been populated from its status JSON.
    for host in SIM_HOSTS:
        if host in ctrl:
            u = ctrl[host]
            check(f"{host}: online={u.online}, model={u.model}",
                  u.online and u.model is not None)

    # ------------------------------------------------------------------
    # [2] Power on / off
    #
    # Tests the simplest command path: fire-and-forget topic
    # `command/{hostname}/on`.  The sim flips its state and includes it
    # in the next periodic status publish.  The controller picks it up
    # via `state/{hostname}/status` and updates the unit object.
    # ------------------------------------------------------------------

    print("\n[2] Power on/off")
    ac1 = ctrl.get("TestAC1")
    if ac1 is None:
        errors.append("TestAC1 not discovered, skipping power test")
        print("   \u2717 TestAC1 not available")
    else:
        ac1.turn_on()
        ok = wait_for(lambda: ac1.power is True)
        check("turn_on() -> power=True", ok, f"power={ac1.power}")

        ac1.turn_off()
        ok = wait_for(lambda: ac1.power is False)
        check("turn_off() -> power=False", ok, f"power={ac1.power}")

    # ------------------------------------------------------------------
    # [3] Mode / temperature / fan
    #
    # Tests the JSON control command path: a single MQTT publish to
    # `command/{hostname}` with a JSON body sets multiple fields atomically.
    # We set mode+temp first, then fan separately, to exercise both
    # `set_mode`/`set_temp` (which use _control) and `set_fan`.
    # ------------------------------------------------------------------

    print("\n[3] Mode, temperature, fan")
    if ac1 is None:
        print("   (skipped — TestAC1 not available)")
    else:
        ac1.turn_on()
        wait_for(lambda: ac1.power is True)

        ac1.set_mode(Mode.COOL)
        ac1.set_temp(21.0)
        ac1.set_fan(Fan.SPEED_3)

        ok = wait_for(lambda: (
            ac1.mode == Mode.COOL
            and ac1.temp == 21.0
            and ac1.fan == Fan.SPEED_3
        ))
        check("mode=COOL, temp=21.0, fan=3", ok,
              f"mode={ac1.mode} temp={ac1.temp} fan={ac1.fan}")

    # ------------------------------------------------------------------
    # [4] Boolean controls (econo, swing)
    #
    # These go through the same _control() JSON path but test boolean
    # field handling.  We set multiple booleans in one call via
    # set_swing() (which bundles swingh+swingv) plus set_econo().
    # ------------------------------------------------------------------

    print("\n[4] Boolean controls")
    if ac1 is None:
        print("   (skipped)")
    else:
        ac1.set_econo(True)
        ac1.set_swing(horizontal=True, vertical=True)

        ok = wait_for(lambda: ac1.econo and ac1.swingh and ac1.swingv)
        check("econo=True, swingh=True, swingv=True", ok,
              f"econo={ac1.econo} swingh={ac1.swingh} swingv={ac1.swingv}")

        # Reset for cleanliness
        ac1.set_econo(False)
        ac1.set_swing(horizontal=False, vertical=False)
        wait_for(lambda: not ac1.econo and not ac1.swingh)

    # ------------------------------------------------------------------
    # [5] Faikout auto mode (env + target)
    #
    # The Faikout's auto mode is engaged by sending a `control` JSON with
    # `env` (reference temperature from an external sensor) and `target`
    # (desired range as [min, max]).  The sim sets auto_control=True and
    # reflects these in its status.  A real Faikout would begin its
    # predictive heating/cooling cycle.
    #
    # We test on a different unit (TestAC2) to confirm multi-unit
    # independence.
    # ------------------------------------------------------------------

    print("\n[5] Faikout auto mode (env + target)")
    ac2 = ctrl.get("TestAC2")
    if ac2 is None:
        errors.append("TestAC2 not discovered")
        print("   \u2717 TestAC2 not available")
    else:
        ac2.set_auto_target(env=23.5, target_min=22.0, target_max=24.0)

        ok = wait_for(lambda: ac2.target is not None and ac2.env is not None)
        check("Auto target set", ok, f"target={ac2.target} env={ac2.env}")

        if ok:
            check("env=23.5", ac2.env == 23.5, f"env={ac2.env}")
            # target could be [22.0, 24.0] depending on how the sim reflects it
            check("target is a list", isinstance(ac2.target, list),
                  f"target type={type(ac2.target)}")

    # ------------------------------------------------------------------
    # [6] Collection API (iteration, contains, len, get)
    #
    # The controller acts as a dict-like collection of units.  These tests
    # verify the Pythonic container interface works correctly.
    # ------------------------------------------------------------------

    print("\n[6] Collection API")
    check("'TestAC1' in ctrl", "TestAC1" in ctrl)
    check("'Ghost' not in ctrl", "Ghost" not in ctrl)
    check(f"len(ctrl) >= 3", len(ctrl) >= 3, f"len={len(ctrl)}")

    # Iteration should yield FaikoutUnit objects
    iterated = [u.hostname for u in ctrl]
    check(f"iter(ctrl) yields all hosts",
          all(h in iterated for h in SIM_HOSTS),
          f"iterated={iterated}")

    # .get() returns None for missing, unit for present
    check("get('TestAC1') returns unit", ctrl.get("TestAC1") is not None)
    check("get('Ghost') returns None", ctrl.get("Ghost") is None)

    # __getitem__ raises KeyError for missing
    try:
        _ = ctrl["Ghost"]
        check("ctrl['Ghost'] raises KeyError", False, "no exception raised")
    except KeyError:
        check("ctrl['Ghost'] raises KeyError", True)

    # ------------------------------------------------------------------
    # [7] Sensor data
    #
    # The sim populates realistic sensor values: room temp, outside temp,
    # inlet/liquid temps, humidity, fan RPM, compressor freq, power draw.
    # We verify that these propagate through to the unit object.
    #
    # We request a fresh status first to ensure the data is current.
    # ------------------------------------------------------------------

    print("\n[7] Sensor data")
    if ac1 is None:
        print("   (skipped)")
    else:
        ac1.request_status()
        # Wait for a fresh status cycle
        time.sleep(SIM_REPORT_INTERVAL + 0.5)

        sensors = {
            "home": ac1.home,
            "outside": ac1.outside,
            "inlet": ac1.inlet,
            "liquid": ac1.liquid,
            "hum": ac1.hum,
            "fanrpm": ac1.fanrpm,
            "comp": ac1.comp,
            "consumption": ac1.consumption,
        }
        for name, val in sensors.items():
            if val is not None:
                print(f"   \u2713 {name} = {val}")
            else:
                print(f"   ~ {name} = None (model may not report)")

        populated = sum(1 for v in sensors.values() if v is not None)
        check(f"At least 6 of 8 sensors populated ({populated}/8)", populated >= 6)

    # ------------------------------------------------------------------
    # [8] Settings
    #
    # Settings are persistent config sent to `setting/{hostname}` or
    # `setting/{hostname}/{key}`.  We can't easily verify they took
    # effect (the sim just logs them), but we verify the publish doesn't
    # raise and the API is ergonomic.
    # ------------------------------------------------------------------

    print("\n[8] Settings")
    if ac1 is None:
        print("   (skipped)")
    else:
        try:
            ac1.set_setting("livestatus", True)
            ac1.set_settings(reporting=5, livestatus=True)
            check("set_setting() and set_settings() succeed", True)
        except Exception as e:
            check("Settings methods", False, str(e))

    # ------------------------------------------------------------------
    # [9] Continuous state refresh
    #
    # Verify that the controller keeps receiving status updates over time,
    # not just the initial retained message on subscribe.  We check that
    # last_seen advances, which proves the MQTT loop is alive and the
    # sim is still publishing.
    #
    # This is the test that motivated running the sim as a subprocess.
    # With both in-process, paho-mqtt's background threads would silently
    # stop delivering messages after ~30s of activity.
    # ------------------------------------------------------------------

    print("\n[9] Continuous state refresh")
    if ac1 is None:
        print("   (skipped)")
    else:
        # Verify the sim subprocess is still alive.
        sim_alive = sim_proc.poll() is None
        print(f"   sim process alive: {sim_alive} (pid={sim_proc.pid})")
        print(f"   ctrl.connected: {ctrl.connected}")

        # Force a fresh subscribe cycle by requesting status, which
        # triggers the sim to publish immediately.
        ac1.request_status()

        ts_before = ac1.last_seen
        ok = wait_for(
            lambda: ac1.last_seen > ts_before,
            timeout=SIM_REPORT_INTERVAL * 5 + 2,
        )
        elapsed = ac1.last_seen - ts_before
        check(f"last_seen advanced by {elapsed:.1f}s", ok,
              f"stuck at {ts_before}, connected={ctrl.connected}")

    # ------------------------------------------------------------------
    # [10] Unit string representation
    #
    # A minor but useful check: __str__ should produce a human-readable
    # summary without crashing, even with None fields.
    # ------------------------------------------------------------------

    print("\n[10] String representation")
    for host in SIM_HOSTS:
        u = ctrl.get(host)
        if u:
            s = str(u)
            check(f"str({host}) = \"{s}\"", len(s) > 10)

    # ------------------------------------------------------------------
    # [11] Offline detection
    #
    # When a Faikout device disconnects, the broker publishes its LWT
    # ("false" on `state/{hostname}`).  We simulate this by killing the
    # sim and checking the controller marks units offline.
    #
    # Note: the sim only sets LWT for the first unit (MQTT limitation —
    # one will per client), so we only check TestAC1.
    # ------------------------------------------------------------------

    print("\n[11] Offline detection")
    offline_hosts: list[str] = []

    @ctrl.on_offline
    def _on_offline(unit: FaikoutUnit):
        offline_hosts.append(unit.hostname)

    stop_simulator(sim_proc)
    # The LWT fires on unclean disconnect.  SIGINT may cause clean
    # disconnect (no LWT), so we also manually publish the offline marker.
    time.sleep(1)
    subprocess.run(
        ["mosquitto_pub", "-t", "state/TestAC1", "-m", "false", "-r"],
        capture_output=True,
    )
    ok = wait_for(lambda: len(offline_hosts) > 0, timeout=5)
    check("Offline callback fired", ok, f"offline_hosts={offline_hosts}")
    if ac1:
        check("TestAC1.online = False", ac1.online is False,
              f"online={ac1.online}")

    # ------------------------------------------------------------------
    # Teardown
    # ------------------------------------------------------------------

    print("\n[teardown] Cleaning up...")
    ctrl.stop()
    stop_simulator(sim_proc)  # idempotent if already stopped
    clear_retained()
    print("   \u2713 Done")

    # ------------------------------------------------------------------
    # Summary
    # ------------------------------------------------------------------

    print("\n" + "=" * 70)
    if errors:
        print(f"  RESULT: {len(errors)} FAILURE(S)")
        for e in errors:
            print(f"    \u2717 {e}")
    else:
        print("  RESULT: ALL TESTS PASSED \u2713")
    print("=" * 70 + "\n")

    return len(errors) == 0


if __name__ == "__main__":
    success = test_full_flow()
    sys.exit(0 if success else 1)
