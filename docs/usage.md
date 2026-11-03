# Search and download

[README](../README.md) · [Configuration](configuration.md) · [Troubleshooting](troubleshooting.md)

Complete the [quick start](../README.md#quick-start) first. Book commands use your
saved Z-Library session.

Run `zlib tui` in a terminal for the persistent shell: type a query or use `/` to
choose a command. See [terminal controls](tui.md). The commands below also work
directly, including in scripts.

## Search

Open the terminal picker without a query:

```sh
zlib search
```

Enter a query, select with `↑/↓`, change pages with `←/→`, and press `Enter` to
download. For a plain table, supply the query on the command line:

```sh
zlib search "dune"
zlib search "dune" --page 2
zlib search "python crash course" --ext epub --ext pdf
zlib search "civilized to death" --full-title
```

`--ext` is repeatable; `--format` is its alias. `--full-title` prevents title
truncation in the table. Use `--json --count 10` for [scripts and agents](agents.md).

## Download a result

Copy the complete reference from a result's ID column or JSON `id` field.
EAPI references contain both the numeric ID and hash, separated by `:`.
Replace `id:hash` in these examples with that reference:

```sh
zlib download "id:hash"
zlib download "id:hash" --dir ./books
zlib download "id:hash" --dir "~/Downloads"
```

Create the destination directory first. `--dir` expands a leading `~`; without
the flag, zlib uses `ZLIB_DOWNLOAD_DIR`, then the current directory. See
[download settings](configuration.md#download-settings) to save a default.

Press `Ctrl+C` to cancel a download; the CLI removes the incomplete file.
After [Kindle setup](kindle.md), add `--send-to-kindle` to send the downloaded file.

Without a terminal, `download` prints `Downloading:` and a progress line per 10%
to stderr, then `Saved to:` with the absolute path on stdout. `--json` replaces
that last line with one JSON document and skips the folder prompt:

```json
{"id":"id:hash","name":"Dune","path":"/home/me/books/Dune.epub","size":1048576,"account":"user@example.com","attempts":1}
```

`account` names the account that served the download and `attempts` counts the
accounts tried before it worked.

With `--send-to-kindle`, the document also has `kindle`: `{"sent":true,"to":"..."}`,
or `{"sent":false,"error":"..."}` when the book was saved but delivery failed.
`history --download` accepts `--json` the same way.

## Several accounts

Downloads rotate through every saved account, so a day's quota is the sum of the
pool. `zlib login` adds an account; it never replaces the default unless it is
the first one or you pass `--use`.

```sh
zlib login --email user@example.com
zlib login --email user@example.com --label work --use
zlib account list
zlib account sync
zlib account sync user@example.com
zlib account use user@example.com
zlib account config --strategy round_robin
zlib account config --reset-time 05:00
zlib account remove user@example.com
zlib download "id:hash" --account user@example.com
zlib download "id:hash" --strategy round_robin
zlib logout --account user@example.com
zlib logout --all
```

`zlib account list` marks the default with `*` and shows each account's cached
quota. Selection follows the stored strategy: `balanced` (the default) prefers
the account with the highest share of its daily allowance left; `round_robin`
walks the saved list in order. Override it for one run with `--strategy`. When an
account runs out of quota, or its session has expired, the download moves to the
next usable account and prints which one took over; `download` with `--json`
reports the same in `account` and `attempts`.

`--account` pins one run to that account. That turns off the rotation, so an
exhausted account still gets its one try and its own error is reported instead of
a silent skip. `--account` and `--strategy` are accepted by every command that
can download from its list: `search`, `history`, `favorites`, `recommended` and
`most-popular`.

When an account's session expires, its entry stays in the pool, is marked as
needing a login, and is skipped until you sign in again. Logging in again with
the same account updates that entry in place — cookies and domain are replaced
while the cached quota is kept — so re-running `zlib login` never creates a
duplicate. Importing a cookie file with `--cookies` behaves the same way.

Quota numbers are a local estimate: refreshed from the server when they are older
than 12 hours, and only for the account about to be used. `zlib account sync`
refreshes every account on demand. `reset_time` is a local wall-clock time,
`05:00` by default. The read commands (`search`, `favorites`, `recommended`,
`most-popular`, `history`, `profile` and Kindle delivery) use the default account,
so `zlib account use` switches whose collection they show.

## Saved books, recommendations and history

In the history browser, press `d` to delete the selected entry, then `y` to
confirm or `n` / `Esc` to cancel. This removes the remote history entry only;
local book files remain. Scripts can delete directly with
`zlib history --delete "id:hash"` (also accepts a numeric ID); add `--json`
for a machine-readable success response.

```sh
zlib favorites
zlib recommended
zlib most-popular
zlib history
```

`favorites` shows your account collection. In the terminal picker,
select with `↑/↓` and press `Enter` to download. Saved books and history support
`←/→` pagination; recommendations and popular books are single lists.

For non-interactive output, use an explicit page or JSON:

```sh
zlib favorites --page 2
zlib favorites --json
zlib recommended --json
zlib most-popular --json
zlib history --page 1
zlib history --format epub
zlib history --json
```

To download a history entry directly, copy its full book reference:

```sh
zlib history --download "id:hash" --dir ./books
```

## Account and appearance

```sh
zlib profile
zlib profile --json
zlib logout
```

`profile` shows the default account's details and daily download quota. JSON
output excludes authentication keys but includes personal account fields; keep it
private. A bare `logout` removes the default account, `--account` names another
one and `--all` removes every saved account. See
[Several accounts](#several-accounts) for the pool these commands work on.

```sh
zlib theme
zlib theme catppuccin-mocha
```

`zlib theme` opens a searchable list with a zlib search and download preview. Use ↑/↓ to
choose, type to filter, Enter to save, or Esc to clear the filter and close.
With redirected output, it prints the current and available themes instead.
`ZLIB_THEME` overrides the saved theme. See [Themes](themes.md) for the built-in
names and preview controls.

See [Terminal interface](tui.md) for interactive controls. For exact flags and
defaults, run `zlib <command> --help`.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, or you left a list without choosing a book |
| 1 | Other error |
| 2 | Usage error: unknown command or flag, wrong number of arguments |
| 3 | Not logged in, or the saved session expired; run `zlib login` |
| 4 | Network: a host (a mirror, or GitHub for `update`) is unreachable, the mirror is behind DiamWall, `doctor` found no usable domain, or `mirrors add` found no Z-Library at the domain |
| 5 | The book was saved, but Send to Kindle failed |
| 130 | You cancelled the download or its folder prompt, or declined to trust a mirror |

Errors go to stderr. A network error names the host; for a Z-Library mirror it
suggests `zlib doctor`.
