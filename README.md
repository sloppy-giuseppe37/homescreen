# Ansible Role: homescreen

Installs and configures the homescreen smart home control panel on FreeBSD.

## Requirements

- FreeBSD target host
- `community.general` collection (for `pkgng` module)

## Role Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `homescreen_repo_url` | `https://sloppy-giuseppe37.github.io/homescreen` | URL of the pkg repository |
| `homescreen_pubkey_file` | `homescreen.pub` | Signing key filename (in `files/`) |
| `homescreen_autoupdate_enabled` | `true` | Enable automatic upgrades via cron (every 5 min) |
| `homescreen_base_url` | (none) | Full URL of the app, e.g. `https://home.example.com:8000` |
| `homescreen_mqtt_broker` | `tcp://localhost:1883` | MQTT broker address |
| `homescreen_mqtt_prefix` | `zigbee2mqtt` | MQTT topic prefix for lights |
| `homescreen_zones` | `[]` | Zone/room/light configuration (see below) |

## Setup

Before using this role:

1. **Replace `files/homescreen.pub`** with the actual repository signing public key
2. **Define `homescreen_zones`** in your `host_vars/` or `group_vars/`

## What This Role Does

1. Creates pkg repository directories and installs the signing key
2. Configures the homescreen pkg repository
3. Installs the `homescreen` package
4. Generates and deploys the config file to `/usr/local/etc/homescreen.yaml`
5. Enables the homescreen service
6. (Optional) Sets up a cron job for automatic upgrades every 5 minutes

## Example

**playbook.yml:**
```yaml
- hosts: home-server
  roles:
    - homescreen
```

**host_vars/home-server.yml:**
```yaml
homescreen_mqtt_broker: "tcp://localhost:1883"
homescreen_mqtt_prefix: "zigbee2mqtt"

homescreen_zones:
  - name: Upstairs
    heating:
      - name: Bedroom
        unit_id: BedroomFaikin
      - name: Guest Room
        unit_id: GuestFaikin
    lights:
      - name: Bedroom
        entities:
          - BedroomLight
      - name: Hallway
        entities:
          - HallLight1
          - HallLight2

  - name: Downstairs
    heating:
      - name: Living Room
        unit_id: LivingRoomFaikin
    lights:
      - name: Kitchen
        entities:
          - KitchenLight
```

## Auto-Update Behavior

When `homescreen_autoupdate_enabled` is `true`, a cron job runs every 5 minutes that:

1. Updates the pkg catalog
2. Checks if a newer version of homescreen is available
3. If yes, upgrades the package and restarts the service
4. Logs upgrades to syslog (`homescreen-autoupdate` tag)

## Dependencies

None.

## License

MIT
