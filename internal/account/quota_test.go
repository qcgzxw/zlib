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
	want := time.Date(2026, 11, 5, 5, 0, 0, 0, loc)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
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
	if got.Remaining != 7 {
		t.Fatalf("remaining = %d, want the stored 7", got.Remaining)
	}
}

func TestApplyResetWithUnparseableTimeIsInert(t *testing.T) {
	loc := utc8()
	resetAt := time.Date(2026, 11, 1, 5, 0, 0, 0, loc)
	quota := config.Quota{Known: true, Allowed: 10, Remaining: 0, ResetAt: resetAt}
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, loc)
	got := ApplyReset(quota, "later", now)
	if got.Remaining != 0 {
		t.Fatalf("remaining = %d, want unchanged", got.Remaining)
	}
	if !got.ResetAt.Equal(resetAt) {
		t.Fatalf("reset at = %v, want unchanged %v", got.ResetAt, resetAt)
	}
}

func TestApplyResetRollsOverExactlyAtTheResetInstant(t *testing.T) {
	loc := utc8()
	reset := time.Date(2026, 11, 3, 5, 0, 0, 0, loc)
	quota := config.Quota{Known: true, Allowed: 10, Remaining: 2, ResetAt: reset}

	got := ApplyReset(quota, "05:00", reset)
	if got.Remaining != 10 {
		t.Fatalf("remaining at the reset instant = %d, want 10", got.Remaining)
	}
	want := time.Date(2026, 11, 4, 5, 0, 0, 0, loc)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
	}

	again := ApplyReset(got, "05:00", reset)
	if again.Remaining != 10 {
		t.Fatalf("second call remaining = %d, want 10 (no double rollover)", again.Remaining)
	}
	if !again.ResetAt.Equal(want) {
		t.Fatalf("second call reset at = %v, want %v unchanged", again.ResetAt, want)
	}
}

func TestApplyResetReanchorsToTheLocalResetTime(t *testing.T) {
	loc := utc8()
	// Stored under UTC, so 05:00 is 13:00 on a UTC+8 machine.
	quota := config.Quota{Known: true, Allowed: 10, Remaining: 3, ResetAt: time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)}
	now := time.Date(2026, 11, 2, 16, 55, 0, 0, loc)
	got := ApplyReset(quota, "05:00", now)
	if got.Remaining != 10 {
		t.Fatalf("remaining = %d, want 10", got.Remaining)
	}
	want := time.Date(2026, 11, 3, 5, 0, 0, 0, loc)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
	}
	if got.ResetAt.Hour() != 5 || got.ResetAt.Minute() != 0 {
		t.Fatalf("reset at wall clock = %02d:%02d, want 05:00 local", got.ResetAt.Hour(), got.ResetAt.Minute())
	}
}

func TestApplyResetHandlesFarPastResetAt(t *testing.T) {
	loc := utc8()
	now := time.Date(2026, 11, 2, 16, 55, 0, 0, loc)
	quota := config.Quota{Known: true, Allowed: 10, Remaining: 0, ResetAt: now.AddDate(-1, 0, 0)}
	got := ApplyReset(quota, "05:00", now)
	if got.Remaining != 10 {
		t.Fatalf("remaining = %d, want 10", got.Remaining)
	}
	want := time.Date(2026, 11, 3, 5, 0, 0, 0, loc)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
	}
}

func TestExhaustedUnknownAllowanceReturnsToUnknownAtReset(t *testing.T) {
	loc := utc8()
	// The shape markExhausted leaves behind on a plan that never reported a
	// limit: known, no allowance, nothing left, reset point in the past.
	quota := config.Quota{Known: true, Remaining: 0, ResetAt: time.Date(2026, 11, 1, 5, 0, 0, 0, loc)}

	before := ApplyReset(quota, "05:00", time.Date(2026, 11, 1, 4, 0, 0, 0, loc))
	if !before.Known || before.Remaining != 0 {
		t.Fatalf("before reset quota = %+v, want still known and empty", before)
	}

	got := ApplyReset(quota, "05:00", time.Date(2026, 11, 2, 6, 0, 0, 0, loc))
	if got.Known {
		t.Fatalf("quota = %+v, want unknown again after the reset", got)
	}
	if want := time.Date(2026, 11, 3, 5, 0, 0, 0, loc); !got.ResetAt.Equal(want) {
		t.Fatalf("reset at = %v, want %v", got.ResetAt, want)
	}
}

func TestNextResetIsStrictlyAfterNow(t *testing.T) {
	loc := utc8()

	at, ok := NextReset("05:00", time.Date(2026, 11, 2, 5, 0, 0, 0, loc))
	if !ok {
		t.Fatal("NextReset at the reset time reported unparseable")
	}
	if want := time.Date(2026, 11, 3, 5, 0, 0, 0, loc); !at.Equal(want) {
		t.Fatalf("reset at = %v, want %v", at, want)
	}

	justBefore, ok := NextReset("05:00", time.Date(2026, 11, 2, 4, 59, 59, 0, loc))
	if !ok {
		t.Fatal("NextReset before the reset time reported unparseable")
	}
	if want := time.Date(2026, 11, 2, 5, 0, 0, 0, loc); !justBefore.Equal(want) {
		t.Fatalf("reset at = %v, want %v", justBefore, want)
	}

	if got, ok := NextReset("later", time.Date(2026, 11, 2, 4, 0, 0, 0, loc)); ok || !got.IsZero() {
		t.Fatalf("unparseable NextReset = %v, %v; want zero, false", got, ok)
	}
}
