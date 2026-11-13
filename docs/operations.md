# Operating hstun

## Commands

| command | what it does |
|---|---|
| `hstun-ctl` | menu: shows what is installed here and only the actions that make sense |
| `hstun-ctl status` | units, versions, carrier, sessions |
| `hstun-ctl test` | end-to-end check: unit, sessions, and a stream through the tunnel to the exit's target |
| `hstun-ctl doctor` | checks of this host (configuration, clock, boot, disk, nginx, certificate, DNS, firewall, reachability of the exit), then `test` |
| `hstun-ctl logs [-f]` | journal of the instance |
| `hstun-ctl restart` | restart the instance's units |
| `hstun-ctl edit` | change settings (interactive or with flags such as `--port`); undone automatically if the tunnel does not come back healthy |
| `hstun-ctl update [--all] [--force]` | download the latest release, check it accepts every instance's config, replace the binary (previous one kept as `hstun.prev`), restart, roll back by itself if the tunnel does not come back; does nothing when already current |
| `hstun-ctl rollback` | go back to the previous binary (run again to return) |
| `hstun-ctl show-join [--no-secret]` | the pair code for an entry (contains the secret); `--no-secret` prints the join string without it |
| `hstun-ctl show-secret` | the exit's secret |
| `hstun-ctl backup [FILE]` | one private `.tar.gz` with every instance: settings, secrets, certificates, units |
| `hstun-ctl restore FILE [--test]` | check a backup (`--test`), or restore it |
| `hstun-ctl adopt [--dry-run] [--unit U]` / `forget` | bring a hand-installed setup under `hstun-ctl`, or stop managing it |
| `hstun-ctl uninstall [--purge]` | remove the units and firewall rules; `--purge` also deletes config, secret, decoy and, when no instance is left, the binary |

Re-running the installer for an instance that exists shows what it has and asks: keep it, change settings (certificate, secret and paths stay, so
paired entries keep working), or reinstall from scratch (new certificate and secret: every entry must be paired again).

Several instances can live on one host (`--name NAME` on every command, or pick from the menu).

## Adopting a hand-installed setup

If you set hstun up by hand (your own systemd units and config under `/etc/hstun[-NAME]/`), `adopt` lets `hstun-ctl` manage it without
reinstalling anything:

```bash
bash <(curl ...) adopt --dry-run     # shows what it found, writes nothing
bash <(curl ...) adopt               # registers it
```

It scans `/etc/systemd/system/hstun*.service` for `ExecStart=... hstun[-x] server|client -config FILE`, checks that the binary accepts the
config, reads role, ports, paths and carrier from the config (and the nginx unit, its reload units and the domain for an nginx-fronted
exit), and writes one file, `/etc/hstun[-NAME]/install.conf`. Your units, configs, binaries and firewall rules are not touched. The config
directory must be `/etc/hstun` (default instance) or `/etc/hstun-NAME` with `server.toml` or `client.toml` inside; anything else is
skipped with a message. Unit and binary names may be custom (`hstun-chunk.service`, `/usr/local/bin/hstun-chunk`): they are recorded.
Afterwards `status`, `logs`, `restart`, `show-join`, `update` and `uninstall` work as for installed instances.
`update` first runs `hstun check` with the new binary on every config and stops before replacing anything if one is rejected; it replaces
the shared binary and any custom binary of an adopted instance and keeps the old ones as `*.prev`.
`install.sh exit|entry` refuses to run over an adopted or otherwise unmanaged `/etc/hstun[-NAME]` (it would rewrite your files): adopt it
or pick another `--name`. `uninstall` of an adopted instance removes its units (and with `--purge` its config directory and binary), but
leaves ufw rules and certificates, which `hstun-ctl` did not create.


## Offline, from source, and releases

- Offline: copy `hstun_VERSION_linux_ARCH.tar.gz` and run with `HSTUN_TARBALL=/path/to/it`.
- From source: `HSTUN_BUILD=1 hstun-ctl update` (or on the first run). It downloads the source, fetches Go if needed and builds with the vendored dependencies.
- Pin a version: `--version 0.3.1` (or `HSTUN_VERSION`). Another repository (a fork): `--repo OWNER/hstun` (or `HSTUN_REPO`).
- Maintainers: `scripts/package.sh` builds `dist/hstun_VERSION_linux_{amd64,arm64}.tar.gz` and `SHA256SUMS`;
  publish with `gh release create vVERSION dist/hstun_*.tar.gz dist/SHA256SUMS --repo OWNER/hstun`. The installer verifies the checksum.


## Versioned installer URL

`.../main/install.sh` always runs the newest script. To pin the script as well, use a tag: `bash <(curl -Ls https://raw.githubusercontent.com/HamidrezaTg/hstun/vX.Y.Z/install.sh)`.
