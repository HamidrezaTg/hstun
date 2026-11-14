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
| `hstun-ctl pin VERSION` / `pin --clear` | make `update` install exactly one release, so every host stays on the same one |
| `hstun-ctl rotate-secret` | new secret without downtime, see below |
| `hstun-ctl show-join [--no-secret]` | the pair code for an entry (contains the secret); `--no-secret` prints the join string without it |
| `hstun-ctl show-secret` | the exit's secret |
| `hstun-ctl backup [FILE]` | one private `.tar.gz` with every instance: settings, secrets, certificates, units |
| `hstun-ctl restore FILE [--test]` | check a backup (`--test`), or restore it |
| `hstun-ctl adopt [--dry-run] [--unit U]` / `forget` | bring a hand-installed setup under `hstun-ctl`, or stop managing it |
| `hstun-ctl uninstall [--purge]` | remove the units and firewall rules; `--purge` also deletes config, secret, decoy and, when no instance is left, the binary |

Re-running the installer for an instance that exists shows what it has and asks: keep it, change settings (certificate, secret and paths stay, so
paired entries keep working), or reinstall from scratch (new certificate and secret: every entry must be paired again).

Several instances can live on one host (`--name NAME` on every command, or pick from the menu).

## Stopping and restarting

`systemctl stop|restart`, `hstun-ctl restart`, `update` and `edit` send SIGTERM, which starts a **graceful stop**:

1. The user ports close (entry) or the tunnel port stops accepting (exit), so nothing new starts; the exit also refuses new streams.
2. Streams already running get up to `drain_timeout` (default `10s`) to finish. A request that is in flight completes; a stream that is still open when the time is up is cut and its two ends see a connection reset.
3. A second SIGTERM or Ctrl+C stops at once.

Long-lived connections (a VLESS session that runs for hours) do not finish in 10 s, so a restart costs at most that much extra and then cuts them, exactly as before. Raise it (`--drain-timeout 60s`, or `drain_timeout` in the config) if your traffic is mostly short transfers and you want them never cut; `0s` restores the old stop-at-once behaviour. The systemd unit allows 90 s, so keep it below that.

While draining, `/stats` shows `"draining": true`, and the log says how many streams it is waiting for.

## Limits per key

`--max-mbit N` and `--max-new-streams N` on the exit (installer flags, also `hstun-ctl edit`) cap one secret key's bandwidth and stream rate, see [config reference](config-reference.md). They protect the exit when an entry misbehaves or its secret leaks; they are not traffic shaping for fairness between your own users, who all share the entry's key.

## Rotating the secret

The exit accepts several secrets at once, so nothing has to be interrupted:

1. On the exit: `hstun-ctl rotate-secret`. It adds a second secret next to the first, restarts the service, checks that it is healthy (and undoes everything if it is not), and prints a new pair code.
2. On every entry: `hstun-ctl rotate-secret --join 'hstun-pair:...'`. The entry switches to the new secret, and goes back to the old one if its tunnel does not come up.
3. On the exit, when every entry is done: `hstun-ctl rotate-secret --finish`. Only the new secret is accepted from then on.

Run `hstun-ctl rotate-secret` again at any time during step 2 to print the same pair code. The menu has the same steps under *Rotate the secret*.

## Keeping hosts on one release

`hstun-ctl pin 0.5.0` stores the version; `hstun-ctl update` then installs exactly that release (and the menu stops announcing newer ones). `pin --clear` goes back to following the latest. The exit prints the entry's command with the installer URL of its own release (`.../vX.Y.Z/install.sh`), so both hosts start on the same version.

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
