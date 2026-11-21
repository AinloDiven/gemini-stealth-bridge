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
1. Calls `cCSu7` (`ListManagedSkills`) to resolve the runtime handle (falls back to same-`fSid` cached handle on transient empty list or RPC error).
2. Sets `StreamGenerate` `inner[81]` to `[handle]` when bound (`nil` otherwise).
3. Prepends `@skill` idempotently at byte 0 of the outbound prompt. Binding RPC errors log and proceed without slot 81.

## 3. Streaming & JSON Extensions (`x_gemini_*`)

Emitted as `choices: []` SSE chunks (with `x_gemini_protocol_version: 3`) on `/v1/chat/completions` and matching `response.x_gemini_*` SSE events + non-streaming top-level fields on `/v1/responses`, alongside standard `reasoning_content` / `reasoning` output:

- **`x_gemini_action_status`**: Pure upstream pass-through of action cards (`part_json[2]["7"]` / `part_json[11]["1"]`: `source_type`, `phase`, optional `progress_detail`, `provider`, and `{tool_logo_placeholder}` `status_template`), buffering the latest pre-commit status until stream headers commit.
- **`x_gemini_media`**: Emits authoritative generated images and videos (`type`, local `/v1/media/...` `url`, optional wire `title`, `alt`, `width`, `height`, `is_gif`, `thumbnail_url`, `mime_type`). Set request header `X-GSB-Media-Mode: structured` (default `markdown`) on `/v1/chat/completions` or `/v1/responses` to omit fallback Markdown links from assistant text while still delivering structured media.
- **`x_gemini_citations`**: Emits deduplicated Google Search grounding sources and inline `candidate[12]` field-44 attachment/file citations (`id`, `title`, optional `url`, `snippet`, `mime_type`) before content deltas when available and on completion, plus top-level `x_gemini_citations` on non-streaming `/v1/chat/completions` and `/v1/responses` JSON responses.

## 4. Multimodal Input Attachments

`ExtractImageParts` and `/v1/responses` normalize inline base64, `data:` URLs, and remote `http(s)` URLs for images, PDFs/documents (`file`/`input_file`/`document`), audio (`input_audio`/`audio`), and video (`video_url`/`input_video`/`video`), advertising `capabilities.vision` / multimodal modalities in `/v1/models`.
