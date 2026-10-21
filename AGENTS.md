# Agent instructions

Guidance for AI coding agents that help a user set up `gemini-stealth-bridge` (`gsb`).

## Connecting a Gemini account

Use the sign-in flow (`--login` or the Web Control Plane `/ui`). Do not start by copying cookies from a daily browser profile that is actively open.

```sh
gsb --login <name>
gsb --probe
```

- **Desktop (`--login` or Web UI "Launch Isolated Browser")**: Launches a temporary, isolated Chrome/Chromium window with a disposable `--user-data-dir`, waits for the user to complete Google sign-in (including 2FA) on `https://gemini.google.com/app`, captures the session cookies via CDP, and immediately closes and deletes the temporary profile. Because the browser profile is destroyed right after capture, no background browser process ever competes with `gsb` for `__Secure-1PSIDTS` cookie rotation.
- **Remote VPS / Headless (`http://127.0.0.1:20133/ui` over SSH tunnel `ssh -L 20133:127.0.0.1:20133`)**:
  - **Option 1 (Works everywhere)**: Open a **New Incognito Window** (`Ctrl+Shift+N`), sign in to `gemini.google.com/app`, export cookies with Cookie-Editor (or copy the `Cookie` header), **immediately close the Incognito window without signing out**, and paste into the Web UI (`+ Add Account`) or `gsb --login <name>`.
  - **Option 2 (One-click desktop popup for remote VPS)**: Run `gsb --login-helper` on the local desktop (`127.0.0.1:20134`) while connected via SSH port forwarding, then click **Launch Isolated Browser** in the Web UI.

`--probe` confirms the captured session can reach Gemini and lists the discovered models and 5-hour + weekly quota for that account. Treat a probe without an auth error as the setup success signal, not the mere existence of `accounts/<name>.json`.
## When cookie import is acceptable

Only when the browser profile is **already signed in** to Gemini:

```sh
gsb --import-chrome <name> --chrome-profile "<Profile Name>"
```

Arc is a supported Chromium fork: use `--chrome-profile arc` (macOS: `~/Library/Application Support/Arc/User Data`; Windows: the MSIX `TheBrowserCompany.Arc_*` package directory). On macOS the cookie key is read from the login Keychain as `<Browser> Safe Storage` (e.g. `Arc Safe Storage`), so the first import triggers a Keychain prompt; `CHROME_PASS` is the escape hatch.

Cookie export via `--import-cookie` is the fallback when browser decryption is unavailable (for example Chrome App-Bound `v20` encryption). Both paths bootstrap the account before reporting success, but neither can create a session that was never established.

## Never recommend

- Copying cookies from a browser that was never signed in to Gemini. Those cookies expire within minutes and surface later as `401 upstream_auth_error` or upstream `code 1016`.
- Pasting cookies, `Cookie` headers, or account JSON into chat, issues, or logs. They are credentials.
- `--rotate` as a fix for an expired session. It only renews rotating cookies such as `__Secure-1PSIDTS` and cannot restore a revoked or never-established sign-in.
- Committing `$BRIDGE_HOME` (default `~/.gemini-stealth-bridge`) to version control.

## Diagnosing auth failures

| Symptom | Action |
|---|---|
| `401 upstream_auth_error` | Session expired. Run `gsb --login <name>` again. |
| upstream `code 1016` | Same as above; the saved cookies are no longer valid. |
| `403 account_banned` / `TOS_VIOLATION` | The account is restricted upstream. Do not retry in a loop; the account is quarantined. |
| `no SAPISID cookie found` | The selected browser profile is not signed in. Use `gsb --login`. |

## Multiple accounts

Repeat `gsb --login` with a different name per account. Each account is a separate credential file under `$BRIDGE_HOME/accounts/`.

## Three kinds of API key

| Key | Value | Use it for |
|---|---|---|
| **Pool key** | `BRIDGE_API_KEY` (default `public`) | Normal use. `gsb` picks an account per request and fails over automatically |
| **Group key** | `BRIDGE_GROUP_TOKENS` entry (`token:group`) | One user's own set of accounts. Serves only that group and fails over inside it |
| **Account key** | `gsb-<name>` or `<name>` | Pinning one account for testing or isolating a project's quota |

Default to the pool key in examples and client configuration. The pool key is the only key that fails over across every account: an account key stays on that account and returns `429` or `401` instead of switching. A group key fails over, but only among the accounts of its own group. When a user reports that a request failed while another account had quota, check which key the client used before assuming a routing bug.

Do not tell users to create one router connection per account. Account rotation belongs to `gsb`, which tracks quota, quarantine state, and conversation continuation affinity. A router that spreads requests over per-account connections cannot see any of that and will discard the cached Gemini session between turns. One connection with the pool key is the intended setup.

For a multi-user deployment, give each user a group key instead of the shared pool key: assign their accounts to a group (`BRIDGE_ACCOUNT_GROUP`) and map their key to it (`BRIDGE_GROUP_TOKENS`). Each user's traffic then stays on their own accounts and their own quota.

## Pool routing

`BRIDGE_ACCOUNT_STRATEGY` controls the pool key's selection:

- `rotate` (default): keep using one account until it is exhausted, banned, rate-limited, or failing, then advance to the next and wrap around. Best session continuity and leaves other accounts at full quota.
- `lru`: spread load to the least recently used healthy account.
- `fill-first`: drain a fixed order set by `BRIDGE_ACCOUNT_PRIORITY`.

Every strategy skips quarantined accounts, accounts whose cached quota shows the requested model exhausted, accounts at their RPM/concurrency limit, and accounts that failed in the last 60 seconds. All strategies prefer the account holding the conversation's continuation cache. When failing over to another account, `gsb` reuses the target account's warm session without forcing a fresh `/app` bootstrap. A failed account becomes eligible again after its cooldown; it is not demoted permanently.

`rotate` does not jump back to an earlier account as soon as that account recovers; it continues forward from the current position. If a user expects traffic to return to a preferred account immediately after a quota reset, that is `fill-first`, not `rotate`.

## Checking account status and quota

```sh
gsb --accounts --accounts-live
```

Or open `http://127.0.0.1:20133/ui` (or query `GET /v1/accounts?live=1` on the running daemon). Each account includes a computed `status` (`ok`, `exhausted`, `expired`, `quarantined`, `cooling`, `degraded`, `disabled`, `unknown`) and `statusReason`:

- `ok` (UI: `Ready`): Loaded, authenticated, and ready to serve traffic. In `rotate` strategy, the account holding the sticky rotation cursor displays a `Current Turn` badge.
- `cooling`: Failed within the last 60 seconds; temporarily skipped while the pool routes to another healthy account.
- `degraded`: Cooldown has elapsed (`>60s`) so the account is eligible for traffic again, but its failure counter (`fails > 0`) has not yet been cleared by a successful request or manual reset.
- `exhausted`: Either the rolling 5-hour window (`2,400` credits) or the 7-day weekly window (`48,384` credits) is spent until `resetAt`, or repeated upstream model substitutions triggered synthetic backoff (`1m -> 5m -> 15m -> 1h -> 6h -> 24h`).
- `disabled` (UI: `Paused`): Operator paused the account via the Web Control Plane (`⋯ -> Pause Account in Pool`).

`gsb` queries both `batchexecute` (`qpEbW`) and Gemini Web's GraphQL `GetUserQuotaService` (`GXU_FIVE_HOURLY` and `GXU_WEEKLY`), exposing both the **5-hour window** and the **7-day weekly window** (`quotaWindow.weeklyExposed: true`). Because the 5-hour window is nested under the weekly allowance, `gsb` automatically caps effective 5-hour remaining credits to the weekly remaining credits when the weekly pool runs low. When an account hits `0` credits, non-lite models (`gemini-flash`, `gemini-pro`, `gemini-argon`) are blocked or downgraded upstream until `resetAt`, while `gemini-flash-lite` remains usable.

## Native image and video generation

`gsb` supports Gemini Web's native image generation (Imagen / Nano Banana) and video generation (Veo MP4 with audio) in three ways:

1. **Coding Agent Mode (`tools` enabled in `/v1/chat/completions` or `/v1/responses`)**: When the user prompt asks for an image, diagram, illustration, or video, `gsb` injects a server-side `generate_media` tool (`media_type`: `"image"` | `"video"`, `prompt`, plus optional `aspect_ratio`: `"1:1"` | `"16:9"` | `"9:16"` | `"4:3"` | `"3:4"`, `format`: `"png"` | `"jpeg"` | `"gif"`, `duration`: `10` | `15` | `30`, and `output_path`). `gsb` downloads original unscaled images (`=s0-d`) or lossless PNGs (`=s0-rp`), proxies the authenticated media at `http://<host>:<port>/v1/media/<id>.png|.jpg|.mp4`, and returns `url`, `revised_prompt`, `markdown`, and `local_download_cmd` (`curl`) so the coding agent saves the file directly into the user's workspace.
2. **Direct Chat / Responses (no tools)**: Asking Gemini to generate an image or video streams the response and rewrites upstream `lh3.googleusercontent.com` / `contribution.usercontent.google.com` links into local `/v1/media/<id>` URLs that open or download without requiring Google cookies.
3. **Dedicated OpenAI Endpoints**: `POST /v1/images/generations` (`response_format`: `url` or `b64_json`, `aspect_ratio`/`size`, `format`/`output_format`, returns `revised_prompt`) and `POST /v1/videos/generations` (`aspect_ratio`, `duration`, returns MP4 `url` and `thumbnail_url`).

## Web search, grounding, and the MCP server

When Gemini Web answers with Google Search grounding, `gsb` extracts the citations and appends them to the end of the answer under a `Sources:` heading. Set `BRIDGE_INCLUDE_GROUNDING=0` to return the answer without them.

The same grounding is exposed as an MCP tool at `POST /mcp` (alias `/v1/mcp`), so a coding agent can search the web through the account pool:

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

The tool is `web_search` with a required `query` argument and an optional `model` override. The endpoint speaks MCP Streamable HTTP with JSON-RPC 2.0: it is `POST`-only (no server-initiated stream), notifications get a `202` with an empty body, and the pool, group, and account keys are all accepted. A search runs as a temporary chat, so it does not add threads to the user's Gemini conversation list, and it consumes the same account quota as a normal request.

A grounded answer is a full model turn and takes as long as the model thinks (measured between 14 and 110 seconds). When reporting a slow or "hung" search, check whether a request is still in flight before assuming the endpoint is broken.

## Per-account proxies

Each account can egress through its own HTTP or SOCKS5 proxy, set per account file (`proxyUrl` in the Web UI's *Add Google Account* dialog) or via `BRIDGE_EGRESS_PER_ACCOUNT="acct-a:socks5://127.0.0.1:1081"`. An account without its own proxy uses `BRIDGE_PROXY_SERVER`. Changing an account's proxy rebuilds its transport session so no connection survives from the previous egress.
