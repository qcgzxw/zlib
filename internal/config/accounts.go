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

// Usable reports whether selection may spend this account's quota. An unknown
// quota counts as usable: refusing to try an account whose numbers were never
// fetched would make a fresh install unable to download.
func (a Account) Usable() bool {
	if a.NeedsRelogin {
		return false
	}
	if !a.Quota.Known {
		return true
	}
	return a.Quota.Remaining > 0
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

// ValidStrategy reports whether s names a selection strategy this build
// understands. Normalize, the manager and the account commands share it so the
// whitelist cannot drift apart.
func ValidStrategy(s string) bool {
	switch s {
	case StrategyBalanced, StrategyRoundRobin:
		return true
	}
	return false
}

// Normalize fills the defaults an older or hand-edited file may be missing.
func (a *Accounts) Normalize() {
	if a.Version == 0 {
		a.Version = 1
	}
	if !ValidStrategy(a.Strategy) {
		a.Strategy = StrategyBalanced
	}
	// A value that does not parse would make the rollover inert and strand a
	// known-empty account forever, so a hand-edited or legacy reset time falls
	// back to the default instead of being trusted.
	if _, err := time.Parse("15:04", strings.TrimSpace(a.ResetTime)); err != nil {
		a.ResetTime = DefaultQuotaResetTime
	}
	if a.Accounts == nil {
		a.Accounts = []Account{}
	}
}

// Resolve finds exactly one account by user id, email or label. An ambiguous
// selector is an error: guessing would spend the wrong account's quota.
func (a Accounts) Resolve(selector string) (Account, error) {
	index, err := a.ResolveIndex(selector)
	if err != nil {
		return Account{}, err
	}
	return a.Accounts[index], nil
}

// ResolveIndex finds the position of exactly one account by user id, email or
// label. Mutating callers must use the index: a hand-edited file can hold two
// entries with the same empty user id, and deleting by id would drop the wrong
// one.
func (a Accounts) ResolveIndex(selector string) (int, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return -1, errors.New("no account given")
	}
	var matches []int
	for i, acct := range a.Accounts {
		if acct.UserID == selector || strings.EqualFold(acct.Email, selector) || strings.EqualFold(acct.Label, selector) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return -1, fmt.Errorf("no account matches %q", selector)
	case 1:
		return matches[0], nil
	default:
		return -1, fmt.Errorf("%q matches %d accounts; use the email or user id", selector, len(matches))
	}
}

// Upsert adds acct, or replaces the entry with the same UserID. The cached
// quota, the last-used time, the email and the label survive a replacement
// when the incoming account does not carry them, so re-importing a cookie
// without an email keeps the identity selectors working. Cookies, the domain
// and the relogin flag always come from acct. It reports whether an existing
// entry was replaced.
func (a *Accounts) Upsert(acct Account) bool {
	for i := range a.Accounts {
		if a.Accounts[i].UserID != acct.UserID {
			continue
		}
		acct.Quota = a.Accounts[i].Quota
		acct.LastUsedAt = a.Accounts[i].LastUsedAt
		if strings.TrimSpace(acct.Email) == "" {
			acct.Email = a.Accounts[i].Email
		}
		if strings.TrimSpace(acct.Label) == "" {
			acct.Label = a.Accounts[i].Label
		}
		a.Accounts[i] = acct
		return true
	}
	a.Accounts = append(a.Accounts, acct)
	return false
}

// Remove drops the first account with this user id. It exists for callers that
// only know the identity; the CLI resolves the entry first and calls RemoveAt,
// so a duplicated id cannot make it delete the wrong row.
func (a *Accounts) Remove(userID string) bool {
	for i := range a.Accounts {
		if a.Accounts[i].UserID == userID {
			return a.RemoveAt(i)
		}
	}
	return false
}

// RemoveAt drops the account at index and leaves the pool with a default when
// any entry remains: the old default if it still names an account, otherwise
// the first remaining account that could still download, falling back to the
// first entry when none can. It reports whether an entry was removed.
func (a *Accounts) RemoveAt(index int) bool {
	if index < 0 || index >= len(a.Accounts) {
		return false
	}
	a.Accounts = append(a.Accounts[:index], a.Accounts[index+1:]...)
	if a.RoundRobinIndex >= len(a.Accounts) {
		a.RoundRobinIndex = 0
	}
	a.EnsureDefault()
	return true
}

// EnsureDefault points DefaultAccount at a saved account when it does not
// already resolve. Candidates are ordered so the pick can actually be selected
// and mirrored: an entry with a user id and a usable quota comes first, then
// any entry with a user id. An id-less entry cannot be named, so those rungs
// leave the pool without a default; the session mirror then falls back to the
// entry that has cookies.
func (a *Accounts) EnsureDefault() {
	if a.DefaultResolves() {
		return
	}
	a.DefaultAccount = ""
	for _, acct := range a.Accounts {
		if acct.UserID != "" && acct.Usable() {
			a.DefaultAccount = acct.UserID
			break
		}
	}
	if a.DefaultAccount == "" {
		for _, acct := range a.Accounts {
			if acct.UserID != "" {
				a.DefaultAccount = acct.UserID
				break
			}
		}
	}
}

// DefaultResolves reports whether DefaultAccount names exactly one saved
// account.
func (a Accounts) DefaultResolves() bool {
	if a.DefaultAccount == "" {
		return false
	}
	_, err := a.Resolve(a.DefaultAccount)
	return err == nil
}

// LoadAccounts reads accounts.json. A legacy session.json is migrated on first
// use and written back immediately, so the pool never exists only in memory.
// Every command loads the pool, so an unreadable file has to name its own
// remedy: the user cannot list, log in or download past it otherwise.
func LoadAccounts() (Accounts, error) {
	data, err := os.ReadFile(AccountsPath())
	if err == nil {
		var a Accounts
		if err := json.Unmarshal(data, &a); err != nil {
			return Accounts{}, fmt.Errorf("read %s: %w (move the file aside to start over)", AccountsPath(), err)
		}
		if a.Version > 1 {
			return Accounts{}, fmt.Errorf("accounts.json version %d is newer than this build supports (move the file aside to start over)", a.Version)
		}
		a.Normalize()
		return a, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Accounts{}, err
	}
	migrated := migrateSession()
	if len(migrated.Accounts) > 0 {
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
