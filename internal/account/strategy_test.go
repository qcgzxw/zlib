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
	if len(ranked) != 3 {
		t.Fatalf("len(ranked) = %d, want 3", len(ranked))
	}
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

func TestRankBalancedDoesNotReorderTheCallersSlice(t *testing.T) {
	input := []config.Account{
		acct("small", 1, 10, time.Time{}),
		acct("big", 9, 10, time.Time{}),
	}
	RankBalanced(input)
	if input[0].UserID != "small" {
		t.Fatalf("input was reordered: %s first", input[0].UserID)
	}
}

func TestRankBalancedTreatsAZeroAllowanceAsNoQuota(t *testing.T) {
	ranked := RankBalanced([]config.Account{
		acct("guarded", 5, 0, time.Time{}),
		acct("normal", 1, 10, time.Time{}),
	})
	if len(ranked) != 2 {
		t.Fatalf("len(ranked) = %d, want 2", len(ranked))
	}
	if ranked[0].UserID != "normal" {
		t.Fatalf("first = %s, want normal", ranked[0].UserID)
	}
}

func TestRankBalancedBreaksFullTiesByName(t *testing.T) {
	at := time.Time{}
	inputs := [][]config.Account{
		{acct("beta", 5, 10, at), acct("alpha", 5, 10, at)},
		{acct("alpha", 5, 10, at), acct("beta", 5, 10, at)},
	}
	for i, input := range inputs {
		ranked := RankBalanced(input)
		if len(ranked) != 2 {
			t.Fatalf("input %d: len(ranked) = %d, want 2", i, len(ranked))
		}
		if ranked[0].Name() != "alpha@example.com" {
			t.Fatalf("input %d: first = %s, want alpha@example.com", i, ranked[0].Name())
		}
	}
}
