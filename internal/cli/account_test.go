package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heartleo/zlib/internal/config"
)

// seedAccounts writes accounts.json into a throwaway config home. Everything
// that later writes session.json must seed the pool first: without an
// accounts.json, LoadAccounts migrates a session with cookies into a
// one-account pool instead of reporting an empty one.
func seedAccounts(t *testing.T, pool config.Accounts) {
	t.Helper()
	isolateConfigHome(t)
	if err := config.SaveAccounts(pool); err != nil {
		t.Fatal(err)
	}
}

// writeRawAccounts plants an accounts.json the loader may reject.
func writeRawAccounts(t *testing.T, content string) {
	t.Helper()
	path := config.AccountsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

func TestAccountListJSONUsesAnArrayForAnEmptyPool(t *testing.T) {
	isolateConfigHome(t)
	out, err := executeRootForTest(t, "account", "list", "--json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var payload struct {
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if string(payload.Accounts) != "[]" {
		t.Fatalf("accounts = %s, want []", payload.Accounts)
	}
}

func TestAccountRemoveKeepsTheDefaultWhenItRemovesAnother(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "2", "remix_userkey": "<key>"}},
		},
	})
	if err := config.SaveSession(config.Session{Cookies: map[string]string{"remix_userid": "1"}, Domain: "https://example.com", Mode: "eapi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRootForTest(t, "account", "remove", "two@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultAccount != "1" {
		t.Fatalf("default = %q, want the untouched default 1", got.DefaultAccount)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror session missing: %v", err)
	}
	if session.Cookies["remix_userid"] != "1" {
		t.Fatalf("mirror user = %q, want the untouched default", session.Cookies["remix_userid"])
	}
}

func TestAccountRemoveWithAnEmptyDefaultKeepsThePool(t *testing.T) {
	seedAccounts(t, config.Accounts{
		// A hand-edited file can name no default while accounts remain; the
		// removal must not read that as "no accounts left".
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "2", "remix_userkey": "<key>"}},
		},
	})
	out, err := executeRootForTest(t, "account", "remove", "two@example.com")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "No accounts left") {
		t.Fatalf("removal claimed the pool was empty:\n%s", out)
	}
	got, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.DefaultAccount != "1" {
		t.Fatalf("accounts = %+v default = %q", got.Accounts, got.DefaultAccount)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror session missing: %v", err)
	}
	if session.Cookies["remix_userid"] != "1" {
		t.Fatalf("mirror user = %q, want the re-pointed default", session.Cookies["remix_userid"])
	}
}

func TestAccountListSaysWhenNothingIsSaved(t *testing.T) {
	isolateConfigHome(t)
	out, err := executeRootForTest(t, "account", "list")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No accounts saved") {
		t.Fatalf("empty pool printed nothing useful: %q", out)
	}
}

func TestAccountSyncSaysWhenNothingIsSaved(t *testing.T) {
	isolateConfigHome(t)
	out, err := executeRootForTest(t, "account", "sync")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No accounts saved") {
		t.Fatalf("empty pool sync = %q", out)
	}
	if strings.Contains(out, "Refreshed") {
		t.Fatalf("empty pool must not claim a refresh: %q", out)
	}
}

func TestAccountListShowsTheDefaultMarkerQuotaAndTimestamps(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Hour)
	lastUsed := time.Now().Add(-time.Hour)
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID:       "1",
			Email:        "one@example.com",
			Domain:       "https://example.com",
			Quota:        config.Quota{Known: true, Allowed: 10, Remaining: 3, ResetAt: resetAt},
			LastUsedAt:   lastUsed,
			NeedsRelogin: true,
		}},
	})
	out, err := executeRootForTest(t, "account", "list")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"* one@example.com", "3/10 left", "resets ", "last used ", "needs re-login"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestAccountListDoesNotMarkAnUnresolvableDefault(t *testing.T) {
	seedAccounts(t, config.Accounts{
		// Two hand-edited entries with no user id: matching on "" would mark
		// both of them as the default.
		Accounts: []config.Account{
			{Email: "first@example.com", Domain: "https://example.com"},
			{Email: "second@example.com", Domain: "https://example.com"},
		},
	})
	out, err := executeRootForTest(t, "account", "list")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "*") {
		t.Fatalf("an empty default must not be marked:\n%s", out)
	}
	raw, err := executeRootForTest(t, "account", "list", "--json")
	if err != nil {
		t.Fatalf("execute json: %v", err)
	}
	var payload struct {
		Accounts []struct {
			Default bool `json:"default"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	for i, row := range payload.Accounts {
		if row.Default {
			t.Fatalf("account %d marked as default in %s", i, raw)
		}
	}
}

func TestAccountRemoveOfTheLastAccountDropsTheMirror(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID:  "1",
			Email:   "one@example.com",
			Domain:  "https://example.com",
			Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"},
		}},
	})
	if err := config.SaveSession(config.Session{Cookies: map[string]string{"remix_userid": "1"}, Domain: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	out, err := executeRootForTest(t, "account", "remove", "one@example.com")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No accounts left") {
		t.Fatalf("last removal = %q", out)
	}
	if _, err := config.LoadSession(); err == nil {
		t.Fatal("mirror session must be gone once the pool is empty")
	}
}

func TestAccountRemoveDeletesTheResolvedEntryWhenIDsRepeat(t *testing.T) {
	seedAccounts(t, config.Accounts{
		Accounts: []config.Account{
			{Email: "first@example.com", Domain: "https://example.com"},
			{Email: "second@example.com", Domain: "https://example.com"},
		},
	})
	if _, err := executeRootForTest(t, "account", "remove", "second@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Email != "first@example.com" {
		t.Fatalf("accounts = %+v, want the entry the selector named to be gone", got.Accounts)
	}
}

func TestAccountConfigWithoutFlagsPrintsTheCurrentValues(t *testing.T) {
	seedAccounts(t, config.Accounts{
		Strategy:       config.StrategyRoundRobin,
		ResetTime:      "06:30",
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	out, err := executeRootForTest(t, "account", "config")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "strategy: "+config.StrategyRoundRobin) || !strings.Contains(out, "reset time: 06:30") {
		t.Fatalf("config = %q", out)
	}
}

func TestAccountCommandsRejectStrayArguments(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	for _, args := range [][]string{
		{"account", "config", "round_robin"},
		{"account", "list", "extra"},
	} {
		if _, err := executeRootForTest(t, args...); err == nil {
			t.Fatalf("%v must reject the stray argument", args)
		}
	}
}

func TestAccountSelectorsRejectAmbiguity(t *testing.T) {
	seedAccounts(t, config.Accounts{
		Accounts: []config.Account{
			{UserID: "1", Label: "backup", Domain: "https://example.com"},
			{UserID: "2", Label: "backup", Domain: "https://example.com"},
		},
	})
	for _, sub := range []string{"use", "remove"} {
		_, err := executeRootForTest(t, "account", sub, "backup")
		if err == nil {
			t.Fatalf("account %s backup must reject the ambiguous selector", sub)
		}
		if !strings.Contains(err.Error(), "matches 2 accounts") {
			t.Fatalf("account %s error = %v, want it to name the ambiguity", sub, err)
		}
	}
}

func TestSaveAccountRefusesAnEmptyID(t *testing.T) {
	seedAccounts(t, config.Accounts{})
	if err := saveAccount(config.Account{Email: "one@example.com", Domain: "https://example.com"}, false); err == nil {
		t.Fatal("an account without a user id must be refused")
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want the pool untouched", pool.Accounts)
	}
}

func TestSaveAccountRepairsADanglingDefaultWithoutStealingIt(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "gone",
		Accounts: []config.Account{{
			UserID:  "1",
			Email:   "one@example.com",
			Domain:  "https://example.com",
			Cookies: map[string]string{"remix_userid": "1"},
		}},
	})
	err := saveAccount(config.Account{
		UserID:  "2",
		Email:   "two@example.com",
		Domain:  "https://example.com",
		Cookies: map[string]string{"remix_userid": "2"},
	}, false)
	if err != nil {
		t.Fatalf("saveAccount: %v", err)
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if pool.DefaultAccount != "1" {
		t.Fatalf("default = %q, want the repaired existing account 1", pool.DefaultAccount)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror session missing: %v", err)
	}
	if session.Cookies["remix_userid"] != "1" {
		t.Fatalf("mirror user = %q, want the default", session.Cookies["remix_userid"])
	}
}

func TestSaveAccountTakesTheDefaultWhenAsked(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	err := saveAccount(config.Account{
		UserID:  "2",
		Email:   "two@example.com",
		Domain:  "https://example.com",
		Cookies: map[string]string{"remix_userid": "2"},
	}, true)
	if err != nil {
		t.Fatalf("saveAccount: %v", err)
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if pool.DefaultAccount != "2" {
		t.Fatalf("default = %q, want the newly saved account", pool.DefaultAccount)
	}
}

func TestAccountRemoveMirrorsTheFirstAccountWithCookies(t *testing.T) {
	seedAccounts(t, config.Accounts{
		// Neither entry has a user id, so no default can be named; the mirror
		// still has to belong to someone, and those commands only need cookies.
		Accounts: []config.Account{
			{Email: "first@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "c1"}},
			{Email: "second@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "c2"}},
		},
	})
	if _, err := executeRootForTest(t, "account", "remove", "second@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror must survive a pool without a default: %v", err)
	}
	if session.Cookies["remix_userid"] != "c1" {
		t.Fatalf("mirror user = %q, want the first entry with cookies", session.Cookies["remix_userid"])
	}
}

func TestAccountRemoveDropsTheMirrorWhenNothingCanBackIt(t *testing.T) {
	seedAccounts(t, config.Accounts{
		Accounts: []config.Account{
			{Email: "first@example.com", Domain: "https://example.com"},
			{Email: "second@example.com", Domain: "https://example.com"},
		},
	})
	if err := config.SaveSession(config.Session{Cookies: map[string]string{"remix_userid": "old"}, Domain: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRootForTest(t, "account", "remove", "second@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := config.LoadSession(); err == nil {
		t.Fatal("mirror must be gone when no saved account carries cookies")
	}
}

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

func TestLogoutAccountRemovesOneAndRepointsTheMirror(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "2", "remix_userkey": "<key>"}},
		},
	})
	if err := syncSessionMirror(mustLoad(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRootForTest(t, "logout", "--account", "one@example.com"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].UserID != "2" || pool.DefaultAccount != "2" {
		t.Fatalf("pool = %+v", pool)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror session missing: %v", err)
	}
	if session.Cookies["remix_userid"] != "2" {
		t.Fatalf("mirror user = %q, want the remaining account", session.Cookies["remix_userid"])
	}
}

func TestLogoutWithoutFlagsRemovesTheDefault(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "<key>"}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "2", "remix_userkey": "<key>"}},
		},
	})
	if err := syncSessionMirror(mustLoad(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := executeRootForTest(t, "logout"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].UserID != "2" || pool.DefaultAccount != "2" {
		t.Fatalf("pool = %+v, want the default 1 removed", pool)
	}
	session, err := config.LoadSession()
	if err != nil {
		t.Fatalf("mirror session missing: %v", err)
	}
	if session.Cookies["remix_userid"] != "2" {
		t.Fatalf("mirror user = %q, want the new default", session.Cookies["remix_userid"])
	}
}

func TestLogoutWithNoAccountsSaysNotLoggedIn(t *testing.T) {
	isolateConfigHome(t)
	out, err := executeRootForTest(t, "logout")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "Not logged in") {
		t.Fatalf("empty pool logout = %q", out)
	}
}

// logout means "remove credentials", so an empty pool must not leave a session
// behind. The mirror outlives the pool when accounts.json was cleared by
// something other than logout, or by hand.
func TestLogoutRemovesAStaleMirrorWithoutAccounts(t *testing.T) {
	seedAccounts(t, config.Accounts{})
	if err := config.SaveSession(config.Session{Cookies: map[string]string{"remix_userid": "old", "remix_userkey": "<key>"}, Domain: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	out, err := executeRootForTest(t, "logout")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := config.LoadSession(); err == nil {
		t.Fatal("stale session.json survived logout")
	}
	if strings.Contains(out, "Not logged in") {
		t.Fatalf("logout claimed there was nothing to remove: %q", out)
	}
}

func TestLogoutWithoutADefaultNamesTheWorkingFlags(t *testing.T) {
	seedAccounts(t, config.Accounts{
		// Neither entry has a user id, so EnsureDefault names none of them and a
		// bare logout has nothing to select.
		Accounts: []config.Account{
			{Email: "first@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "c1"}},
			{Email: "second@example.com", Domain: "https://example.com", Cookies: map[string]string{"remix_userid": "c2"}},
		},
	})
	_, err := executeRootForTest(t, "logout")
	if err == nil {
		t.Fatal("logout without a default must not guess an account")
	}
	for _, want := range []string{"--account", "--all"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to name %s", err, want)
		}
	}
	pool, loadErr := config.LoadAccounts()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want the pool untouched", pool.Accounts)
	}
}

func TestClearSavedSessionFailureKeepsThePool(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	// A non-empty directory where session.json belongs makes the mirror removal
	// fail, which is what the TUI reports as "could not log out". The pool must
	// survive that, or the message would lie about a logout that happened.
	mirror := config.SessionPath()
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "blocker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clearSavedSession(); err == nil {
		t.Fatal("clearSavedSession() ignored the failed mirror removal")
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || pool.DefaultAccount != "1" {
		t.Fatalf("pool = %+v, want the accounts kept", pool)
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

// An accounts.json this build cannot read — a rollback from a newer build, or a
// hand-edit — must not be the reason credentials stay on disk, and must not lock
// every later command out of the pool either.
func TestLogoutRemovesTheMirrorWhenThePoolIsUnreadable(t *testing.T) {
	for name, accounts := range map[string]string{
		"malformed":   `{ not json`,
		"newer build": `{"version":2,"accounts":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			isolateConfigHome(t)
			writeRawAccounts(t, accounts)
			if err := config.SaveSession(config.Session{Cookies: map[string]string{"remix_userid": "old", "remix_userkey": "<key>"}, Domain: "https://example.com"}); err != nil {
				t.Fatal(err)
			}
			out, err := executeRootForTest(t, "logout")
			if err == nil {
				t.Fatal("logout ignored the unreadable pool file")
			}
			if !strings.Contains(err.Error(), "move the file aside") {
				t.Fatalf("error = %v, want it to name the remedy", err)
			}
			if _, err := config.LoadSession(); err == nil {
				t.Fatal("session.json survived an unreadable pool file")
			}
			// The pool holds credentials too. Rename, never delete: whatever the
			// user had stays recoverable.
			broken := config.AccountsPath() + ".broken"
			if data, err := os.ReadFile(broken); err != nil {
				t.Fatalf("the unreadable pool was not moved aside: %v", err)
			} else if string(data) != accounts {
				t.Fatalf("moved file = %q, want the original %q", data, accounts)
			}
			if _, err := os.Stat(config.AccountsPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the unreadable pool is still in place: %v", err)
			}
			if !strings.Contains(out, broken) {
				t.Fatalf("logout did not report the move: %q", out)
			}
			// The next command starts from an empty pool instead of the same parse
			// error, which is the dead end this fixes.
			list, err := executeRootForTest(t, "account", "list")
			if err != nil {
				t.Fatalf("account list after the move: %v", err)
			}
			if !strings.Contains(list, "No accounts saved") {
				t.Fatalf("account list = %q, want the empty pool wording", list)
			}
		})
	}
}

func TestClearSavedSessionRemovesTheMirrorWhenThePoolIsUnreadable(t *testing.T) {
	isolateConfigHome(t)
	writeRawAccounts(t, `{ not json`)
	if err := config.SaveSession(config.Session{Cookies: map[string]string{"remix_userid": "old", "remix_userkey": "<key>"}, Domain: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	removed, err := clearSavedSession()
	if err == nil {
		t.Fatal("clearSavedSession() hid the unreadable pool file")
	}
	if !removed {
		t.Fatal("clearSavedSession() did not report the removed mirror")
	}
	if _, err := config.LoadSession(); err == nil {
		t.Fatal("session.json survived an unreadable pool file")
	}
	if _, err := os.Stat(config.AccountsPath() + ".broken"); err != nil {
		t.Fatalf("the unreadable pool was not moved aside: %v", err)
	}
	// The TUI reports errors as one line, so the move has to travel in the error
	// it already shows.
	if !strings.Contains(err.Error(), ".broken") {
		t.Fatalf("error = %v, want it to report the move", err)
	}
	if _, err := config.LoadAccounts(); err != nil {
		t.Fatalf("the pool is still unreadable after the move: %v", err)
	}
}

func TestLogoutRejectsContradictoryFlagsAndArguments(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts:       []config.Account{{UserID: "1", Email: "one@example.com", Domain: "https://example.com"}},
	})
	for _, args := range [][]string{
		{"logout", "extra"},
		{"logout", "--all", "--account", "one@example.com"},
	} {
		if _, err := executeRootForTest(t, args...); err == nil {
			t.Fatalf("%v must be rejected", args)
		}
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("a rejected logout still removed accounts: %+v", pool.Accounts)
	}
}

func TestLogoutNamesWhatItRemoved(t *testing.T) {
	seedAccounts(t, config.Accounts{
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com"},
		},
	})
	out, err := executeRootForTest(t, "logout", "--account", "one@example.com")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "one@example.com") {
		t.Fatalf("logout did not name the account it removed: %q", out)
	}
	if strings.Contains(out, "account(s)") {
		t.Fatalf("logout printed the unpluralised counter: %q", out)
	}
	out, err = executeRootForTest(t, "logout", "--all")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "account(s)") {
		t.Fatalf("logout --all printed the unpluralised counter: %q", out)
	}
}

func TestAccountUseExplainsHowToFixAnAccountWithoutAnID(t *testing.T) {
	seedAccounts(t, config.Accounts{
		Accounts: []config.Account{{Email: "one@example.com", Domain: "https://example.com"}},
	})
	_, err := executeRootForTest(t, "account", "use", "one@example.com")
	if err == nil {
		t.Fatal("an account without a user id cannot become the default")
	}
	if !strings.Contains(err.Error(), "zlib account remove") {
		t.Fatalf("error = %v, want a workable remedy", err)
	}
}
