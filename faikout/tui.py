#!/usr/bin/env python3
"""Faikout TUI — live dashboard for all Faikout units on an MQTT broker.

Displays a continuously-updating terminal dashboard showing:
-   Power state, mode, target temp, fan speed
-   Room / outdoor / inlet / liquid temperatures
-   Humidity, compressor frequency, fan RPM, power draw
-   Swing, econo, powerful, quiet, and other feature flags
-   Activity sparkline showing recent temperature history
-   Per-unit staleness indicator (seconds since last update)

Usage::

    # Default broker (localhost:1883)
    uv run python tui.py

    # Custom broker
    uv run python tui.py --broker 192.168.1.10 --port 1883

    # Faster refresh
    uv run python tui.py --refresh 0.5

Requires: rich, faikout
"""

from __future__ import annotations

import argparse
import sys
import time
from collections import defaultdict
from typing import Optional

from rich.console import Console
from rich.layout import Layout
from rich.live import Live
from rich.panel import Panel
from rich.table import Table
from rich.text import Text

from faikout import FaikoutController, FaikoutUnit, Mode, Fan

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# Spark characters for the temperature history sparkline (low to high).
SPARK_CHARS = "▁▂▃▄▅▆▇█"

# How many history points to keep for sparklines.
HISTORY_LEN = 30

# Mode display: emoji + colour
MODE_DISPLAY = {
    Mode.COOL:  ("\u2744\ufe0f ", "cyan"),
    Mode.HEAT:  ("\U0001f525", "red"),
    Mode.AUTO:  ("\u267b\ufe0f ", "green"),
    Mode.DRY:   ("\U0001f4a7", "yellow"),
    Mode.FAN:   ("\U0001f4a8", "blue"),
}

# Fan display
FAN_DISPLAY = {
    Fan.AUTO:    "Auto",
    Fan.SPEED_1: "\u2581",
    Fan.SPEED_2: "\u2582\u2582",
    Fan.SPEED_3: "\u2584\u2584\u2584",
    Fan.SPEED_4: "\u2586\u2586\u2586\u2586",
    Fan.SPEED_5: "\u2588\u2588\u2588\u2588\u2588",
    Fan.QUIET:   "\U0001f910",
}


# ---------------------------------------------------------------------------
# Sparkline helper
# ---------------------------------------------------------------------------

def sparkline(values: list[float], width: int = 20) -> str:
    """Render a list of floats as a sparkline string."""
    if not values:
        return ""
    recent = values[-width:]
    lo, hi = min(recent), max(recent)
    span = hi - lo if hi != lo else 1.0
    chars = []
    for v in recent:
        idx = int((v - lo) / span * (len(SPARK_CHARS) - 1))
        idx = max(0, min(len(SPARK_CHARS) - 1, idx))
        chars.append(SPARK_CHARS[idx])
    return "".join(chars)


def fmt_temp(val: Optional[float]) -> str:
    """Format a temperature value."""
    if val is None:
        return "—"
    return f"{val:.1f}\u00b0"


def fmt_val(val, suffix: str = "") -> str:
    """Format a numeric value with optional suffix."""
    if val is None:
        return "—"
    return f"{val}{suffix}"


def age_str(last_seen: float) -> Text:
    """Format seconds-since-last-update with colour coding."""
    age = time.time() - last_seen
    if age < 5:
        return Text(f"{age:.0f}s", style="green")
    elif age < 30:
        return Text(f"{age:.0f}s", style="yellow")
    elif age < 120:
        return Text(f"{age:.0f}s", style="red")
    else:
        return Text(f"{age:.0f}s", style="red bold")


def flags_str(unit: FaikoutUnit) -> str:
    """Build a compact flags string for boolean features."""
    flags = []
    if unit.swingh and unit.swingv:
        flags.append("\u21c4\u21c5")  # H+V swing
    elif unit.swingh:
        flags.append("\u21c4")  # H swing
    elif unit.swingv:
        flags.append("\u21c5")  # V swing
    if unit.econo:
        flags.append("\U0001f331")  # eco
    if unit.powerful:
        flags.append("\u26a1")  # powerful
    if unit.quiet:
        flags.append("\U0001f910")  # quiet
    if unit.comfort:
        flags.append("\U0001f6cb\ufe0f")  # comfort
    if unit.streamer:
        flags.append("\U0001f32c\ufe0f")  # streamer/purifier
    if unit.sensor:
        flags.append("\U0001f441\ufe0f")  # intelligent eye
    if unit.control:
        flags.append("\U0001f916")  # auto/external control
    return " ".join(flags) if flags else "\u2014"


# ---------------------------------------------------------------------------
# Dashboard builder
# ---------------------------------------------------------------------------

def build_unit_panel(
    unit: FaikoutUnit,
    temp_history: list[float],
) -> Panel:
    """Build a rich Panel for a single unit."""

    # Header: power + mode
    if not unit.online:
        title_style = "dim red"
        title_text = f"{unit.hostname}  \u26a0 OFFLINE"
    elif not unit.power:
        title_style = "dim"
        title_text = f"{unit.hostname}  \u23fb OFF"
    else:
        mode_icon, mode_colour = MODE_DISPLAY.get(
            unit.mode, ("?", "white")
        )
        mode_name = unit.mode.name if unit.mode else "?"
        title_style = f"bold {mode_colour}"
        title_text = f"{unit.hostname}  {mode_icon} {mode_name}"

    # Temperatures table
    t = Table.grid(padding=(0, 2))
    t.add_column("label", style="dim", width=10)
    t.add_column("value", width=8)
    t.add_column("label2", style="dim", width=10)
    t.add_column("value2", width=8)

    t.add_row(
        "Target", Text(fmt_temp(unit.temp), style="bold"),
        "Room", Text(fmt_temp(unit.home), style="bold cyan"),
    )
    t.add_row(
        "Outside", fmt_temp(unit.outside),
        "Inlet", fmt_temp(unit.inlet),
    )
    t.add_row(
        "Liquid", fmt_temp(unit.liquid),
        "Humidity", fmt_val(unit.hum, "%"),
    )

    # Mechanical
    m = Table.grid(padding=(0, 2))
    m.add_column("label", style="dim", width=10)
    m.add_column("value", width=8)
    m.add_column("label2", style="dim", width=10)
    m.add_column("value2", width=8)

    fan_display = FAN_DISPLAY.get(unit.fan, "?") if unit.fan else "\u2014"
    m.add_row(
        "Fan", fan_display,
        "Fan RPM", fmt_val(unit.fanrpm),
    )
    m.add_row(
        "Compressor", fmt_val(unit.comp, "Hz"),
        "Power", fmt_val(unit.consumption, "W"),
    )

    # Sparkline
    spark = sparkline(temp_history)
    lo = min(temp_history) if temp_history else 0
    hi = max(temp_history) if temp_history else 0
    spark_label = f"Room temp ({lo:.1f}\u2013{hi:.1f}\u00b0)" if temp_history else "Room temp"

    # Flags
    flags = flags_str(unit)

    # Model + age
    model_str = unit.model or "unknown"
    age = age_str(unit.last_seen)

    # Assemble
    content = Table.grid(padding=(0, 0))
    content.add_column()
    content.add_row(t)
    content.add_row("")
    content.add_row(m)
    content.add_row("")
    content.add_row(Text(spark, style="cyan"))
    content.add_row(Text(spark_label, style="dim"))
    content.add_row("")

    # Footer line: flags | model | age
    footer = Table.grid(padding=(0, 2))
    footer.add_column(width=22)
    footer.add_column(style="dim", width=12)
    footer.add_column(width=8, justify="right")
    footer.add_row(flags, model_str, age)
    content.add_row(footer)

    border_style = "dim" if not unit.power else MODE_DISPLAY.get(
        unit.mode, ("?", "white")
    )[1]

    return Panel(
        content,
        title=title_text,
        title_align="left",
        style=title_style,
        border_style=border_style,
        width=50,
    )


def build_header(ctrl: FaikoutController, broker: str, port: int) -> Table:
    """Build the top status bar."""
    t = Table.grid(padding=(0, 3))
    t.add_column(justify="left")
    t.add_column(justify="center", ratio=1)
    t.add_column(justify="right")

    status = Text("\u25cf CONNECTED", style="bold green") if ctrl.connected else Text("\u25cf DISCONNECTED", style="bold red")
    n_units = len(ctrl)
    n_on = sum(1 for u in ctrl if u.power)
    total_w = sum(u.consumption or 0 for u in ctrl)

    t.add_row(
        Text(f" \U0001f3e0 Faikout Dashboard", style="bold"),
        Text(f"{n_units} units  \u00b7  {n_on} active  \u00b7  {total_w}W total", style="dim"),
        Text.assemble(status, f"  {broker}:{port} "),
    )
    return t


def build_dashboard(
    ctrl: FaikoutController,
    broker: str,
    port: int,
    histories: dict[str, list[float]],
    console_width: int = 120,
) -> Table:
    """Build the full dashboard layout."""
    outer = Table.grid(padding=0)
    outer.add_column()

    # Header
    outer.add_row(build_header(ctrl, broker, port))
    outer.add_row("")

    # Unit panels in rows of N
    units = sorted(ctrl.units, key=lambda u: u.hostname)
    if not units:
        outer.add_row(
            Panel(
                Text("\n  Waiting for Faikout units...\n\n"
                     "  Listening on state/+/status\n", style="dim"),
                title="No units discovered",
                border_style="dim",
                width=50,
                height=8,
            )
        )
        return outer

    # Figure out how many columns fit (each panel is 50 chars wide + 2 gap)
    cols_per_row = max(1, console_width // 52)

    row_panels: list[Panel] = []
    for unit in units:
        hist = histories.get(unit.hostname, [])
        row_panels.append(build_unit_panel(unit, hist))

    # Lay out panels in grid rows
    for i in range(0, len(row_panels), cols_per_row):
        row_batch = row_panels[i : i + cols_per_row]
        row_table = Table.grid(padding=(0, 1))
        for _ in row_batch:
            row_table.add_column()
        row_table.add_row(*row_batch)
        outer.add_row(row_table)

    # Footer
    outer.add_row("")
    outer.add_row(Text("  Press Ctrl+C to exit", style="dim italic"))

    return outer


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="Faikout TUI \u2014 live dashboard for Faikout AC units",
    )
    parser.add_argument(
        "--broker", "-b", default="localhost", help="MQTT broker hostname",
    )
    parser.add_argument(
        "--port", "-p", type=int, default=1883, help="MQTT broker port",
    )
    parser.add_argument(
        "--refresh", "-r", type=float, default=1.0,
        help="Dashboard refresh interval in seconds",
    )
    parser.add_argument(
        "--username", "-u", default=None, help="MQTT username",
    )
    parser.add_argument(
        "--password", default=None, help="MQTT password",
    )
    args = parser.parse_args()

    # Temperature history per unit (for sparklines).
    histories: dict[str, list[float]] = defaultdict(list)

    ctrl = FaikoutController(
        broker=args.broker,
        port=args.port,
        username=args.username,
        password=args.password,
        client_id="faikout-tui",
    )

    @ctrl.on_updated
    def _on_update(unit: FaikoutUnit, changed: list[str]):
        """Record room temp history for sparklines."""
        if unit.home is not None:
            hist = histories[unit.hostname]
            hist.append(unit.home)
            if len(hist) > HISTORY_LEN:
                del hist[: len(hist) - HISTORY_LEN]

    ctrl.run_in_thread()

    console = Console()
    try:
        with Live(
            build_dashboard(ctrl, args.broker, args.port, histories, console.width),
            console=console,
            refresh_per_second=4,
            screen=True,
        ) as live:
            while True:
                time.sleep(args.refresh)
                live.update(
                    build_dashboard(ctrl, args.broker, args.port, histories, console.width)
                )
    except KeyboardInterrupt:
        pass
    finally:
        ctrl.shutdown()
        console.print("\n[dim]Disconnected.[/dim]")


if __name__ == "__main__":
    main()
