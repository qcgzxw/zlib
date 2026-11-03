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
