# codex-balancer

A quota-aware proxy that routes Codex requests through a pool of ChatGPT accounts.
It runs as one Go process and stores accounts, client keys, usage, and conversation
routing state in one SQLite database.

This is a compatibility fork of `supabitapp/codex-balancer`, based on upstream
commit `dcd4d957d37be6cef7d2f84585a0e9cedab95880`. The imported source matches fork
commit `e8f9b935bc2d74acb27ad7d3eb2371d536e4800e`, with documentation and deployment
examples cleaned up for this repository. The initial publication uses a fresh
history without local deployment records. The upstream [MIT license](LICENSE)
and third-party asset licenses are retained.

## How it works

1. Add ChatGPT accounts to the pool through browser or device login. The proxy
   manages their OAuth credentials independently of the Codex client's login.
2. For a new conversation, choose an eligible account using manual priority,
   expiring reset credits, and remaining quota. Paused, exhausted, cooling, and
   signed-out accounts are excluded from fresh placement.
3. Replace incoming provider credentials with the selected account's credentials
   and relay requests to `https://chatgpt.com/backend-api/codex` by default.
4. Keep accepted conversations on their account using durable thread/session
   affinity. Quota polling updates availability; a healthy conversation owner
   takes precedence over fresh-placement preferences.

This is not round-robin routing for every turn. Response IDs, encrypted reasoning,
compaction, and other opaque state can belong to one upstream account. WebSocket
account changes require a portable replay from the client; the proxy never strips
that state to force a switch or replays in-flight work itself. HTTP requests keep
an existing session on its owner. See [ROUTING.md](ROUTING.md) for the full policy.

The fork adds streaming HTTP and generic provider forwarding alongside the
Responses WebSocket relay, native Codex login support, and a reloadable client
network policy. Forwarding a path does not grant account entitlements or translate
public OpenAI API schemas into ChatGPT backend schemas. See
[provider compatibility](PROVIDER_COMPATIBILITY.md) for details and limitations.

## Build

Requires Go 1.26 or newer and Git access to this private repository:

```sh
git clone git@github.com:johnpyp/codex-balancer.git
cd codex-balancer
go build -trimpath -o codex-balancer .
mkdir -p "$HOME/.local/bin"
install -m 0755 codex-balancer "$HOME/.local/bin/codex-balancer"
```

Ensure `~/.local/bin` is on `PATH`. Build from this checkout: the Go module path
remains `github.com/supabitapp/codex-balancer` for compatibility, so installing
that module's `@latest` would install upstream rather than this fork.

## Add accounts

Run account and key commands on the proxy host, as the user running the service:

```sh
codex-balancer accounts add
# Or sign in on another device:
codex-balancer accounts add --device-auth
codex-balancer accounts list
```

Enrollment checks the account's current model-training setting. If training is
already disabled, it adds the account without sending an opt-out update. If
training is enabled, it disables training before adding the account. Browser,
device, and web enrollment use this same flow; no manual bypass flag is needed.
If the setting cannot be read or the required update fails, enrollment stops.

## Connect Codex

Choose one of these two modes. The default listen address is `127.0.0.1:8317`.

### Native ChatGPT login on the same machine

Start the proxy:

```sh
codex-balancer server -no-auth -addr 127.0.0.1:8317
```

Set these top-level options in `~/.codex/config.toml`, before any TOML tables:

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8317/backend-api/codex"
```

Codex retains its own ChatGPT login and built-in provider. The proxy ignores the
incoming login credentials and uses its independently enrolled pool accounts.
Any local process can use the pool in this mode. Leave `chatgpt_base_url`
unchanged so account services and connectors keep their normal behavior.
Restart the client to load its configuration; saved sessions with a custom
provider may need their provider selection updated separately.

### Client API key

Provision a key on the proxy host, then start the server with authentication:

```sh
codex-balancer keys add my-laptop
codex-balancer server
```

Store the printed key securely. On the client, set it in the environment before
launching Codex, then configure:

```sh
export CODEX_BALANCER_API_KEY="<key printed by keys add>"
```

```toml
model_provider = "balancer"

[model_providers.balancer]
name = "OpenAI"
base_url = "http://127.0.0.1:8317/backend-api/codex"
env_key = "CODEX_BALANCER_API_KEY"
requires_openai_auth = true
supports_websockets = true
```

Use the provider name `OpenAI` and the `/backend-api/codex` base for Codex backend
feature selection, including compaction. Native mode preserves built-in provider
eligibility checks that a custom provider may not. Compatibility behavior was
established against Codex 0.153.4; client versions and feature flags can differ.

## Network access

For remote clients, use a protected connection such as an SSH tunnel or trusted
private network. The server serves plain HTTP. Provider API keys protect the
inference/model endpoints; `/dashboard`, `/stats`, and `/accounts` do not require
those keys. Restrict access to the whole listener when sharing it.

`-client-access-config` applies a socket-peer CIDR allowlist to every route. Start
with the [loopback-only example](deploy/client-access.example.json), copy it to
`~/.codex-balancer/client-access.json`, and add only the intended client networks.
Then, for example:

```sh
codex-balancer server -addr 0.0.0.0:8317 \
  -client-access-config "$HOME/.codex-balancer/client-access.json" -no-tui
```

This example still requires a client API key. Adding `-no-auth` allows every
permitted peer to use the pool without one. Without a policy file, `-no-auth`
requires a literal loopback address and verifies the actual listener, including
inherited systemd sockets.

The policy is read at startup and for every request or WebSocket handshake.
Replace it atomically when editing. An empty list denies everyone; missing or
invalid files fail closed. Existing streams remain connected. Forwarding headers
do not change the peer address used for access decisions.

## Endpoints and operation

| Endpoint | Purpose |
| --- | --- |
| `/v1/responses`, `/v1/guardian`, `/v1/guardian-classifier` | WebSocket and streaming HTTP inference |
| Other `/v1/*` paths | Provider-relative HTTP and WebSocket forwarding |
| `/backend-api/codex/*` | Equivalent alias with Codex backend request shapes |
| `/v1/models` | Union of known account model catalogs |
| `/dashboard`, `/stats` | Browser dashboard and JSON statistics |
| `/accounts` | Browser account enrollment |

The interactive server shows a TUI with account pause and priority controls.
Use `-no-tui` for a service, `-json` for structured logs, `-poll 2m` to change
quota polling from its 10-minute default, and `-log-file=` to disable file logging.
Run `codex-balancer server -h` for all flags.

```sh
codex-balancer accounts mode you@example.com priority
codex-balancer accounts mode you@example.com normal
codex-balancer accounts rm you@example.com
codex-balancer keys list
codex-balancer keys rm my-laptop
```

`keys list` includes token usage attributed to each key. State defaults to
`~/.codex-balancer/state.db`; use `-state` consistently for the server and CLI
commands if you change it. The database contains credentials and account data.
Keep it, its SQLite sidecars, backups, access policies, and runtime logs outside
Git. Back up a live database with SQLite's backup mechanism or stop the service
before copying its state.

The [deployment directory](deploy/README.md) describes the optional Linux
systemd socket-activated setup and its deployment helper.

## Development

```sh
./scripts/verify
```

This runs `go vet`, the tests with the race detector, and a build in a temporary
directory. Most protocol tests use synthetic credentials and a mock upstream.
The optional real-Codex recovery test is described in [ROUTING.md](ROUTING.md).
