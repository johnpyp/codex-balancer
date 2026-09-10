# Provider compatibility

This fork extends upstream `dcd4d957` to support the provider surfaces used by
Codex 0.153.4. It retains the original quota-aware pool, SQLite identities, and
Responses WebSocket relay.

## Native ChatGPT login with keyless loopback access

Run the proxy with `-no-auth -addr 127.0.0.1:8317` and use these top-level
Codex settings:

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8317/backend-api/codex"
```

Codex keeps its real ChatGPT login and built-in OpenAI provider. No
`experimental_bearer_token`, provider `env_key`, or replacement provider auth
is configured. In Codex 0.153.4 this retains the native eligibility checks for
experimental context, routing hints, and Guardian endpoints. Account entitlement
and feature flags still apply.

The proxy does not inspect Codex's login file, validate the incoming ChatGPT
token, import it, or refresh it. Incoming authorization and account ID headers
are replaced with the independently managed pool account selected by existing
thread/session affinity. New requests work regardless of client token rotation.

This mode deliberately allows local processes to use the pool without a proxy
key. Without `-client-access-config`, the fork rejects `-no-auth` unless both the
configured address and the actual listener are loopback. The actual listener is
also checked for systemd socket activation, which can otherwise override the configured address. Use a
literal IP such as `127.0.0.1` or `::1`, not a hostname or wildcard. The existing
key-authenticated mode remains available when `-no-auth` is absent.

For direct access, use `-client-access-config /path/to/client-access.json`
and `-addr 0.0.0.0:8317`. The JSON file controls allowed socket-peer networks:

```json
{"allowed_cidrs": ["127.0.0.1/32", "::1/128"]}
```

This example allows only loopback; add the specific client CIDRs you intend to
trust before enabling remote access.

The file is validated at startup and re-read for every HTTP request and WebSocket
handshake. Edits apply without a restart; existing streams are not interrupted.
Use an atomic file replacement when editing. Empty lists deny everyone; missing
or invalid files fail closed. Forwarding headers cannot override the peer address.
This policy covers all routes. With `-no-auth`, configuring the policy permits a
non-loopback listener. Remote Codex uses
`openai_base_url = "http://balancer.example:8317/backend-api/codex"`.

Keep `chatgpt_base_url` unchanged: account services, connectors, and remote
control retain their normal account-specific behavior. This setup does not merge
account entitlements or make encrypted state portable across accounts. Existing
loaded tasks and saved custom-provider selections need migration when activating
native mode; changing a global default does not rewrite them. The code change
does not itself deploy a new service, edit Codex settings, or restart tasks.

## Forwarding contract

- `/v1/responses`, `/v1/guardian`, and `/v1/guardian-classifier` support both
  WebSocket inference and streaming HTTP Responses. Endpoint paths and query
  parameters are retained when opening upstream WebSockets.
- Other `/v1/*` HTTP requests and WebSocket upgrades are forwarded relative to
  the configured upstream. This includes `alpha/search`, `responses/compact`,
  `images/generations`, `images/edits`, `memories/trace_summarize`, `realtime/calls`,
  and future provider endpoints. There is no arbitrary upstream-host parameter.
- `/backend-api/codex/*` is an equivalent alias, including the models catalog.
  Prefer this base URL for Codex because some features select a backend request
  shape by inspecting the URL.
- Local client credentials are replaced with the selected account's OAuth
  authorization and account ID. Cookies, secondary API keys, proxy credentials,
  and hop-by-hop headers are removed. No redirect is followed with credentials.
- Request JSON, unknown fields, gzip/zstd payloads, multipart bodies, binary
  bodies, and provider metadata are preserved. Only a bounded inspection copy
  is decoded. Wire and decoded request limits are each 64 MiB.
- Responses stream immediately. Unknown SSE events, statuses, error bodies,
  provider headers, compression, and trailers pass through. Usage observation
  is bounded to 8 MiB per JSON response or SSE event and never changes output.
- Downstream cancellation and account invalidation cancel the upstream request.
  A definitive HTTP 401 may refresh and retry once on the same identity. Network
  failures, 429s, 5xx, partial streams, and ambiguous writes are never replayed.

## Identity rules

HTTP and WebSocket requests share provisional claims and durable thread/session
owners. HTTP extracts ordinary session/thread headers, Codex turn metadata, and
standalone search's session ID in the JSON `id` field. Image model names are not
filtered through the text-model catalog.

HTTP requests never move an existing session to a replacement account. Search
references, encrypted reasoning/compaction, and response IDs may be bound to
their originating identity. The WebSocket relay additionally refuses an account
move when the replay carries encrypted input or item references. These fields
are neither stripped nor rewritten. Fresh sessions still use quota-aware
placement; token refresh preserves identity.

## Validation and limits

Regression tests cover endpoint routing, the backend alias, authentication,
credential removal, exact JSON/multipart/compressed bodies, compressed responses,
binary upgrades, Guardian WebSockets, SSE flushing/multiline data/trailers/usage,
cancellation, quota errors, same-account refresh, and encrypted-state affinity.
The suite also retains upstream WebSocket routing and account lifecycle tests.

Forwarding an endpoint does not imply the ChatGPT backend implements every public
OpenAI API operation or that an account is entitled to it. In particular, generic
realtime/image forwarding has synthetic coverage; this repository does not
include private deployment records or claim live validation of every endpoint.
The proxy does not translate public API request schemas into different backend schemas or invent missing features.
