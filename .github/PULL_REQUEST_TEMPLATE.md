## What and why

## Checked

- [ ] `scripts/test.sh -short` passes (vet, race detector, short fuzz)
- [ ] `shellcheck -S warning install.sh scripts/*.sh` is clean if a shell script changed
- [ ] `scripts/test-install.sh` / `test-adopt.sh` / `test-ctl.sh` pass if the installer changed
- [ ] docs and `deploy/*.toml` examples updated if a flag or config key changed
- [ ] no host names, IP addresses, secrets or pair codes in the change
