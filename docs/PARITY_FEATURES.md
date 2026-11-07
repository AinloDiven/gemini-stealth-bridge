# Fork Extensions (`gsb` Parity)

Additive extensions built on upstream `gsb` without altering standard OpenAI/Responses behavior or upstream interfaces.

## 1. Explicit Session Binding (`X-GSB-Session`)

`/v1/chat/completions` and `/v1/responses` accept an optional header:

```http
X-GSB-Session: telegram-chat-42
```

- Charset: `[A-Za-z0-9._:-]`, length 1–128 (`400 invalid_request_error` otherwise).
- Keyed by `(auth_scope, session_id)` (pool-key requests normalize to `"pool"` across pool size changes).
- Persists `(account_id, model, cid/rid/rcid/context_token, next_len, history_hash)` in `$BRIDGE_HOME/session-bindings.json` (`0600`, atomic replace; flushed on `FlushSessionCacheNow()` and purged when an account is deleted).
- Prefers the bound account ahead of rotation without mutating `rotateCursor`; if that account is unavailable, falls back to a clean prompt on another account.
- Model switches reuse the stored continuation after prefix validation; history-trimmed requests send only the latest turn over the bound conversation. `temporary: true` skips reuse.

| Variable | Default | Description |
|---|---|---|
| `BRIDGE_SESSION_BINDING_TTL_MS` | `86400000` (24 h) | Binding TTL |
| `BRIDGE_SESSION_BINDING_MAX` | `1024` | Max live bindings (LRU eviction) |

## 2. Cloud Skill Binding (`inner[81]` + Byte-0 `@skill`)

| Source | Scope | Example |
|---|---|---|
| `BRIDGE_SKILL_PER_KEY` | Per API key / pool key | `BRIDGE_SKILL_PER_KEY="team-key:@research"` |
| `"skill"` in `accounts/<id>.json` | Per account | `"skill": "@research"` |
| `BRIDGE_SKILL` | Global fallback | `BRIDGE_SKILL=@research` |

Precedence: per-key > account file > global. Empty disables binding.

Per attempt (re-evaluated on account failover or re-bootstrap):
1. Calls `cCSu7` (`ListManagedSkills`) to resolve the runtime handle (case-insensitive exact name or handle match; falls back to same-`fSid` cached handle on transient empty list).
2. Populates `StreamGenerate` `inner[81]` as `[handle]`.
3. Prepends `@skill` idempotently at byte 0 of the outbound prompt. Binding RPC errors log and proceed without slot 81.

## 3. Runtime Default Model (`BRIDGE_DEFAULT_MODEL`)

Used when a request omits `model`. Editable live via `PATCH /v1/admin/config` (`{"defaultModel": "gemini-flash-lite", "persist": true}`). If no eligible account serves the configured default, `gsb` logs and falls back to the selected account's first model; explicit request models still return `404 model_not_found`.

## 4. Streaming Extensions (`x_gemini_*`)

Emitted as `choices: []` chunks with `x_gemini_protocol_version: 3` alongside unchanged `choices[0].delta.reasoning_content`:

- **`x_gemini_reasoning`**: `start` -> `delta` -> `done` lifecycle (`done` is guaranteed before content, tool calls, stream completion, or mid-stream error frames).
- **`x_gemini_action_status`**: Relays upstream action cards (`part_json[2]["7"]` / `part_json[11]["1"]`) as non-terminal progress updates (`progress_detail`, `source_type`, `phase`, optional wire `provider` and `{tool_logo_placeholder}` `status_template`, monotonic `sequence`).

## 5. Multimodal Input Attachments

`ExtractImageParts` and `/v1/responses` normalize inline base64, `data:` URLs, and remote `http(s)` URLs for images, PDFs/documents (`file`/`input_file`/`document`), audio (`input_audio`/`audio`), and video (`video_url`/`input_video`/`video`), emitting `[Attached image|audio|video|file #N]` placeholders and advertising `text+image+audio+file+video->text` in `/v1/models`.
