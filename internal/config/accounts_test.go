package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	second := Accounts{Accounts: []Account{{
		UserID:  "43",
		Email:   "other@example.com",
		Domain:  "https://example.com",
		Cookies: map[string]string{"remix_userid": "43", "remix_userkey": "<key>"},
	}}}
	if err := SaveAccounts(second); err != nil {
		t.Fatalf("SaveAccounts() overwrite error = %v", err)
	}
	overwritten, err := LoadAccounts()
	if err != nil {
		t.Fatalf("LoadAccounts() after overwrite error = %v", err)
	}
	if len(overwritten.Accounts) != 1 || overwritten.Accounts[0].UserID != "43" {
		t.Fatalf("overwritten accounts = %+v", overwritten.Accounts)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(AccountsPath()), ".accounts-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestSaveAccountsWritesStableJSON(t *testing.T) {
	isolateHome(t)
	if err := SaveAccounts(Accounts{Accounts: []Account{{
		UserID:  "42",
		Email:   "reader@example.com",
		Domain:  "https://example.com",
		Cookies: map[string]string{"remix_userid": "42", "remix_userkey": "<key>"},
	}}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version": 1`, `"strategy": "balanced"`, `"reset_time": "05:00"`, `"user_id": "42"`, `"accounts": [`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("accounts.json is missing %s:\n%s", want, raw)
		}
	}
	if bytes.Contains(raw, []byte("0001-01-01")) {
		t.Fatalf("zero timestamps must be omitted:\n%s", raw)
	}
}

func TestSaveAccountsWritesAnEmptyListNotEmptyNull(t *testing.T) {
	isolateHome(t)
	var pool Accounts
	if err := SaveAccounts(pool); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"accounts": null`)) {
		t.Fatalf("empty pool must serialise as []:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte(`"accounts": []`)) {
		t.Fatalf("empty pool must serialise as []:\n%s", raw)
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

func TestLoadAccountsReportsFailedMigrationWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not restrict access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := isolateHome(t)
	configDir := filepath.Join(dir, ".config", "zlib")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := `{"cookies":{"remix_userid":"7","remix_userkey":"<key>"},"domain":"https://example.com"}`
	if err := os.WriteFile(filepath.Join(configDir, "session.json"), []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(configDir, 0o700) })

	if _, err := LoadAccounts(); err == nil {
		t.Fatal("LoadAccounts() must report a failed migration write")
	} else if !strings.Contains(err.Error(), "migrate ") {
		t.Fatalf("error = %v, want a migration write failure", err)
	}
	if _, err := os.Stat(AccountsPath()); err == nil {
		t.Fatal("accounts.json must not exist when the migration write failed")
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

func TestNormalizeRejectsAnUnparseableResetTime(t *testing.T) {
	broken := Accounts{ResetTime: "5am"}
	broken.Normalize()
	if broken.ResetTime != DefaultQuotaResetTime {
		t.Fatalf("reset time = %q, want the default", broken.ResetTime)
	}

	valid := Accounts{ResetTime: "06:30"}
	valid.Normalize()
	if valid.ResetTime != "06:30" {
		t.Fatalf("reset time = %q, want the configured 06:30", valid.ResetTime)
	}
}

func TestUpsertReplacesAndKeepsQuota(t *testing.T) {
	var a Accounts
	a.Normalize()
	quota := Quota{Known: true, Allowed: 10, Remaining: 4, ResetAt: time.Now().Add(time.Hour)}
	used := time.Now().Add(-time.Hour).Truncate(time.Second)
	a.Accounts = []Account{{UserID: "42", Email: "old@example.com", Quota: quota, LastUsedAt: used}}
	replaced := a.Upsert(Account{UserID: "42", Email: "new@example.com", Domain: "https://example.com"})
	if !replaced || len(a.Accounts) != 1 {
		t.Fatalf("replaced = %v, accounts = %+v", replaced, a.Accounts)
	}
	got := a.Accounts[0]
	if got.Email != "new@example.com" || got.Quota.Remaining != 4 {
		t.Fatalf("account = %+v", got)
	}
	if !got.LastUsedAt.Equal(used) {
		t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, used)
	}
}

func TestUpsertKeepsIdentityWhenImportHasNone(t *testing.T) {
	var a Accounts
	a.Normalize()
	a.Accounts = []Account{{UserID: "42", Email: "old@example.com", Label: "primary"}}
	if !a.Upsert(Account{UserID: "42", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "42"}}) {
		t.Fatal("Upsert() must report a replacement")
	}
	got := a.Accounts[0]
	if got.Email != "old@example.com" || got.Label != "primary" {
		t.Fatalf("identity = %q/%q, want the stored email and label to survive", got.Email, got.Label)
	}
	if got.Domain != "https://example.com" || got.Cookies["remix_userid"] != "42" {
		t.Fatalf("session = %+v, want the caller's domain and cookies", got)
	}
}

func TestUpsertAppendsAnUnknownAccount(t *testing.T) {
	var a Accounts
	a.Normalize()
	if a.Upsert(Account{UserID: "42", Email: "reader@example.com"}) {
		t.Fatal("Upsert() must report a new account, not a replacement")
	}
	if len(a.Accounts) != 1 || a.Accounts[0].UserID != "42" {
		t.Fatalf("accounts = %+v", a.Accounts)
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

func TestRemoveUnknownIDReportsNothing(t *testing.T) {
	var a Accounts
	a.DefaultAccount = "1"
	a.Accounts = []Account{{UserID: "1"}, {UserID: "2"}}
	if a.Remove("nope") {
		t.Fatal("Remove(nope) must report no removal")
	}
	if len(a.Accounts) != 2 || a.DefaultAccount != "1" {
		t.Fatalf("accounts = %+v, default = %q", a.Accounts, a.DefaultAccount)
	}
}

func TestRemoveClampsRoundRobinIndex(t *testing.T) {
	var a Accounts
	a.RoundRobinIndex = 2
	a.Accounts = []Account{{UserID: "1"}, {UserID: "2"}, {UserID: "3"}}
	if !a.Remove("3") {
		t.Fatal("Remove(3) must report a removal")
	}
	if a.RoundRobinIndex != 0 {
		t.Fatalf("RoundRobinIndex = %d, want 0 after the slice shrank", a.RoundRobinIndex)
	}
}

func TestRemoveLeavesAnUnrelatedDefaultAlone(t *testing.T) {
	var a Accounts
	a.DefaultAccount = "1"
	a.Accounts = []Account{{UserID: "1"}, {UserID: "2"}}
	a.Remove("2")
	if a.DefaultAccount != "1" {
		t.Fatalf("default = %q, want the untouched default 1", a.DefaultAccount)
	}
}

func TestRemoveFillsAnEmptyDefault(t *testing.T) {
	var a Accounts
	// A hand-edited file can name no default at all; removing any entry must
	// still leave the pool with one, or the next download has no identity.
	a.Accounts = []Account{
		{UserID: "1", Quota: Quota{Known: true, Allowed: 10, Remaining: 0}},
		{UserID: "2"},
	}
	a.Remove("1")
	if a.DefaultAccount != "2" {
		t.Fatalf("default = %q, want the remaining usable account 2", a.DefaultAccount)
	}
}

func TestRemoveReplacesAStaleDefault(t *testing.T) {
	var a Accounts
	a.DefaultAccount = "gone"
	a.Accounts = []Account{{UserID: "1"}, {UserID: "2"}}
	a.Remove("2")
	if a.DefaultAccount != "1" {
		t.Fatalf("default = %q, want 1 when the stored default is not in the pool", a.DefaultAccount)
	}
}

func TestRemoveOfTheLastAccountClearsTheDefault(t *testing.T) {
	var a Accounts
	a.DefaultAccount = "1"
	a.Accounts = []Account{{UserID: "1"}}
	a.Remove("1")
	if len(a.Accounts) != 0 || a.DefaultAccount != "" {
		t.Fatalf("accounts = %+v, default = %q, want an empty pool", a.Accounts, a.DefaultAccount)
	}
}

func TestRemoveAtDropsTheResolvedEntryWhenIDsRepeat(t *testing.T) {
	var a Accounts
	// A hand-edited file can hold entries without a user id; removing by id
	// would delete the first of them instead of the one the selector named.
	a.Accounts = []Account{
		{Email: "first@example.com"},
		{Email: "second@example.com"},
	}
	index, err := a.ResolveIndex("second@example.com")
	if err != nil {
		t.Fatalf("ResolveIndex: %v", err)
	}
	if index != 1 {
		t.Fatalf("index = %d, want 1", index)
	}
	if !a.RemoveAt(index) {
		t.Fatal("RemoveAt(1) must report a removal")
	}
	if len(a.Accounts) != 1 || a.Accounts[0].Email != "first@example.com" {
		t.Fatalf("accounts = %+v", a.Accounts)
	}
}

func TestResolveIndexRejectsAnOutOfRangePosition(t *testing.T) {
	var a Accounts
	a.Accounts = []Account{{UserID: "1"}}
	if a.RemoveAt(-1) || a.RemoveAt(1) {
		t.Fatal("RemoveAt must refuse a position outside the pool")
	}
	if len(a.Accounts) != 1 {
		t.Fatalf("accounts = %+v", a.Accounts)
	}
}

func TestValidStrategyAcceptsOnlyTheKnownNames(t *testing.T) {
	for _, name := range []string{StrategyBalanced, StrategyRoundRobin} {
		if !ValidStrategy(name) {
			t.Errorf("ValidStrategy(%q) = false", name)
		}
	}
	for _, name := range []string{"", "nonsense", "Balanced", "round-robin"} {
		if ValidStrategy(name) {
			t.Errorf("ValidStrategy(%q) = true", name)
		}
	}
}

func TestEnsureDefaultPrefersAnEntryThatCanBeNamed(t *testing.T) {
	var a Accounts
	// An id-less entry cannot be selected at all, so naming it would leave the
	// pool with a default that resolves to nothing.
	a.Accounts = []Account{
		{Email: "first@example.com"},
		{UserID: "2", Email: "second@example.com"},
	}
	a.EnsureDefault()
	if a.DefaultAccount != "2" {
		t.Fatalf("default = %q, want the entry that has a user id", a.DefaultAccount)
	}
}

func TestEnsureDefaultPrefersAUsableEntry(t *testing.T) {
	var a Accounts
	a.Accounts = []Account{
		{UserID: "1", Quota: Quota{Known: true, Allowed: 5, Remaining: 0}},
		{UserID: "2"},
	}
	a.EnsureDefault()
	if a.DefaultAccount != "2" {
		t.Fatalf("default = %q, want the entry that can still download", a.DefaultAccount)
	}
}

func TestEnsureDefaultLeavesAPoolOfIdLessEntriesWithoutOne(t *testing.T) {
	var a Accounts
	a.Accounts = []Account{{Email: "first@example.com"}, {Email: "second@example.com"}}
	a.EnsureDefault()
	if a.DefaultAccount != "" {
		t.Fatalf("default = %q, want none", a.DefaultAccount)
	}
}

func TestResolveAcceptsAnySelectorAndRejectsAmbiguity(t *testing.T) {
	var a Accounts
	a.Accounts = []Account{
		{UserID: "1", Email: "one@example.com"},
		{UserID: "2", Label: "backup"},
		{UserID: "3", Label: "backup"},
	}
	for _, tc := range []struct{ selector, want string }{
		{"one@example.com", "1"},
		{"ONE@EXAMPLE.COM", "1"},
		{"2", "2"},
	} {
		got, err := a.Resolve(tc.selector)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", tc.selector, err)
		}
		if got.UserID != tc.want {
			t.Fatalf("Resolve(%q) = account %q, want %q", tc.selector, got.UserID, tc.want)
		}
	}
	if _, err := a.Resolve("backup"); err == nil {
		t.Fatal("Resolve(backup) must reject an ambiguous selector")
	}
	if _, err := a.Resolve("nope"); err == nil {
		t.Fatal("Resolve(nope) must fail")
	}
	if _, err := a.Resolve("  "); err == nil {
		t.Fatal("Resolve() must reject an empty selector")
	} else if !strings.Contains(err.Error(), "no account given") {
		t.Fatalf("error = %v, want the blank-selector guard", err)
	}
}

func TestLoadAccountsRejectsMalformedFile(t *testing.T) {
	dir := isolateHome(t)
	configDir := filepath.Join(dir, ".config", "zlib")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AccountsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAccounts(); err == nil {
		t.Fatal("LoadAccounts() must report a malformed accounts.json")
	} else if !strings.Contains(err.Error(), "move the file aside") {
		t.Fatalf("error = %v, want it to name the remedy", err)
	}
}

func TestLoadAccountsRejectsNewerVersion(t *testing.T) {
	dir := isolateHome(t)
	configDir := filepath.Join(dir, ".config", "zlib")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AccountsPath(), []byte(`{"version":2,"accounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAccounts(); err == nil {
		t.Fatal("LoadAccounts() must reject a version it cannot read")
	} else if !strings.Contains(err.Error(), "move the file aside") {
		t.Fatalf("error = %v, want it to name the remedy", err)
	}
}
