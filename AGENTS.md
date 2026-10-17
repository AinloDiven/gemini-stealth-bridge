# Agent instructions

Guidance for AI coding agents that help a user set up `gemini-stealth-bridge` (`gsb`).

## Connecting a Gemini account

Use the sign-in flow. Do not start with cookie import.

```sh
gsb --login <name>
gsb --probe
```

`--login` opens Gemini Web in a browser and waits for the terminal. The user must complete Google sign-in themselves — including any 2FA or "verify it's you" prompt — and send one message in Gemini before pressing Enter. An agent cannot complete this step on the user's behalf, so instruct the user to run it interactively and wait for them.

`--probe` then confirms the captured session can reach Gemini and lists the models and quota for that account. Treat a probe without an auth error as the setup success signal, not the mere existence of `accounts/<name>.json`.

## When cookie import is acceptable

Only when the browser profile is **already signed in** to Gemini:

```sh
gsb --import-chrome <name> --chrome-profile "<Profile Name>"
```

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

Repeat `gsb --login` with a different name per account. Pool routing is controlled by `BRIDGE_ACCOUNT_STRATEGY` (`rotate` by default, plus `lru` and `fill-first`); see the README configuration tables.
