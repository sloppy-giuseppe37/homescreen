#!/bin/sh
# /usr/local/sbin/homescreen-autoupdate.sh
# Updates the homescreen package if a newer version is available.
# Intended to run from cron every 5 minutes.

set -eu

# Update the repo catalog quietly
pkg update -q

# Check if an upgrade is available (exit 0 = yes, exit 1 = no)
if pkg upgrade -n -q homescreen | grep -q homescreen; then
    pkg upgrade -y homescreen
    service homescreen restart
    logger -t homescreen-autoupdate "Upgraded homescreen and restarted service"
fi
