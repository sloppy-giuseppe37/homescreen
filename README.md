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
| `homescreen_autoupdate_enabled` | `true` | Enable automatic upgrades via cron |
| `homescreen_autoupdate_cron_minute` | `*/5` | Cron minute spec for auto-updates |
| `homescreen_autoupdate_cron_hour` | `*` | Cron hour spec for auto-updates |

## Files to Customize

Before using this role, you must:

1. **Replace `files/homescreen.pub`** with the actual repository signing public key
2. **Edit `files/homescreen.yaml`** with your actual zone/room/light configuration

## What This Role Does

1. Creates pkg repository directories and installs the signing key
2. Configures the homescreen pkg repository
3. Installs the `homescreen` package
4. Deploys the config file to `/usr/local/etc/homescreen.yaml`
5. Enables the homescreen service
6. (Optional) Sets up a cron job for automatic upgrades every 5 minutes

## Example Playbook

```yaml
- hosts: home-server
  roles:
    - role: homescreen
      vars:
        homescreen_autoupdate_enabled: true
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
