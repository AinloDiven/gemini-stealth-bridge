# Parity features: sessions, cloud Skills, model default, thinking stream

This page documents the additive features introduced for the eksie-bot parity
round. They are designed around the existing gsb concepts: one account pool, one
API key, browserless transport, and small wire additions that old clients can
ignore.

## 1. Explicit session binding (`X-GSB-Session`)

`/v1/chat/completions` and `/v1/responses` accept an optional request header:

```http
X-GSB-Session: telegram-chat-42
```

Rules:

- Header charset is `[A-Za-z0-9._:-]`, length 1–128. Anything else returns
  `400 invalid_request_error` before generation.
- The first successful turn saves the upstream continuation metadata
  (`cid`, `rid`, `rcid`, `context_token`) under a key derived from the API-key
  scope plus the session id. Later turns with the same header continue that
  Gemini conversation even when the client's message history is shortened.
- The binding stores the account ID, model ID, message count and the hash of
  the saved history. Reuse therefore requires the same account and model, and
  the incoming history must still match the saved prefix. A mismatch falls back
  to the normal content-hash continuation or a full prompt, then overwrites the
  binding after a successful turn.
- The bound account is preferred when the pool has it healthy, not busy, and
  able to serve the model. Failover still applies if it is unavailable.
- Upstream rejection of a continuation deletes both the content hash entry and
  the explicit binding before the retry, so the retry sends a full prompt.
- Without the header, gsb keeps the previous behaviour byte-for-byte.

Storage and limits:

| Setting | Default | Meaning |
|---|---|---|
| `BRIDGE_SESSION_BINDING_TTL_MS` | `86400000` (24 h) | Binding lifetime |
| `BRIDGE_SESSION_BINDING_MAX` | `1024` | Maximum live bindings |

Bindings persist in `$BRIDGE_HOME/session-bindings.json` (mode `0600`, atomic
replace), separately from the 30-minute `session-cache.json`.

## 2. Fixed cloud Skill invocation

Configure a Gemini cloud Skill once instead of sending it on every request:

| Setting | Scope | Example |
|---|---|---|
| `BRIDGE_SKILL` | Global default | `BRIDGE_SKILL=/伊珂丝` |
| `BRIDGE_SKILL_PER_KEY` | API key override | `BRIDGE_SKILL_PER_KEY="bot-key:/伊珂丝"` |
| `"skill"` in `accounts/<id>.json` | Account override | `"skill": "/伊珂丝"` |

Precedence: per-key > account file > global. An empty value disables the skill.

When a skill is resolved for a turn:

1. gsb calls the existing `cCSu7` (`ListManagedSkills`) RPC with `[]` and reads
   the server-issued runtime handle. The listing is fetched per turn, matching
   the web client; one extra retry is made when the list is empty.
2. The handle is written to StreamGenerate `inner[81]` as `[handle]`.
3. The same slash command is prepended to the byte 0 of the prompt that is sent
   for this turn (both a full prompt and a continuation delta), idempotently and
   without changing any other character.

Matching accepts the Skill name, its runtime handle, and the aliases
`伊珂丝` / `eksie` / `eks` / `ex` / `エクス` (case-insensitive, leading `/`
optional). Binding failures are logged and the turn runs without slot 81 rather
than failing; the upstream account-level `45808580` experiment can still keep a
valid handle from mounting, which is controlled by Google.

## 3. Persisted default model switch

| Setting | Default | Meaning |
|---|---|---|
| `BRIDGE_DEFAULT_MODEL` | empty | Model used when the request omits `model` |

The running daemon can change it without a restart through the existing admin
config endpoint:

```http
PATCH /v1/admin/config
{"defaultModel": "gemini-flash-lite", "persist": true}
```

`persist: true` writes `BRIDGE_DEFAULT_MODEL` to `$BRIDGE_HOME/config.env`.
An explicit request `model` still wins. If the configured default is not
advertised by any usable account, gsb logs the condition and falls back to the
selected account's first model; an explicit request model keeps the strict
`model_not_found` path. An empty configured value keeps the old behaviour (first
model advertised by the selected account). The `/v1/admin/config` payload
exposes `defaultModel` and its source.

## 4. Thinking stream extensions

The OpenAI-compatible `choices[].delta.reasoning_content` channel is unchanged.
Two additive frames ride beside it; both use
`"choices":[]` and `"x_gemini_protocol_version":3`, so existing clients ignore
them. Frames are emitted only after the SSE response has started.

### 4.1 Reasoning lifecycle (`x_gemini_reasoning`)

```jsonc
{"object":"chat.completion.chunk","choices":[],"x_gemini_protocol_version":3,
 "x_gemini_reasoning":{"id":"rs_0","index":0,"status":"in_progress","event":"start"}}
{"object":"chat.completion.chunk","choices":[],"x_gemini_protocol_version":3,
 "x_gemini_reasoning":{"id":"rs_0","index":0,"status":"in_progress","event":"delta","delta":"<chunk>"}}
{"object":"chat.completion.chunk","choices":[],"x_gemini_protocol_version":3,
 "x_gemini_reasoning":{"id":"rs_0","index":0,"status":"completed","event":"done",
                       "text":"<full reasoning>","delta_count":N}}
```

`start` is emitted once per thinking phase, `done` once when content/tool output
begins or at stream end. This frame carries the reasoning **detail only**.

### 4.2 Action/title stream (`x_gemini_action_status`)

```jsonc
{"object":"chat.completion.chunk","choices":[],"x_gemini_protocol_version":3,
 "x_gemini_action_status":{
   "operation_id":"chatcmpl-...:generation","sequence":1,
   "type":"generation","source_type":"thinking_progress",
   "state":"active","phase":"thinking",
   "text":"Analyzing Video Content","progress_detail":"Analyzing Video Content",
   "terminal":false,"lease_seconds":300,"turn_admitted":false,
   "artifacts_expected":0,"artifacts_ready":0,"artifacts_failed":0}}
```

The title comes from Gemini Web's top-level action payload
`part_json[2]["7"]` (`[5]` for branch-1 thinking micro-step titles); it is
passed through unchanged. Those status frames can arrive without any candidate
frames, so the parser exposes the action on the line result rather than on a
candidate.

Tool cards add the icon inputs the bot already understands:

```jsonc
{"x_gemini_action_status":{
  "source_type":"tool_progress","phase":"thinking","provider":"skills",
  "progress_detail":"<tool title>",
  "status_template":"{tool_logo_placeholder} Skills: 伊珂丝"}}
```

`provider` is the wire tool id and `status_template` is field `[1][1][5]` only
when it still contains `{tool_logo_placeholder}`. The bot resolves that pair to
its own icon; no icon URL or invented id is sent. Upstream's branch-1 thinking
payload has no icon field at all, so a thinking title is text-only. A tool state
of `complete`/`failed` is projected to the matching terminal phase so the
lifecycle remains schema-consistent.

If the `cCSu7` handle cannot be bound, the turn still runs and still keeps the
configured `/skill` text projection at byte 0; only slot 81 is left empty.

`x_gemini_action_status` frames are deduplicated by their payload (excluding
`sequence`) and `sequence` increases monotonically per completion.

## 5. Tests

- `internal/gemini/action_test.go`, `skill_test.go`
- `internal/server/thinking_stream_test.go`, `skill_test.go`,
  `session_binding_test.go`, and the updated `TestAdminConfigPatchAndEnvFilePersistence`
