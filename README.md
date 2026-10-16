# gemini-stealth-bridge (`gsb`)

Browserless Go daemon that exposes Google Gemini Web sessions (`gemini.google.com`) as an OpenAI-compatible API server (`/v1/chat/completions` and `/v1/responses`). It replaces headless Chrome / CDP bridges with a native `Chrome_150_PSK` uTLS and HTTP/2 client (~20 MB RSS vs ~600-800 MB for headless Chromium).

---

## Technical Overview

| Subsystem | Implementation Details |
|---|---|
| **TLS Fingerprint (JA3/JA4)** | `Chrome_150_PSK` uTLS profile (BoringSSL cipher suites, Post-Quantum `X25519MLKEM768`, ECH GREASE, ALPS, Brotli certificate compression, PSK session tickets, randomized TLS extension order) |
| **HTTP/2 & Header Ordering** | Deterministic Chrome HTTP/2 pseudo-header order (`:method`, `:authority`, `:scheme`, `:path`) and request-kind header templates (`navigate`, `iframe`, `cors`) across direct and HTTP/SOCKS5 proxy connections |
| **OS / TCP Stack Alignment** | Matches `User-Agent` and `Sec-CH-UA-Platform` to the host kernel (`Linux`, `macOS`, `Windows`) so TCP/IP stack parameters (JA4T) match HTTP headers |
| **Per-Account Isolation** | Dedicated per-account `Session` and `StealthClient` pool with bounded wait queues, HTTP/2 stream multiplexing (`RunStream`), and deferred cookie updates |
| **Dual-Path Cookie Rotation** | Renews `__Secure-1PSIDTS` (`POST /RotateCookies [000,"-0000000000000000000"]`) and `SIDCC` (`GET /RotateCookiesPage?og_pid=658` followed by `POST /RotateCookies [658,"<id>"]`); automatically self-heals expired `SNlM0e` tokens during `/app` bootstrap |
| **Atomic Cookie Persistence** | Absorbs `Set-Cookie` headers from every upstream response (`/app`, `RotateCookies`, `batchexecute`, `StreamGenerate`, `upload`) and writes them atomically (`0600`) to `~/.gemini-stealth-bridge/accounts/<id>.json` |
| **Live Model & Quota Enforcement** | Discovers account models via `otAQ7b` (`GET_USER_STATUS`) and queries quota buckets via `qpEbW` (`CHECK_GEMINI_QUOTA`); detects silent model downgrades (`pj[39]` / `pj[42]`) and returns HTTP `429 model_substitution` with `Retry-After` and `X-Resets-At` headers |
| **Thinking Effort Mapping** | Maps Gemini Web's three native thinking levels: `low` (`1`, default), `medium` (`4`), and `high` (`2`) from `reasoning_effort`, `thinking`, `thinking_budget`, or `@think=<level>` model suffixes |
| **1M-Token Context & Tool Calling** | Draft-07/2020-12 JSON Schema validator (`$ref`, `anyOf`/`oneOf`/`allOf`), automatic JSON/HTML escape repair, incremental SSE `tool_calls` streaming, persistent `SessionCache` continuation (`inner[2]` + `inner[71]` contextToken), and automatic offloading of older conversation turns into `<=340 KB` `text/plain` file attachments |
| **OpenAI API Surface** | Serves both `POST /v1/chat/completions` and `POST /v1/responses` with real-time `reasoning_content` / `response.reasoning_summary_text.delta` SSE events, base64 data URLs, and SSRF-guarded remote `https://` image fetching |

---

## Installation

### Build from Source

Requires Go 1.24 or newer:

```sh
git clone https://github.com/azrinaldi/gemini-stealth-bridge.git
cd gemini-stealth-bridge
go build -o gsb ./cmd/gsb
```

Or install directly with `go install`:

```sh
go install github.com/azrinaldi/gemini-stealth-bridge/cmd/gsb@latest
```

---

## Account Setup & CLI Usage

Account credentials are stored as individual JSON files (`0600` permissions) in `~/.gemini-stealth-bridge/accounts/<name>.json`.

### 1. Import Cookies from a Local Browser Profile (No Browser Launch Required)

Reads and decrypts `google.com` cookies directly from a local Chrome, Chromium, Brave, Edge, or Firefox SQLite database:

```sh
./gsb --import-chrome acc1
```

To specify a custom browser profile name, profile directory, or `Cookies` SQLite path:

```sh
# By profile name (Linux, macOS, Windows)
./gsb --import-chrome acc1 --chrome-profile "Profile 4"

# By profile directory path (Linux / macOS)
./gsb --import-chrome acc1 --chrome-profile ~/.config/google-chrome/Default

# By profile directory path (Windows PowerShell / cmd)
./gsb --import-chrome acc1 --chrome-profile "C:\Users\<user>\AppData\Local\Google\Chrome\User Data\Profile 4"
```

### 2. Import from a Cookie Header or Cookie-Editor JSON Export

```sh
# Raw Cookie header string (requires at least SAPISID and __Secure-1PSID)
./gsb --import-cookie acc1 --cookie-value "SAPISID=...; __Secure-1PSID=...; __Secure-1PSIDTS=..."

# Or from a file containing a raw Cookie header or Cookie-Editor JSON array
./gsb --import-cookie acc1 --cookie-file ./cookies.json
```

### 3. Interactive Browser Login

Launches a local browser window, waits for Google authentication on `gemini.google.com/app`, extracts the session cookies, and saves `~/.gemini-stealth-bridge/accounts/<name>.json`:

```sh
./gsb --login acc1

# Target a specific browser profile on multi-profile setups:
./gsb --login acc1 --chrome-profile "Profile 4"
```

### 4. Verify Accounts, Models, and Quota (`--probe`)

```sh
./gsb --probe
```

Example output:

```text
[acc1] email=user@example.com capacity=2 (AI Pro) models=[gemini-flash-lite, gemini-flash, gemini-pro, gemini-argon]
  quota: Gemini Pro remaining=48384/48384 resets=2026-10-16T08:00:00Z
```

### 5. Manual Cookie Rotation (`--rotate`)

```sh
./gsb --rotate
```

---

## Running the Daemon

Start the server using `./start.sh` (or run `./gsb` directly):

```sh
./start.sh
```

By default, `gsb` listens on `http://127.0.0.1:20133`.
Every account file `~/.gemini-stealth-bridge/accounts/<name>.json` is automatically bound to two API keys:
- `gsb-<name>` (for example, `gsb-acc1`)
- `<name>` (for example, `acc1`)

---

## Configuration & Environment Variables

| Variable | Default | Description |
|---|---|---|
| `HOST` / `BRIDGE_HOST` | `127.0.0.1` | Bind address (non-loopback addresses require `BRIDGE_API_KEY` or `BRIDGE_ACCOUNT_TOKENS`) |
| `PORT` / `BRIDGE_PORT` | `20133` | HTTP listen port |
| `BRIDGE_HOME` | `~/.gemini-stealth-bridge` | Data directory for `accounts/*.json` and `session-cache.json` |
| `BRIDGE_API_KEY` | *(empty)* | Optional admin bearer token for `/v1/accounts`, `/v1/quota`, `/v1/sessions`, and `/v1/refresh` |
| `BRIDGE_ACCOUNT_TOKENS` | Auto-populated from `accounts/*.json` | Comma-separated `token:account_id` pairs |
| `BRIDGE_IMPERSONATE` | `chrome_150_psk` | uTLS client profile (`chrome_150_psk`, `chrome_146`, `chrome_133`, `firefox_135`, `safari_18_0`) |
| `BRIDGE_PROXY_SERVER` | *(empty)* | Optional outbound proxy URL (`http://...` or `socks5://...`) |
| `BRIDGE_DEFAULT_THINKING` | `1` (`low`) | Default thinking level (`1` = `low`, `4` = `medium`, `2` = `high`) |
| `BRIDGE_REJECT_DOWNGRADE` | `true` | Reject silent upstream model downgrades with HTTP `429 model_substitution` |
| `BRIDGE_LIVE_THOUGHTS` | `true` | Stream model reasoning in `reasoning_content` (Chat API) and `reasoning_summary_text.delta` (Responses API) |
| `BRIDGE_SESSION_CACHE` | `true` | Cache multi-turn Gemini Web continuation tokens (`cid`, `rid`, `rcid`, `contextToken`) in RAM and disk |
| `BRIDGE_TOOL_PREAMBLE` | `false` | Preserve assistant text preamble before tool call blocks when `true`; move preamble to `reasoning_content` when `false` |
| `BRIDGE_MAX_PROMPT_CHARS` | `850000` | Inline prompt character budget before older turns are offloaded as `text/plain` file attachments |
| `BRIDGE_MAX_OLD_TOOL_CHARS` | `32000` | Per-message character cap for older tool outputs before compaction |
| `BRIDGE_FIRST_BYTE_TIMEOUT_MS` | `60000` | Timeout (ms) waiting for initial HTTP response stream bytes |
| `BRIDGE_STALL_TIMEOUT_MS` | `90000` | Timeout (ms) between active generation frames before triggering failover |
| `BRIDGE_STREAM_TIMEOUT_MS` | `600000` | Maximum total duration (ms) for a single generation request |
| `BRIDGE_ROTATE_COOKIES_MS` | `540000` | Background cookie rotation interval (ms, default 9 minutes) |
| `BRIDGE_MODEL_REFRESH_MS` | `600000` | Background model and quota refresh interval (ms, default 10 minutes) |

---

## HTTP API Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Service status, active uTLS profile, loaded account count, and live model count |
| `GET` | `/v1/models` | OpenAI-compatible model list discovered via `otAQ7b` (`1,048,576` context window, `low`/`medium`/`high` thinking efforts) |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions API (streaming SSE and non-streaming, tool calling, `reasoning_effort`, base64 and remote `https://` images, 1M context offloading) |
| `POST` | `/v1/responses` | OpenAI Responses API (`response.created`, `response.reasoning_summary_text.delta`, `response.output_text.delta`, `response.function_call_arguments.delta`, `response.completed`) |
| `GET` | `/v1/accounts` | Per-account status, tier capacity, available models, and cached quota buckets |
| `GET` | `/v1/quota` | Live `qpEbW` quota buckets per account (`?refresh=1` forces an upstream refresh) |
| `GET` | `/v1/sessions` | Active uTLS/HTTP2 sessions, queue depths, and idle timers |
| `POST` | `/v1/refresh` | Hot-reload `accounts/*.json` from disk and refresh the `otAQ7b` model registry |

---

## Integration with `9router` and Coding Agents

### Option A: Multi-Account Pool via `9router`

#### 1. Register the Provider Node in `9router`

In the `9router` dashboard (`http://127.0.0.1:20128` -> **Providers** -> **Add Compatible Provider**):

| Field | Value |
|---|---|
| **Provider Type** | `OpenAI Compatible` |
| **API Type** | `Chat Completions` (`chat`) or `Responses` (`responses`) |
| **Name** | `Gemini Stealth Bridge` |
| **Prefix** | `gsb` |
| **Base URL** | `http://127.0.0.1:20133/v1` |

Or via `curl`:

```sh
NODE_ID=$(curl -s -X POST http://127.0.0.1:20128/api/provider-nodes \
  -H "Content-Type: application/json" \
  -d '{
    "type": "openai-compatible",
    "apiType": "chat",
    "name": "Gemini Stealth Bridge",
    "prefix": "gsb",
    "baseUrl": "http://127.0.0.1:20133/v1"
  }' | node -pe 'JSON.parse(fs.readFileSync(0,"utf8")).node.id')
echo "Created 9router Node ID: $NODE_ID"
```

#### 2. Register Each Google Account (`gsb-<name>`)

```sh
NODE_ID=$(curl -s http://127.0.0.1:20128/api/provider-nodes \
  | node -pe 'JSON.parse(fs.readFileSync(0,"utf8")).nodes.find(n => n.prefix === "gsb").id')

for f in ~/.gemini-stealth-bridge/accounts/*.json; do
  [ -e "$f" ] || continue
  acc="$(basename "$f" .json)"
  curl -s -X POST http://127.0.0.1:20128/api/providers \
    -H "Content-Type: application/json" \
    -d "{
      \"provider\": \"$NODE_ID\",
      \"name\": \"gsb-$acc\",
      \"apiKey\": \"gsb-$acc\",
      \"priority\": 1,
      \"allowOverwrite\": true
    }"
  echo
done
```

#### 3. Configure Your Coding Agent (`~/.omp/agent/models.yml`)

```yaml
providers:
  9router-local:
    baseUrl: http://127.0.0.1:20128/v1
    apiKey: <your-9router-api-key>
    api: openai-completions
    compat:
      supportsReasoningEffort: true
    models:
      - id: gsb/gemini-argon
        name: gsb/gemini-argon
        reasoning: true
        thinking:
          mode: effort
          efforts: [low, medium, high]
        input: [text, image]
        contextWindow: 1048576
        maxTokens: 65536
        compat:
          supportsReasoningEffort: true
```

### Option B: Direct Connection (Without `9router`)

Connect `omp`, Codex CLI, OpenCode, Cline, or Cursor directly to `http://127.0.0.1:20133/v1`:

| Setting | Value | Notes |
|---|---|---|
| **Base URL** | `http://127.0.0.1:20133/v1` | Supports both `/v1/chat/completions` and `/v1/responses` |
| **API Key** | `gsb-<name>` or `<name>` | Passed via `Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, or `?key=` |
| **Model ID** | `gemini-argon` (or `gemini-pro`, `gemini-flash`, `gemini-flash-lite`) | Supports `gsb/` prefix and `@think=low\|medium\|high` suffix |

---

## Package Architecture

| Package / File | Responsibility |
|---|---|
| `cmd/gsb/main.go` | CLI flags (`--probe`, `--rotate`, `--login`, `--import-chrome`, `--import-cookie`), server bootstrap, and graceful shutdown |
| `internal/config/config.go` | Environment configuration, timeout defaults, and OS-aligned browser header profiles |
| `internal/cookies/jar.go` | Cookie parsing, canonicalization, `SAPISIDHASH`, and atomic `0600` credential file persistence |
| `internal/cookies/rotate.go` | Dual-path Google cookie rotation (`__Secure-1PSIDTS` and `SIDCC` via `RotateCookiesPage`) |
| `internal/cookies/chrome_sqlite.go` | Browserless SQLite cookie extraction and AES-GCM/CBC decryption for Chrome, Brave, Edge, and Firefox |
| `internal/transport/client.go` | `Chrome_150_PSK` uTLS + HTTP/2 client, deterministic header order, `FirstByteTimeout`, and `StallTimeout` |
| `internal/transport/session.go` | Per-account session pool, HTTP/2 stream multiplexing (`RunStream`), and idle session reaping |
| `internal/gemini/models.go` | Model discovery metadata, capacity calculation, and `low`/`medium`/`high` thinking level mapping |
| `internal/gemini/client.go` | Per-account authentication (`SNlM0e`/`cfb2h`/`FdrFJe`), `batchexecute` RPCs (`otAQ7b`, `qpEbW`), and cookie absorption |
| `internal/gemini/stream.go` | `StreamGenerate` 81-slot payload builder, live frame streaming, and multipart/resumable file uploads |
| `internal/gemini/parser.go` | `wrb.fr` frame parser, inline `<think>` / Reasoning block extraction, and model identity detection |
| `internal/prompt/prompt.go` | Multi-turn prompt flattening, 1M-token context attachment offloading, image extraction, and $O(N)$ `HistoryHasher` |
| `internal/toolbridge/contract.go` | Tool contract validation and cached system-prompt / reminder rendering |
| `internal/toolbridge/schema.go` | Draft-07/2020-12 JSON Schema validator (`$ref`, `anyOf`/`oneOf`/`allOf`, types, enums, numeric/array bounds) |
| `internal/toolbridge/parser.go` | Tool call JSON extraction, escape/HTML sanitization repair, bare-call recovery, and `SafeStreamablePrefix` |
| `internal/toolbridge/streamer.go` | Incremental SSE `tool_calls` delta streamer (`LiveToolCallStreamer`) |
| `internal/server/server.go` | HTTP router (`ServeHTTP`), auth scope resolution, background workers, and JSON/SSE error formatting |
| `internal/server/accounts.go` | Credential loading/deduplication, model & quota refresh, preflight quota enforcement, `SessionCache` affinity, and `prepareGeneration` |
| `internal/server/cache.go` | Multi-turn session continuation cache (`SessionCache`) with disk persistence and SHA-256 attachment upload cache |
| `internal/server/chat.go` | `POST /v1/chat/completions` handler, shared attempt completion validator, and reasoning effort normalizer |
| `internal/server/responses.go` | `POST /v1/responses` handler and OpenAI Responses API input/tool normalizer |

---

## Testing

Run the full test suite with the Go race detector enabled:

```sh
go vet ./...
go test -race ./...
```

---

## License

MIT License. See [LICENSE](./LICENSE).
