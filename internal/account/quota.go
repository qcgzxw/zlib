package account

import (
	"strings"
	"time"

	"github.com/heartleo/zlib/internal/config"
)

// ApplyReset advances a cached quota record to now, rolling the daily counter
// over at the configured local wall-clock time. An unknown quota is left
// unknown: a missing number is not the same as a full or an empty allowance.
// The time is local wall clock: a value that falls inside a DST spring-forward
// gap is normalised by time.Date, which moves it an hour earlier (Go's choice
// for the skipped hour is unspecified).
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
