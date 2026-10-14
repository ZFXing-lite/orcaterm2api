# 🐋 OrcaTerm AI Gateway (orcaterm2api)

**Version 1.0.1**

*Single binary + one YAML config · no database · zero third-party dependencies · packs the OrcaTerm built-in AI assistant's account pool, rate limiting, cross-account retries and SOCKS5 egress into one process, exposing OpenAI / Anthropic / Responses APIs.*

> Unofficial project, not affiliated with Tencent Cloud or OrcaTerm. For personally authorized accounts and local / private research use only; please confirm compliance with the OrcaTerm Terms of Service before use.

- [中文 README](README.md)

## Features

- **Three downstream APIs**: OpenAI `/v1/chat/completions`, Anthropic `/v1/messages`, OpenAI Responses `/v1/responses`, all with SSE streaming
- **Multi-account pool**: weighted-random picking, per-account in-flight caps, 429/5xx cooldowns, consecutive-failure circuit breaking (exponential backoff), auto-disable on auth expiry
- **Dual credentials + one-click pairing**: desktop `ot_session` and browser-console `uin/skey` cookies mix in one pool; the panel issues single-use pairing codes with a bookmarklet — click it on any logged-in web page and the account lands in the pool (credentials probe-verified first)
- **Cross-account retry**: per-request failover driven by `retry-on` status codes, upstream semantics preserved (a 429 is passed through as 429, never masked as 502)
- **Session stickiness**: `X-Session-Id` (and friends) pins account + upstream conversation; multi-turn context never bounces accounts; `system` only applies to the first turn (upstream semantics)
- **Closed-loop tool calls**: upstream `review` becomes OpenAI `tool_calls`; results are fed back into the same conversation in the exact format the OrcaTerm client uses
- **SOCKS5/HTTP proxy pool**: hand-rolled dialer (no-auth / user-pass), round-robin / random / sticky-key policies, health checks, failure cooldown, per-account dedicated proxies
- **Web management panel**: zero-dependency single-file page; add/remove/import accounts, downstream keys, proxy pool, usage stats, gateway settings — all online
- **Hot reload**: config file changes apply live (accounts, proxies, pool parameters, upstream URL) without restart
- **Secure by default**: with no panel password set, the management panel and pprof accept loopback connections only; the password is only ever transmitted via request headers

## Upstream Protocol

The gateway wraps the OrcaTerm built-in AI assistant as an OpenAI-compatible API. The upstream protocol comes from packet captures of the OrcaTerm desktop client (protocol research credit: [cirnosalt/orcabridge](https://github.com/cirnosalt/orcabridge)):

- `POST https://lightai.cloud.tencent.com/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true`
- Auth: `Authorization: Bearer <ot_session>` + cookies (`ot_session` / `sid` / `userId`)
- Session: `conversationId` in the `cid-<uuid>-orcaterm` format; history is kept server-side
- Models: 18 (8 current + 9 legacy + 1 alias), default `deepseek-v4-flash`
- Tools: upstream `action=review` + `data.tool` becomes OpenAI `tool_calls`; results are fed back in the native format
- Auth expiry: HTTP 200 + `code=10050000` → the account is disabled and the request retries on another account

## Quick Start

### Option 1: Docker Compose (recommended)

```
git clone https://github.com/ZFXing-lite/orcaterm2api.git
cd orcaterm2api
cp config.example.yaml config.yaml
# Edit config.yaml and set remote-management.secret-key (the panel password)
ORCATERM2API_MGMT_KEY=your-panel-password docker compose up -d --build
```

Open `http://<server>:8317/v0/management/panel`, log in with the password, and add accounts on the "Accounts" page.

> `ORCATERM2API_ALLOW_REMOTE` defaults to `0` (panel local-only). To access the panel remotely set `ORCATERM2API_ALLOW_REMOTE=1` and **always set a panel password**. **When no panel password is configured, the management panel and `/debug/pprof` accept loopback connections only, regardless of this switch.**

### Option 2: Build locally

Requires Go ≥ 1.22.

```
go build -ldflags="-s -w" -o orcaterm2api ./cmd/server
cp config.example.yaml config.yaml
ORCATERM2API_MGMT_KEY=your-panel-password ./orcaterm2api -c config.yaml
```

### After install: three steps

1. Visit `http://127.0.0.1:8317/v0/management/panel`
2. Log in with the panel password
3. Add account cookies on the "Accounts" page (see below); confirm the default model under "Settings"

With no upstream accounts `/healthz` reports `ready: false`; it flips to `ready: true` once accounts exist.

## Getting Account Cookies

Each upstream account = one OrcaTerm login session. Two flavors are supported and can be mixed in one pool:

| Flavor | Key cookie fields | How to get |
|---|---|---|
| Desktop (OAuth) | `ot_session` (JWT, `sid` optional) | Desktop client DevTools → Network → `assistant/chat` request header; or `%LOCALAPPDATA%\com.orcaterm-desktop.app\.cookies` |
| Browser console | `uin` + `skey` + `userId` | Log in to the [OrcaTerm web app](https://orcaterm.cloud.tencent.com), then run `copy(document.cookie)` in the F12 console |

- The gateway picks the auth style per credential automatically (`Bearer ot_session` or cookie replay).
- Console cookies have no standalone expiry; when they go stale the upstream returns `NEED_UIN_AND_SKEY`, which is handled exactly like the desktop `code=10050000`: auto-disable + failover to another account.

### One-click pairing (bookmarklet, recommended)

Panel → "Accounts" → **one-click pairing** → get a pairing code plus a bookmarklet → drag the bookmarklet into your browser's bookmarks bar → click it on any page where you are logged in to the OrcaTerm web app, and the login state is delivered to the gateway automatically:

- Before persisting, the gateway validates the credential against the upstream read-only API; bad cookies are rejected with the reason
- Pairing codes are single-use and valid for 15 minutes; the consume endpoint authenticates by pairing code alone (no admin key needed)
- Local deployments (`http://127.0.0.1:*`) work out of the box; remote deployments should put https in front of the gateway (browsers allow https pages to call localhost, but block cross-origin plain http)

Paste into the panel's "Accounts" page; bulk import is supported (one `name----cookie` per line).

> The upstream has no refresh endpoint; re-pair when the login expires.

## Client Integration

| Client | Base URL | API |
| --- | --- | --- |
| OpenAI SDK / curl | `http://host:8317/v1` | `/v1/chat/completions` |
| Anthropic SDK / Claude Code | `http://host:8317` | `/v1/messages` (`x-api-key`) |
| Codex / Qoder | `http://host:8317/v1` | `/v1/responses` |

curl example:

```
export OPENAI_API_KEY=<downstream key you set in the panel>
curl -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

`GET /v1/models` returns 18 model entries plus the alias `orcaterm-assistant`.

### Session stickiness

Send the `X-Session-Id` header (or the OpenAI `user` field / `metadata.conversation_id`) to reuse the upstream conversation and **pin it to one account**: multi-turn context never bounces accounts. `system` messages only apply to the first turn of a new conversation (upstream semantics) and are ignored when a session is reused.

### Streaming

`stream: true` returns gateway-synthesized SSE (the upstream is called non-streaming; the gateway parses the full result then synthesizes the stream, flushing chunk by chunk). Response headers are committed only after the upstream result arrives, so upstream failures before the first byte are already retried/failovered internally. `stream_options.include_usage` appends a usage chunk (locally estimated).

### Tool calls

Declared tool names must be OrcaTerm native tools (`fetch`, `execute_terminal_command`, `list_terminals`, `run_commands`, `submit_agent_tasks`, `read_cloud_space_file`, ...). The gateway turns upstream `review` into OpenAI `tool_calls`; your client executes them and returns results, which the gateway feeds back into the same conversation in the native format.

Limits (upstream capability boundaries; explicit 400s instead of silent ignores):

- Arbitrary custom functions cannot be registered (the upstream only knows native tool names; unknown names are honestly listed in the `X-OrcaTerm-Unsupported-Tools` response header)
- No `tool_choice: "required"`, no named forced tool choice (including the `{"type":"tool"}` form), no `parallel_tool_calls: true`, and only one tool result per turn
- The upstream `ask` action (confirmation question) is a normal 200 with `finish_reason` `stop`; the real action is in the `X-OrcaTerm-Action: ask` response header

## Configuration

`config.yaml` is auto-generated if missing and must be writable (the panel writes back). Environment variables override matching entries. Config file changes hot-reload (accounts, proxies, rate-limit parameters, upstream URL all follow) without restart. Full reference: [`config.example.yaml`](config.example.yaml).

| Variable | Description |
| --- | --- |
| `ORCATERM2API_MGMT_KEY` | Panel password (overrides `remote-management.secret-key`) |
| `ORCATERM2API_ALLOW_REMOTE` | `1` = allow remote panel access (requires a panel password; otherwise loopback-only regardless) |
| `ORCATERM2API_API_KEYS` | Downstream keys, comma-separated |
| `ORCATERM2API_BASE_URL` | Upstream base URL override |
| `ORCATERM2API_DEFAULT_MODEL` | Default model override |
| `ORCATERM2API_PORT` | Listen port override |

## Security Model

- **Downstream calls**: empty `api-keys` = no auth (use carefully on localhost / private networks); otherwise `Authorization: Bearer` or `x-api-key`
- **Management panel & ops API**: password via `X-Mgmt-Key` / `Authorization` headers only — never via URL query; with an empty password, **loopback connections only**
- **Diagnostics**: `/debug/pprof` requires the same permission as the panel; unreachable from the internet by default
- **Persistence**: config and state files are written atomically (tmp + rename) with mode 0600; upstream cookies never appear in logs or the `/status` snapshot

## Management Panel

`http://127.0.0.1:8317/v0/management/panel` — zero-dependency single-file page, auto-refreshing every 5 seconds:

- **Overview**: status, total requests, error rate, account/proxy counts, default model
- **Accounts**: per-account status / cooldown reason / token expiry / in-flight / success-fail counters, one-click disable/enable/delete, form add, bulk import
- **Downstream keys**: add/remove client keys; deletions take effect immediately
- **Proxy pool**: node health/success/fail counters, paste-to-replace whole pool
- **API stats**: requests / errors / in-flight / last status code per `/v1/` path
- **Token usage**: per-account and per-model totals (locally estimated), persisted to `state/usage.json`
- **Settings**: default model, proxy policy, upstream URL, etc. — saved changes apply instantly

Management API lives at `/v0/management/api/*` (`X-Mgmt-Key` required).

## Account Pool Behavior

- **Picking**: top-5 by effective weight, weighted-random draw; LRU breaks ties; when everything is cooling down, falls back to the soonest-to-recover account (still capped by the per-account in-flight limit)
- **In-flight lease**: max concurrent requests per account (`rate-limit.max-in-flight`); full accounts sit out
- **Cooldowns**: 429 (honors `Retry-After`, capped at 10m, no stacking), 5xx, consecutive-failure circuit breaker (exponential backoff capped at 6h)
- **Auth expiry**: `code=10050000` disables the account (re-enable in the panel) and fails the current request over to the next account; no refresh endpoint, cookies must be updated manually
- **Transport failures** (dead proxy / timeout): temporary `err-cooldown` demotion, not counted as account taint
- **Persistence**: pool state and usage are written atomically to `state/` (tmp + rename, 0600) and restored on restart

## API Reference

| Path | Description |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions (streaming supported) |
| `POST /v1/messages` | Anthropic Messages (streaming supported) |
| `POST /v1/responses` | OpenAI Responses (streaming supported) |
| `GET /v1/models` | 18-model catalog + alias |
| `GET /healthz` | Liveness probe; `ready` = has usable upstream accounts |
| `GET /status` | Redacted pool & usage snapshot (downstream key required) |
| `*/v0/management/panel` | Web management panel |
| `*/v0/management/*` | Ops API (panel password required) |

## Project Layout

```
orcaterm2api/
├── cmd/server/main.go        # entrypoint
├── internal/
│   ├── config/               # YAML config (hand-rolled parser, zero deps) + hot reload + atomic write-back
│   ├── orca/                 # OrcaTerm upstream protocol client (auth/session/models/tools)
│   ├── keypool/              # upstream account pool (picking/cooldown/circuit-breaker/retry/persistence)
│   ├── proxypool/            # SOCKS5/HTTP proxy pool (hand-rolled dialer, zero deps)
│   ├── relay/                # orchestration: session stickiness / tool loop / OpenAI responses / SSE synthesis
│   ├── metrics/              # token usage (local estimate) + API stats
│   └── server/               # HTTP routing + Anthropic/Responses adapters + management panel
├── config.example.yaml
├── docker-compose.yml
├── Dockerfile
└── README.md / README_EN.md
```

## Development

```
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

## Changelog

### v1.1.0

**Dual credentials + one-click pairing**

- The account pool accepts two login flavors: desktop `ot_session` (OAuth) and browser-console `uin/skey` cookies (log in to the OrcaTerm web app and run `copy(document.cookie)`); both can be mixed in one pool
- New "one-click pairing" in the panel: 15-minute single-use pairing codes with a ready-made bookmarklet; click the bookmark on any logged-in web page and the account lands in the pool automatically
- Credentials are probe-verified against the upstream before persisting; invalid cookies are rejected with the reason; codes are consumed before verification, which makes replay impossible
- Console-cookie expiry (`NEED_UIN_AND_SKEY`) is treated the same as desktop `code=10050000`: auto-disable + cross-account retry
- The panel shows per-account credential kind (JWT expiry / console marker)

### v1.0.1

**Security**

- With no panel password set, the management panel and ops API accept loopback connections only (previously: default config allowed unauthenticated access from the network)
- Removed the URL-query channel for the panel password (headers only)
- `/debug/pprof` now requires the same permission as the management panel (previously exposed)

**Fixes**

- SSE streams were buffered by the response wrapper so clients received nothing for long stretches (`statusRecorder` did not forward `http.Flusher`; all three APIs were affected)
- First bytes after an HTTP proxy CONNECT were lost to the bufio pre-read (TLS handshakes could fail)
- Upstream clients no longer follow hot-reloaded base-url / timeout / proxy settings
- 429s were misreported as 502 after cross-account retries were exhausted; auth expiry is no longer misreported as 500
- Auth-expiry failover never actually retried because 401 was not in `retry-on`
- Named forced tool choice (`{"type":"tool"}`) was silently accepted, contradicting the documented 400

**Other**

- Account-pool rate-limit parameters (max-in-flight / cooldowns / retry policy) now hot-reload
- Proxy pool snapshots display the real protocol; usage persistence unified under `state/`
- Added `internal/server` and `internal/relay` regression tests

## Caveats

- The upstream `lightai.cloud.tencent.com` AI capabilities and model catalog may change at any time; `GET /v1/models` is a static snapshot and does not auto-sync
- The upstream returns no token counts; gateway usage figures are local estimates for trend-watching only
- The `ot_session` JWT has no refresh endpoint; cookies must be re-copied manually on expiry
- Tool calls depend on the OrcaTerm native tool table (server-published); custom functions cannot be registered

## Disclaimer

This project is for personal learning and research only. Use only OrcaTerm accounts you personally own and are authorized for, comply with the OrcaTerm Terms of Service and local laws; account risks arising from use are borne by the user.

MIT License
