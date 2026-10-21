# gemini-stealth-bridge (`gsb`)

[![release](https://img.shields.io/github/v/release/azrinaldi/gemini-stealth-bridge?sort=semver)](https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest)
[![release workflow](https://github.com/azrinaldi/gemini-stealth-bridge/actions/workflows/release.yml/badge.svg)](https://github.com/azrinaldi/gemini-stealth-bridge/actions/workflows/release.yml)
[![go](https://img.shields.io/github/go-mod/go-version/azrinaldi/gemini-stealth-bridge)](go.mod)
[![license](https://img.shields.io/github/license/azrinaldi/gemini-stealth-bridge)](LICENSE)
[![ghcr](https://img.shields.io/badge/ghcr.io-gemini--stealth--bridge-blue)](https://github.com/azrinaldi/gemini-stealth-bridge/pkgs/container/gemini-stealth-bridge)

OpenAI-compatible proxy for [Gemini Web](https://gemini.google.com/app). Connect coding agents and other API clients to your Gemini account through Chat Completions or Responses endpoints.

![gsb Web Control Plane](docs/assets/web-ui.png)

`gsb` translates OpenAI-style messages, images, and tool definitions into Gemini Web requests, then converts the results back into JSON or Server-Sent Events (SSE). It authenticates with your Google session cookies, not a Gemini Developer API key. Model access and quotas depend on the connected account.

The daemon runs without headless Chrome or CDP. Browserless transport is an implementation detail; the API bridge is the interface your clients use.

## Contents

- [Capabilities and limits](#capabilities-and-limits)
- [What's new in v0.4.0](#whats-new-in-v040)
- [What's new in v0.3.0](#whats-new-in-v030)
- [Quick start](#quick-start)
- [Web Control Plane (`/ui`)](#web-control-plane-ui)
- [Account setup](#account-setup)
- [Client integration](#client-integration)
- [API usage](#api-usage)
- [Native image and video generation](#native-image-and-video-generation)
- [Docker deployment](#docker-deployment)
- [Authentication and security](#authentication-and-security)
- [Configuration](#configuration)
- [Operations and troubleshooting](#operations-and-troubleshooting)
- [How it works](#how-it-works)
- [Development](#development)
- [License](#license)

## Capabilities and limits

| Capability | Behavior |
|---|---|
| Chat Completions | `POST /v1/chat/completions`, JSON or SSE output |
| Responses | `POST /v1/responses`, JSON or SSE output |
| Function calling | Translates tool definitions into a prompt contract, validates returned arguments, and emits OpenAI-style calls; the client executes the tools |
| Image & video generation | Native Gemini Web image (Imagen / Nano Banana) and video (Veo MP4 with audio) generation via Chat, Coding Agent tool-calling (`generate_media`), `POST /v1/images/generations`, `POST /v1/videos/generations`, and `GET /v1/media/<id>` proxy |
| Image input | Base64 image data URLs and remote HTTPS images |
| Thinking effort | Maps `low`, `medium`, and `high` to Gemini Web thinking levels |
| Conversation history | Reuses Gemini continuation tokens when matching history is available; offloads older turns into text attachments when the inline prompt budget is exceeded |
| Multiple accounts & quota | Pool key routes across accounts (`rotate`, `lru`, `fill-first`); per-account keys pin one account; group keys serve one user's accounts with failover inside the group. Exposes both rolling **5-hour (`2,400`)** and **7-day weekly (`48,384`)** quota windows |
| Per-account proxies | Each account can egress through its own HTTP/SOCKS5 proxy, configured per account file or via `BRIDGE_EGRESS_PER_ACCOUNT` |
| Web search & grounding | Extracts the Google Search citations Gemini Web returned and appends them to the answer |
| MCP server | Built-in Streamable HTTP MCP endpoint at `POST /mcp` exposing the grounding as a `web_search` tool for Claude Code, Cursor, OMP, and other MCP clients |
| Web Control Plane | Embedded single-binary dashboard at `GET /ui` (`gsb --web` or `./start.sh`) for account onboarding, 5h/weekly quota bars, priority ordering, and runtime configuration |
| Session maintenance | Background cookie rotation and persistence of updated credentials |

This is an unofficial bridge to the Gemini web app, not Google's supported Gemini Developer API or an OpenAI service. Changes to Google's web protocol, sign-in requirements, account restrictions, and quotas can interrupt operation. The transport does not guarantee protection from detection or account restrictions. Use only accounts you are authorized to access and review Google's applicable terms.

Compatibility covers the endpoints and fields described below, not the entire OpenAI API:

- Sampling and output controls such as `temperature`, `top_p`, `max_tokens`, and `max_output_tokens` are not forwarded as generation controls. Do not rely on them to enforce output limits.
- Responses clients must send conversation history in `input`; `previous_response_id` is not a continuation mechanism here. Response retrieval, deletion, and background jobs are not implemented.
- Function calling is prompt-based, not native Gemini Developer API function calling. Malformed or schema-invalid calls can fail with `tool_call_error`.
- Token usage is estimated locally, not reported by Google's billing/tokenizer API. The model registry advertises a 1,048,576-token context window and 65,536-token output limit as metadata, not a guarantee for every account or request.
- Audio input/output, embeddings, and the general OpenAI Files API are not exposed. Internal text-attachment offloading is not a general file-upload API.

## What's new in v0.4.0

Version `0.4.0` adds Google Search grounding with citation extraction, a built-in MCP server, per-account egress proxies, and group keys for multi-user deployments:

1. **Web Search Grounding (`Sources:`)**: When Gemini Web answers with Google Search grounding, `gsb` extracts the cited pages and appends them to the answer as a numbered `Sources:` list of title and URL. `BRIDGE_INCLUDE_GROUNDING=0` returns the answer without the citation list.
2. **Built-in MCP Server (`POST /mcp`, `/v1/mcp`)**: A Streamable HTTP MCP endpoint (JSON-RPC 2.0: `initialize`, `tools/list`, `tools/call`, `ping`) exposes the same grounding as a `web_search` tool for Claude Code, Cursor, OMP, and other MCP clients. It accepts `query`, an optional `model` override, and an optional `timeout_seconds` deadline (10–600s, default 180s, overridable with `BRIDGE_SEARCH_TIMEOUT_MS`). Searches run on the same account pool, quota accounting, and failover as chat requests, and each search is a temporary conversation, so it does not add threads to the account's Gemini history.
3. **Per-Account Egress Proxies**: `BRIDGE_EGRESS_PER_ACCOUNT="acct-a:socks5://127.0.0.1:1081,acct-b:http://proxy.example:3128"` (or `proxyUrl` in an account file / the Web UI's *Outbound Proxy* field) sends each account through its own HTTP or SOCKS5 proxy. Accounts without one keep using `BRIDGE_PROXY_SERVER`; changing a proxy rebuilds that account's transport session.
4. **Group Keys for Multi-User Deployments**: `BRIDGE_GROUP_TOKENS="alice-key:team-a,bob-key:team-b"` maps a token to an account group (`BRIDGE_ACCOUNT_GROUP`), so each user's key serves only that group's accounts, fails over inside the group, and never consumes another group's quota. The pool key still spans every loaded account, and account keys still pin exactly one.
5. **Streaming Performance**: The tool-envelope detector no longer uses a regexp. It previously re-scanned the whole accumulated answer on every frame and was 98% of the server's CPU on a 50-frame turn. A 50-frame streaming turn with tools now runs in ~46 µs instead of ~172 ms, with allocations down from 1.8 MB to 8 KB.

## What's new in v0.3.0

Version `0.3.0` adds an embedded Web Control Plane, dual-window (`5h` + `7d Weekly`) quota telemetry, zero-conflict isolated browser login, native image & video generation across chat/agents/REST, hardened tool-call parsing with schema coercion, and Docker packaging:

1. **Embedded Web Control Plane (`/ui`)**: Zero-dependency fluid full-width HTML/CSS/JS dashboard compiled directly into the `gsb` binary (`./start.sh`, `gsb --web`, or `BRIDGE_WEB_UI=1`; compile-time removable via `-tags nowebui`). Displays side-by-side **Pool Connection** vs. **Individual Account** quick-connect keys, dual-window **5h & Weekly** quota progress bars, a `Current Turn` badge on the active rotation account, priority reordering (`↑`/`↓`), a compact `⋯` per-account action menu (Refresh, Pause/Resume, Clear Quarantine, Reset Failures, Copy Key, Remove), and live runtime configuration persisted to `$BRIDGE_HOME/config.json`.
2. **Dual-Window Quota Telemetry (`5h` + `7d Weekly`)**: Queries Gemini Web's GraphQL `GetUserQuotaService` (`GXU_FIVE_HOURLY` and `GXU_WEEKLY`) alongside `batchexecute` (`qpEbW`), exposing both the rolling **5-hour window (`2,400` credits)** and the **7-day weekly window (`48,384` credits)** with exact reset timestamps in `/v1/accounts`, `/ui`, and `gsb --accounts --accounts-live`. Effective 5-hour remaining credits are automatically capped when the weekly pool runs lower than the 5-hour window.
3. **Isolated Browser Login (Zero Cookie-Rotation Conflicts)**: `gsb --login <name>` and the Web UI's **Launch Isolated Browser** button spawn a temporary Chrome/Chromium/Brave/Edge instance with a disposable `--user-data-dir`, auto-detect Google sign-in completion (`SAPISID`) via CDP, capture the cookies, and immediately close and delete the temporary profile so no browser process ever competes with `gsb` for `__Secure-1PSIDTS` cookie rotation. Includes `gsb --login-helper` (`127.0.0.1:20134`) for one-click desktop popup login when managing a remote VPS over SSH port forwarding, plus guided Incognito cookie paste.
4. **Native Image & Video Generation (Chat, Coding Agents, and REST)**:
   - **Coding Agent Mode (`tools` enabled)**: Intent-gated injection of the built-in `generate_media` tool (`media_type`, `prompt`, `aspect_ratio`, `format`, `duration`, `output_path`) only when the prompt requests visual/image/diagram/video generation—keeping `SessionCache` fingerprints intact on normal coding turns. When invoked (or when Gemini Web triggers its native image generator directly), `gsb` proxies the media at `GET /v1/media/<id>.png|.jpg|.mp4` and guides the agent through saving the file directly into the user's workspace via `curl`.
   - **Full-Resolution `=s0-d` & Lossless PNG `=s0-rp`**: Downloads unscaled original images (`=s0-d`) by default, supports lossless PNG (`=s0-rp`) when `format: "png"` is requested, follows application-level `text/plain` redirects (`?alr=yes`), and extracts Gemini Web's internal `modelDescription` as `revised_prompt`.
   - **OpenAI Endpoints & Direct Chat**: `POST /v1/images/generations` (`url` or `b64_json`, `size`/`aspect_ratio`, `output_format`/`format`) and `POST /v1/videos/generations` (Veo MP4 with audio, `aspect_ratio`, `duration`, and `thumbnail_url`), plus automatic `/v1/media/<id>` URL rewriting in direct chat.
5. **Hardened Tool-Call Parser, Schema Coercion & Refusal Recovery**:
   - **Automatic Schema Coercion**: Coerces stringified numbers, booleans, and JSON objects/arrays emitted by smaller models into the exact types declared by the tool's JSON Schema.
   - **Redundant Brace/Bracket Repair**: Strips stray closing braces/brackets (`}}]}}`) outside string literals and detects truncated `{"tool_calls":[...` arrays for automatic retry so raw JSON envelopes never leak as plain answer text.
   - **Refusal Recovery**: Extracts valid tool calls even when preceded by refusal prose, retries automatically when a model refuses a required tool turn, and excludes client/schema `tool_call_error` from marking an account `degraded`.
6. **Zero-CGO Docker Packaging**: Multi-stage `Dockerfile` (~14 MB Alpine image, non-root `uid 10001`, built-in `--healthcheck`) and `docker-compose.yml` with persistent `/data` volume.
## Quick start

### 1. Install

Pick whichever path fits: a prebuilt binary (no Go toolchain, no Docker), a Go install, or the container image.

**Prebuilt binary (recommended)** — every release publishes static binaries for Linux, macOS, and Windows on both `amd64` and `arm64`, plus `SHA256SUMS`:

```sh
# Linux x86_64; swap in darwin_arm64, linux_arm64, windows_amd64.exe, ...
curl -fL -o gsb https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest/download/gsb_linux_amd64
chmod +x gsb
./gsb --version
```

**From source** — requires **Go 1.26.0 or newer**, Git, and an account that can use Gemini Web. Shell examples use a POSIX shell; on Windows, run the binary as `gsb.exe` and adapt environment-variable assignments to your shell.

```sh
git clone https://github.com/azrinaldi/gemini-stealth-bridge.git
cd gemini-stealth-bridge
go build -o gsb ./cmd/gsb
```

Alternatively, install into your Go binary directory and use `gsb` instead of `./gsb`:

```sh
go install github.com/azrinaldi/gemini-stealth-bridge/cmd/gsb@latest
```

**Container** — Docker is optional; see [Docker deployment](#docker-deployment). If you only want the binary, you can use the image purely as an installer and never leave a container running:

```sh
cid=$(docker create ghcr.io/azrinaldi/gemini-stealth-bridge:latest)
docker cp "$cid:/usr/local/bin/gsb" ./gsb
docker rm "$cid"
```

### 2. Connect a Gemini account

Sign in through `gsb` so the session is captured in an isolated disposable browser profile that will never conflict with background cookie rotation:

```sh
./gsb --login acc1
./gsb --probe
```

### 3. Start the server (with Web Control Plane)

```sh
./start.sh
# or directly:
./gsb --web
```

The default address is `http://127.0.0.1:20133` and the Web Control Plane is available at `http://127.0.0.1:20133/ui`. Keep this default local-only setup on loopback; see [Authentication and security](#authentication-and-security) before sharing access.

## Web Control Plane (`/ui`)

Start `gsb` with `./start.sh`, `./gsb --web`, or `BRIDGE_WEB_UI=1` and open `http://127.0.0.1:20133/ui`:

- **Quick-Connect Guide**: Copy the **Pool Connection** Base URL and Pool Key (recommended for `9router`, OpenCode, Cursor, OMP, Cline) or click any account's `Key: gsb-<name>` chip to pin a single account.
- **Account Pool & Dual-Window Quota Bars**: Inspect every account's live status (`Ready`, `Cooling`, `Degraded`, `Exhausted`, `Expired`, `Quarantined`, `Paused`), the `Current Turn` sticky rotation cursor badge, and both **5h (`2,400`)** and **Weekly (`48,384`)** credit bars with reset countdowns.
- **Add Account Modal (`+ Add Account`)**:
  - **Option 1 (Works Local & Remote SSH)**: Open a New Incognito Window (`Ctrl+Shift+N`), sign in to `gemini.google.com/app`, copy cookies via Cookie-Editor, immediately close the Incognito window without signing out, and paste.
  - **Option 2 (One-Click Isolated Browser)**: Launches a temporary Chrome/Chromium window that auto-captures cookies on sign-in and wipes its profile on exit. When managing a remote VPS over SSH port forwarding (`ssh -L 20133:127.0.0.1:20133`), run `gsb --login-helper` on your laptop so the button pops open a local browser window and sends the captured session to the VPS.
  - **Option 3 (Local Profile Import)**: Import from an already-signed-in local browser profile directory.
- **Runtime Configuration Tab**: Edit routing strategy (`rotate`, `lru`, `fill-first`), default thinking effort, rate limits, and timeouts live, persisted atomically to `$BRIDGE_HOME/config.json`.

### 4. Send a request

There are three kinds of key. Pick one:

| Key | Value | Behavior |
|---|---|---|
| **Pool key** | `BRIDGE_API_KEY` (default `public`) | Routes each request across all loaded accounts using `BRIDGE_ACCOUNT_STRATEGY`. Use this normally |
| **Group key** | `BRIDGE_GROUP_TOKENS` entry (`token:group`) | Serves only the accounts of its group and fails over inside that group. Use this in multi-user deployments |
| **Account key** | `gsb-<name>` or `<name>` | Pins the request to that account and never fails over. Use this to test or isolate one account |

List the models available to the pool:

```sh
curl -sS http://127.0.0.1:20133/v1/models \
  -H 'Authorization: Bearer public'
```

Set `MODEL` to an `id` returned by that endpoint. `gemini-flash` below is an example, not a promise of availability:

```sh
export MODEL=gemini-flash
curl -sS http://127.0.0.1:20133/v1/chat/completions \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}]}"
```

Read the answer from `choices[0].message.content`. `/health` checks that the daemon is running; only a successful model request verifies upstream generation.

`X-GSB-Account` in the response headers names the account that served the request, which is the quickest way to see the pool working. Replace `public` with `gsb-acc1` to pin the same request to one account.

## Account setup

Each imported account is stored in `~/.gemini-stealth-bridge/accounts/<name>.json`, or under `$BRIDGE_HOME/accounts/` when configured. Names accept 1-64 characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, and `-`.

Cookie imports bootstrap the account against Gemini Web before reporting success. Treat both source cookie exports and saved account files as credentials.

### Sign in with `--login` (recommended)

```sh
./gsb --login acc1
```

`--login` launches a temporary Chrome/Chromium/Brave/Edge window using a disposable `--user-data-dir`, opens `https://gemini.google.com/app`, and monitors cookies via CDP (with a terminal prompt fallback if no local browser is available). As soon as Google sign-in completes and the `SAPISID` cookie is present, `gsb` captures the session, verifies it against Gemini Web, and deletes the temporary browser directory so no background browser process ever competes with `gsb` for `__Secure-1PSIDTS` rotation:

1. Run `./gsb --login <name>` and complete Google sign-in in the opened isolated browser window, including any 2FA or "verify it's you" prompt.
2. If operating on a headless server without a local browser, paste a `Cookie` header or Cookie-Editor JSON export exported from an **Incognito** window that you immediately close after copying.
3. Run `./gsb --probe` and confirm the account reports models and 5h/weekly quota without an auth error.

If an account later starts failing with an expired-session error, repeat `gsb --login <name>` instead of re-exporting stale cookies. `./gsb --rotate` only renews rotating cookies such as `__Secure-1PSIDTS`; it cannot restore a revoked or never-established sign-in.
### Reuse a signed-in browser profile

Despite its name, `--import-chrome` also recognizes other Chromium browsers and Firefox cookie databases. No browser is launched during import. Use this only when that browser profile is already signed in to Gemini.

```sh
# Auto-detect a local cookie database.
./gsb --import-chrome acc1

# Choose a profile by name.
./gsb --import-chrome acc1 --chrome-profile "Profile 4"

# Choose a profile directory or cookie database explicitly.
./gsb --import-chrome acc1 --cookie-db "$HOME/.config/google-chrome/Default"
```

`--chrome-profile` also accepts a directory or cookie database path. `--cookie-db` takes precedence when both are supplied.

Arc is supported too. On macOS its profile lives at `~/Library/Application Support/Arc/User Data`; on Windows it lives under the MSIX package directory `%LOCALAPPDATA%\Packages\TheBrowserCompany.Arc_*\LocalCache\Local\Arc\User Data` (auto-detected via glob):

```sh
# macOS
./gsb --import-chrome acc1 --cookie-db "$HOME/Library/Application Support/Arc/User Data/Default"

# Or just name the browser and let gsb find the profile.
./gsb --import-chrome acc1 --chrome-profile arc
```

When no profile is given, auto-detection scans every known browser and prefers the first database that actually contains a `SAPISID` cookie, so an unsigned-in Chrome install no longer shadows a signed-in Arc, Brave, or Edge profile.

Decryption depends on the browser, OS, and access to the browser's encryption keys. Linux keyring-backed cookies may require `secret-tool` and an unlocked desktop keyring. On macOS `gsb` queries the login Keychain for the matching `<Browser> Safe Storage` item (`Chrome Safe Storage`, `Arc Safe Storage`, `Brave Safe Storage`, …) and macOS asks you to allow access the first time; `CHROME_PASS` overrides the lookup when Keychain access is unavailable. Windows DPAPI decryption requires the same Windows user context; App-Bound (`v20`) encryption requires a manual export. A detected database is not a guarantee that its cookies can be decrypted.

### Cookie header or JSON export

Import either a raw `Cookie` header value or a Cookie-Editor JSON array:

```sh
./gsb --import-cookie acc1 --cookie-file /path/to/cookies.json
```

The same payload can be supplied on stdin:

```sh
./gsb --import-cookie acc1 < /path/to/cookies.json
```

`--cookie-value` accepts an inline payload, but puts credentials in shell history and potentially process listings. Prefer a protected file or stdin. Export the Google cookies used by the authenticated Gemini session; an incomplete or expired export will not bootstrap successfully.

### Additional accounts

Repeat the login with another name:

```sh
./gsb --login acc2
```

Or, if that account is already signed in to a different browser profile:

```sh
./gsb --import-chrome acc2 --chrome-profile "Profile 2"
```

Each account has its own cookie state, model list, quota state, and transport session. Multiple files belonging to the same Google account may be deduplicated; additional files do not create additional upstream quota.

## Client integration

### Direct connection

Configure any client that supports a custom OpenAI-compatible endpoint:

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:20133/v1` |
| API key | Pool key (`BRIDGE_API_KEY`, default `public`) for automatic rotation/failover, or `gsb-acc1` to pin `acc1` |
| API mode | Chat Completions or Responses, matching the client's adapter |
| Model | An ID from `GET /v1/models` using the same key |

Choose Chat Completions for clients that consume `choices` / `tool_calls`, or Responses for clients that consume `output` items and `response.*` events. Client support for reasoning fields varies. OpenAI compatibility does not imply that every coding agent or every client-specific feature has been tested.

### Through `9router`

Add `gsb` as an OpenAI-compatible provider in `9router`:

| Provider setting | Value |
|---|---|
| Name | `Gemini Stealth Bridge` |
| Base URL | `http://127.0.0.1:20133/v1` |
| API type | Chat Completions (`chat`) or Responses (`responses`) |
| Prefix | `gsb` |
| Credential | Pool key (`BRIDGE_API_KEY`) to let `gsb` rotate accounts, or an account key (`gsb-acc1`) to pin one account |

Add a separate provider credential for each account if you want the router to manage account selection. Point the coding agent at the router's URL with the router's own API key; use the model name/prefix exposed by the router, for example `gsb/gemini-flash`. The bridge also accepts the `gsb/` model prefix directly.

**Prefer one connection with the pool key.** Account rotation belongs to `gsb` because it is the only component that knows per-account quota, quarantine state, and conversation continuation affinity. A router that spreads requests across per-account connections cannot see any of that: it only learns about exhaustion from a `429`, and it will move the next turn to a different account, discarding the cached Gemini session. Use per-account connections only when you deliberately want to pin traffic to one account.

The base URL must be reachable **from the router process**. Inside a container, `127.0.0.1` refers to that container, not the host running `gsb`. Any non-loopback deployment needs the security measures below. If your router caches or overrides model metadata, verify what survives:

```sh
# Check the bridge alone (all fields required)
./scripts/check-router-metadata.sh http://127.0.0.1:20133 public

# Compare the bridge against the router; reports exactly which fields the router dropped
./scripts/check-router-metadata.sh --compare \
  http://127.0.0.1:20133 http://127.0.0.1:20130 - sk-router-key
```

The compare form distinguishes a bridge problem from a router problem. A router that drops `capabilities.thinkingLevels` is why a harness cannot render a low/medium/high selector, and the script names that field rather than blaming the bridge.

## API usage

The following examples assume the daemon is running, `acc1` is imported, and `MODEL` is set to an available model as shown in the quick start. They use `gsb-acc1` so the account serving each request is predictable. Replace it with your pool key (`BRIDGE_API_KEY`, `public` by default) to let `gsb` rotate across accounts.

### Streaming chat

Use `curl -N` to avoid buffering SSE output:

```sh
curl -N -sS http://127.0.0.1:20133/v1/chat/completions \
  -H 'Authorization: Bearer gsb-acc1' \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"stream\":true,\"reasoning_effort\":\"high\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain how this bridge works.\"}]}"
```

Chat streams emit `chat.completion.chunk` objects and finish with `data: [DONE]`. Text uses `choices[].delta.content`; reasoning, when extracted and enabled, uses the extension `choices[].delta.reasoning_content`. Tool calls use `choices[].delta.tool_calls`.

### Responses

```sh
curl -sS http://127.0.0.1:20133/v1/responses \
  -H 'Authorization: Bearer gsb-acc1' \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"instructions\":\"Keep answers concise.\",\"input\":\"Explain how this bridge works.\"}"
```

Read the result from the returned `output` items. Set `stream: true` for events including `response.created`, `response.output_text.delta`, `response.reasoning_summary_text.delta`, `response.function_call_arguments.delta`, and `response.completed`.

For multi-turn calls, resend the conversation in `input`, including assistant output and any `function_call` / `function_call_output` items. The bridge's internal continuation cache is separate from OpenAI's `previous_response_id` feature.

### Tools, images, and thinking

- For Chat Completions, supply function tools using `tools: [{"type":"function","function":{"name":"get_status","description":"Read service status","parameters":{"type":"object","properties":{},"additionalProperties":false}}}]`. Responses accepts flat function definitions with `type`, `name`, `description`, and `parameters`.
- `tool_choice` supports `auto`, `none`, `required`, or a named function. To force a function, use `{"type":"function","function":{"name":"get_status"}}` for Chat or `{"type":"function","name":"get_status"}` for Responses. The bridge returns calls; it never executes your functions. Append the assistant call and matching tool result to the next request's history.
- Chat image content uses `{"type":"image_url","image_url":{"url":"..."}}`; Responses uses `{"type":"input_image","image_url":"..."}`. URLs can be base64 image data URLs or public HTTPS URLs. Remote-image downloads are limited to 20 MiB and reject private/loopback destinations by default. The total JSON request body is limited to 32 MiB.
- Set Chat `reasoning_effort` or Responses `reasoning.effort` to `low`, `medium`, or `high`. A model suffix such as `gemini-flash@think=high` is also accepted; an explicit body setting takes precedence.
- The configured default thinking effort is `low` for reasoning-capable models. Disabling-style values map to `low`; they do not disable Gemini's thinking entirely.
- `BRIDGE_LIVE_THOUGHTS=false` suppresses separately extracted reasoning output; it does not change the upstream thinking level or guarantee removal of every model-written reasoning passage from ordinary answer text.
- When tools are enabled, prose answers longer than 320 bytes stream live instead of being held for the full turn; mid-answer tool envelopes and fenced envelope look-alikes inside plain prose stay out of the streamed prefix until parsing commits the turn. Envelope mentions quoted as documentation pass through as text and never error as malformed calls.
- The thinking-effort config maps only `low`, `medium`, and `high`; an explicit `BRIDGE_DEFAULT_THINKING=medium` in the environment or an explicit body `effort` raises the built-in `low` default for that deployment or request.


## Native image and video generation

`gsb` exposes Gemini Web's native image generation (**Nano Banana / Imagen**) and video generation (**Veo MP4 with audio**) across three surfaces. Because upstream `lh3.googleusercontent.com` and `contribution.usercontent.google.com` media URLs require authenticated Google session cookies, `gsb` automatically registers and serves generated media through its local proxy endpoint `GET /v1/media/<id>.jpg|.mp4`.

### 1. Coding Agent Mode (`tools` enabled)

When a coding agent (`OpenCode`, `OMP`, `Cursor`, `Cline`, etc.) sends a `/v1/chat/completions` or `/v1/responses` request with `tools` and the user prompt asks for an image, diagram, illustration, or video, `gsb` injects a built-in server-side tool named `generate_media` (leaving non-media coding turns untouched so `SessionCache` hits are preserved).

Supported `generate_media` parameters:
- `media_type` (`"image"` | `"video"`, required)
- `prompt` (detailed visual description, required)
- `aspect_ratio` (`"1:1"`, `"16:9"`, `"9:16"`, `"4:3"`, `"3:4"`)
- `format` (`"png"` for lossless `=s0-rp` diagrams/text, `"jpeg"` for original unscaled `=s0-d`, or `"gif"`)
- `duration` (`10`, `15`, or `30` seconds for video)
- `output_path` (optional relative path in the user's workspace, e.g. `./gsb-workflow.png`)

When the model invokes `generate_media` (or triggers Gemini Web's native media generator directly on a tool-enabled turn):
1. `gsb` runs the generation on Gemini Web and downloads the full-resolution media (`=s0-d` / `=s0-rp`, following any `?alr=yes` redirects).
2. `gsb` caches and serves the file at `http://127.0.0.1:20133/v1/media/<id>.png|.jpg|.mp4`.
3. `gsb` feeds the tool result (including `url`, `revised_prompt`, `markdown`, and a ready-to-run `local_download_cmd` `curl` command) back into a continuation turn with the full tool contract so the coding agent saves the file directly into your project workspace via `bash`.

### 2. Direct Chat / Responses (without tools)

Ask for an image or video in a normal chat request; `gsb` enables Gemini Web's media extensions and rewrites the returned Markdown URLs to `http://127.0.0.1:20133/v1/media/<id>.jpg|.mp4`.

### 3. Dedicated OpenAI Media Endpoints

Generate an image (`url` or `b64_json`, with optional `aspect_ratio` / `size` and `format` / `output_format`):

```sh
curl -sS http://127.0.0.1:20133/v1/images/generations \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Minimalist flat vector architecture diagram of an API bridge in light mode","aspect_ratio":"16:9","format":"png","response_format":"url"}'
```

Generate a video (Veo MP4 with audio, typically 60–150 seconds, with optional `aspect_ratio` and `duration`):

```sh
curl -sS --max-time 180 http://127.0.0.1:20133/v1/videos/generations \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"A glowing neon paper airplane flying smoothly through a rainy cyberpunk alley at night","aspect_ratio":"16:9","duration":10}'
```
## Docker deployment

Docker is **optional**. The published image is multi-stage and zero-CGO (measured: `11.9 MB` on `overlayfs`, `~5 MB` RSS idle, non-root `uid 10001`, Web UI enabled on `:20133`). `docker compose up` pulls it from GHCR, so no Go toolchain and no local build is needed:

```sh
docker compose up -d
```

To build from this checkout instead:

```sh
BRIDGE_IMAGE=gemini-stealth-bridge:dev docker compose up -d --build
```

Compose keeps state in the named volume `gsb-data`, which is pre-owned by the container user. To keep data on the host instead, point `BRIDGE_DATA_DIR` at a directory and either `chown` it to `10001:10001` or run the container as your own UID:

```sh
# host bind mount instead of the named volume
mkdir -p data
# option A: give the directory to the container user
sudo chown -R 10001:10001 data
BRIDGE_DATA_DIR=./data docker compose up -d

# option B: FUSE / NTFS / exFAT mounts that reject chown -> run as your own UID
BRIDGE_DATA_DIR=./data BRIDGE_UID=$(id -u) BRIDGE_GID=$(id -g) docker compose up -d
```

The published port is bound to `127.0.0.1` by default, so `/ui` is reached from the host or through an SSH tunnel. Because Docker's bridge NAT makes every request look remote, the control plane stays locked (HTTP `403`) while `BRIDGE_API_KEY` is the default `public`; set one of these before opening it:

```sh
BRIDGE_ADMIN_KEY=$(openssl rand -hex 24) docker compose up -d
# then open http://127.0.0.1:20133/ui and paste that key into the auth prompt
```

Or with `docker run` bound to loopback:

```sh
docker run -d --name gsb \
  -p 127.0.0.1:20133:20133 \
  -v gsb-data:/data \
  -e BRIDGE_ADMIN_KEY="$(openssl rand -hex 24)" \
  ghcr.io/azrinaldi/gemini-stealth-bridge:latest
```

Open `http://127.0.0.1:20133/ui` (or forward the port via SSH `ssh -L 20133:127.0.0.1:20133 user@server`) to add accounts and configure the pool.

### Endpoint reference

All `/v1/*` endpoints below use the same authentication resolver. Account keys scope models, accounts, and quota to their account; session status and refresh are not restricted to an admin-only API.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/ui` | Embedded Web Control Plane (when `--web` or `BRIDGE_WEB_UI=1` is enabled) |
| `GET` | `/health` | Unauthenticated daemon status and account/model counts; does not prove upstream readiness |
| `GET` | `/readyz` | Readiness probe reporting per-account `models_fresh`, `quota_fresh`, `auth_ok`, and `quarantined` (`?strict=1` returns HTTP `503` when no healthy account is available) |
| `GET` | `/v1/models` | Model IDs, per-model documented token limits, registry-discovered thinking levels, and OpenRouter/OpenAI capability metadata (`ETag` / `If-None-Match` caching; `?fresh=1` forces live discovery) |
| `POST` | `/v1/chat/completions` | Chat messages, tools, images, native image/video generation, and JSON/SSE output (includes `X-GSB-*` diagnostic headers) |
| `POST` | `/v1/responses` | Responses input, instructions, tools, native image/video generation, and JSON/SSE output (includes `X-GSB-*` diagnostic headers) |
| `POST` | `/v1/images/generations` | OpenAI-compatible image generation (`url` or `b64_json`) backed by Gemini Web |
| `POST` | `/v1/videos/generations` | Video generation (Veo MP4 + audio + thumbnail URL) backed by Gemini Web |
| `GET` | `/v1/media/<id>` | Unauthenticated local media proxy serving cached/streamed generated images (`.jpg`) and videos (`.mp4`) |
| `POST` | `/mcp`, `/v1/mcp` | Built-in MCP server (Streamable HTTP) exposing Gemini Web's Google Search grounding as the `web_search` tool |
| `GET` | `/v1/accounts` | Account IDs, email addresses, computed `status` and `statusReason`, `lastError`/`lastFailAt`, provenance, timestamps, quarantine/disabled state, models, cached 5h + weekly quotas, `quotaWindow` metadata, and load telemetry |
| `GET` | `/v1/quota` | Cached or live (`?fresh=1`) upstream 5h + weekly quota buckets for scoped accounts |
| `GET` | `/v1/sessions` | Transport session status and continuation cache hit/miss counters |
| `POST` | `/v1/refresh` | Reloads credential files and refreshes the model registry across the daemon |
| `GET/POST/PUT/DELETE` | `/v1/admin/*` | Web Control Plane REST endpoints for runtime config and account lifecycle management |

`/v1/refresh` rejects requests carrying an `Origin` header. Model names and quota information come from Gemini Web and can change independently of this repository.

## Authentication and security

The daemon listens on loopback by default. **`BRIDGE_API_KEY` defaults to `public`, and unauthenticated access is allowed in that mode.**

Three kinds of key exist, and they do different things:

| | **Pool key** | **Group key** | **Account key** |
|---|---|---|---|
| Value | `BRIDGE_API_KEY` (default `public`) | A `BRIDGE_GROUP_TOKENS` entry (`token:group`) | `gsb-<name>` and `<name>` |
| Scope | All loaded accounts | The accounts assigned to one group via `BRIDGE_ACCOUNT_GROUP` | Exactly one account |
| Account selection | Automatic, per `BRIDGE_ACCOUNT_STRATEGY` | Automatic, inside the group only | None; the named account is always used |
| Failover on quota, ban, or failure | Yes | Yes, but only among the group's accounts | No; the error is returned to the caller |
| `/v1/accounts` and `/v1/quota` | All accounts | Only the group's accounts | Only the named account |
| Best for | Normal use, agents, routers | Multi-user deployments where each user has their own accounts | Testing one account, per-project quota isolation |

Group keys are the right shape for a shared daemon. Assign each user's accounts to a group (`BRIDGE_ACCOUNT_GROUP="acct-a:team-a,acct-b:team-a"`) and map a token to that group (`BRIDGE_GROUP_TOKENS="alice-key:team-a"`). Alice's traffic then stays on Alice's accounts, fails over only inside them, and never spends another user's quota. A group key cannot reach accounts outside its group, and `/v1/accounts` and `/v1/quota` show it only that group's accounts.

Use the pool key unless you specifically need to pin an account or isolate a user. Pool routing is described under `BRIDGE_ACCOUNT_STRATEGY` below.

- `BRIDGE_API_KEY=public` means the pool key is `public` and requests without a key are accepted. Set `BRIDGE_API_KEY` to a random value before exposing the daemon beyond loopback; the account keys then keep working alongside it.
- Account keys are registered for every loaded account file. They are predictable from the filename, so treat them as routing aliases, not secrets. Setting a pool key does not remove them.
- `BRIDGE_ACCOUNT_TOKENS` adds `token:account_id` mappings for custom account keys. It does not disable the automatically registered aliases.
- `BRIDGE_GROUP_TOKENS` adds `token:group_name` mappings for group keys. A group key serves only the accounts of that group and fails over inside it, so several users can share one daemon without touching each other's accounts or quota.
- `Authorization: Bearer <key>` is recommended. `x-api-key`, `x-goog-api-key`, and `?key=` are also accepted; query keys can leak into URL logs.
- The daemon serves plain HTTP, not HTTPS. For access outside the local machine, use a private network/tunnel or an authenticated TLS reverse proxy that restricts access independently of the daemon's routing keys. Do not publish the port directly to an untrusted network.
- Startup warns about a non-loopback bind with default authentication; it does **not** refuse to start. `/health` remains unauthenticated even when a pool key is set.

Account files contain Google session credentials and are written with mode `0600` on POSIX systems. The session cache also contains sensitive continuation metadata. Protect the whole data directory with OS permissions, exclude it from version control and shared backups, and do not paste cookies or account responses into issues. On Windows, use appropriate filesystem ACLs rather than relying on POSIX mode bits.

## Configuration

Set environment variables before starting `./gsb` (or configure runtime settings in `/ui`, persisted to `$BRIDGE_HOME/config.json`). Both `HOST`/`PORT` and `BRIDGE_HOST`/`BRIDGE_PORT` are supported.

### Listener, credentials, and transport

| Variable | Default | Meaning |
|---|---|---|
| `HOST` / `BRIDGE_HOST` | `127.0.0.1` | Listen address |
| `PORT` / `BRIDGE_PORT` | `20133` | Listen port |
| `BRIDGE_HOME` | `~/.gemini-stealth-bridge` | Root for account files, `config.json`, and continuation cache |
| `BRIDGE_WEB_UI` | `false` (`1` in `start.sh`/Docker) | Enable the Web Control Plane at `/ui` and `/v1/admin/*` (also enabled by `--web`) |
| `BRIDGE_ADMIN_KEY` | Empty (falls back to `BRIDGE_API_KEY`) | Dedicated bearer key for `/v1/admin/*` and non-loopback `/ui` access |
| `BRIDGE_API_KEY` | `public` | Pool key: routes across all accounts. Default `public` permits unauthenticated access |
| `CHROME_COOKIES` | Empty | Browser cookie database/profile hint for import/login; also enables a live-cookie fallback when no saved accounts load |
| `CHROME_PASS` | Empty | Browser cookie-encryption password override, not your Google sign-in password; treat as a secret |
| `BRIDGE_PROXY_SERVER` | Empty | Outbound HTTP or SOCKS5 proxy URL for Gemini transport, used by every account that has no proxy of its own |
| `BRIDGE_EGRESS_PER_ACCOUNT` | Empty | `account_id:proxy_url` pairs. Each account leaves through its own proxy instead of `BRIDGE_PROXY_SERVER` |
| `BRIDGE_GROUP_TOKENS` | Empty | `token:group_name` pairs. A group key serves only the accounts of that group, with failover inside the group |
| `BRIDGE_INCLUDE_GROUNDING` | `true` | Append the Google Search citations Gemini Web returned to the end of an answer |
| `BRIDGE_IMPERSONATE` | `chrome_150_psk` | uTLS profile; alternatives include `chrome_146`, `chrome_133`, `firefox_135`, and `safari_18_0` |
| `BRIDGE_IPV6` | `auto` | IPv6 transport mode: `auto`, `off` (disables IPv6; also set by `BRIDGE_DISABLE_IPV6=1`), or `on` |
| `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS` | Disabled | Exactly `1` permits private/loopback image destinations; weakens SSRF protection, so leave unset for untrusted requests |

### Generation and history

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_DEFAULT_THINKING` | `low` | Effort applied when a request sends no `reasoning_effort`: `low`, `medium`, or `high`. Also advertised as `thinking.default` in `/v1/models`, so metadata matches behaviour. Models that do not list the configured level keep their own default |
| `BRIDGE_REJECT_DOWNGRADE` | `true` | Reject detected upstream model substitution instead of silently returning another model |
| `BRIDGE_LIVE_THOUGHTS` | `true` | Include separately extracted reasoning in API output |
| `BRIDGE_TEMPORARY` | `false` | Request temporary Gemini conversations; skips continuation-cache reuse |
| `BRIDGE_SESSION_CACHE` | `true` | Persist and reuse conversation continuation metadata |
| `BRIDGE_TOOL_PREAMBLE` | `false` | Preserve assistant text before tool calls as answer text rather than moving it into reasoning |
| `BRIDGE_LIVE_TOOL_CALLS` | `false` | Opt in to incremental tool-call deltas; default buffers and validates the complete call before emitting it, avoiding failures from upstream revisions |
| `BRIDGE_MAX_PROMPT_CHARS` | `850000` | Inline history budget before older turns are offloaded; not a token count |
| `BRIDGE_MAX_OLD_TOOL_CHARS` | `32000` | Compaction threshold for older tool-result messages |
| `BRIDGE_HISTORY_OFFLOAD` | `true` | Upload older conversation turns as text attachments when inline budget is exceeded; `false` forces inline compaction only |
| `BRIDGE_PERSONAL_CONTEXT` | `auto` | Personal context flag (`inner[67]`/`inner[68]`): `auto` (disabled in `bridge` mode, enabled in pure `web` mode), `on`, or `off` |
| `BRIDGE_CONTEXT_WINDOW` | `1048576` | Fallback context window for models Google has not published limits for. Models with a documented limit always use the documented value |
| `BRIDGE_MAX_OUTPUT_TOKENS` | `65536` | Fallback output limit, used on the same terms as `BRIDGE_CONTEXT_WINDOW` |
| `BRIDGE_QUOTA_PREFLIGHT` | `strict` | What to do when the rolling quota window looks spent: `strict` skips the account, `advisory` logs and proceeds, `off` disables the check |
| `BRIDGE_MAX_RPM_PER_ACCOUNT` | `24` | Per-account requests-per-minute limit (`0` disables the limit) |
| `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` | `2` | Per-account concurrent active stream limit (`0` disables the limit) |
| `BRIDGE_ACCOUNT_STRATEGY` | `rotate` | Pool routing for the pool key. `rotate` keeps using one account until it is exhausted, banned, rate-limited, or failing, then advances to the next and wraps around. `lru` spreads load to the least recently used healthy account. `fill-first` drains the highest-priority account first |
| `BRIDGE_ACCOUNT_PRIORITY` | Empty | Comma-separated account IDs setting the `fill-first` priority and `rotate` / `/ui` account order |

All three strategies share the same eligibility rules. An account is skipped when it is quarantined after an upstream restriction, when its cached quota shows the requested model exhausted, when its per-account RPM or concurrency limit is reached, or when it failed within the last 60 seconds. An account that already holds the conversation's continuation cache is preferred, so a multi-turn conversation stays on one Gemini session even when the pool rotates. A failed account is retried after its cooldown instead of being demoted permanently.

`rotate` is the default because it keeps one Gemini session per conversation and leaves the other accounts at full quota as standby. It does not reclaim an earlier account the moment that account recovers; it continues forward from the current position. Use `fill-first` when you want a fixed preferred account to take traffic again as soon as it is healthy.

### Timeouts and background work

Duration values are in milliseconds.

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_FIRST_BYTE_TIMEOUT_MS` | `60000` | Initial upstream response timeout |
| `BRIDGE_EARLY_SSE_GRACE_MS` | `20000` | For streaming requests, flush response headers after this long even if the first Gemini frame has not arrived, so proxies with a 120s response-header timeout do not fail long thinking turns. Headers also flush as soon as the served model id is verified, which is usually much earlier. `0` keeps headers buffered until the first frame |
| `BRIDGE_EARLY_SSE` | `false` | Flush streaming headers immediately instead of waiting for the grace period |
| `BRIDGE_STALL_TIMEOUT_MS` | `90000` | Timeout between upstream generation frames |
| `BRIDGE_STREAM_TIMEOUT_MS` | `600000` | Overall request timeout |
| `BRIDGE_ROTATE_COOKIES_MS` | `540000` | Background cookie rotation interval; accounts idle longer than two intervals rotate at half frequency (clamped to 60s–3600s) |
| `BRIDGE_MODEL_REFRESH_MS` | `1800000` | Model/quota refresh interval |
| `BRIDGE_IDLE_SESSION_MS` | `180000` | Idle transport-session expiry |
| `BRIDGE_MAX_SESSIONS` | `16` | Transport session-pool limit |
| `BRIDGE_MAX_QUEUE` | `4` | Per-session wait-queue bound |
| `BRIDGE_SESSION_WAIT_MS` | `10000` | Wait timeout for an available transport session |
| `BRIDGE_SEARCH_TIMEOUT_MS` | `180000` | Default deadline for one `web_search` MCP call; clamped to 10s–600s. A caller can override it per call with the `timeout_seconds` argument |

For example, use an outbound proxy without changing the local API address:

```sh
BRIDGE_PROXY_SERVER=socks5://127.0.0.1:1080 ./gsb
```

`BRIDGE_PROXY_SERVER` applies to Gemini transport, not the separate remote-image downloader. It does not provide a process-wide proxy or privacy boundary. `WEB_CRED_FILE` is parsed by configuration but is not used by the daemon's credential loader; use `$BRIDGE_HOME/accounts/` for account management.

### Per-account proxies and per-user keys

Each account can leave through its own proxy, so several accounts can sit behind different egress IPs without the daemon restarting:

```sh
BRIDGE_EGRESS_PER_ACCOUNT="acct-a:socks5://127.0.0.1:1081,acct-b:http://user:pass@proxy.example:3128" ./gsb
```

The same is available in the Web Control Plane: the **Add Google Account** dialog has an *Outbound Proxy* field. The value is stored in the account file and applied to that account's transport session only; changing it rebuilds the session so no connection survives from the previous egress.

To give a user their own key over a set of accounts, give those accounts the same **account group** and map a key to that group:

```sh
BRIDGE_ACCOUNT_GROUP="acct-a:team-a,acct-b:team-a,acct-c:team-b" \
BRIDGE_GROUP_TOKENS="alice-key:team-a,bob-key:team-b" ./gsb
```

A group key serves only the accounts of its group, fails over inside that group when one account is exhausted or rate-limited, and never touches another group's accounts or their quota. A group key is the right shape for multi-user deployments; a per-account key (`gsb-<name>`) pins exactly one account.

| Key | Scope | Failover |
|---|---|---|
| `BRIDGE_API_KEY` (pool key) | Every loaded account | Yes, across the whole pool |
| `BRIDGE_GROUP_TOKENS` entry | The accounts of one group | Yes, inside the group |
| `gsb-<account>` / `<account>` | Exactly one account | No |

### Web search and the built-in MCP server

When Gemini Web answers with Google Search grounding, `gsb` extracts the citations and appends them to the answer:

```
<answer text>

---
Sources:
1. Article title — https://example.com/article
2. Another page — https://example.com/other
```

Set `BRIDGE_INCLUDE_GROUNDING=0` to return the answer without the citation list. The MCP `web_search` result uses the same `Sources:` block and additionally includes a snippet line for each source when Gemini provided one.

A grounded answer is a full model turn, not a search-API lookup: Gemini has to run the query, read the results, and write the synthesis. Measured searches on a live pool took **14–110 seconds**, so give the client a generous timeout rather than the few seconds a conventional search API needs. Cutting a search short throws away the quota it already spent.

The MCP `web_search` call has its own deadline, defaulting to `180000` ms (`180s`). Set `BRIDGE_SEARCH_TIMEOUT_MS` to change that default for the deployment; it is clamped to 10s–600s. A caller can override it per call with the `timeout_seconds` argument (10–600, seconds), which is useful for clients that abort tool calls on their own shorter schedule.

The same grounding is exposed as an MCP tool, so a coding agent can search the web through the account pool (with its quota accounting and failover):

```sh
curl -sS --max-time 240 http://127.0.0.1:20133/mcp \
  -H 'Authorization: Bearer gsb-pool' \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"latest Go release notes","timeout_seconds":180}}}'
```

The `web_search` tool takes a required `query`, an optional `model` override (`gemini-flash`, `gemini-flash-lite`, `gemini-pro`, `gemini-argon`), and the optional `timeout_seconds` deadline described above.

Configure it in a client as an MCP server over Streamable HTTP:

```json
{
  "mcpServers": {
    "gemini-search": {
      "type": "http",
      "url": "http://127.0.0.1:20133/mcp",
      "headers": { "Authorization": "Bearer gsb-pool" }
    }
  }
}
```

The endpoint is `POST`-only (no server-initiated stream), accepts the pool key, a group key, or an account key, and returns the synthesized answer followed by the source list. `/v1/mcp` is an alias for clients that expect the versioned path.

## Operations and troubleshooting

### Inspect and refresh

```sh
./gsb --help
./gsb --probe
./gsb --rotate
curl -sS http://127.0.0.1:20133/health
curl -sS http://127.0.0.1:20133/v1/quota \
  -H 'Authorization: Bearer gsb-acc1'
curl -sS -X POST http://127.0.0.1:20133/v1/refresh \
  -H 'Authorization: Bearer gsb-acc1'
```

`--accounts` prints one row per loaded account with a computed status, so you can see at a glance which accounts are usable without calling `/v1/accounts` and interpreting raw fields:

```sh
./gsb --accounts              # read the saved account files
./gsb --accounts --accounts-live   # query upstream for fresh models and quota first
```

| Status | Meaning |
|---|---|
| `ok` | Loaded, authenticated, no known limit |
| `exhausted` | A quota window is fully spent, or a model stayed exhausted across retries |
| `expired` | The saved session is no longer valid; run `gsb --login <name>` |
| `quarantined` | Upstream restricted the account |
| `cooling` | A recent failure put the account in a short cooldown |
| `degraded` | Usable, but has recorded failures |
| `unknown` | No quota check has run in this process; use `--accounts-live` or read the running daemon |

`--probe` bootstraps configured accounts and prints model/quota details; inspect per-account errors, not just the command's exit status. `--rotate` attempts cookie renewal, but cannot restore a revoked Google session. Re-import credentials when sign-in is required.

Run `./gsb` directly for predictable environment handling. The optional Bash helper `./start.sh` builds a missing or outdated binary when a Go toolchain is available (or runs the existing binary if not) and starts the daemon.

### Common failures

| Symptom | What to check |
|---|---|
| `503 auth_unavailable` / no accounts configured | Connect an account with `gsb --login <name>`; ensure the daemon and the import use the same `BRIDGE_HOME`; reload with `/v1/refresh` or restart |
| `401 authentication_error` | Confirm the key: pool key is `BRIDGE_API_KEY`, account keys are `gsb-<name>` or `<name>`. Check the account filename and whether that account is loaded |
| Cookie import/decryption failure | Run `gsb --login <name>` instead; cookie import only works when that browser profile is already signed in. If decryption itself fails, unlock the keyring or use a cookie export |
| Bootstrap/sign-in failure | Open Gemini Web with the same account, complete any Google sign-in checks, then re-import |
| `401 upstream_auth_error` / `code 1016` on a previously working account | The saved session expired. Run `gsb --login <name>`, complete sign-in, then `gsb --probe`. Re-exporting the same browser cookies will not help |
| `404 model_not_found` | Query `/v1/models` with the same key; do not assume another account's model is available |
| `429` quota/rate-limit error | Inspect `/v1/quota`; wait for reset or choose an account/model with available quota |
| `429 model_substitution` | Gemini served a different model and downgrade rejection is enabled; retry after checking quota or explicitly select an available model |
| Tool-call validation error | Check the tool schema and `tool_choice`; the generated call must match the requested contract |
| Stream appears stalled or fails | Check daemon logs, upstream connectivity/proxy, client buffering, and client/reverse-proxy timeouts |
| Reasoning appears in normal chat output | Check the client's reasoning-field support; model-authored inline reasoning may not be extracted |

For a pool-key request, a `429` may say that no pool account can serve the requested model and summarize unavailable accounts by reason (`exhausted`, `cooling`, `rate_limited`, or `busy`). The message intentionally reports counts rather than emails or credentials; use `/v1/accounts?live=1` for per-account detail. An account-specific quota message is preserved for account-key requests.

Quota-related errors may include `Retry-After` and `X-Resets-At` when usable reset information is available. When an error is caused by a silent model downgrade rather than an upstream quota timer, `gsb` uses a short retry cooldown and includes `X-GSB-Retry-Reason` so clients can tell the difference. SSE requests can emit an error after HTTP headers have been sent; clients must inspect stream events, not only the HTTP status.

## How it works

```mermaid
flowchart LR
    C[OpenAI-compatible client] -->|Chat Completions or Responses| B[gsb API bridge]
    B -->|Authenticated web requests| G[Gemini Web]
    G -->|Generation frames| B
    B -->|JSON or SSE| C
```

The request path has four parts:

1. Resolve the caller's account scope and requested model, then check tool contracts and known quota state.
2. Normalize conversation history, images, thinking effort, and tools. Reuse cached Gemini continuation metadata where possible; upload older history as text attachments when necessary.
3. Send requests using a per-account uTLS/HTTP2 transport. The default `Chrome_150_PSK` profile and ordered headers replace a headless browser process; they do not emulate every property of a real browser.
4. Parse Gemini's generation frames, separate available reasoning and tool calls, validate the result, and return the selected OpenAI-compatible response format.

Background workers refresh model/quota information, rotate cookies, and retire idle sessions. Updated cookies are persisted to account files, and continuation metadata is stored in `$BRIDGE_HOME/session-cache.json`.

## Development

| Path | Responsibility |
|---|---|
| [`cmd/gsb`](./cmd/gsb) | CLI, imports/login, daemon startup and shutdown |
| [`internal/server`](./internal/server) | API routing, authentication, account selection, response formatting, continuation cache |
| [`internal/gemini`](./internal/gemini) | Gemini Web authentication, model/quota discovery, uploads, generation and frame parsing |
| [`internal/prompt`](./internal/prompt) | History normalization, text offloading, image handling and usage estimation |
| [`internal/toolbridge`](./internal/toolbridge) | Prompt-based function-call contract, JSON Schema validation, call parsing and streaming |
| [`internal/cookies`](./internal/cookies) | Cookie import/decryption, persistence, authentication headers and rotation |
| [`internal/transport`](./internal/transport) | uTLS/HTTP2 client, account sessions, queues and timeouts |
| [`internal/config`](./internal/config) | Environment configuration and browser header profiles |
| [`internal/webui`](./internal/webui) | Embedded single-file Web Control Plane (`/ui`) with compile-time `-tags nowebui` toggle |

[`AGENTS.md`](./AGENTS.md) records the setup rules for AI coding agents. Its main point: connect an account with `gsb --login`, and treat cookie import as a fallback for browser profiles that are already signed in.

Build and run the existing checks from the repository root:

```sh
go build -o gsb ./cmd/gsb
go vet ./...
go test -race ./...
```

When reporting a bug, include the endpoint, model ID, HTTP/error code, OS, Go version, and redacted daemon logs. Remove cookies, API keys, email addresses, prompts, and tool outputs containing private data.

## License

[MIT](./LICENSE).
