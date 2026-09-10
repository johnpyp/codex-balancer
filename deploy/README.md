# Linux deployment

The supplied user units use systemd socket activation on port **8000**. The
socket's `ListenStream` controls the actual listener; it overrides the ordinary
server default of `127.0.0.1:8317`. This template binds all interfaces and keeps
provider key authentication enabled. Restrict the listener with a firewall,
protected network, or `-client-access-config` before enabling it. Dashboard,
statistics, and enrollment routes are not protected by provider API keys.

## Manual installation

Build and install the binary from the repository root, then enroll at least one
account and provision a client key as the service user. See the root README.
These units expect the binary in `/usr/local/bin`:

```sh
sudo install -m 0755 codex-balancer /usr/local/bin/codex-balancer
mkdir -p "$HOME/.config/systemd/user"
cp deploy/codex-balancer.service deploy/codex-balancer.socket "$HOME/.config/systemd/user/"
```

Review the copied units and their network exposure. For a local-only service,
change `ListenStream=8000` to `ListenStream=127.0.0.1:8000` and set the service's
`-addr` to `127.0.0.1:8000`. Native keyless mode additionally needs `-no-auth`.
Keep both addresses consistent. If using a client policy, copy
`client-access.example.json` outside the repository and pass its absolute path
with `-client-access-config` in `ExecStart`.

After configuring the units:

```sh
systemctl --user daemon-reload
systemctl --user enable --now codex-balancer.socket codex-balancer.service
journalctl --user -u codex-balancer.service -f
```

State belongs to the service user and defaults to `~/.codex-balancer/state.db`.
Socket activation preserves the listener across service restarts, but existing
streams still disconnect. A changed socket address requires restarting the
socket as well as the service.

## Optional deployment helper

`deploy/deploy-codex-balancer` is an operator-run update script. It requires a
clean checkout on `main`, fetches `origin/main`, fast-forwards, builds the binary,
installs it with noninteractive sudo, copies the units, and starts/restarts the
service as needed. It checks `/stats` on `127.0.0.1:8000`, records the deployed
revision, and installs an updated copy of itself in `/usr/local/bin`.

It does not run the test suite; run `scripts/verify` before deploying. It replaces
the installed units from the checkout, so use manual deployment if you maintain
machine-specific copies. Its health check assumes port 8000 and a policy that
allows loopback. It requires an active user systemd bus and preconfigured sudo
rights for the installation commands.

Defaults can be overridden with these environment variables:

| Variable | Default |
| --- | --- |
| `CODEX_BALANCER_REPO` | `$HOME/code/codex-balancer` |
| `CODEX_BALANCER_BINARY` | `/usr/local/bin/codex-balancer` |
| `CODEX_BALANCER_DEPLOY_COMMAND` | `/usr/local/bin/deploy-codex-balancer` |
| `CODEX_BALANCER_BRANCH` | `main` |
| `CODEX_BALANCER_SERVICE` | `codex-balancer.service` |
| `CODEX_BALANCER_SOCKET` | `codex-balancer.socket` |
| `CODEX_BALANCER_STATE_DIR` | `${XDG_STATE_HOME:-$HOME/.local/state}/codex-balancer` |
| `CODEX_BALANCER_UNIT_DIR` | `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user` |

No machine credentials, enrolled accounts, or live network policy are included.
