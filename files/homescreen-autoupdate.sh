#!/bin/sh
# /usr/local/sbin/homescreen-autoupdate.sh
# Updates the homescreen package if a newer version is available.
# Intended to run from cron every 5 minutes.
# Silent on success; stderr preserved on failure.

set -eu

# Update the repo catalog quietly
pkg update -q >/dev/null

# Check if an upgrade is available (exit 0 = yes, exit 1 = no)
if pkg upgrade -n -q homescreen 2>/dev/null | grep -q homescreen; then
    pkg upgrade -y homescreen >/dev/null
    service homescreen restart >/dev/null 2>&1
    logger -t homescreen-autoupdate "Upgraded homescreen and restarted service"
fi
