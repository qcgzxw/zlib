# Multi-account download pool — design

Status: proposed. Owner decisions are recorded inline; anything marked
"Builder call" is left to whoever plans the implementation.

## Intent

One user, several Z-Library accounts, one command. A download should succeed
as long as *any* saved account still has quota left today, without the user
picking an account per download.

Each account keeps a local record of its remaining quota and the moment that
quota resets. Which account is chosen is governed by a strategy with two
modes: spread the load evenly (balanced) or rotate strictly (round-robin).

Why this shape: Z-Library's daily download limit is enforced per account by
the server. The client never learns a reset timestamp — only a used count and
an allowed count (`internal/zlib/eapi_profile.go:57-80`) — so any quota
numeric the CLI shows is a local estimate that must be refreshable, not
authoritative.

## Goals

- Store N accounts, each with its own cookies and domain.
- Keep per-account quota state: allowed, remaining, when it was observed,
  when it resets.
- Treat the daily reset as a fixed local wall-clock time, configurable,
  default derived from an observed page countdown.
- Two selection strategies: `balanced` and `round_robin`, persisted.
- Rotate inside a single download: an account that reports the daily limit is
  marked exhausted and the same request is retried on the next account.
- Fail with one clear message naming the next reset when every account is
  exhausted.
- Manage accounts without editing JSON by hand.
- Migrate an existing `session.json` install with no user action.

## Non-goals

- Downloading one book from several accounts in parallel.
- Scraping the web page for the authoritative reset instant.
- Per-account Kindle/SMTP settings; the Kindle configuration stays global.
- Syncing account state between machines.
- Changing what `favorites`, `history` or `profile` show based on rotation.

## Anti-goals

- Silently changing the identity behind an identity-sensitive read command.
- Writing credentials with permissions looser than 0600.
- Presenting a cached quota number as if it came from the server this minute.
- Retrying a quota rejection on the same account, or looping forever.

## Constraints found in the code

- One session only: `internal/config/session.go:9-18` stores a single cookie
  map and domain; `internal/cli/root.go:52-70` builds the one client every
  command uses.
- The download URL carries no per-account token:
  `internal/zlib/eapi.go:337` builds `domain + /eapi/book/<id>/<hash>/file`
  from book metadata only. `FetchBook` reads display metadata
  (`internal/zlib/eapi_book.go:23-56`). The cookie-bearing exchange that
  yields the CDN link happens in `resolveEAPIFileURL`
  (`internal/zlib/eapi_book.go:63-115`) and is the point at which an account
  is actually spent. This is what makes handing the same reference to another
  account possible.
- The fallback URL for results without id/hash comes from the `dl` field
  (`internal/zlib/eapi.go:44,339`); whether that form ever embeds a token is
  unverified, so the pool must treat it as account-bound.
- `resolveEAPIFileURL` only exchanges a link when the URL host equals the
  client's domain (`internal/zlib/eapi_book.go:70-80`). Accounts that logged
  in on different domains therefore cannot share a URL verbatim.
- No reset field: `DownloadLimit.DailyReset` (`internal/zlib/model.go:37`) is
  rendered when non-empty (`internal/cli/profile_tui.go:160`) but never
  populated by the EAPI parser.
- All shell download paths converge on `runDownload`
  (`internal/cli/download.go:64`) via `newDownloadModel`
  (`internal/cli/download.go:252`) or `newDirectDownloadModel`
  (`internal/cli/download.go:272`, used by `internal/cli/history.go:197`).
  The TUI reuses the same model (`internal/cli/shell.go:712-758`). The MCP
  server has its own fetch-then-download path
  (`internal/cli/mcp.go:560-600`).
- Quota exhaustion is currently not classified: EAPI bodies are passed through
  so the server's own message survives (`internal/zlib/eapi.go:228-248`), and
  only session rejection is turned into a sentinel error.

## Approaches

**A. Rotation inside the download model (CLI only).** The Bubble Tea model
holds the account list and rebuilds the client on failure. Smallest diff, but
it puts quota bookkeeping and retry policy inside a UI state machine that also
has a plain mode, a JSON mode and a TUI host, and leaves the MCP path
uncovered.

**B. Account pool inside `internal/zlib`.** Selection and failover live next to
the protocol client. Rejected by the owner: it makes the protocol package
depend on stored credentials, file locking and user configuration, and the
package's tests are pure unit tests that would stop being pure.

**D. Separate `internal/account` manager (chosen).** `internal/zlib` keeps its
single-session client and gains only one new sentinel error. A new package owns
account storage, quota arithmetic, selection and the failover loop; the CLI,
TUI and MCP call it instead of holding a client directly. Storage paths and
file IO stay in `internal/config`, which already owns file layout, so
`internal/account` depends on neither.

Chosen because the two hard parts — deciding whether the next account can
still be used, and re-issuing the request under a different session — are
policy, not protocol, and because both the CLI and MCP have to share exactly
one implementation of them.

## Design

### Package layout and dependency direction

Go forbids an import cycle, so the split has to be stated before anything else:

- `internal/config` defines the plain data types — `Account`, `Quota`,
  `Accounts` — plus `AccountsPath()`, `LoadAccounts()` and `SaveAccounts()`
  with the atomic write. This mirrors the existing `config.Session` and
  `config.Config` pattern rather than inventing a second one.
- `internal/account` holds the `Manager`, the strategy implementations, the
  quota arithmetic and the failover loop. It imports `internal/config` and
  `internal/zlib`, never the reverse.
- `internal/zlib` stays a single-session protocol client and gains only
  `ErrDailyLimitReached`.
- `internal/cli` imports `internal/account` and stops constructing clients from
  `config.LoadSession()` directly.

### Error classification in `internal/zlib`

Add `ErrDailyLimitReached` and classify it where session rejection is already
classified (`internal/zlib/eapi.go:245-250`), matching the EAPI envelope's
`error` text. Matching must be tolerant and table-driven: the exact server
string is unverified, so the test table carries the wordings that name a
download (`daily download limit reached`, `download limit reached`) and the
matcher compares case-insensitively after trimming. The bare `daily limit
reached` is covered separately, on the file endpoint, because the path gate
below is what makes it a quota.

The classifier takes the request path, not just the body. A daily limit that
mentions "download", or that comes back from the file endpoint
(`/eapi/book/<id>/<hash>/file`), is a download quota; a bare "daily limit
reached" from any other endpoint is a request-rate limit and must not become
the sentinel. Without that gate a failing search would tell the user their
downloads are spent, and the manager would blacklist an account that still has
quota for the rest of the day — a silent loss of the pool's whole purpose.

No other change to the protocol package: no account list, no storage, no
selection.

### Storage: `~/.config/zlib/accounts.json`

Owned by `internal/config` for paths and IO, modelled by `internal/account`.
Written with mode 0600 in a 0700 directory, atomically (temp file in the same
directory, then rename), the way session and config files are written today.

```json
{
  "version": 1,
  "strategy": "balanced",
  "reset_time": "05:00",
  "default_account": "<remix_userid>",
  "round_robin_index": 0,
  "accounts": [
    {
      "user_id": "<remix_userid>",
      "email": "user@example.com",
      "label": "",
      "domain": "https://example.invalid",
      "cookies": {"remix_userid": "<id>", "remix_userkey": "<key>"},
      "quota": {
        "allowed": 10,
        "remaining": 7,
        "observed_at": "2026-11-02T16:55:00+08:00",
        "reset_at": "2026-11-03T05:00:00+08:00"
      },
      "last_used_at": "2026-11-02T16:40:00+08:00",
      "needs_relogin": false
    }
  ]
}
```

The key is `user_id` — the `remix_userid` the server issues at login
(`internal/zlib/eapi.go:73-81`) and returns in the profile — not the email. The
email is only what the user typed, an import from a cookie file has none, and
keying on it would let the same physical account enter twice (password login
once, cookie import once) as two entries each claiming the full quota, which
`balanced` would then over-spend while `account list` shows contradictory
numbers. Keying on the server-issued id makes the duplicate impossible and
survives a changed email. `email` and `label` are display fields; `label` is
required only when no email is known. A cookie import never lacks the id: the
existing import already rejects a file without `remix_userid` and
`remix_userkey` (`internal/cli/cookies.go:152-156`), so no extra profile call
is needed at import time. Replacing an existing entry keeps its quota record.

`needs_relogin` marks an account whose cookies came back `ErrSessionExpired`.
It is skipped by selection but kept in the list so the user can re-run `login`.

### Domain precedence

`ZLIB_DOMAIN` keeps outranking every account's stored `domain`, exactly as it
does today (`internal/cli/root.go:41-46,59`). This is deliberate: it is the
documented escape hatch when a mirror is blocked, and `zlib login` writes the
variable on every login (`internal/cli/login.go:102` → `dotenv.go:75-101`), so
an install that has logged in twice already has it set. The consequence has to
be stated in the docs: with several accounts on different domains the variable
holds the last login's domain and pins all of them there. The per-account
`domain` is used only when the variable is unset, and it is still stored so
removing the variable restores per-account behaviour. Account credentials are
mirror-independent, so pointing account A at account B's origin is not a
credential leak — the same two cookies are sent to whichever mirror is chosen.

### Reset arithmetic

`reset_at` is the next occurrence of `reset_time` in local wall-clock time,
stored with the offset in force when written. On every read:

- a quota that was never observed stays unknown: there is nothing to roll over;
- if `now >= reset_at` and the allowance is known, set `remaining = allowed`
  and re-anchor `reset_at` to the next occurrence of `reset_time` in `now`'s
  location;
- if `now >= reset_at` and the allowance is unknown (`allowed == 0`), the
  account returns to *unknown* rather than to zero: forgetting the number makes
  it usable again and lets the next download fetch the real one. This is the
  state an account lands in after a quota rejection on a plan that never
  reports its limit — leaving it known with `remaining == 0` and `allowed == 0`
  would keep it out of the pool forever, because nothing would ever roll it
  over.

Owner decision: fixed local reset time, default `05:00`. The default comes from
one observation — a page reporting "Will reset in 12h 05m" at 16:55 on a
UTC+8 machine — and is configurable because a wall-clock default is only right
while the machine's clock and timezone stay put. Because the server computes
that countdown itself, a user in another timezone or one whose rule differs
must set `reset_time`.

For context, not as a justification: 05:00 at UTC+8 is 21:00 UTC, which is
midnight at UTC+3. That the reset is a midnight in a European timezone is a
plausible reading of one observation, not a verified fact, which is why the
value stays configurable.

### Cache freshness

Owner decision: local values drive selection; a refresh is not required per
download. Builder call: the staleness rule. The recommended shape is a
`quota_ttl` (default 12h): when the selected account's `observed_at` is older,
refresh that one account from `/eapi/user/profile` before using it, and never
refresh more than one account per download. `zlib account sync` refreshes all
accounts on demand.

The refresh is best-effort and must never block or fail a download: if the
profile request times out, errors, or the quota comes back unknown, log a
warning, keep the cached numbers, and download anyway. A refresh that
downgrades a working download into a failure would be worse than the staleness
it was meant to fix.

An account whose quota was never observed is stale by definition and is
refreshed on first use. A refresh that succeeds but reports no numbers still
stamps the observation time, so a plan that never exposes a quota is asked once
per TTL rather than on every download.

### Selection

Both strategies skip accounts with `remaining == 0` and `needs_relogin`.
`--account <email|label|user_id>` pins one account and disables rotation
entirely, and the pin also bypasses both filters: the local numbers are an
estimate, so pinning is the "I know better, try it anyway" escape hatch, and a
server rejection is reported as such instead of being pre-empted by a cached
zero or a stale `needs_relogin` flag. A pinned account that is really expired
answers with `ErrSessionExpired`, not with an all-exhausted error.

`balanced`: order by `remaining / allowed` descending, then `remaining`
descending, then least recently used, then alphabetical. Accounts with unknown
quota rank after known ones but ahead of nothing.
Idea behind the ratio, not the absolute count: a 5/day account and a 20/day
account are spent at the same relative pace instead of the small one idling.

Owner decision: `balanced` is the default strategy.

`round_robin`: start at the persisted `round_robin_index`, scan forward
wrapping once, take the first usable account, and persist the index of the
*next* account to try (the one after the account used), not the index of the
account used. Persisting the used account would make every run pick it again,
since the scan starts at that index.

### Failover loop

The manager's entry point takes either a book reference (`id:hash`, which the
`download` command, the search/history/saved pickers and MCP all have) or a
finished download URL (the direct path used by `internal/cli/history.go:197`).
For a reference, the metadata fetch happens *inside* the loop, on the account
about to be used. Fetching once on the default account and then rotating would
contradict the whole point: a revoked or expired default session would kill the
command before any other account got a chance. `ErrSessionExpired` from
`FetchBook` is therefore a rotation reason too, exactly like one from the
download.

```
excluded := {}
for {
    acct := select(strategy, excluding excluded)
    if acct == nil { return ErrAllAccountsExhausted(nextReset) }
    client := clientFor(acct)                     // SetDomain, then SetCookies
    url, err := ref.urlFor(client)                // FetchBook for id:hash; host rewrite for a raw URL
    if errors.Is(err, zlib.ErrSessionExpired) {
        markNeedsRelogin(acct); excluded += acct; continue
    }
    if err != nil { return err }                  // unknown book, network, bad id
    res, err := client.Download(ctx, url, dir, progress)
    switch {
    case errors.Is(err, zlib.ErrDailyLimitReached):
        markExhausted(acct); excluded += acct; continue
    case errors.Is(err, zlib.ErrSessionExpired):
        markNeedsRelogin(acct); excluded += acct; continue
    case err != nil:
        return err                                // network, permissions, 404
    }
    acct.remaining--; acct.lastUsedAt = now; persist(); return res
}
```

Notes that the implementation has to honour:

- Only a quota rejection and a rejected session cause a rotation. A network
  error or a missing file is the user's problem, not a reason to burn another
  account's quota.
- `remaining` is decremented only after a completed transfer. A transfer that
  broke halfway may or may not have been counted by the server; the next
  `sync` corrects it. Marking it 0 would be a lie in the other direction.
- An account that reports the limit is marked 0 immediately, so the next
  download in the same session does not retry it.
- The loop is bounded by the number of accounts; `excluded` guarantees
  termination even if the server keeps returning the same answer.
- Progress reporting restarts on each attempt, and the UI must show which
  account is being tried — an unexplained progress bar resetting to zero looks
  like a crash.

The manager therefore takes callbacks, not a UI:

```go
type DownloadOptions struct {
    ProgressFn       func(written, total int64)
    OnAccountAttempt func(acct config.Account, attempt int, reason error)
}
```

`OnAccountAttempt` fires before each attempt, with the previous attempt's
reason (`nil` on the first). The Bubble Tea model turns it into a line such as
"quota exhausted on <account>, trying <next>"; the plain and JSON paths turn it
into a stderr line and an `attempts` field in the result. Without that hook
each caller would have to re-derive the reason from an error it never sees.

### Cross-domain references

`accounts[].domain` may differ per account, but a `id:hash` reference never
needs rewriting: it is fetched on the account about to be used, so the URL it
yields is already on that account's origin. Rewriting applies only to the raw
URL form (`history.go:197`), where the URL was minted under another account's
domain. There, rewrite the host to the selected account's domain when the path
is the `/eapi/book/<id>/<hash>/file` form; anything else is treated as
account-bound and is not rotated at all. If a rewrite gets a 404, the next
attempt uses the original URL, because whether every domain serves the same
path is unverified.

### Where the pool applies

- Download commands (`download`, `search`, `history`, `saved`, feed, TUI,
  MCP `download_book`) use the pool.
- Identity-sensitive reads (`favorites`, `history` listing, `profile`,
  `kindle`) use `default_account`. Owner decision: rotation is for quota, not
  for identity, so the collection a user sees does not change depending on who
  still has downloads left. Switching identity is `zlib account use`, not a
  per-command flag: those commands keep the single-client shape they have
  today. One consequence worth naming:
  `history --download <id>` looks the item up in the default account's history
  and only then downloads through the pool, so a book that exists only in
  another account's history is reported as not found.
- The TUI's own login (`internal/cli/shell.go:882-899`) must write through the
  manager. Left as is, a login from the TUI would create a session the pool
  cannot see and the next mirror write would overwrite it — a login that
  appears to work and then does nothing. The shell's `signedIn` check
  (`shell.go:115-116`) keeps reading `session.json`, which the manager
  rewrites whenever the default account changes: `config.LoadAccounts()`
  migrates and writes on first use, so calling it from `newShellModel` would
  turn every TUI start into a config write.
- MCP gains an optional `account` input on `download_book` and reports the
  serving account in `downloadResult` (additive schema change). Without it an
  agent cannot reproduce a download or explain which account spent quota.
- `zlib account use <email>` changes `default_account`.

### CLI surface

| Command | Behaviour |
| --- | --- |
| `zlib login` | Adds or updates an account; becomes default only when it is the first |
| `zlib account list` | Email/label, domain, remaining/allowed, reset time, last used, default marker; `--json` |
| `zlib account sync [email...]` | Refresh quota from the server, all accounts or the named ones |
| `zlib account use <email>` | Set `default_account` |
| `zlib account config` | Show or set `strategy` and `reset_time` |
| `zlib account remove <email>` | Delete; re-point or clear the default |
| `zlib logout [--account <email>]` | Drop the cookies of that account, or all with `--all` |
| `download`, `search`, `history`, `saved` | `--account` to pin, `--strategy` to override for one run |

Every `--account` selector accepts an email, a label or a `user_id`, resolved
exactly once; an ambiguous selector is an error rather than a guess.

`--strategy` on a download overrides the stored value for that run without
persisting it.

`login` gains `--label` (an optional display name; a cookie import always
carries an account id because the importer rejects files without one) and
`--use` (make the account just logged in the default). Without `--use` a new
account never steals the default: logging into a second account is a step
towards a pool, not a request to switch identity. An existing `user_id` is
updated in place — cookies and domain replaced, quota history kept — so
re-running `login` to refresh an expired key does not create a second entry.

Removing the default account re-points the default to the first remaining
usable account and says so; removing the last one clears the default and asks
the user to log in. `logout --all` does the same as removing every account.
Neither ever leaves a `default_account` pointing at an entry that is gone.

### Migration

On first load of `accounts.json`:

1. If it exists, use it.
2. Otherwise, if `session.json` exists, create `accounts.json` with one
   account from it; the key is the `remix_userid` in its cookies, the email
   comes from `config.json`'s `LastLoginEmail`, and an empty email becomes the
   label `account-1`.
3. Keep writing `session.json` as a mirror of the default account for one
   release, so an older binary in the same config directory still works;
   remove the mirror later.

### Concurrency

One process at a time is the norm, but the MCP server answers tool calls
concurrently, so the manager holds a mutex and every mutation reloads, applies,
and writes atomically. Two separate processes editing at once remain
last-writer-wins; the design accepts it and the document says so rather than
adding a lock file.

## Testing

Unit tests in `internal/account`, using the existing pure-test style of
`internal/zlib`:

- reset arithmetic across midnight, across a month boundary, when
  `reset_time` is later than now, and with a clock already past reset;
- selection ties in both strategies, unknown quota, `remaining == 0`
  skipping, pinned account, `needs_relogin` skipping;
- failover: exhaustion rotates, session expiry rotates — from the fetch as
  well as from the download — network error does not rotate, all-exhausted
  error carries the next reset, loop terminates;
- selection: a pinned account is used even when its cached `remaining` is 0;
- storage: atomic write, migration from `session.json`, replacement on a
  repeated login, a password login and a cookie import of the same account
  collapsing into one entry, removal re-pointing the default, removing the last
  account clearing it, an ambiguous `--account` selector rejected;
- freshness: a stale account refreshes once, a failing profile refresh still
  downloads on the cached numbers, an unknown quota does not become zero;
- callbacks: `OnAccountAttempt` fires once per attempt with the previous
  reason, and the plain path's JSON carries the attempt count.

CLI tests: `--json` shape of `account list`, and one rotation scenario driven
through the existing demo fixture harness so the progress UI and JSON output
are both covered.

One manual verification, because the numbers cannot be proven from the code:
run `profile`, download one book, run `profile` again, and compare the used
count before and after. That settles where quota is charged and whether an
aborted transfer counts.

## Open items to verify during implementation

- The exact server text for an exhausted quota. The classifier is written to a
  table so a new string is one line, but the first real response has to be
  captured.
- Whether quota is charged at `/eapi/book/<id>/<hash>/file` or at the CDN
  request, and whether a cancelled transfer counts. This decides whether the
  local decrement belongs on success or on link resolution.
- Whether the reset is really a fixed local time rather than a rolling 24h
  window per account. One more day of observation closes it.
- Whether `/eapi/book/<id>/<hash>/file` is served identically on every domain.
- Whether the `dl` field ever carries a token.
