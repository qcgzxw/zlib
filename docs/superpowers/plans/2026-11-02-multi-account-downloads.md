# Multi-Account Download Pool Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pool several Z-Library accounts behind one download command, so a download succeeds while any saved account still has daily quota, with a local per-account quota cache and two selection strategies.

**Architecture:** `internal/config` stores `accounts.json` as plain data (atomic write, migration from `session.json`). A new `internal/account` package owns quota arithmetic, selection and the failover loop; `internal/zlib` gains only `ErrDailyLimitReached`. CLI, TUI and MCP call the manager instead of building clients from `session.json` for downloads, while identity-sensitive reads keep using the default account.

**Tech Stack:** Go 1.26, cobra, bubbletea/bubbles/lipgloss, `go test` with `httptest` and table-driven tests.

**Spec:** `docs/superpowers/specs/2026-11-02-multi-account-downloads-design.md`

**Testing rule:** run only the tests a step names. Run `go test ./...` once, in Task 10, before the final commit. `make check` (docs + gitleaks) runs in Task 10 too.

**Commit rule:** Gitmoji format, no tool attribution, stage explicit paths, never `--amend`.

---

## File structure

| File | Responsibility |
| --- | --- |
| `internal/zlib/errors.go` (modify) | `ErrDailyLimitReached` sentinel |
| `internal/zlib/eapi.go` (modify) | classify the EAPI quota envelope |
| `internal/config/accounts.go` (create) | `Account`, `Quota`, `Accounts` types, atomic load/save, `session.json` migration, `Resolve` |
| `internal/account/quota.go` (create) | reset-time arithmetic |
| `internal/account/strategy.go` (create) | usability, balanced ranking |
| `internal/account/manager.go` (create) | failover loop, callbacks, persistence, `Sync` |
| `internal/cli/account.go` (create) | `zlib account list\|sync\|use\|remove` |
| `internal/cli/login.go` (modify) | `--label`, `--use`, upsert into `accounts.json` |
| `internal/cli/logout.go` (modify) | remove one account or all, re-point the default, refresh the mirror |
| `internal/cli/download.go` (modify) | download model drives the manager; `--account`, `--strategy` |
| `internal/cli/search.go`, `history.go`, `saved.go`, `book_feed.go` (modify) | pass the manager; register the same two flags |
| `internal/cli/shell.go` (modify) | TUI login, logout and download use the manager |
| `internal/cli/mcp.go`, `mcp_output.go` (modify) | `account` input, `account` and `attempts` in results |
| `docs/usage.md`, `docs/configuration.md` (modify) | document the pool |

---

### Task 1: `zlib.ErrDailyLimitReached`

**Files:**
- Modify: `internal/zlib/errors.go:8-31`
- Modify: `internal/zlib/eapi.go:228-268`
- Test: `internal/zlib/eapi_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/zlib/eapi_test.go` (add `"errors"` to its imports if missing):

```go
func TestEAPIDailyLimitIsClassified(t *testing.T) {
	for _, message := range []string{
		"Daily download limit reached",
		"Download limit reached.",
	} {
		t.Run(message, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"success":0,"error":%q}`, message)
			}))
			defer server.Close()
			_, err := newLoggedInEAPITestClient(server.URL).GetProfile()
			if !errors.Is(err, ErrDailyLimitReached) {
				t.Fatalf("error = %v, want ErrDailyLimitReached", err)
			}
		})
	}
}

// The bare wording carries no "download", so it only counts on the file
// endpoint. On any other endpoint it is a request-rate limit.
func TestEAPIBareDailyLimitIsQuotaOnlyOnTheFileEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"success":0,"error":"Daily limit reached"}`)
	}))
	defer server.Close()

	client := NewClient(WithDomain(server.URL))
	client.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "<key>"})

	if _, err := client.Download(server.URL+"/eapi/book/1/abc/file", t.TempDir(), nil); !errors.Is(err, ErrDailyLimitReached) {
		t.Fatalf("Download error = %v, want ErrDailyLimitReached", err)
	}
	if _, err := client.GetProfile(); errors.Is(err, ErrDailyLimitReached) {
		t.Fatalf("profile error = %v, must not be ErrDailyLimitReached", err)
	}
}

func TestEAPILoginIsNeverADailyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"success":0,"error":"Daily download limit reached"}`)
	}))
	defer server.Close()
	client := NewClient(WithDomain(server.URL))
	if err := client.Login("reader@example.com", "pw"); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("Login error = %v, want ErrLoginFailed", err)
	}
}

func TestEAPIUnrelatedEnvelopeIsNotADailyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"success":0,"error":"Book not found"}`)
	}))
	defer server.Close()
	_, err := newLoggedInEAPITestClient(server.URL).GetProfile()
	if errors.Is(err, ErrDailyLimitReached) {
		t.Fatalf("error = %v, must not be ErrDailyLimitReached", err)
	}
}
```

The `daily limit reached` case moved out of the first table on purpose: after the
path gate it is only a quota on the file endpoint, and `GetProfile` would
(correctly) no longer classify it.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/zlib -run 'TestEAPIDailyLimit|TestEAPIUnrelatedEnvelope' -v`
Expected: FAIL with `undefined: ErrDailyLimitReached`.

- [ ] **Step 3: Add the sentinel**

In `internal/zlib/errors.go`, inside the `var (...)` block after `ErrDownloadFailed`:

```go
	// ErrDailyLimitReached is returned when the account has spent today's
	// download allowance. It is per account, so a caller holding several
	// sessions can retry the same request with another one.
	ErrDailyLimitReached = errors.New("zlibrary: daily download limit reached")
```

- [ ] **Step 4: Classify the envelope**

In `internal/zlib/eapi.go`, next to `isEAPISessionRejection` (around line 251), add:

```go
// isEAPIDailyLimitRejection reports whether body is the envelope EAPI returns
// when the account has no downloads left today. The exact wording is not
// documented, so every candidate is matched loosely.
//
// The path matters: a bare "daily limit reached" from a search is a request
// rate limit, not a download quota. Classifying it as quota would tell the
// user their downloads are spent and make the manager blacklist an account
// that still has some, so only a message that says "download" or a response
// from the file endpoint produces the sentinel.
func isEAPIDailyLimitRejection(body []byte, path string) bool {
	var envelope struct {
		Success *int   `json:"success"`
		Error   string `json:"error"`
	}
	if err := decodeEAPIJSON(body, &envelope); err != nil || envelope.Success == nil || *envelope.Success != 0 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(envelope.Error))
	if !strings.Contains(text, "limit") {
		return false
	}
	if strings.Contains(text, "download") {
		return true
	}
	return strings.Contains(text, "daily") && isEAPIFilePath(path)
}

// isEAPIFilePath reports whether path is the download endpoint.
func isEAPIFilePath(path string) bool {
	return strings.HasPrefix(path, "/eapi/book/") && strings.HasSuffix(path, "/file")
}
```

In `eapiRequest`, immediately before the session-rejection block (line ~245):

```go
	if path != eapiLoginPath && isEAPIDailyLimitRejection(responseBody, path) {
		return nil, ErrDailyLimitReached
	}
```

Order matters: the daily-limit check goes first, because a rejected quota is not a rejected session.

- [ ] **Step 4b: Update the two existing fixtures that use the quota text**

Two tests already used the daily-limit wording as their example of "the
server's message must survive"; they now get the sentinel instead of the raw
message and fail. Replace the fixture text with a non-quota error so they keep
testing what they were written for:

- `internal/zlib/eapi_test.go:102` (`TestEAPIPreservesServerErrorOnNon2xx`):
  `{"success":0,"error":"Daily download limit reached"}` → `{"success":0,"error":"Book not found"}`, and the `strings.Contains` check on line 116 accordingly.
- `internal/zlib/eapi_test.go:267` (`TestEAPIRequestKeepsOtherRejectionsAndLoginFailures`):
  `{"success":0,"error":"Daily limit reached"}` → `{"success":0,"error":"Book not found"}`, and the check on line 277 accordingly.

Do not relax the new classifier to keep those fixtures passing: the guarantee
they protect is covered by `TestEAPIUnrelatedEnvelopeIsNotADailyLimit`.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/zlib -run 'TestEAPIDailyLimit|TestEAPIUnrelatedEnvelope' -v`
Expected: PASS.

- [ ] **Step 6: Run the package tests**

Run: `go test ./internal/zlib`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/zlib/errors.go internal/zlib/eapi.go internal/zlib/eapi_test.go
git commit -m ":sparkles: (classify the daily download limit)"
```

---

### Task 2: `accounts.json` storage and migration

**Files:**
- Create: `internal/config/accounts.go`
- Test: `internal/config/accounts_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/config/accounts_test.go`. It reuses the package's existing `isolateConfigHome` helper (`internal/cli` has one; for `internal/config` add the same helper here) and skips the mode assertion on Windows, where Go reports 0666:

```go
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

func TestSaveAccountsWritesPrivateFile(t *testing.T) {
	isolateHome(t)
	in := Accounts{Accounts: []Account{{
		UserID:  "42",
		Email:   "reader@example.com",
		Domain:  "https://example.com",
		Cookies: map[string]string{"remix_userid": "42", "remix_userkey": "<key>"},
	}}}
	if err := SaveAccounts(in); err != nil {
		t.Fatalf("SaveAccounts() error = %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(AccountsPath())
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("mode = %v, want 0600", mode)
		}
	}
	got, err := LoadAccounts()
	if err != nil {
		t.Fatalf("LoadAccounts() error = %v", err)
	}
	if got.Strategy != StrategyBalanced || got.ResetTime != DefaultQuotaResetTime || got.Version != 1 {
		t.Fatalf("defaults = %+v", got)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].UserID != "42" {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
}

func TestLoadAccountsMigratesLegacySession(t *testing.T) {
	dir := isolateHome(t)
	configDir := filepath.Join(dir, ".config", "zlib")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := `{"cookies":{"remix_userid":"7","remix_userkey":"<key>"},"domain":"https://example.com","mode":"eapi"}`
	if err := os.WriteFile(filepath.Join(configDir, "session.json"), []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"last_login_email":"reader@example.com"}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAccounts()
	if err != nil {
		t.Fatalf("LoadAccounts() error = %v", err)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("accounts = %+v", got.Accounts)
	}
	acct := got.Accounts[0]
	if acct.UserID != "7" || acct.Email != "reader@example.com" || acct.Domain != "https://example.com" {
		t.Fatalf("migrated account = %+v", acct)
	}
	if got.DefaultAccount != "7" {
		t.Fatalf("default = %q, want 7", got.DefaultAccount)
	}
	if _, err := os.Stat(AccountsPath()); err != nil {
		t.Fatalf("migration did not persist accounts.json: %v", err)
	}
}

func TestLoadAccountsWithoutSessionStartsEmpty(t *testing.T) {
	isolateHome(t)
	got, err := LoadAccounts()
	if err != nil {
		t.Fatalf("LoadAccounts() error = %v", err)
	}
	if len(got.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", got.Accounts)
	}
	if got.ResetTime != DefaultQuotaResetTime {
		t.Fatalf("reset time = %q", got.ResetTime)
	}
}

func TestUpsertReplacesAndKeepsQuota(t *testing.T) {
	var a Accounts
	a.Normalize()
	quota := Quota{Known: true, Allowed: 10, Remaining: 4, ResetAt: time.Now().Add(time.Hour)}
	a.Accounts = []Account{{UserID: "42", Email: "old@example.com", Quota: quota}}
	replaced := a.Upsert(Account{UserID: "42", Email: "new@example.com", Domain: "https://example.com"})
	if !replaced || len(a.Accounts) != 1 {
		t.Fatalf("replaced = %v, accounts = %+v", replaced, a.Accounts)
	}
	got := a.Accounts[0]
	if got.Email != "new@example.com" || got.Quota.Remaining != 4 {
		t.Fatalf("account = %+v", got)
	}
}

func TestRemoveRepointsToTheFirstUsableAccount(t *testing.T) {
	var a Accounts
	a.DefaultAccount = "1"
	a.Accounts = []Account{
		{UserID: "1"},
		{UserID: "2", Quota: Quota{Known: true, Allowed: 10, Remaining: 0}},
		{UserID: "3", Quota: Quota{Known: true, Allowed: 10, Remaining: 4}},
	}
	a.Remove("1")
	if a.DefaultAccount != "3" {
		t.Fatalf("default = %q, want the first usable account 3", a.DefaultAccount)
	}
}

func TestResolveAcceptsAnySelectorAndRejectsAmbiguity(t *testing.T) {
	var a Accounts
	a.Accounts = []Account{
		{UserID: "1", Email: "one@example.com"},
		{UserID: "2", Label: "backup"},
		{UserID: "3", Label: "backup"},
	}
	for _, selector := range []string{"one@example.com", "2"} {
		if _, err := a.Resolve(selector); err != nil {
			t.Fatalf("Resolve(%q) error = %v", selector, err)
		}
	}
	if _, err := a.Resolve("backup"); err == nil {
		t.Fatal("Resolve(backup) must reject an ambiguous selector")
	}
	if _, err := a.Resolve("nope"); err == nil {
		t.Fatal("Resolve(nope) must fail")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config -run 'TestSaveAccounts|TestLoadAccounts|TestUpsert|TestResolve' -v`
Expected: FAIL with `undefined: Accounts`.

- [ ] **Step 3: Write the implementation**

Create `internal/config/accounts.go`:

```go
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Selection strategies stored in accounts.json.
const (
	StrategyBalanced   = "balanced"
	StrategyRoundRobin = "round_robin"
)

// DefaultQuotaResetTime is the local wall-clock time the daily download
// counter resets. It is a default, not a fact about the server: the EAPI
// exposes used and allowed counts but no reset instant, so the value comes
// from an observed page countdown and stays configurable.
const DefaultQuotaResetTime = "05:00"

// Quota is the locally cached daily download allowance of one account.
// Known is false until the server has reported numbers at least once.
type Quota struct {
	Known      bool      `json:"known"`
	Allowed    int       `json:"allowed"`
	Remaining  int       `json:"remaining"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
	ResetAt    time.Time `json:"reset_at,omitzero"`
}

// Account is one saved Z-Library session plus its cached quota.
type Account struct {
	UserID       string            `json:"user_id"`
	Email        string            `json:"email,omitempty"`
	Label        string            `json:"label,omitempty"`
	Domain       string            `json:"domain"`
	Cookies      map[string]string `json:"cookies"`
	Quota        Quota             `json:"quota"`
	LastUsedAt   time.Time         `json:"last_used_at,omitzero"`
	NeedsRelogin bool              `json:"needs_relogin,omitempty"`
}

// Name is the short label commands show for the account.
func (a Account) Name() string {
	if a.Email != "" {
		return a.Email
	}
	if a.Label != "" {
		return a.Label
	}
	return a.UserID
}

// Accounts is the whole accounts.json document.
type Accounts struct {
	Version         int       `json:"version"`
	Strategy        string    `json:"strategy,omitempty"`
	ResetTime       string    `json:"reset_time,omitempty"`
	DefaultAccount  string    `json:"default_account,omitempty"`
	RoundRobinIndex int       `json:"round_robin_index,omitempty"`
	Accounts        []Account `json:"accounts"`
}

func AccountsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "zlib", "accounts.json")
}

// Normalize fills the defaults an older or hand-edited file may be missing.
func (a *Accounts) Normalize() {
	if a.Version == 0 {
		a.Version = 1
	}
	switch a.Strategy {
	case StrategyBalanced, StrategyRoundRobin:
	default:
		a.Strategy = StrategyBalanced
	}
	if strings.TrimSpace(a.ResetTime) == "" {
		a.ResetTime = DefaultQuotaResetTime
	}
}

// Resolve finds exactly one account by user id, email or label. An ambiguous
// selector is an error: guessing would spend the wrong account's quota.
func (a Accounts) Resolve(selector string) (Account, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Account{}, errors.New("no account given")
	}
	var matches []Account
	for _, acct := range a.Accounts {
		if acct.UserID == selector || strings.EqualFold(acct.Email, selector) || strings.EqualFold(acct.Label, selector) {
			matches = append(matches, acct)
		}
	}
	switch len(matches) {
	case 0:
		return Account{}, fmt.Errorf("no account matches %q", selector)
	case 1:
		return matches[0], nil
	default:
		return Account{}, fmt.Errorf("%q matches %d accounts; use the email or user id", selector, len(matches))
	}
}

// Upsert adds acct, or replaces the entry with the same UserID while keeping
// its quota record. It reports whether an existing entry was replaced.
func (a *Accounts) Upsert(acct Account) bool {
	for i := range a.Accounts {
		if a.Accounts[i].UserID != acct.UserID {
			continue
		}
		acct.Quota = a.Accounts[i].Quota
		acct.LastUsedAt = a.Accounts[i].LastUsedAt
		a.Accounts[i] = acct
		return true
	}
	a.Accounts = append(a.Accounts, acct)
	return false
}

// Remove drops the account and re-points the default when it was the one
// removed: to the first remaining account that could still download, falling
// back to the first entry when none can. It reports whether an entry was
// removed.
func (a *Accounts) Remove(userID string) bool {
	for i := range a.Accounts {
		if a.Accounts[i].UserID != userID {
			continue
		}
		a.Accounts = append(a.Accounts[:i], a.Accounts[i+1:]...)
		if a.DefaultAccount == userID {
			a.DefaultAccount = ""
			for _, acct := range a.Accounts {
				if !acct.NeedsRelogin && (!acct.Quota.Known || acct.Quota.Remaining > 0) {
					a.DefaultAccount = acct.UserID
					break
				}
			}
			if a.DefaultAccount == "" && len(a.Accounts) > 0 {
				a.DefaultAccount = a.Accounts[0].UserID
			}
		}
		return true
	}
	return false
}

// LoadAccounts reads accounts.json. A legacy session.json is migrated on first
// use and written back immediately, so the pool never exists only in memory.
func LoadAccounts() (Accounts, error) {
	data, err := os.ReadFile(AccountsPath())
	if err == nil {
		var a Accounts
		if err := json.Unmarshal(data, &a); err != nil {
			return Accounts{}, fmt.Errorf("parse %s: %w", AccountsPath(), err)
		}
		a.Normalize()
		return a, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Accounts{}, err
	}
	migrated := migrateSession()
	if len(migrated.Accounts) > 0 {
		// A migration that cannot be written must be reported, not swallowed:
		// the pool would otherwise exist only in this process's memory and
		// vanish on the next command.
		if err := SaveAccounts(migrated); err != nil {
			return Accounts{}, fmt.Errorf("migrate %s: %w", AccountsPath(), err)
		}
	}
	return migrated, nil
}

// migrateSession turns a single saved session into a one-account pool.
func migrateSession() Accounts {
	var a Accounts
	a.Normalize()
	session, err := LoadSession()
	if err != nil || len(session.Cookies) == 0 {
		return a
	}
	acct := Account{
		UserID:  strings.TrimSpace(session.Cookies["remix_userid"]),
		Domain:  session.Domain,
		Cookies: session.Cookies,
	}
	if cfg, err := LoadConfig(); err == nil {
		acct.Email = strings.TrimSpace(cfg.LastLoginEmail)
	}
	if acct.UserID == "" {
		acct.Label = "account-1"
		acct.UserID = "label:account-1"
	}
	a.Accounts = []Account{acct}
	a.DefaultAccount = acct.UserID
	return a
}

// SaveAccounts writes accounts.json atomically: a temporary file in the same
// directory, then a rename, so an interrupted write never truncates the file.
func SaveAccounts(a Accounts) error {
	a.Normalize()
	path := AccountsPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/config -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/accounts.go internal/config/accounts_test.go
git commit -m ":sparkles: (store multiple accounts)"
```

**Amendments made during implementation — the committed code is authoritative:**

- `LoadAccounts` returns the migration write error instead of `_ = SaveAccounts(...)`.
- `Upsert` keeps `Email` and `Label` when the incoming account leaves them empty, so re-importing a cookie file does not erase the identity `--account` resolves by.
- `Account.Usable()` holds the usability rule, and `Remove` uses it; `account.Usable` (Task 4) delegates.
- `Normalize` turns a nil slice into an empty one, so an empty pool is `[]`, not `null`.
- `Remove` clamps `RoundRobinIndex`, which is a slice index.
- `LoadAccounts` rejects a file whose `version` is newer than this build supports.
- A `TestSaveAccountsWritesStableJSON` pins the JSON field names and the defaults, and the `time.Time` tags use `omitzero`.

---

### Task 3: Quota reset arithmetic

**Files:**
- Create: `internal/account/quota.go`
- Test: `internal/account/quota_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/account/quota_test.go`. It uses `time.FixedZone`, not a tzdata lookup, so it runs in minimal containers:

```go
package account

import (
	"testing"
	"time"

	"github.com/heartleo/zlib/internal/config"
)

func utc8() *time.Location { return time.FixedZone("UTC+8", 8*60*60) }

func TestApplyResetRollsOverAtTheConfiguredTime(t *testing.T) {
	loc := utc8()
	reset := time.Date(2026, 11, 3, 5, 0, 0, 0, loc)
	quota := config.Quota{Known: true, Allowed: 10, Remaining: 2, ResetAt: reset}

	before := time.Date(2026, 11, 3, 4, 59, 0, 0, loc)
	if got := ApplyReset(quota, "05:00", before); got.Remaining != 2 {
		t.Fatalf("before reset remaining = %d, want 2", got.Remaining)
	}

	after := time.Date(2026, 11, 3, 5, 1, 0, 0, loc)
	got := ApplyReset(quota, "05:00", after)
	if got.Remaining != 10 {
		t.Fatalf("after reset remaining = %d, want 10", got.Remaining)
	}
	if want := reset.AddDate(0, 0, 1); !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
	}
}

func TestApplyResetSkipsSeveralDays(t *testing.T) {
	loc := utc8()
	quota := config.Quota{Known: true, Allowed: 5, Remaining: 0, ResetAt: time.Date(2026, 11, 1, 5, 0, 0, 0, loc)}
	now := time.Date(2026, 11, 4, 6, 0, 0, 0, loc)
	got := ApplyReset(quota, "05:00", now)
	if got.Remaining != 5 {
		t.Fatalf("remaining = %d, want 5", got.Remaining)
	}
	if !got.ResetAt.After(now) {
		t.Fatalf("reset at = %v, want in the future", got.ResetAt)
	}
}

func TestApplyResetLeavesUnknownQuotaAlone(t *testing.T) {
	if got := ApplyReset(config.Quota{Known: false}, "05:00", time.Now()); got.Known {
		t.Fatalf("quota = %+v, want still unknown", got)
	}
}

func TestApplyResetSeedsResetAtWhenMissing(t *testing.T) {
	loc := utc8()
	now := time.Date(2026, 11, 2, 16, 55, 0, 0, loc)
	got := ApplyReset(config.Quota{Known: true, Allowed: 10, Remaining: 7}, "05:00", now)
	want := time.Date(2026, 11, 3, 5, 0, 0, 0, loc)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
	}
}

func TestApplyResetWithUnparseableTimeIsInert(t *testing.T) {
	quota := config.Quota{Known: true, Allowed: 10, Remaining: 0, ResetAt: time.Now().Add(-time.Hour)}
	if got := ApplyReset(quota, "later", time.Now()); got.Remaining != 0 {
		t.Fatalf("remaining = %d, want unchanged", got.Remaining)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/account -run TestApplyReset -v`
Expected: FAIL, package has no `ApplyReset`.

- [ ] **Step 3: Write the implementation**

Create `internal/account/quota.go`:

```go
package account

import (
	"strings"
	"time"

	"github.com/heartleo/zlib/internal/config"
)

// ApplyReset advances a cached quota record to now, rolling the daily counter
// over at the configured local wall-clock time. An unknown quota is left
// unknown: a missing number is not the same as a full or an empty allowance.
func ApplyReset(q config.Quota, resetTime string, now time.Time) config.Quota {
	if !q.Known {
		return q
	}
	hour, minute, ok := parseResetTime(resetTime)
	if !ok {
		return q
	}
	if q.ResetAt.IsZero() {
		q.ResetAt = nextReset(now, hour, minute)
		return q
	}
	if now.Before(q.ResetAt) {
		return q
	}
	if q.Allowed <= 0 {
		// The only thing we knew was "no downloads left". After the reset the
		// allowance is unknown again, which makes the account usable and
		// refreshable instead of keeping it out of the pool forever.
		return config.Quota{ResetAt: nextReset(now, hour, minute)}
	}
	q.Remaining = q.Allowed
	// Re-anchor to the configured wall-clock time in now's location instead of
	// stepping days from a stored instant: the stored offset may be stale, and
	// a far-past value must not be walked day by day.
	q.ResetAt = nextReset(now, hour, minute)
	return q
}

// NextReset returns the next occurrence of the configured reset time.
func NextReset(resetTime string, now time.Time) (time.Time, bool) {
	hour, minute, ok := parseResetTime(resetTime)
	if !ok {
		return time.Time{}, false
	}
	return nextReset(now, hour, minute), true
}

func nextReset(now time.Time, hour, minute int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func parseResetTime(raw string) (int, int, bool) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(raw))
	if err != nil {
		return 0, 0, false
	}
	return parsed.Hour(), parsed.Minute(), true
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/account -run TestApplyReset -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/account/quota.go internal/account/quota_test.go
git commit -m ":sparkles: (roll the daily quota over at the reset time)"
```

---

### Task 4: Balanced selection

**Files:**
- Create: `internal/account/strategy.go`
- Test: `internal/account/strategy_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/account/strategy_test.go`:

```go
package account

import (
	"testing"
	"time"

	"github.com/heartleo/zlib/internal/config"
)

func acct(id string, remaining, allowed int, lastUsed time.Time) config.Account {
	return config.Account{
		UserID:     id,
		Email:      id + "@example.com",
		Quota:      config.Quota{Known: true, Allowed: allowed, Remaining: remaining},
		LastUsedAt: lastUsed,
	}
}

func TestUsableSkipsExhaustedAndStaleSessions(t *testing.T) {
	cases := map[string]struct {
		acct config.Account
		want bool
	}{
		"has quota":     {acct("a", 1, 10, time.Time{}), true},
		"empty":         {acct("a", 0, 10, time.Time{}), false},
		"unknown quota": {config.Account{UserID: "a"}, true},
		"needs relogin": {config.Account{UserID: "a", NeedsRelogin: true}, false},
	}
	for name, tc := range cases {
		if got := Usable(tc.acct); got != tc.want {
			t.Fatalf("%s: Usable() = %v, want %v", name, got, tc.want)
		}
	}
}

func TestRankBalancedPrefersTheHighestRatio(t *testing.T) {
	now := time.Now()
	ranked := RankBalanced([]config.Account{
		acct("small", 2, 5, now), // 0.4
		acct("big", 10, 20, now), // 0.5
		acct("empty", 0, 5, now), // 0.0
	})
	if ranked[0].UserID != "big" || ranked[1].UserID != "small" || ranked[2].UserID != "empty" {
		t.Fatalf("order = %v", []string{ranked[0].UserID, ranked[1].UserID, ranked[2].UserID})
	}
}

func TestRankBalancedBreaksTiesByCountThenIdleTime(t *testing.T) {
	now := time.Now()
	ranked := RankBalanced([]config.Account{
		acct("more", 5, 10, now),
		acct("fewer", 2, 4, now),
		acct("idle", 2, 4, now.Add(-time.Hour)),
	})
	if ranked[0].UserID != "more" {
		t.Fatalf("first = %s, want more", ranked[0].UserID)
	}
	if ranked[1].UserID != "idle" {
		t.Fatalf("second = %s, want the least recently used", ranked[1].UserID)
	}
}

func TestRankBalancedPutsUnknownQuotaLast(t *testing.T) {
	ranked := RankBalanced([]config.Account{
		{UserID: "unknown"},
		acct("known", 1, 10, time.Time{}),
	})
	if ranked[0].UserID != "known" {
		t.Fatalf("first = %s, want known", ranked[0].UserID)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/account -run 'TestUsable|TestRankBalanced' -v`
Expected: FAIL, `undefined: Usable`.

- [ ] **Step 3: Write the implementation**

Create `internal/account/strategy.go`:

```go
package account

import (
	"sort"

	"github.com/heartleo/zlib/internal/config"
)

// Usable reports whether selection may spend this account's quota. The rule
// lives on config.Account so that config.Remove and the manager cannot drift
// apart; this wrapper only keeps call sites in this package short.
func Usable(acct config.Account) bool {
	return acct.Usable()
}

// RankBalanced orders accounts so the highest remaining ratio comes first,
// then the highest absolute remainder, then the least recently used. The ratio
// is what keeps a 5/day and a 20/day account in step instead of draining one.
func RankBalanced(accounts []config.Account) []config.Account {
	ranked := append([]config.Account(nil), accounts...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Quota.Known != b.Quota.Known {
			return a.Quota.Known
		}
		if ra, rb := ratio(a), ratio(b); ra != rb {
			return ra > rb
		}
		if a.Quota.Remaining != b.Quota.Remaining {
			return a.Quota.Remaining > b.Quota.Remaining
		}
		if !a.LastUsedAt.Equal(b.LastUsedAt) {
			return a.LastUsedAt.Before(b.LastUsedAt)
		}
		return a.Name() < b.Name()
	})
	return ranked
}

func ratio(acct config.Account) float64 {
	if !acct.Quota.Known || acct.Quota.Allowed <= 0 {
		return 0
	}
	return float64(acct.Quota.Remaining) / float64(acct.Quota.Allowed)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/account -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/account/strategy.go internal/account/strategy_test.go
git commit -m ":sparkles: (balanced account selection)"
```

---

### Task 5: Manager, failover loop and sync

> The code blocks in this task predate implementation. Where they disagree with
> the "Amendments made during implementation" list at the end of the task, the
> amendments and the committed code win.

**Files:**
- Create: `internal/account/manager.go`
- Test: `internal/account/manager_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/account/manager_test.go`:

```go
package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
)

// fakeSession answers the three calls the manager makes, per account.
type fakeSession struct {
	fetchErr error
	dlErr    error
	profile  zlib.UserProfile
	lastURL  string
}

func (f *fakeSession) FetchBookContext(ctx context.Context, ref string) (zlib.Book, error) {
	if f.fetchErr != nil {
		return zlib.Book{}, f.fetchErr
	}
	return zlib.Book{Name: "Book " + ref, DownloadURL: "https://example.com/eapi/book/1/abc/file"}, nil
}

func (f *fakeSession) DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error) {
	f.lastURL = url
	if f.dlErr != nil {
		return zlib.DownloadResult{}, f.dlErr
	}
	if progress != nil {
		progress(10, 10)
	}
	return zlib.DownloadResult{FilePath: dir + "/book.epub", Size: 10}, nil
}

func (f *fakeSession) GetProfileContext(ctx context.Context) (zlib.UserProfile, error) {
	return f.profile, nil
}

func newTestManager(t *testing.T, pool []config.Account, sessions map[string]*fakeSession) (*Manager, *[]config.Accounts) {
	t.Helper()
	saved := []config.Accounts{}
	data := config.Accounts{Accounts: pool}
	data.Normalize()
	m := NewManager(data, func() time.Time {
		return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	})
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		session, ok := sessions[acct.UserID]
		if !ok {
			t.Fatalf("no fake session for %s", acct.UserID)
		}
		return session, nil
	})
	m.SetPersist(func(a config.Accounts) error {
		saved = append(saved, a)
		return nil
	})
	return m, &saved
}

func TestDownloadRotatesOnQuotaRejection(t *testing.T) {
	// The failing account must be the one balanced picks first: highest ratio.
	first := &fakeSession{dlErr: zlib.ErrDailyLimitReached}
	second := &fakeSession{}
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
		{UserID: "2", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 5}},
	}, map[string]*fakeSession{"1": first, "2": second})

	var attempts []int
	var reasons []error
	res, err := m.Download(context.Background(), "1:abc", Options{
		Dir: "/tmp",
		OnAttempt: func(acct config.Account, attempt int, reason error) {
			attempts = append(attempts, attempt)
			reasons = append(reasons, reason)
		},
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if res.Account.UserID != "2" || res.Attempts != 2 {
		t.Fatalf("result = %+v", res)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v", attempts)
	}
	if reasons[0] != nil || !errors.Is(reasons[1], zlib.ErrDailyLimitReached) {
		t.Fatalf("reasons = %v, want nil then ErrDailyLimitReached", reasons)
	}
	last := (*saved)[len(*saved)-1]
	if last.Accounts[0].Quota.Remaining != 0 {
		t.Fatalf("exhausted account remaining = %d, want 0", last.Accounts[0].Quota.Remaining)
	}
	if last.Accounts[1].Quota.Remaining != 4 {
		t.Fatalf("used account remaining = %d, want 4", last.Accounts[1].Quota.Remaining)
	}
}

func TestDownloadDoesNotRotateOnNetworkError(t *testing.T) {
	networkErr := errors.New("connection reset")
	first := &fakeSession{dlErr: networkErr}
	second := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
		{UserID: "2", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": first, "2": second})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if !errors.Is(err, networkErr) {
		t.Fatalf("error = %v, want the network error", err)
	}
}

func TestDownloadRotatesOnExpiredSessionDuringFetch(t *testing.T) {
	first := &fakeSession{fetchErr: zlib.ErrSessionExpired}
	second := &fakeSession{}
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
		{UserID: "2", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": first, "2": second})

	res, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if res.Account.UserID != "2" {
		t.Fatalf("account = %s", res.Account.UserID)
	}
	last := (*saved)[len(*saved)-1]
	if !last.Accounts[0].NeedsRelogin {
		t.Fatal("expired account not marked")
	}
}

func TestDownloadReportsAllAccountsExhausted(t *testing.T) {
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 0}},
		{UserID: "2", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 0}},
	}, map[string]*fakeSession{})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	var exhausted *AllExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("error = %v, want AllExhaustedError", err)
	}
	if exhausted.NextReset.IsZero() {
		t.Fatal("next reset is zero")
	}
}

func TestPinnedAccountBypassesTheEmptyGate(t *testing.T) {
	only := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 0}},
	}, map[string]*fakeSession{"1": only})

	if _, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp", Account: "one@example.com"}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
}

func TestPinnedQuotaRejectionIsReportedAsSuch(t *testing.T) {
	only := &fakeSession{dlErr: zlib.ErrDailyLimitReached}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 4}},
	}, map[string]*fakeSession{"1": only})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp", Account: "one@example.com"})
	if !errors.Is(err, zlib.ErrDailyLimitReached) {
		t.Fatalf("error = %v, want ErrDailyLimitReached, not an all-exhausted error", err)
	}
}

func TestRoundRobinAdvancesAcrossDownloads(t *testing.T) {
	first := &fakeSession{}
	second := &fakeSession{}
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
		{UserID: "2", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
	}, map[string]*fakeSession{"1": first, "2": second})
	m.data.Strategy = config.StrategyRoundRobin

	opts := Options{Dir: "/tmp", Strategy: config.StrategyRoundRobin}
	firstRun, err := m.Download(context.Background(), "1:abc", opts)
	if err != nil {
		t.Fatal(err)
	}
	secondRun, err := m.Download(context.Background(), "1:abc", opts)
	if err != nil {
		t.Fatal(err)
	}
	if firstRun.Account.UserID == secondRun.Account.UserID {
		t.Fatalf("both downloads used account %s", firstRun.Account.UserID)
	}
	last := (*saved)[len(*saved)-1]
	if last.RoundRobinIndex != 0 {
		t.Fatalf("next index = %d, want 0 (wrapped after account 2)", last.RoundRobinIndex)
	}
}

func TestDownloadRewritesHostForRawURL(t *testing.T) {
	session := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://mirror.example.com"},
	}, map[string]*fakeSession{"1": session})

	if _, err := m.Download(context.Background(), "", Options{
		Dir: "/tmp",
		URL: "https://other.example.com/eapi/book/1/abc/file",
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if session.lastURL != "https://mirror.example.com/eapi/book/1/abc/file" {
		t.Fatalf("url = %s", session.lastURL)
	}
}

func TestStaleQuotaRefreshFailureStillDownloads(t *testing.T) {
	stale := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Quota: config.Quota{
			Known: true, Allowed: 10, Remaining: 3,
			ObservedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
			ResetAt:    time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC),
		}},
	}, map[string]*fakeSession{"1": stale})
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		return &failingProfileSession{inner: stale}, nil
	})

	var warned bool
	if _, err := m.Download(context.Background(), "1:abc", Options{
		Dir:    "/tmp",
		OnWarn: func(error) { warned = true },
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if !warned {
		t.Fatal("expected a warning about the failed refresh")
	}
}

// A Sync must leave ResetAt in the future. Otherwise the observation it just
// fetched is discarded by the next rollover, which trusts a stale reset point.
func TestSyncReanchorsResetAt(t *testing.T) {
	session := &fakeSession{profile: zlib.UserProfile{
		QuotaKnown:    true,
		DownloadLimit: zlib.DownloadLimit{DailyAllowed: 10, DailyRemaining: 7},
	}}
	m, saved := newTestManager(t, []config.Account{{
		UserID: "1", Domain: "https://example.com",
		Quota: config.Quota{
			Known: true, Allowed: 10, Remaining: 0,
			ResetAt: time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC),
		},
	}}, map[string]*fakeSession{"1": session})

	if _, err := m.Sync(context.Background(), nil); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	last := (*saved)[len(*saved)-1]
	quota := last.Accounts[0].Quota
	if quota.Remaining != 7 {
		t.Fatalf("remaining = %d, want the observed 7", quota.Remaining)
	}
	if !quota.ResetAt.After(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset at = %v, want in the future", quota.ResetAt)
	}
}

type failingProfileSession struct{ inner *fakeSession }

func (f *failingProfileSession) FetchBookContext(ctx context.Context, ref string) (zlib.Book, error) {
	return f.inner.FetchBookContext(ctx, ref)
}

func (f *failingProfileSession) DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error) {
	return f.inner.DownloadWithContext(ctx, url, dir, progress)
}

func (f *failingProfileSession) GetProfileContext(ctx context.Context) (zlib.UserProfile, error) {
	return zlib.UserProfile{}, errors.New("profile unavailable")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/account -run TestDownload -v`
Expected: FAIL, `undefined: NewManager`.

- [ ] **Step 3: Write the implementation**

Create `internal/account/manager.go`:

```go
package account

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
)

// DefaultQuotaTTL is how old a cached quota may be before one account is
// refreshed from the server. Only the account about to be used is refreshed.
const DefaultQuotaTTL = 12 * time.Hour

// Session is the part of *zlib.Client the manager drives.
type Session interface {
	FetchBookContext(ctx context.Context, ref string) (zlib.Book, error)
	DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error)
	GetProfileContext(ctx context.Context) (zlib.UserProfile, error)
}

// Result describes a completed download and who served it.
type Result struct {
	FilePath string
	Size     int64
	BookName string
	Account  config.Account
	Attempts int
}

// Options configures one download.
type Options struct {
	Dir       string
	Account   string // pin to one account; disables rotation
	Strategy  string // override the stored strategy for this run
	URL       string // download a known URL instead of fetching a reference
	Progress  func(written, total int64)
	OnBook    func(name string)
	OnAttempt func(acct config.Account, attempt int, reason error)
	OnWarn    func(err error)
}

// AllExhaustedError reports that no account can download today.
type AllExhaustedError struct {
	NextReset time.Time
}

func (e *AllExhaustedError) Error() string {
	if e.NextReset.IsZero() {
		return "all accounts have used today's download quota"
	}
	return fmt.Sprintf("all accounts have used today's download quota; next reset at %s", e.NextReset.Format("2006-01-02 15:04"))
}

// Manager owns the account pool for one process.
type Manager struct {
	mu             sync.Mutex
	data           config.Accounts
	now            func() time.Time
	ttl            time.Duration
	newSession     func(config.Account) (Session, error)
	persist        func(config.Accounts) error
	onPersistError func(error)
	clientOptions  []zlib.ClientOption
}

// NewManager builds a manager over already-loaded data. Production code uses
// Open; tests use this to inject a clock and a session factory.
func NewManager(data config.Accounts, now func() time.Time) *Manager {
	data.Normalize()
	if now == nil {
		now = time.Now
	}
	m := &Manager{data: data, now: now, ttl: DefaultQuotaTTL, persist: config.SaveAccounts}
	m.newSession = m.defaultSession
	return m
}

// Open loads the pool from disk.
func Open() (*Manager, error) {
	data, err := config.LoadAccounts()
	if err != nil {
		return nil, err
	}
	return NewManager(data, time.Now), nil
}

func (m *Manager) SetSessionFactory(factory func(config.Account) (Session, error)) {
	if factory != nil {
		m.newSession = factory
	}
}

func (m *Manager) SetPersist(persist func(config.Accounts) error) {
	if persist != nil {
		m.persist = persist
	}
}

func (m *Manager) SetQuotaTTL(ttl time.Duration) { m.ttl = ttl }

func (m *Manager) SetPersistErrorHandler(handler func(error)) { m.onPersistError = handler }

// SetClientOptions applies zlib client options to every session the manager
// builds. The MCP server uses it to share one connection pool.
func (m *Manager) SetClientOptions(opts ...zlib.ClientOption) {
	m.clientOptions = append([]zlib.ClientOption(nil), opts...)
}

// defaultSession builds a real client for one account. ZLIB_DOMAIN still wins
// over the domain stored with the account, as it does for every other command.
func (m *Manager) defaultSession(acct config.Account) (Session, error) {
	domain := acct.Domain
	if env := strings.TrimSpace(os.Getenv(zlib.EnvDomain)); env != "" {
		domain = env
	}
	if strings.TrimSpace(domain) == "" {
		return nil, errors.New("account has no domain; log in again")
	}
	allowed, err := zlib.ParseAllowedDomain(domain)
	if err != nil {
		return nil, fmt.Errorf("invalid account domain: %w", err)
	}
	c := zlib.NewClient(m.clientOptions...)
	c.SetDomain(allowed)
	c.SetCookies(acct.Cookies)
	return c, nil
}

// Snapshot returns a copy of the pool for listing.
func (m *Manager) Snapshot() config.Accounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyResetsLocked()
	out := m.data
	out.Accounts = append([]config.Account(nil), m.data.Accounts...)
	return out
}

// Default returns the account identity-sensitive commands use.
func (m *Manager) Default() (config.Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyResetsLocked()
	return m.defaultLocked()
}

func (m *Manager) defaultLocked() (config.Account, bool) {
	if m.data.DefaultAccount != "" {
		if acct, err := m.data.Resolve(m.data.DefaultAccount); err == nil {
			return acct, true
		}
	}
	if len(m.data.Accounts) > 0 {
		return m.data.Accounts[0], true
	}
	return config.Account{}, false
}

// Download runs the failover loop: pick an account, fetch and download with
// it, and move to the next one only when the quota or the session is rejected.
func (m *Manager) Download(ctx context.Context, ref string, opts Options) (Result, error) {
	if strings.TrimSpace(ref) == "" && strings.TrimSpace(opts.URL) == "" {
		return Result{}, errors.New("account: no book reference or URL to download")
	}
	m.mu.Lock()
	m.applyResetsLocked()
	strategy := m.strategyLocked(opts.Strategy)
	count := len(m.data.Accounts)
	m.mu.Unlock()

	excluded := make(map[string]bool, count)
	var reason error
	for attempt := 1; ; attempt++ {
		m.mu.Lock()
		acct, err := m.pickLocked(strategy, opts.Account, excluded)
		m.mu.Unlock()
		if err != nil {
			return Result{}, err
		}
		if opts.OnAttempt != nil {
			opts.OnAttempt(acct, attempt, reason)
		}
		result, err := m.attempt(ctx, acct, ref, opts)
		if err == nil {
			m.recordSuccess(acct.UserID, strategy)
			result.Account = acct
			result.Attempts = attempt
			return result, nil
		}
		switch {
		case errors.Is(err, zlib.ErrDailyLimitReached):
			m.markExhausted(acct.UserID)
			// A pinned account has no fallback: report the quota rejection
			// itself instead of claiming the whole pool is empty.
			if opts.Account != "" {
				return Result{}, err
			}
			excluded[acct.UserID] = true
			reason = err
		case errors.Is(err, zlib.ErrSessionExpired):
			m.markNeedsRelogin(acct.UserID)
			excluded[acct.UserID] = true
			reason = err
		default:
			return Result{}, err
		}
	}
}

// attempt performs one account's fetch and download.
func (m *Manager) attempt(ctx context.Context, acct config.Account, ref string, opts Options) (Result, error) {
	session, err := m.newSession(acct)
	if err != nil {
		return Result{}, err
	}
	m.refreshIfStale(ctx, session, acct.UserID, opts.OnWarn)

	rawURL := strings.TrimSpace(opts.URL)
	// original is only set for the raw-URL form. For a reference it must stay
	// empty, or the rewrite fallback below would retry with an empty URL.
	original := ""
	name := ""
	if rawURL == "" {
		book, err := session.FetchBookContext(ctx, ref)
		if err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(book.DownloadURL) == "" {
			return Result{}, fmt.Errorf("no download URL available for book %s", ref)
		}
		rawURL, name = book.DownloadURL, book.Name
		if opts.OnBook != nil && name != "" {
			opts.OnBook(name)
		}
	} else {
		original = rawURL
		if rewritten, ok := rewriteHost(rawURL, acct.Domain); ok {
			rawURL = rewritten
		}
	}

	downloaded, err := session.DownloadWithContext(ctx, rawURL, opts.Dir, opts.Progress)
	// A host rewrite is a guess: if the mirror does not serve the path, retry
	// once on the original URL before blaming the account.
	if err != nil && original != "" && rawURL != original && errors.Is(err, zlib.ErrDownloadFailed) {
		downloaded, err = session.DownloadWithContext(ctx, original, opts.Dir, opts.Progress)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{FilePath: downloaded.FilePath, Size: downloaded.Size, BookName: name}, nil
}

// refreshIfStale refreshes one account's quota, best effort: a failing profile
// request must never fail a download that would otherwise work.
func (m *Manager) refreshIfStale(ctx context.Context, session Session, userID string, warn func(error)) {
	m.mu.Lock()
	acct, resolveErr := m.data.Resolve(userID)
	ttl := m.ttl
	now := m.now()
	m.mu.Unlock()
	if resolveErr != nil {
		return
	}
	if !acct.Quota.ObservedAt.IsZero() && now.Sub(acct.Quota.ObservedAt) < ttl {
		return
	}
	profile, err := session.GetProfileContext(ctx)
	if err != nil {
		if warn != nil {
			warn(fmt.Errorf("could not refresh quota for %s: %w", acct.Name(), err))
		}
		return
	}
	if !profile.QuotaKnown {
		// Stamp the attempt even when the server reports no numbers, so a
		// plan that never exposes a quota is not asked on every download.
		m.mu.Lock()
		defer m.mu.Unlock()
		if index, found := m.indexOfLocked(userID); found {
			m.data.Accounts[index].Quota.ObservedAt = m.now()
			m.persistLocked()
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	index, found := m.indexOfLocked(userID)
	if !found {
		return
	}
	quota := &m.data.Accounts[index].Quota
	quota.Known = true
	quota.Allowed = profile.DailyAllowed
	quota.Remaining = profile.DailyRemaining
	quota.ObservedAt = m.now()
	// Re-anchor on every observation, not only when the field is empty: a
	// stale ResetAt from before the last rollover would otherwise let the next
	// applyResetsLocked overwrite the number the server just reported.
	if next, ok := NextReset(m.data.ResetTime, m.now()); ok {
		quota.ResetAt = next
	}
	m.persistLocked()
}

// Sync refreshes the quota of the named accounts, or of all of them when no
// selector is given. A failing account is skipped, not fatal.
func (m *Manager) Sync(ctx context.Context, selectors []string) (int, error) {
	m.mu.Lock()
	targets := make([]config.Account, 0, len(m.data.Accounts))
	if len(selectors) == 0 {
		targets = append(targets, m.data.Accounts...)
	}
	for _, selector := range selectors {
		acct, err := m.data.Resolve(selector)
		if err != nil {
			m.mu.Unlock()
			return 0, err
		}
		targets = append(targets, acct)
	}
	m.mu.Unlock()

	refreshed := 0
	for _, acct := range targets {
		session, err := m.newSession(acct)
		if err != nil {
			continue
		}
		profile, err := session.GetProfileContext(ctx)
		if err != nil || !profile.QuotaKnown {
			continue
		}
		m.mu.Lock()
		if index, ok := m.indexOfLocked(acct.UserID); ok {
			quota := &m.data.Accounts[index].Quota
			quota.Known = true
			quota.Allowed = profile.DailyAllowed
			quota.Remaining = profile.DailyRemaining
			quota.ObservedAt = m.now()
			// Same re-anchor as refreshIfStale: Sync must leave a ResetAt in
			// the future, or the next rollover discards this observation.
			if next, ok := NextReset(m.data.ResetTime, m.now()); ok {
				quota.ResetAt = next
			}
			m.persistLocked()
			refreshed++
		}
		m.mu.Unlock()
	}
	return refreshed, nil
}

func (m *Manager) strategyLocked(override string) string {
	switch strings.TrimSpace(override) {
	case config.StrategyBalanced, config.StrategyRoundRobin:
		return strings.TrimSpace(override)
	}
	return m.data.Strategy
}

func (m *Manager) applyResetsLocked() {
	now := m.now()
	for i := range m.data.Accounts {
		m.data.Accounts[i].Quota = ApplyReset(m.data.Accounts[i].Quota, m.data.ResetTime, now)
	}
}

func (m *Manager) pickLocked(strategy, pin string, excluded map[string]bool) (config.Account, error) {
	if pin != "" {
		acct, err := m.data.Resolve(pin)
		if err != nil {
			return config.Account{}, err
		}
		if excluded[acct.UserID] {
			return config.Account{}, m.exhaustedLocked()
		}
		return acct, nil
	}
	if len(m.data.Accounts) == 0 {
		return config.Account{}, errors.New("no accounts saved; run `zlib login` first")
	}
	if strategy == config.StrategyRoundRobin {
		start := m.data.RoundRobinIndex % len(m.data.Accounts)
		for offset := 0; offset < len(m.data.Accounts); offset++ {
			acct := m.data.Accounts[(start+offset)%len(m.data.Accounts)]
			if excluded[acct.UserID] || !Usable(acct) {
				continue
			}
			return acct, nil
		}
		return config.Account{}, m.exhaustedLocked()
	}
	for _, acct := range RankBalanced(m.data.Accounts) {
		if excluded[acct.UserID] || !Usable(acct) {
			continue
		}
		return acct, nil
	}
	return config.Account{}, m.exhaustedLocked()
}

func (m *Manager) exhaustedLocked() error {
	next, ok := NextReset(m.data.ResetTime, m.now())
	if !ok {
		return &AllExhaustedError{}
	}
	return &AllExhaustedError{NextReset: next}
}

func (m *Manager) recordSuccess(userID, strategy string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index, ok := m.indexOfLocked(userID)
	if !ok {
		return
	}
	acct := &m.data.Accounts[index]
	if acct.Quota.Known && acct.Quota.Remaining > 0 {
		acct.Quota.Remaining--
	}
	acct.Quota.ObservedAt = m.now()
	acct.LastUsedAt = m.now()
	if strategy == config.StrategyRoundRobin {
		// Persist the next account to try, not the one just used: the scan
		// starts at this index, so storing the used account would repeat it.
		m.data.RoundRobinIndex = (index + 1) % len(m.data.Accounts)
	}
	m.persistLocked()
}

func (m *Manager) markExhausted(userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index, ok := m.indexOfLocked(userID)
	if !ok {
		return
	}
	acct := &m.data.Accounts[index]
	acct.Quota.Known = true
	acct.Quota.Remaining = 0
	acct.Quota.ObservedAt = m.now()
	m.persistLocked()
}

func (m *Manager) markNeedsRelogin(userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index, ok := m.indexOfLocked(userID)
	if !ok {
		return
	}
	m.data.Accounts[index].NeedsRelogin = true
	m.persistLocked()
}

func (m *Manager) indexOfLocked(userID string) (int, bool) {
	for i := range m.data.Accounts {
		if m.data.Accounts[i].UserID == userID {
			return i, true
		}
	}
	return 0, false
}

// persistLocked writes the pool. A failed write is reported, never fatal: the
// download it belongs to already succeeded.
func (m *Manager) persistLocked() {
	if err := m.persist(m.data); err != nil && m.onPersistError != nil {
		m.onPersistError(err)
	}
}

// rewriteHost moves a download URL onto another mirror. Only the EAPI file
// path is rewritten; anything else is account-bound and left alone.
func rewriteHost(rawURL, domain string) (string, bool) {
	target, err := url.Parse(rawURL)
	if err != nil || target.Host == "" {
		return rawURL, false
	}
	base, err := url.Parse(domain)
	if err != nil || base.Host == "" {
		return rawURL, false
	}
	if strings.EqualFold(target.Host, base.Host) {
		return rawURL, false
	}
	if !strings.HasPrefix(target.Path, "/eapi/book/") || !strings.HasSuffix(target.Path, "/file") {
		return rawURL, false
	}
	target.Scheme = base.Scheme
	target.Host = base.Host
	return target.String(), true
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/account -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/account/manager.go internal/account/manager_test.go
git commit -m ":sparkles: (rotate accounts on quota rejection)"
```

**Amendments made during implementation — the committed code is authoritative:**

- `Result.Account` has its `Cookies` map nil-ed out: the result reaches the CLI
  and MCP surfaces, and a session key there is one `%+v` away from a leak.
- `Snapshot`, `Default` and `NewManager` deep-copy the `Cookies` maps, so a
  caller cannot mutate live pool state without the mutex.
- `Sync` returns the joined failure when it refreshed nothing and at least one
  account failed, instead of `(0, nil)`.
- Errors that end the loop are wrapped with the account name (`%s: %w`), so a
  failure says whose account hit it while `errors.Is` still works.
- `effectiveDomain` applies the same `ZLIB_DOMAIN`-wins rule as
  `defaultSession`, and the URL rewrite uses it.
- `strategyLocked` returns an error for an unknown `--strategy` override
  instead of silently using the stored strategy.
- `recordSuccess` leaves `ObservedAt` alone when the quota is unknown: that
  field means "the server reported numbers", not "the local counter moved".
- `pickLocked` clamps a negative `RoundRobinIndex`, and the pin branch no
  longer carries an unreachable `excluded` check.
- The pin bypasses both the zero-remaining gate and `NeedsRelogin`, on purpose:
  a pinned account gets one honest attempt.

---

### Task 6: `zlib account` commands

**Files:**
- Create: `internal/cli/account.go`
- Test: `internal/cli/account_test.go`

The command registers itself in `account.go`'s `init()` through
`rootCmd.AddCommand`, the way `theme` and `update` do; no edit to `root.go` is
needed.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/account_test.go`. It reuses `executeRootForTest` (`internal/cli/root_test.go:48`) and `isolateConfigHome` (`internal/cli/theme_test.go:71`):

```go
package cli

import (
	"encoding/json"
	"testing"

	"github.com/heartleo/zlib/internal/config"
)

func seedAccounts(t *testing.T, pool config.Accounts) {
	t.Helper()
	isolateConfigHome(t)
	if err := config.SaveAccounts(pool); err != nil {
		t.Fatal(err)
	}
}

func TestAccountListJSONReportsQuotaAndDefault(t *testing.T) {
	seedAccounts(t, config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 7}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 5, Remaining: 0}},
		},
	})

	out, err := executeRootForTest(t, "account", "list", "--json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var payload struct {
		Strategy string `json:"strategy"`
		Accounts []struct {
			Email     string `json:"email"`
			Remaining int    `json:"daily_remaining"`
			Default   bool   `json:"default"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if payload.Strategy != config.StrategyBalanced || len(payload.Accounts) != 2 {
		t.Fatalf("payload = %+v", payload)
	}
	if !payload.Accounts[0].Default || payload.Accounts[0].Remaining != 7 {
		t.Fatalf("first account = %+v", payload.Accounts[0])
	}
}

func TestAccountUseSwitchesTheDefault(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com"},
		},
	})
	if _, err := executeRootForTest(t, "account", "use", "two@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultAccount != "2" {
		t.Fatalf("default = %q, want 2", got.DefaultAccount)
	}
}

func TestAccountConfigSetsStrategyAndResetTime(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	if _, err := executeRootForTest(t, "account", "config", "--strategy", "round_robin", "--reset-time", "06:30"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if pool.Strategy != config.StrategyRoundRobin || pool.ResetTime != "06:30" {
		t.Fatalf("pool = %+v", pool)
	}
	if _, err := executeRootForTest(t, "account", "config", "--strategy", "nonsense"); err == nil {
		t.Fatal("unknown strategy must be rejected")
	}
}

func TestAccountRemoveRepointsTheDefaultAndTheMirror(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "2", "remix_userkey": "<key>"}},
		},
	})
	if _, err := executeRootForTest(t, "account", "remove", "one@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.DefaultAccount != "2" {
		t.Fatalf("accounts = %+v default = %q", got.Accounts, got.DefaultAccount)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror session missing: %v", err)
	}
	if session.Cookies["remix_userid"] != "2" {
		t.Fatalf("mirror user = %q, want the new default", session.Cookies["remix_userid"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli -run 'TestAccount' -v`
Expected: FAIL, unknown command `account`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/account.go`:

```go
package cli

import (
	"fmt"
	"time"

	"charm.land/lipgloss/v2"
	accounts "github.com/heartleo/zlib/internal/account"
	"github.com/heartleo/zlib/internal/config"
	"github.com/spf13/cobra"
)

var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Manage the saved account pool",
	Long:  "List, refresh, select and remove the Z-Library accounts downloads rotate through.",
}

var accountListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the saved accounts and their quota",
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := accounts.Open()
		if err != nil {
			return err
		}
		pool := manager.Snapshot()
		if jsonFlag(cmd) {
			return printJSONTo(cmd.OutOrStdout(), accountListJSON(pool))
		}
		for _, acct := range pool.Accounts {
			marker := " "
			if acct.UserID == pool.DefaultAccount {
				marker = "*"
			}
			quota := "quota unknown"
			if acct.Quota.Known {
				quota = fmt.Sprintf("%d/%d left", acct.Quota.Remaining, acct.Quota.Allowed)
			}
			extra := ""
			if !acct.Quota.ResetAt.IsZero() {
				extra += " resets " + acct.Quota.ResetAt.Format("2006-01-02 15:04")
			}
			if !acct.LastUsedAt.IsZero() {
				extra += " last used " + acct.LastUsedAt.Format("2006-01-02 15:04")
			}
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s %s  %s  %s%s\n", marker, acct.Name(), acct.Domain, quota, extra)
		}
		return nil
	},
}

var accountSyncCmd = &cobra.Command{
	Use:   "sync [email...]",
	Short: "Refresh the cached quota from the server",
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := accounts.Open()
		if err != nil {
			return err
		}
		refreshed, err := manager.Sync(cmd.Context(), args)
		if err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Refreshed %d account(s).\n", colorGreen(symbolSuccess), refreshed)
		return nil
	},
}

var accountUseCmd = &cobra.Command{
	Use:   "use <account>",
	Short: "Choose the account reads and pinned downloads use",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		acct, err := pool.Resolve(args[0])
		if err != nil {
			return err
		}
		pool.DefaultAccount = acct.UserID
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		if err := config.SaveSession(config.Session{Cookies: acct.Cookies, Domain: acct.Domain, Mode: "eapi"}); err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Default account is now %s.\n", colorGreen(symbolSuccess), acct.Name())
		return nil
	},
}

var accountRemoveCmd = &cobra.Command{
	Use:   "remove <account>",
	Short: "Remove a saved account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		acct, err := pool.Resolve(args[0])
		if err != nil {
			return err
		}
		pool.Remove(acct.UserID)
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		if err := syncSessionMirror(pool); err != nil {
			return err
		}
		if pool.DefaultAccount == "" {
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s Removed %s. No accounts left; run `zlib login`.\n", colorFaint(symbolSuccess), acct.Name())
			return nil
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Removed %s. Default is now %s.\n", colorGreen(symbolSuccess), acct.Name(), pool.DefaultAccount)
		return nil
	},
}

// accountListJSON is the --json shape of `account list`.
type accountListJSONDoc struct {
	Strategy  string               `json:"strategy"`
	ResetTime string               `json:"reset_time"`
	Default   string               `json:"default,omitempty"`
	Accounts  []accountListJSONRow `json:"accounts"`
}

type accountListJSONRow struct {
	Email      string `json:"email,omitempty"`
	Label      string `json:"label,omitempty"`
	UserID     string `json:"user_id"`
	Domain     string `json:"domain"`
	Known      bool   `json:"quota_known"`
	Allowed    int    `json:"daily_allowed"`
	Remaining  int    `json:"daily_remaining"`
	ResetAt    string `json:"reset_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	Default    bool   `json:"default"`
	NeedsLogin bool   `json:"needs_relogin,omitempty"`
}

func accountListJSON(pool config.Accounts) accountListJSONDoc {
	doc := accountListJSONDoc{Strategy: pool.Strategy, ResetTime: pool.ResetTime, Default: pool.DefaultAccount}
	for _, acct := range pool.Accounts {
		row := accountListJSONRow{
			Email:      acct.Email,
			Label:      acct.Label,
			UserID:     acct.UserID,
			Domain:     acct.Domain,
			Known:      acct.Quota.Known,
			Allowed:    acct.Quota.Allowed,
			Remaining:  acct.Quota.Remaining,
			Default:    acct.UserID == pool.DefaultAccount,
			NeedsLogin: acct.NeedsRelogin,
		}
		if !acct.Quota.ResetAt.IsZero() {
			row.ResetAt = acct.Quota.ResetAt.Format(time.RFC3339)
		}
		if !acct.LastUsedAt.IsZero() {
			row.LastUsedAt = acct.LastUsedAt.Format(time.RFC3339)
		}
		doc.Accounts = append(doc.Accounts, row)
	}
	return doc
}

// syncSessionMirror keeps session.json pointing at the current default, so
// commands that still read it (profile, favorites, kindle) do not keep using a
// removed account.
func syncSessionMirror(pool config.Accounts) error {
	if pool.DefaultAccount == "" {
		if err := os.Remove(config.SessionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	acct, err := pool.Resolve(pool.DefaultAccount)
	if err != nil {
		return err
	}
	return config.SaveSession(config.Session{Cookies: acct.Cookies, Domain: acct.Domain, Mode: "eapi"})
}

// saveAccount stores one account in the pool. It never steals the default
// unless asked to or unless the pool was empty; logging into a second account
// is a step towards a pool, not a request to switch identity.
func saveAccount(acct config.Account, use bool) error {
	pool, err := config.LoadAccounts()
	if err != nil {
		return err
	}
	pool.Upsert(acct)
	if use || pool.DefaultAccount == "" {
		pool.DefaultAccount = acct.UserID
	}
	if err := config.SaveAccounts(pool); err != nil {
		return err
	}
	return syncSessionMirror(pool)
}

// accountConfigCmd changes the stored strategy and reset time, so neither is
// reachable only by hand-editing accounts.json.
var accountConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Show or change the selection strategy and reset time",
	RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		strategy, _ := cmd.Flags().GetString("strategy")
		resetTime, _ := cmd.Flags().GetString("reset-time")
		if strategy == "" && resetTime == "" {
			lipgloss.Fprintf(cmd.OutOrStdout(), "strategy: %s\nreset time: %s\n", pool.Strategy, pool.ResetTime)
			return nil
		}
		if strategy != "" {
			switch strategy {
			case config.StrategyBalanced, config.StrategyRoundRobin:
				pool.Strategy = strategy
			default:
				return fmt.Errorf("unknown strategy %q: use %s or %s", strategy, config.StrategyBalanced, config.StrategyRoundRobin)
			}
		}
		if resetTime != "" {
			if _, ok := accounts.NextReset(resetTime, time.Now()); !ok {
				return fmt.Errorf("invalid reset time %q: use HH:MM", resetTime)
			}
			pool.ResetTime = resetTime
		}
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s strategy: %s, reset time: %s\n", colorGreen(symbolSuccess), pool.Strategy, pool.ResetTime)
		return nil
	},
}

func init() {
	accountListCmd.Flags().Bool("json", false, "Print accounts as JSON")
	accountConfigCmd.Flags().String("strategy", "", "balanced or round_robin")
	accountConfigCmd.Flags().String("reset-time", "", "Local wall-clock reset time, HH:MM")
	accountCmd.AddCommand(accountListCmd, accountSyncCmd, accountUseCmd, accountRemoveCmd, accountConfigCmd)
	rootCmd.AddCommand(accountCmd)
}
```

Add `"errors"` and `"os"` to the imports. `jsonFlag` must exist in the package: if `grep -rn "func jsonFlag" internal/cli` finds nothing, add:

```go
func jsonFlag(cmd *cobra.Command) bool {
	flag := cmd.Flags().Lookup("json")
	if flag == nil {
		return false
	}
	value, _ := cmd.Flags().GetBool("json")
	return value
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli -run 'TestAccount' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/account.go internal/cli/account_test.go
git commit -m ":sparkles: (manage the account pool)"
```

**Amendments made during implementation — the committed code is authoritative:**

- `config.Accounts.Remove` re-points the default whenever it is empty or no
  longer resolves, not only when the removed account was the default.
- The default rule lives in `config.Accounts.EnsureDefault` and
  `config.Accounts.DefaultResolves`, so `Remove`, `RemoveAt` and `saveAccount`
  cannot drift apart. `EnsureDefault` orders candidates: an entry with a user
  id and a usable quota first, then any entry with a user id. An id-less entry
  cannot be named, so a pool made only of those has no default.
- `saveAccount` repairs a dangling or empty default *before* `SaveAccounts`
  writes, refuses an account without a `user_id`, and no longer steals the
  default; `Upsert` matches on the user id, so re-logging in would append
  rather than repair an id-less entry.
- Removal goes through `config.Accounts.ResolveIndex` and `RemoveAt`;
  `Remove(userID)` stays as a wrapper. Two entries can share an empty user id
  in a hand-edited file, and deleting by id drops the entry the selector did
  not name.
- `account remove` branches on the pool being empty for the "no accounts left"
  message and lets `syncSessionMirror` decide the mirror. The mirror follows a
  resolvable default, otherwise the first entry with cookies — profile,
  favorites and kindle only need cookies and a domain — and is deleted only
  when no entry can back it.
- `account use` accepts only an account with a user id; when the entry has
  none, the error names the removal-then-login path, because logging in again
  with the current one would append a duplicate.
- `account list` and `account config` take `cobra.NoArgs`; an empty pool prints
  "No accounts saved" for both `list` and `sync`; the human `list` line shows
  `needs re-login`; `use`, `remove` and `saveAccount` share `syncSessionMirror`
  and `zlib.ClientModeEAPI`.
- The strategy whitelist is `config.ValidStrategy`, shared by `Normalize`, the
  manager and `account config`.
- `account list --json` initialises the row slice so an empty pool marshals as
  `[]`, matching `accounts.json`.
- `jsonFlag` lives in `account.go`; it did not exist before this task.
- `executeRootForTest` drops cobra's cached `completion` command before each
  execution, because cobra binds that command to the writer current at the
  first `ExecuteC` and every later test then reads the first test's buffer. It
  also resets every flag in the tree, because cobra keeps flag values on the
  shared commands and `--json` or `--strategy` from one test otherwise decided
  what `-shuffle=on` fed the next one.

---

### Task 7: Login and logout write through the pool

**Files:**
- Modify: `internal/cli/login.go:19-105,201-208`
- Modify: `internal/cli/cookies.go:129-167`
- Modify: `internal/cli/logout.go:13-42`
- Test: `internal/cli/account_test.go`
- Test: `internal/cli/cookies_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/account_test.go`:

```go
func TestSaveAccountUpsertsByUserIDAndKeepsQuota(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 3}},
		},
	})
	if err := saveAccount(config.Account{UserID: "1", Email: "one@example.com", Domain: "https://mirror.example.com"}, false); err != nil {
		t.Fatalf("saveAccount() error = %v", err)
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("accounts = %+v", pool.Accounts)
	}
	if pool.Accounts[0].Quota.Remaining != 3 || pool.Accounts[0].Domain != "https://mirror.example.com" {
		t.Fatalf("account = %+v", pool.Accounts[0])
	}
}

func TestSaveAccountWithUseTakesTheDefault(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	if err := saveAccount(config.Account{UserID: "2", Email: "two@example.com", Domain: "https://example.com"}, true); err != nil {
		t.Fatalf("saveAccount() error = %v", err)
	}
	pool, _ := config.LoadAccounts()
	if pool.DefaultAccount != "2" {
		t.Fatalf("default = %q, want 2", pool.DefaultAccount)
	}
}

func TestSaveAccountKeepsTheDefaultForANewAccount(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	if err := saveAccount(config.Account{UserID: "2", Email: "two@example.com", Domain: "https://example.com"}, false); err != nil {
		t.Fatalf("saveAccount() error = %v", err)
	}
	pool, _ := config.LoadAccounts()
	if pool.DefaultAccount != "1" {
		t.Fatalf("default = %q, want the existing 1", pool.DefaultAccount)
	}
}

func TestLogoutAllClearsBothFiles(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"}}},
	})
	if err := syncSessionMirror(mustLoad(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRootForTest(t, "logout", "--all"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pool, _ := config.LoadAccounts()
	if len(pool.Accounts) != 0 || pool.DefaultAccount != "" {
		t.Fatalf("pool = %+v", pool)
	}
	if _, err := config.LoadSession(); err == nil {
		t.Fatal("session.json survived logout --all")
	}
}

func mustLoad(t *testing.T) config.Accounts {
	t.Helper()
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli -run 'TestSaveAccount|TestLogoutAll' -v`
Expected: FAIL, `undefined: saveAccount`.

- [ ] **Step 3: Wire `login`**

In `internal/cli/login.go`:

1. Add flags in `init()`:

```go
	loginCmd.Flags().String("label", "", "Display name for the account (optional)")
	loginCmd.Flags().Bool("use", false, "Make the account just logged in the default")
```

2. Replace the password-login save block (`login.go:90-96`) with the snippet
   below, and delete the now-unused `mode := string(zlib.ClientModeEAPI)`
   declaration at `login.go:32`: neither the import call nor the save uses it
   any more.

```go
		userID := strings.TrimSpace(c.Cookies()["remix_userid"])
		if userID == "" {
			return fmt.Errorf("login response carried no account id")
		}
		label, _ := cmd.Flags().GetString("label")
		use, _ := cmd.Flags().GetBool("use")
		if err := saveAccount(config.Account{
			UserID:  userID,
			Email:   strings.TrimSpace(email),
			Label:   strings.TrimSpace(label),
			Domain:  c.Domain(),
			Cookies: c.Cookies(),
		}, use); err != nil {
			return fmt.Errorf("failed to save session: %w", err)
		}
```

3. Change `importCookieSession` (`internal/cli/cookies.go:129`) to return the cookies instead of saving a session:

```go
func importCookieSession(cookieFile, domain string, now time.Time, modes ...string) (map[string]string, int, error) {
```

Return `cookies, len(cookies), nil` on success and drop the `config.SaveSession` call and the `mode` parameter. Update the single caller in `login.go`:

```go
			cookies, count, err := importCookieSession(cookieFile, domain, time.Now())
			if err != nil {
				return fmt.Errorf("failed to import cookies: %w", err)
			}
			label, _ := cmd.Flags().GetString("label")
			use, _ := cmd.Flags().GetBool("use")
			if err := saveAccount(config.Account{
				UserID:  strings.TrimSpace(cookies["remix_userid"]),
				Label:   strings.TrimSpace(label),
				Domain:  domain,
				Cookies: cookies,
			}, use); err != nil {
				return fmt.Errorf("failed to save session: %w", err)
			}
			lipgloss.Fprintf(os.Stdout, "%s Imported %d cookies.\n", colorGreen(symbolSuccess), count)
```

No `--label` requirement is needed: `importCookieSession` already rejects a file without `remix_userid`/`remix_userkey` (`internal/cli/cookies.go:152-156`), so every imported account has an id. `--label` is display-only.

Drop the `config` import from `internal/cli/cookies.go:12` in the same step: once
the function no longer calls `config.SaveSession`, nothing in the file uses it.

4. Update `internal/cli/cookies_test.go` for the new signature, or the package
   stops compiling: four call sites use the old two-value form (`:76`, `:104`,
   `:119`, `:132`), and two tests assert that a session file was written
   (`:84-94`, `:135-140`). Change the call sites to
   `cookies, count, err := importCookieSession(path, domain, now)` and move the
   "session was saved" assertions to a call of `saveAccount` followed by
   `config.LoadSession()`.

- [ ] **Step 4: Wire `logout`**

Replace `internal/cli/logout.go` with:

```go
package cli

import (
	"errors"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/heartleo/zlib/internal/config"
	"github.com/spf13/cobra"
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove saved accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		selectorFlag, _ := cmd.Flags().GetString("account")
		all, _ := cmd.Flags().GetBool("all")
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		if len(pool.Accounts) == 0 {
			lipgloss.Fprintf(cmd.OutOrStdout(), "Not logged in.\n")
			return nil
		}
		removed := 0
		if all {
			removed = len(pool.Accounts)
			pool.Accounts = nil
			pool.DefaultAccount = ""
		} else {
			// A bare `zlib logout` means the default account, as it always has.
			selector := strings.TrimSpace(selectorFlag)
			if selector == "" {
				selector = pool.DefaultAccount
			}
			acct, err := pool.Resolve(selector)
			if err != nil {
				return err
			}
			pool.Remove(acct.UserID)
			removed = 1
		}
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		if err := syncSessionMirror(pool); err != nil {
			return err
		}
		if pool.DefaultAccount == "" {
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s Logged out.\n", colorFaint(symbolSuccess))
			return nil
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Logged out %d account(s). Default is %s.\n", colorGreen(symbolSuccess), removed, pool.DefaultAccount)
		return nil
	},
}

// clearSavedSession removes every saved account and the session mirror.
func clearSavedSession() (bool, error) {
	pool, err := config.LoadAccounts()
	if err != nil {
		return false, err
	}
	had := len(pool.Accounts) > 0
	pool.Accounts = nil
	pool.DefaultAccount = ""
	if err := config.SaveAccounts(pool); err != nil {
		return had, err
	}
	err = os.Remove(config.SessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return had, nil
	}
	return had, err
}

func init() {
	logoutCmd.Flags().String("account", "", "Remove one account by email, label or user id")
	logoutCmd.Flags().Bool("all", false, "Remove every saved account")
}
```

`clearSavedSession` keeps its old signature so the TUI keeps compiling; Task 9 changes what it does.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/cli -run 'TestSaveAccount|TestLogoutAll|TestAccount' -v`
Expected: PASS.

- [ ] **Step 6: Run the package tests**

Run: `go test ./internal/cli ./internal/config`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/login.go internal/cli/logout.go internal/cli/cookies.go internal/cli/account.go internal/cli/account_test.go
git commit -m ":necktie: (login and logout manage the account pool)"
```

---

### Task 8: Downloads go through the manager

**Files:**
- Modify: `internal/cli/download.go:29-45,64-125,141-250,252-300,311-360`
- Modify: `internal/cli/search.go:52`, `history.go:53,60,79,85,197`, `saved.go:50`, `book_feed.go:55`
- Modify: `internal/cli/root.go:50-70` (add the `loadManager` seam)
- Test: `internal/cli/download_test.go`, `internal/cli/tui_smoke_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/download_test.go`:

```go
func TestDownloadModelRotatesAndReportsTheServingAccount(t *testing.T) {
	first := &fakeDownloadSession{err: zlib.ErrDailyLimitReached}
	second := &fakeDownloadSession{}
	manager := newFakeManager(t, map[string]*fakeDownloadSession{"1": first, "2": second})

	model := newDownloadModel("1:abc", t.TempDir(), manager)
	done, err := model.runPlain(io.Discard)
	if err != nil {
		t.Fatalf("runPlain() error = %v", err)
	}
	if done.accountName != "two@example.com" {
		t.Fatalf("account = %q, want the second one", done.accountName)
	}
	if done.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", done.attempts)
	}
}

type fakeDownloadSession struct {
	err error
}

func (f *fakeDownloadSession) FetchBookContext(ctx context.Context, ref string) (zlib.Book, error) {
	return zlib.Book{Name: "Book", DownloadURL: "https://example.com/eapi/book/1/abc/file"}, nil
}

func (f *fakeDownloadSession) DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error) {
	if f.err != nil {
		return zlib.DownloadResult{}, f.err
	}
	if progress != nil {
		progress(10, 10)
	}
	return zlib.DownloadResult{FilePath: filepath.Join(dir, "book.epub"), Size: 10}, nil
}

func (f *fakeDownloadSession) GetProfileContext(ctx context.Context) (zlib.UserProfile, error) {
	return zlib.UserProfile{}, nil
}

// newFakeManager builds a pool whose first balanced pick is the failing
// account, so the test observes exactly one rotation.
func newFakeManager(t *testing.T, sessions map[string]*fakeDownloadSession) *accounts.Manager {
	t.Helper()
	data := config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 5}},
		},
	}
	manager := accounts.NewManager(data, nil)
	manager.SetSessionFactory(func(acct config.Account) (accounts.Session, error) {
		return sessions[acct.UserID], nil
	})
	manager.SetPersist(func(config.Accounts) error { return nil })
	return manager
}
```

Add `accounts "github.com/heartleo/zlib/internal/account"` and `"github.com/heartleo/zlib/internal/config"` to the test file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli -run TestDownloadModelRotates -v`
Expected: FAIL, `newDownloadModel` takes a `*zlib.Client`.

- [ ] **Step 3: Add the injection seam**

In `internal/cli/root.go`, next to `loadClient`:

```go
// loadManager opens the account pool for download commands. It is a variable
// for the same reason loadClient is: tests and the demo recorder substitute a
// fixture-backed manager.
var loadManager = openAccountManager

func openAccountManager() (*accounts.Manager, error) {
	manager, err := accounts.Open()
	if err != nil {
		return nil, err
	}
	manager.SetPersistErrorHandler(func(err error) {
		lipgloss.Fprintf(os.Stderr, "%s could not save account state: %v\n", colorFaint(symbolInfo), err)
	})
	return manager, nil
}
```

Add `accounts "github.com/heartleo/zlib/internal/account"` to `root.go`'s imports, and handle the exhausted error in `formatCLIError`:

```go
	var exhausted *accounts.AllExhaustedError
	if errors.As(err, &exhausted) {
		return prefix + " " + safeText(exhausted.Error()) + ". Run `zlib account list` to see the pool."
	}
```

- [ ] **Step 4: Change the model's collaborator**

In `internal/cli/download.go`:

1. Replace the `client *zlib.Client` field with:

```go
	manager     *accounts.Manager
	accountName string
	attempts    int
	pinAccount  string
	strategy    string
```

2. Constructors take the manager:

```go
func newDownloadModel(id, dir string, manager *accounts.Manager) downloadModel {
	m := downloadModel{id: id, dir: dir, manager: manager}
	m.spinner = spinner.New()
	m.spinner.Spinner = spinner.Dot
	m.spinner.Style = lipgloss.NewStyle().Foreground(currentTheme.Accent)
	m.progress = progress.New(progress.WithDefaultBlend(), progress.WithWidth(40))
	return m
}

func newDirectDownloadModel(bookName, downloadURL, dir string, manager *accounts.Manager) downloadModel {
	m := newDownloadModel("", dir, manager)
	m.bookName = bookName
	m.directURL = downloadURL
	return m
}
```

3. Messages. Replace `fetchDoneMsg` with:

```go
type bookNameMsg struct{ name string }
type attemptMsg struct {
	account string
	attempt int
	reason  error
}
type startedMsg struct {
	ch     chan tea.Msg
	cancel context.CancelFunc
}
type downloadDoneMsg struct {
	path     string
	size     int64
	account  string
	attempts int
}
```

4. One download command, one channel owner. `Init` and the retry path both call it:

```go
func (m downloadModel) Init() tea.Cmd {
	if m.state == stateComplete {
		return nil
	}
	return tea.Batch(m.spinner.Tick, m.startCmd())
}

// startCmd hands the message channel to Update through startedMsg; Update then
// streams from it with waitForMsg. Building the channel inside the command
// keeps the model copy-free while still letting Update own the cancel func.
func (m downloadModel) startCmd() tea.Cmd {
	return func() tea.Msg {
		ch := make(chan tea.Msg, 64)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer close(ch)
			emit := func(msg tea.Msg) {
				select {
				case ch <- msg:
				case <-ctx.Done():
				}
			}
			result, err := m.manager.Download(ctx, m.id, accounts.Options{
				Dir:      m.dir,
				URL:      m.directURL,
				Account:  m.pinAccount,
				Strategy: m.strategy,
				Progress: func(written, total int64) { emit(progressMsg{written, total}) },
				OnBook:   func(name string) { emit(bookNameMsg{name}) },
				OnAttempt: func(acct config.Account, attempt int, reason error) {
					emit(attemptMsg{account: acct.Name(), attempt: attempt, reason: reason})
				},
			})
			if err != nil {
				emit(errMsg{err})
				return
			}
			emit(downloadDoneMsg{
				path:     result.FilePath,
				size:     result.Size,
				account:  result.Account.Name(),
				attempts: result.Attempts,
			})
		}()
		return startedMsg{ch: ch, cancel: cancel}
	}
}
```

Delete `fetchCmd` and its `fetchDoneMsg` branch.

5. In `Update`, replace the `fetchDoneMsg` case with:

```go
	case startedMsg:
		m.downloadCh = msg.ch
		m.cancel = msg.cancel
		m.state = stateDownloading
		return m, waitForMsg(msg.ch)

	case bookNameMsg:
		m.bookName = msg.name
		m.state = stateDownloading
		return m, nil

	case attemptMsg:
		if msg.attempt > 1 {
			m.attempts = msg.attempt
			m.written, m.total = 0, 0
		}
		m.accountName = msg.account
		return m, nil
```

and extend the `downloadDoneMsg` case, keeping the existing exit behaviour —
`tea.Quit` unless `stayOnComplete`, and the absolute-path normalization:

```go
	case downloadDoneMsg:
		m.savedPath = msg.path
		if path, err := filepath.Abs(msg.path); err == nil {
			m.savedPath = path
		}
		m.savedSize = msg.size
		m.state = stateComplete
		m.accountName, m.attempts = msg.account, msg.attempts
		if m.stayOnComplete {
			return m, nil
		}
		return m, tea.Quit
```

Without the `tea.Quit` an interactive download saves the file and then hangs
forever in the program loop.

6. `runPlain` keeps every behaviour the current tests assert: the Ctrl+C trap, the `Downloading: <title>` header, and `plainProgress`:

```go
func (m downloadModel) runPlain(status io.Writer) (downloadModel, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	progress := plainProgress{w: status}
	headerPrinted := false
	printHeader := func(name string) {
		if headerPrinted || name == "" {
			return
		}
		// The title comes from the server; bookCell strips control sequences.
		fmt.Fprintf(status, "Downloading: %s\n", bookCell(name))
		headerPrinted = true
	}
	if m.directURL != "" {
		printHeader(m.bookName)
	}

	result, err := m.manager.Download(ctx, m.id, accounts.Options{
		Dir:      m.dir,
		URL:      m.directURL,
		Account:  m.pinAccount,
		Strategy: m.strategy,
		Progress: progress.report,
		OnBook:   printHeader,
		OnAttempt: func(acct config.Account, attempt int, reason error) {
			if attempt > 1 {
				fmt.Fprintf(status, "  Quota exhausted on %s; trying %s\n", bookCell(m.accountName), bookCell(acct.Name()))
				progress.reported = 0
			}
			m.accountName = acct.Name()
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			m.state = stateCancelled
			return m, nil
		}
		return m, err
	}
	m.bookName = result.BookName
	m.savedPath = result.FilePath
	m.savedSize = result.Size
	m.accountName = result.Account.Name()
	m.attempts = result.Attempts
	m.state = stateComplete
	return m, nil
}
```

7. Report the serving account when more than one attempt was needed. In `runDownload`, after the saved-file line:

```go
	if !jsonOut && dm.attempts > 1 {
		lipgloss.Fprintf(os.Stdout, "%s Served by %s after %d attempts.\n", colorFaint(symbolInfo), safeText(dm.accountName), dm.attempts)
	}
```

and extend `downloadResult`:

```go
	Account  string `json:"account,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
```

populated from `dm.accountName` / `dm.attempts`.

8. Register the flags on every command that builds a download model. In
   `download.go`'s `init()`, and in `search.go`, `history.go`, `saved.go` and
   `book_feed.go`:

```go
	downloadCmd.Flags().String("account", "", "Pin the download to one saved account")
	downloadCmd.Flags().String("strategy", "", "Selection strategy for this run: balanced or round_robin")
```

Register them on each command object by its real name: `searchCmd`,
`historyCmd`, `favoritesCmd` (in `saved.go:10`), and **both**
`recommendedCmd` and `mostPopularCmd` — `book_feed.go` has no `init()` and
builds those two commands through `newBookFeedCommand` (`:16,:20`), so
registering on only one leaves the other failing with `unknown flag:
--account`. A missing registration is silent in the other direction too:
`cmd.Flags().GetString` returns an empty string for an unknown flag, so the
download would quietly ignore `--account`.

Read them where the model is built:

```go
		pin, _ := cmd.Flags().GetString("account")
		strategy, _ := cmd.Flags().GetString("strategy")
		model := newDownloadModel(args[0], dir, manager)
		model.pinAccount, model.strategy = pin, strategy
		return runDownload(model, sendToKindle, cmd)
```

Apply the same two assignments at every `runDownload(...)` call site listed in the Files block.

- [ ] **Step 5: Update every entry point**

- `internal/cli/download.go:38`: `manager, err := loadManager()` then build the model as above.
- `search.go:52`, `history.go:53,60,79,85`, `saved.go:50`, `book_feed.go:55`: same substitution. These commands still call `loadClient()` for their listings.
- `history.go:197`: `newDirectDownloadModel(bookName, item.DownloadURL, dir, manager)`.
- `internal/cli/tui_smoke_test.go:69`: pass a manager instead of `c`; build one with `newFakeManager(t, ...)` or `newManagerForClient(t, c)`.

Add that second helper to `download_test.go`; the existing tests already own a
`*zlib.Client` pointed at an `httptest` server, and a real client satisfies
`accounts.Session`:

```go
// newManagerForClient wraps an existing client in a one-account pool, for the
// tests that download from an httptest server.
func newManagerForClient(t *testing.T, client *zlib.Client) *accounts.Manager {
	t.Helper()
	data := config.Accounts{
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID:  "1",
			Email:   "reader@example.com",
			Domain:  "https://example.com",
			Quota:   config.Quota{Known: true, Allowed: 10, Remaining: 10},
		}},
	}
	manager := accounts.NewManager(data, nil)
	manager.SetSessionFactory(func(config.Account) (accounts.Session, error) { return client, nil })
	manager.SetPersist(func(config.Accounts) error { return nil })
	return manager
}
```

Then update the five existing call sites that still pass a client:
`download_test.go:80,111,128,156,177` (`newDirectDownloadModel` and
`historyDirectDownload`) and `tui_smoke_test.go:69`. `historyDirectDownload`
takes the manager as its fourth parameter now, so its own signature changes
with it.

Also add the three imports `download_test.go` now needs —
`accounts "github.com/heartleo/zlib/internal/account"`,
`"github.com/heartleo/zlib/internal/config"` and `"io"` (for
`runPlain(io.Discard)`) — and in `download.go` swap the `internal/zlib` import
for `internal/account` plus `internal/config` once `fetchCmd` is gone.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/cli -run 'TestDownload|TestPlain|TestHistoryDirect|TestTUI' -v`
Expected: PASS. `go build ./...` does not compile test files, so this test run is the real check.

- [ ] **Step 7: Vet and build**

Run: `go vet ./... && go build ./...`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/download.go internal/cli/search.go internal/cli/history.go internal/cli/saved.go internal/cli/book_feed.go internal/cli/root.go internal/cli/download_test.go internal/cli/tui_smoke_test.go
git commit -m ":necktie: (downloads rotate through the account pool)"
```

---

### Task 9: TUI and MCP

**Files:**
- Modify: `internal/cli/shell.go:110-120,700-760,820-845,882-900`
- Modify: `internal/cli/mcp.go:152-156,560-618`
- Modify: `internal/cli/mcp_output.go`
- Test: `internal/cli/mcp_test.go`, `internal/cli/demo_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/mcp_test.go` (add `"bytes"` to its imports):

```go
func TestMCPDownloadReportsTheServingAccount(t *testing.T) {
	out := downloadResult{Path: "/tmp/book.epub", Account: "one@example.com", Attempts: 2}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"account":"one@example.com"`)) {
		t.Fatalf("json = %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"attempts":2`)) {
		t.Fatalf("json = %s", encoded)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/cli -run TestMCPDownloadReports -v`
Expected: FAIL, unknown field `Account`.

- [ ] **Step 3: Add the MCP input and output**

In `internal/cli/mcp.go`:

```go
type mcpDownloadBookInput struct {
	ID           string `json:"id" jsonschema:"Book reference in id:hash format from search, saved, feed, or history results."`
	Dir          string `json:"dir,omitempty" jsonschema:"Destination directory (supports leading ~). Defaults to ZLIB_DOWNLOAD_DIR or current directory."`
	SendToKindle bool   `json:"send_to_kindle,omitempty" jsonschema:"Email the downloaded file to the configured Kindle address after saving."`
	Account      string `json:"account,omitempty" jsonschema:"Pin the download to one saved account (email, label or user id). Omit to let the pool choose."`
}
```

Replace the fetch+download block in `handleMCPDownloadBook` (`mcp.go:572-596`) with:

```go
	// Guard on ctx before touching the config directory, the way
	// loadMCPClient does: a cancelled call must not read or migrate files.
	if err := ctx.Err(); err != nil {
		return nil, downloadResult{}, formatMCPError(err)
	}
	manager, err := loadManager()
	if err != nil {
		return nil, downloadResult{}, formatMCPError(err)
	}
	dir, err := resolveMCPDownloadDir(in.Dir, policy.AllowedDir)
	if err != nil {
		return nil, downloadResult{}, formatMCPError(err)
	}

	dl, err := manager.Download(ctx, id, accounts.Options{
		Dir:      dir,
		Account:  strings.TrimSpace(in.Account),
		Progress: mcpDownloadProgressReporter(ctx, req, id),
	})
	if err != nil {
		return nil, downloadResult{}, formatMCPError(err)
	}
```

Then fix the result construction, which currently reads `book.Name` from the
deleted fetch (`mcp.go:602-607`):

```go
	out := downloadResult{
		ID:       id,
		Name:     dl.BookName,
		Path:     path,
		Size:     dl.Size,
		Account:  dl.Account.Name(),
		Attempts: dl.Attempts,
	}
```

Keep the connection pool shared: `loadMCPClient` applies
`mcpOptions(ctx).transportSource` (`mcp.go:118-121`), and the manager's own
clients would otherwise open a second pool. Give the manager the same option:

```go
	if source := policy.transportSource; source != nil {
		manager.SetClientOptions(zlib.WithTransportFrom(source))
	}
```

Add to `internal/account/manager.go` (Task 5):

```go
// SetClientOptions applies zlib client options to every session the manager
// builds. The MCP server uses it to share one connection pool.
func (m *Manager) SetClientOptions(opts ...zlib.ClientOption) {
	m.clientOptions = append([]zlib.ClientOption(nil), opts...)
}
```

and use `zlib.NewClient(m.clientOptions...)` in `defaultSession`, which
therefore becomes a method on `*Manager` rather than a package function:

```go
func (m *Manager) defaultSession(acct config.Account) (Session, error) {
	// ... same validation ...
	c := zlib.NewClient(m.clientOptions...)
	c.SetDomain(allowed)
	c.SetCookies(acct.Cookies)
	return c, nil
}
```

`NewManager` sets `m.newSession = m.defaultSession`.

`loadManager` is the variable from Task 8, so the existing fixtures that replace `loadClient` must also replace `loadManager` — see Step 5.

- [ ] **Step 4: Route the TUI through the manager**

In `internal/cli/shell.go`:

1. `shellLogin` (`:882-899`): save through the pool instead of `config.SaveSession`:

```go
	if err = saveAccount(config.Account{
		UserID:  strings.TrimSpace(c.Cookies()["remix_userid"]),
		Email:   strings.TrimSpace(fields[0].value),
		Domain:  c.Domain(),
		Cookies: c.Cookies(),
	}, false); err != nil {
		return shellText{err: err}
	}
	_ = rememberLoginEmail(fields[0].value)
	return shellText{text: "Logged in"}
```

2. Leave `newShellModel`'s `signedIn` check (`:112-116`) reading
   `config.LoadSession()`. The mirror is kept in step by `saveAccount`,
   `account use`, `account remove` and `logout`, so it stays accurate, and it
   is a plain read: switching it to `config.LoadAccounts()` would make every
   `newShellModel` call (including the ones in tests that do not isolate
   `HOME`) migrate and write `accounts.json` into the developer's real config
   directory.

3. The two logout paths (`:602-612` and `:827-840`) keep calling `clearSavedSession()`, which Task 7 redefined to clear the pool and the mirror.

4. Do not cache a manager on the shell model. Open it where a *new* download
   starts (`shell.go:712,721`) and surface the error through `m.setError`:

```go
			manager, err := loadManager()
			if err != nil {
				m.setError(err.Error())
				return nil
			}
			m.download = newDownloadModel(id, "", manager)
```

The retry path (`shell.go:758`) must reuse `m.download.manager` instead of
opening another pool: `TestShellDownloadFailureKeepsContextAndRetries`
(`shell_test.go:357-397`) builds the model with a nil manager and does not
isolate `HOME`, so a second `loadManager()` there would read (and migrate) the
developer's real config directory.

A cached manager goes stale the moment the TUI logs in or out: a fresh install would download with an empty pool, and after a logout the cached value would be used after its accounts are gone. Opening the pool per download costs one small file read.

- [ ] **Step 5: Update the fixtures**

`internal/cli/demo_test.go:232` and `internal/cli/mcp_regression_test.go:27,37,214` replace `loadClient`. So does `withDemoMCPFixtures` in `internal/cli/mcp_test.go:41-59`, which backs `TestMCPWriteToolsAndProgress` — a real `download_book` call that would otherwise see an empty pool and fail with "no accounts saved". Add a parallel replacement in each place:

```go
	prevManager := loadManager
	loadManager = func() (*accounts.Manager, error) {
		data := config.Accounts{Accounts: []config.Account{{
			UserID: "1", Email: "reader@example.com", Domain: server.URL,
			Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "demo"},
		}}}
		manager := accounts.NewManager(data, nil)
		manager.SetSessionFactory(func(config.Account) (accounts.Session, error) {
			c := zlib.NewClient(zlib.WithDomain(server.URL))
			c.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "demo"})
			return c, nil
		})
		manager.SetPersist(func(config.Accounts) error { return nil })
		return manager, nil
	}
	t.Cleanup(func() { loadManager = prevManager })
```

The `SetSessionFactory` call is what keeps the demo and MCP tests off the network: `defaultSession` would otherwise validate `server.URL` through `ParseAllowedDomain`, which rejects `127.0.0.1`.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/cli -run 'TestMCP|TestShell|TestDemo|TestTUI' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/mcp.go internal/cli/mcp_output.go internal/cli/shell.go internal/cli/mcp_test.go internal/cli/demo_test.go internal/cli/mcp_regression_test.go
git commit -m ":sparkles: (mcp and tui use the account pool)"
```

---

### Task 10: Documentation and final verification

**Files:**
- Modify: `docs/usage.md`
- Modify: `docs/configuration.md`
- Modify: `README.md` (only if it lists commands)

- [ ] **Step 1: Document the pool**

Add to `docs/usage.md` after the download section:

```markdown
## Several accounts

Downloads rotate through every saved account, so a day's quota is the sum of
the pool. Add an account with `zlib login`; it never replaces the default
unless it is the first one or you pass `--use`.

```sh
zlib login --email user@example.com
zlib account list
zlib account sync
zlib account use user@example.com
zlib account config --strategy round_robin
zlib account remove user@example.com
zlib download "id:hash" --account user@example.com
zlib download "id:hash" --strategy round_robin
```

Selection follows the stored strategy: `balanced` (the default) prefers the
account with the highest share of its daily allowance left; `round_robin`
walks the list in order. Override it for one run with `--strategy`. Quota
numbers are a local estimate refreshed from the server when they are older than
the configured TTL; `zlib account sync` refreshes them on demand. `favorites`,
`history`, `profile` and `kindle` always use the default account.
```

Add to the saved-files table in `docs/configuration.md`:

```markdown
| `accounts.json` | Saved accounts, quota cache and selection strategy |
```

and a section:

```markdown
## Account pool

`accounts.json` holds every saved account, the cached daily quota and the
selection strategy. `reset_time` is a local wall-clock time (`05:00` by
default); the EAPI reports used and allowed counts but no reset instant, so the
value is an estimate and can be changed. `ZLIB_DOMAIN` still overrides the
domain stored with each account. `session.json` mirrors the default account for
older binaries and is rewritten whenever the default changes.
```

- [ ] **Step 2: Run the documentation check**

Run: `node scripts/check-docs.mjs` (or `bun scripts/check-docs.mjs` where node is
not installed)
Expected: `docs: N files clean`.

`make check` is the full gate (gofmt, `go mod tidy -diff`, plugin-skill sync,
docs privacy, six `go vet` targets, `go test -race ./...`, govulncheck,
gitleaks). Run it too, but read its output rather than expecting the docs line:
it fails early on a missing tool, and that is not a docs problem.

- [ ] **Step 3: Run the whole suite once**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 4: Manual verification with two real accounts**

1. `zlib account list` — both accounts present, quota shown.
2. `zlib profile` before a download, note `downloads_today`.
3. `zlib download "id:hash"` — succeeds.
4. `zlib profile` again — the count moved by one on the account that served it.
5. Mark the first account's quota 0 by hand, download again — the second
   account serves it and the command prints which one.

This closes the spec's open items: where quota is charged, and whether the
local decrement belongs on success.

- [ ] **Step 5: Commit**

```bash
git add docs/usage.md docs/configuration.md
git commit -m ":pencil: (document the account pool)"
```

---

## Self-review notes

Corrections folded in after the plan review:

- `--account` and `--strategy` are registered and plumbed in Task 8, so Task 10
  does not document flags that do not exist.
- The rotation tests make the failing account the one `balanced` picks first;
  the earlier version asserted a rotation that never happened.
- `round_robin` persists the *next* index; storing the used index made every
  run pick the same account. A test covers two consecutive downloads.
- `loadManager` is a package variable, like `loadClient`, and MCP uses it, so
  the demo and MCP fixtures can stay offline.
- Task 8 defines every helper its test uses and lists `tui_smoke_test.go`,
  which constructs a download model directly.
- Task 2 uses the repo's `isolateConfigHome` pattern (both `HOME` and
  `USERPROFILE`) and skips the file-mode assertion on Windows.
- Task 3 uses `time.FixedZone`, so no tzdata is required.
- Task 6 uses `printJSONTo(cmd.OutOrStdout(), ...)`, which is what
  `executeRootForTest` can capture.
- Task 7 drops the unreachable "label required" branch: the cookie importer
  already rejects files without `remix_userid`.
- `session.json` is rewritten by `account use`, `account remove` and `logout`,
  so identity-sensitive commands never keep using a removed account.
- `runPlain` keeps the Ctrl+C trap, the sanitized header and `plainProgress`.
- `downloadDoneMsg` keeps `tea.Quit` and the absolute-path normalization; the
  first rewrite dropped both and would have hung every interactive download.
- The TUI opens the pool per download instead of caching it, so a login or
  logout inside the TUI cannot leave a stale or nil manager behind.
- `cookies_test.go` and the five existing download tests are in the task's
  Files block; both would stop the package compiling otherwise.
- MCP builds its result from `dl.BookName` and passes the server's transport
  source to the manager, so it neither dereferences the deleted `book` nor
  opens a second connection pool.
- A bare `zlib logout` removes the default account instead of erroring.
- `account config` writes `strategy` and `reset_time`, so the documented knobs
  are not JSON-only.
- `Accounts.Remove` re-points to the first *usable* account, and `ApplyReset`
  leaves an account with `Allowed == 0` untouched instead of inventing quota.
- `Download` reads the account count under the mutex, for `-race`.
- Read commands take no `--account`; `zlib account use` switches identity. The
  spec was amended to match.
- Task 1 updates the two existing tests that used the quota wording as their
  "server message survives" fixture; the new sentinel would otherwise turn them
  red.
- The TUI retry path reuses `m.download.manager` instead of opening a second
  pool, which would touch the real config directory in a test that does not
  isolate `HOME`.
- `withDemoMCPFixtures` in `mcp_test.go` is listed with the other fixtures;
  without it the MCP download test sees an empty pool.
- `login.go`'s unused `mode` and `cookies.go`'s unused `config` import are both
  called out, and the feed flags go on both `recommendedCmd` and
  `mostPopularCmd`.
