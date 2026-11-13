# Troubleshooting

Start with `hstun-ctl doctor` on the host that has the problem. It checks the configuration, clock, boot setup, disk, nginx, certificate, DNS, firewall and (on an entry) whether the exit can be reached, then runs `hstun-ctl test`. `hstun-ctl logs -f` shows what the service says. Remove secrets and pair codes before you share any output.

## The entry says "no tunnel session"

The entry cannot talk to the exit.

- `hstun-ctl doctor` on the entry: does the exit's address resolve, does a plain TCP connection to it work?
- No TCP connection: the exit's firewall or your cloud provider's firewall is closed on the public port (the installer opens `ufw` only), or the route is blocked. Through a CDN that cuts long connections, install the entry with `--carrier chunk`.
- TCP works but there is no session: wrong secret or paths, or a stale pin. Pair again: run `hstun-ctl show-join` on the exit and the printed command on the entry. The exit's `hstun-ctl status` shows `handshake_fail` going up when secrets differ.
- Check that both clocks are right (`timedatectl`). TLS and the handshake need a correct time.
- After "Reinstall from scratch" on the exit the certificate and secret are new, so every entry must be paired again. "Change settings" keeps them.

## The tunnel is up but nothing works

`hstun-ctl test` says which part is missing. The common cause is that nothing listens on the exit's forward target: the installer asked "where should tunnelled traffic go" and expects your proxy (for example a VLESS inbound) on that address. `ss -ltn` on the exit shows whether it is there.
Also check that your proxy clients connect to the entry's forward port (default `20151`) and that this port is open in the entry host's firewall.

## "server is an IP address, so no TLS SNI is sent"

A log line from the entry when the exit is addressed by IP. With a pinned self-signed certificate (the `direct` default) it is normal and logged as information. If connections are reset right after connecting and the certificate is a normal one, set `--sni some.hostname` (or `sni` in `client.toml`).

## Some clients connect, others do not (IPv6)

If the domain has an AAAA record, clients that prefer IPv6 go there. The exit must listen on IPv6 (`[::]`) and its firewall must allow it. `hstun-ctl doctor` says when there is an AAAA record and the exit does not listen on IPv6; fix it with `hstun-ctl edit --ipv6` (or remove the AAAA record).

## "port N is already in use"

Pick another public port with `--port`, or stop the service that holds it (`ss -ltnp`). nginx mode and direct mode default to 443 and fall back to 2083 when 443 is taken.

## certbot / Let's Encrypt fails

`--acme` needs port 80 free and the domain's DNS pointing at this host. `hstun-ctl doctor` checks the DNS. Otherwise give your own certificate with `--cert` and `--key`.

## An update went wrong

`hstun-ctl update` checks every config with the new binary first, and if the tunnel does not come back healthy it restores the previous binary by itself. `hstun-ctl rollback` goes back manually. `hstun-ctl edit` undoes a settings change the same way.

## Remove everything

`hstun-ctl uninstall --purge` removes the units, firewall rules, configuration, secret, certificate and decoy of that instance, and the binary when no instance is left. Without `--purge` the settings are kept.
