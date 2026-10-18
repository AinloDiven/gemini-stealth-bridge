# Agent instructions

Guidance for AI coding agents that help a user set up `gemini-stealth-bridge` (`gsb`).

## Connecting a Gemini account

Use the sign-in flow. Do not start with cookie import.

```sh
gsb --login <name>
gsb --probe
```

`--login` opens Gemini Web in a browser and waits for the terminal. The user must complete Google sign-in themselves (including any 2FA or "verify it's you" prompt) and send one message in Gemini before pressing Enter. An agent cannot complete this step on the user's behalf, so instruct the user to run it interactively and wait for them.

`--probe` then confirms the captured session can reach Gemini and lists the models and quota for that account. Treat a probe without an auth error as the setup success signal, not the mere existence of `accounts/<name>.json`.

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

## Two kinds of API key

| Key | Value | Use it for |
|---|---|---|
| **Pool key** | `BRIDGE_API_KEY` (default `public`) | Normal use. `gsb` picks an account per request and fails over automatically |
| **Account key** | `gsb-<name>` or `<name>` | Pinning one account for testing or isolating a project's quota |

Default to the pool key in examples and client configuration. It is the only key that can fail over: a request made with an account key stays on that account and returns `429` or `401` instead of switching. When a user reports that a request failed while another account had quota, check which key the client used before assuming a routing bug.

Do not tell users to create one router connection per account. Account rotation belongs to `gsb`, which tracks quota, quarantine state, and conversation continuation affinity. A router that spreads requests over per-account connections cannot see any of that and will discard the cached Gemini session between turns. One connection with the pool key is the intended setup.

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

Or query `GET /v1/accounts` on the running daemon (`GET /v1/accounts?live=1` forces a fresh upstream check). Each account includes a computed `status` (`ok`, `exhausted`, `expired`, `quarantined`, `cooling`, `degraded`, `unknown`) and `statusReason`.

Cached quota in `/v1/accounts` updates on traffic and every 30-minute refresh cycle, so an idle account can still display stale remaining credits from before the window exhausted. Always verify with `--accounts-live` or `?live=1` when requests return `429` (`model_substitution` or `rate_limit_error`) despite nonzero cached `remaining`. When an account's 5-hour credit pool hits `0/2400`, all non-lite models (`gemini-flash`, `gemini-pro`, `gemini-argon`) are blocked or silently downgraded upstream until `resetAt`, while `gemini-flash-lite` continues to work.

Gemini also enforces two limits at once: a rolling ~5-hour window and a weekly cap. The upstream quota RPC only exposes the rolling ~5-hour window (`quotaWindow.weeklyExposed: false`), so a nonzero live remaining count does not prove the weekly cap has room. When a model repeatedly fails despite remaining 5-hour quota, `gsb` escalates its cooldown (`1m -> 5m -> 15m -> 1h -> 6h -> 24h`) and marks the account `exhausted` (`still exhausted after N retries`).
