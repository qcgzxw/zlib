package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	book     *zlib.Book // overrides the default metadata when set
}

func (f *fakeSession) FetchBookContext(ctx context.Context, ref string) (zlib.Book, error) {
	if f.fetchErr != nil {
		return zlib.Book{}, f.fetchErr
	}
	if f.book != nil {
		return *f.book, nil
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

// A book whose metadata carries no title still reports that metadata arrived:
// callers use OnBook to prepare the destination directory, so skipping the call
// would make those downloads fail before the first byte.
func TestDownloadReportsMetadataWithoutATitle(t *testing.T) {
	untitled := zlib.Book{DownloadURL: "https://example.com/eapi/book/1/abc/file"}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": {book: &untitled}})

	var calls int
	var gotName string
	res, err := m.Download(context.Background(), "1:abc", Options{
		Dir: "/tmp",
		OnBook: func(name string) {
			calls++
			gotName = name
		},
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("OnBook calls = %d, want 1", calls)
	}
	if gotName != "" || res.BookName != "" {
		t.Fatalf("name = %q, result = %q, want empty", gotName, res.BookName)
	}
	if res.FilePath != "/tmp/book.epub" {
		t.Fatalf("result = %+v", res)
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

func TestPinnedExpiredSessionIsReportedAsSuch(t *testing.T) {
	only := &fakeSession{fetchErr: zlib.ErrSessionExpired}
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": only})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp", Account: "one@example.com"})
	if !errors.Is(err, zlib.ErrSessionExpired) {
		t.Fatalf("error = %v, want ErrSessionExpired, not an all-exhausted error", err)
	}
	var exhausted *AllExhaustedError
	if errors.As(err, &exhausted) {
		t.Fatalf("error = %v, want the session error, not AllExhaustedError", err)
	}
	last := (*saved)[len(*saved)-1]
	if !last.Accounts[0].NeedsRelogin {
		t.Fatal("expired account not marked")
	}
}

// A successful download must not invent quota numbers for an account the
// server never reported: unknown stays unknown, and only the usage time moves.
func TestSuccessfulDownloadLeavesUnknownQuotaUnknown(t *testing.T) {
	session := &fakeSession{}
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": session})

	if _, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	last := (*saved)[len(*saved)-1]
	quota := last.Accounts[0].Quota
	if quota.Known {
		t.Fatalf("quota = %+v, want still unknown", quota)
	}
	if quota.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0", quota.Remaining)
	}
	if last.Accounts[0].LastUsedAt.IsZero() {
		t.Fatal("last used at not recorded")
	}
}

// A pin bypasses the relogin flag on purpose: the caller asked for this account
// by name, so it gets one honest attempt.
func TestPinnedAccountWithReloginFlagIsStillTried(t *testing.T) {
	only := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com", NeedsRelogin: true},
	}, map[string]*fakeSession{"1": only})

	if _, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp", Account: "one@example.com"}); err != nil {
		t.Fatalf("Download() error = %v", err)
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

// A reference download has no caller-supplied URL to fall back to, so a failed
// fetch-derived download must not be retried with an empty string.
func TestReferenceDownloadDoesNotRetryWithAnEmptyURL(t *testing.T) {
	recording := &recordingSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
	}, map[string]*fakeSession{})
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		return recording, nil
	})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if !errors.Is(err, zlib.ErrDownloadFailed) {
		t.Fatalf("error = %v, want ErrDownloadFailed", err)
	}
	if len(recording.urls) != 1 {
		t.Fatalf("download attempts = %d %q, want exactly 1", len(recording.urls), recording.urls)
	}
	if recording.urls[0] == "" {
		t.Fatal("download retried with an empty URL")
	}
}

// recordingSession records every download URL and always fails the way a
// mirror that does not serve the path does.
type recordingSession struct {
	fakeSession
	urls []string
}

func (r *recordingSession) DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error) {
	r.urls = append(r.urls, url)
	return zlib.DownloadResult{}, fmt.Errorf("%w: HTTP 404", zlib.ErrDownloadFailed)
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

// An account whose quota was never observed is stale by definition: it must
// learn its numbers on first use, not only from a sync or a rejection.
func TestUnknownQuotaIsRefreshedOnFirstDownload(t *testing.T) {
	session := &fakeSession{profile: zlib.UserProfile{
		QuotaKnown:    true,
		DownloadLimit: zlib.DownloadLimit{DailyAllowed: 10, DailyRemaining: 10},
	}}
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": session})

	if _, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	last := (*saved)[len(*saved)-1]
	quota := last.Accounts[0].Quota
	if !quota.Known {
		t.Fatalf("quota = %+v, want the observed one", quota)
	}
	if quota.Remaining != 9 {
		t.Fatalf("remaining = %d, want 9 (10 observed, 1 spent)", quota.Remaining)
	}
	if quota.ObservedAt.IsZero() {
		t.Fatal("observed at not stamped")
	}
}

// A server that never reports a quota must cost one profile request per TTL,
// not one per download.
func TestRefreshWithoutNumbersBacksOff(t *testing.T) {
	counting := &countingProfileSession{inner: &fakeSession{}}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
	}, map[string]*fakeSession{})
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		return counting, nil
	})

	for i := 0; i < 2; i++ {
		if _, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"}); err != nil {
			t.Fatalf("Download() %d error = %v", i+1, err)
		}
	}
	if counting.calls != 1 {
		t.Fatalf("profile requests = %d, want 1 (the TTL backs off)", counting.calls)
	}
	if m.Snapshot().Accounts[0].Quota.ObservedAt.IsZero() {
		t.Fatal("the attempt that returned no numbers was not stamped")
	}
}

type countingProfileSession struct {
	inner *fakeSession
	calls int
}

func (c *countingProfileSession) FetchBookContext(ctx context.Context, ref string) (zlib.Book, error) {
	return c.inner.FetchBookContext(ctx, ref)
}

func (c *countingProfileSession) DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error) {
	return c.inner.DownloadWithContext(ctx, url, dir, progress)
}

func (c *countingProfileSession) GetProfileContext(ctx context.Context) (zlib.UserProfile, error) {
	c.calls++
	return c.inner.GetProfileContext(ctx)
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

// An account rejected on a plan that never reported a limit must come back at
// its reset instant instead of staying out of the pool forever with an
// allowance of zero, which nothing would ever roll over.
func TestExhaustedUnknownAllowanceAccountRecoversAfterReset(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	rejecting := &fakeSession{dlErr: zlib.ErrDailyLimitReached}
	spare := &fakeSession{}
	m := NewManager(config.Accounts{Accounts: []config.Account{
		{UserID: "1", Domain: "https://example.com"},
		{UserID: "2", Domain: "https://example.com"},
	}}, func() time.Time { return now })
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		if acct.UserID == "1" {
			return rejecting, nil
		}
		return spare, nil
	})
	m.SetPersist(func(config.Accounts) error { return nil })

	res, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if res.Account.UserID != "2" {
		t.Fatalf("account = %s, want the fallback", res.Account.UserID)
	}
	exhausted := m.Snapshot().Accounts[0]
	if !exhausted.Quota.Known || exhausted.Quota.Remaining != 0 {
		t.Fatalf("quota = %+v, want known and empty", exhausted.Quota)
	}
	if exhausted.Quota.ResetAt.IsZero() {
		t.Fatal("exhausted account has no reset point")
	}
	if exhausted.Usable() {
		t.Fatal("exhausted account is usable before its reset")
	}

	now = time.Date(2026, 11, 3, 5, 0, 1, 0, time.UTC)
	recovered := m.Snapshot().Accounts[0]
	if recovered.Quota.Known {
		t.Fatalf("quota = %+v, want unknown again after the reset", recovered.Quota)
	}
	if !recovered.Usable() {
		t.Fatal("recovered account is not usable after its reset")
	}

	rejecting.dlErr = nil
	res, err = m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if err != nil {
		t.Fatalf("Download() after the reset error = %v", err)
	}
	if res.Account.UserID != "1" {
		t.Fatalf("account = %s, want the recovered one", res.Account.UserID)
	}
}

func TestSnapshotAndDefaultCloneCookies(t *testing.T) {
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Cookies: map[string]string{"remix_userkey": "fixture-key"}},
	}, map[string]*fakeSession{})

	snapshot := m.Snapshot()
	snapshot.Accounts[0].Cookies["remix_userkey"] = "tampered"
	if got := m.Snapshot().Accounts[0].Cookies["remix_userkey"]; got != "fixture-key" {
		t.Fatalf("pool key after a snapshot edit = %q, want the original", got)
	}

	acct, ok := m.Default()
	if !ok {
		t.Fatal("no default account")
	}
	acct.Cookies["remix_userkey"] = "tampered"
	if got := m.Snapshot().Accounts[0].Cookies["remix_userkey"]; got != "fixture-key" {
		t.Fatalf("pool key after a default edit = %q, want the original", got)
	}
}

func TestNewManagerClonesCookies(t *testing.T) {
	pool := config.Accounts{Accounts: []config.Account{
		{UserID: "1", Domain: "https://example.com", Cookies: map[string]string{"remix_userkey": "fixture-key"}},
	}}
	m := NewManager(pool, func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) })

	pool.Accounts[0].Cookies["remix_userkey"] = "tampered"
	if got := m.Snapshot().Accounts[0].Cookies["remix_userkey"]; got != "fixture-key" {
		t.Fatalf("pool key after mutating the caller's map = %q, want the original", got)
	}
}

func TestOnAttemptGetsItsOwnCookies(t *testing.T) {
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Cookies: map[string]string{"remix_userkey": "fixture-key"}},
	}, map[string]*fakeSession{"1": {}})

	if _, err := m.Download(context.Background(), "1:abc", Options{
		Dir: "/tmp",
		OnAttempt: func(acct config.Account, attempt int, reason error) {
			acct.Cookies["remix_userkey"] = "tampered"
		},
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if got := m.Snapshot().Accounts[0].Cookies["remix_userkey"]; got != "fixture-key" {
		t.Fatalf("pool key after the callback edited its copy = %q, want the original", got)
	}
}

// ObservedAt means "the server reported numbers": a known quota advances it on
// a successful download, an unknown one keeps its zero value so the next
// attempt asks the server rather than waiting out a TTL.
func TestRecordSuccessStampsObservedAtOnlyWhenKnown(t *testing.T) {
	observed := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	m, saved := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Quota: config.Quota{
			Known: true, Allowed: 10, Remaining: 5, ObservedAt: observed,
		}},
		{UserID: "2", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": {}, "2": {}})

	res, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if res.Account.UserID != "1" {
		t.Fatalf("account = %s, want the observed one", res.Account.UserID)
	}
	known := (*saved)[len(*saved)-1].Accounts[0].Quota
	if !known.ObservedAt.After(observed) {
		t.Fatalf("observed at = %v, want it advanced past %v", known.ObservedAt, observed)
	}

	// The unknown half is pinned on recordSuccess itself: the download path
	// stamps ObservedAt earlier, when the server answers without numbers.
	m.recordSuccess("2", config.StrategyBalanced)
	unknown := (*saved)[len(*saved)-1].Accounts[1].Quota
	if !unknown.ObservedAt.IsZero() {
		t.Fatalf("unknown quota observed at = %v, want zero", unknown.ObservedAt)
	}
}

func TestResultAccountCarriesNoCookies(t *testing.T) {
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Cookies: map[string]string{"remix_userkey": "fixture-key"}},
	}, map[string]*fakeSession{"1": {}})

	res, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if len(res.Account.Cookies) != 0 {
		t.Fatalf("result cookies = %v, want none", res.Account.Cookies)
	}
	if got := m.Snapshot().Accounts[0].Cookies["remix_userkey"]; got != "fixture-key" {
		t.Fatalf("pool key = %q, the result must not clear the stored one", got)
	}
}

func TestSyncFailsWhenNoAccountCouldBeRefreshed(t *testing.T) {
	m := NewManager(config.Accounts{Accounts: []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
		{UserID: "2", Email: "two@example.com", Domain: "https://example.com"},
	}}, func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) })
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		return &failingProfileSession{inner: &fakeSession{}}, nil
	})
	m.SetPersist(func(config.Accounts) error { return nil })

	refreshed, err := m.Sync(context.Background(), nil)
	if refreshed != 0 {
		t.Fatalf("refreshed = %d, want 0", refreshed)
	}
	if err == nil {
		t.Fatal("Sync() error = nil, want the collected failures")
	}
	for _, name := range []string{"one@example.com", "two@example.com"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error = %q, want it to name %s", err, name)
		}
	}
}

func TestSyncReportsTheCountWhenSomeAccountsWork(t *testing.T) {
	m := NewManager(config.Accounts{Accounts: []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
		{UserID: "2", Email: "two@example.com", Domain: "https://example.com"},
	}}, func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) })
	m.SetSessionFactory(func(acct config.Account) (Session, error) {
		if acct.UserID == "1" {
			return &failingProfileSession{inner: &fakeSession{}}, nil
		}
		return &fakeSession{profile: zlib.UserProfile{
			QuotaKnown:    true,
			DownloadLimit: zlib.DownloadLimit{DailyAllowed: 10, DailyRemaining: 6},
		}}, nil
	})
	m.SetPersist(func(config.Accounts) error { return nil })

	refreshed, err := m.Sync(context.Background(), nil)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil when one account worked", err)
	}
	if refreshed != 1 {
		t.Fatalf("refreshed = %d, want 1", refreshed)
	}
	if got := m.Snapshot().Accounts[1].Quota.Remaining; got != 6 {
		t.Fatalf("remaining = %d, want the observed 6", got)
	}
}

func TestDownloadErrorNamesTheAccount(t *testing.T) {
	downloadErr := errors.New("connection reset")
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": {dlErr: downloadErr}})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if !errors.Is(err, downloadErr) {
		t.Fatalf("error = %v, want the underlying download error", err)
	}
	if !strings.Contains(err.Error(), "one@example.com") {
		t.Fatalf("error = %q, want it to name the account", err)
	}

	fetchErr := errors.New("book lookup failed")
	m, _ = newTestManager(t, []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": {fetchErr: fetchErr}})

	_, err = m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if !errors.Is(err, fetchErr) {
		t.Fatalf("error = %v, want the underlying fetch error", err)
	}
	if !strings.Contains(err.Error(), "one@example.com") {
		t.Fatalf("error = %q, want it to name the account", err)
	}
}

func TestRewriteUsesTheEnvDomain(t *testing.T) {
	t.Setenv(zlib.EnvDomain, "https://env.example.com")
	session := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://stored.example.com"},
	}, map[string]*fakeSession{"1": session})

	if _, err := m.Download(context.Background(), "", Options{
		Dir: "/tmp",
		URL: "https://other.example.com/eapi/book/1/abc/file",
	}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if session.lastURL != "https://env.example.com/eapi/book/1/abc/file" {
		t.Fatalf("url = %s, want the env domain", session.lastURL)
	}
}

func TestDownloadStopsAfterEveryAccountRejects(t *testing.T) {
	rejecting := &fakeSession{dlErr: zlib.ErrDailyLimitReached}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
		{UserID: "2", Domain: "https://example.com"},
		{UserID: "3", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": rejecting, "2": rejecting, "3": rejecting})

	var tried []string
	_, err := m.Download(context.Background(), "1:abc", Options{
		Dir: "/tmp",
		OnAttempt: func(acct config.Account, attempt int, reason error) {
			tried = append(tried, acct.UserID)
		},
	})
	var exhausted *AllExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("error = %v, want AllExhaustedError", err)
	}
	if len(tried) != 3 {
		t.Fatalf("attempts = %v, want exactly one per account", tried)
	}
	distinct := map[string]bool{}
	for _, id := range tried {
		if distinct[id] {
			t.Fatalf("attempts = %v, want one attempt per account", tried)
		}
		distinct[id] = true
	}
}

func TestUnknownStrategyIsRejected(t *testing.T) {
	session := &fakeSession{}
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com"},
	}, map[string]*fakeSession{"1": session})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp", Strategy: "fastest"})
	if err == nil || !strings.Contains(err.Error(), "fastest") {
		t.Fatalf("error = %v, want an unknown-strategy error", err)
	}
	if session.lastURL != "" {
		t.Fatalf("a download ran under an unknown strategy: %s", session.lastURL)
	}
}

func TestNegativeRoundRobinIndexIsClamped(t *testing.T) {
	m, _ := newTestManager(t, []config.Account{
		{UserID: "1", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
		{UserID: "2", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
	}, map[string]*fakeSession{"1": {}, "2": {}})
	m.data.RoundRobinIndex = -1

	res, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp", Strategy: config.StrategyRoundRobin})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if res.Account.UserID != "2" {
		t.Fatalf("account = %s, want account 2 (index -1 folded to 1)", res.Account.UserID)
	}
}

func TestRewriteHost(t *testing.T) {
	cases := []struct {
		name   string
		rawURL string
		domain string
		want   string
		wantOK bool
	}{
		{
			name:   "same host is left alone",
			rawURL: "https://example.com/eapi/book/1/abc/file",
			domain: "https://example.com",
			want:   "https://example.com/eapi/book/1/abc/file",
			wantOK: false,
		},
		{
			name:   "another host is rewritten",
			rawURL: "https://other.example.com/eapi/book/1/abc/file",
			domain: "https://mirror.example.com",
			want:   "https://mirror.example.com/eapi/book/1/abc/file",
			wantOK: true,
		},
		{
			name:   "a non-file path is account-bound",
			rawURL: "https://other.example.com/eapi/book/1/abc",
			domain: "https://mirror.example.com",
			want:   "https://other.example.com/eapi/book/1/abc",
			wantOK: false,
		},
		{
			name:   "an unparseable url is left alone",
			rawURL: "://bad",
			domain: "https://mirror.example.com",
			want:   "://bad",
			wantOK: false,
		},
		{
			name:   "an unparseable domain is left alone",
			rawURL: "https://other.example.com/eapi/book/1/abc/file",
			domain: "://bad",
			want:   "https://other.example.com/eapi/book/1/abc/file",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		got, ok := rewriteHost(tc.rawURL, tc.domain)
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("%s: rewriteHost() = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

// A fresh install has no pool, and `zlib download` must report the same
// sentinel the read commands use: without it the CLI exits 1 while `zlib
// profile` on the same machine exits 3.
func TestDownloadWithoutAccountsReportsNotLoggedIn(t *testing.T) {
	m, _ := newTestManager(t, nil, map[string]*fakeSession{})

	_, err := m.Download(context.Background(), "1:abc", Options{Dir: "/tmp"})
	if !errors.Is(err, zlib.ErrNotLoggedIn) {
		t.Fatalf("Download() error = %v, want ErrNotLoggedIn", err)
	}
	// The CLI words the remedy itself, so the message must not repeat it.
	if !strings.Contains(err.Error(), "no accounts saved") {
		t.Fatalf("error = %q, want it to say no accounts are saved", err)
	}
	if strings.Contains(err.Error(), "zlib login") {
		t.Fatalf("error = %q, the CLI adds the remedy itself", err)
	}
}

// A failed write is not fatal, but it must reach the download's warning channel:
// in the TUI nothing else reports it, so a read-only config directory would stop
// the quota cache from ever recording anything, silently.
func TestPersistFailureReachesTheDownloadWarning(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m, _ := newTestManager(t, []config.Account{{
		UserID: "1", Domain: "https://example.com",
		// Fresh enough that no quota refresh runs: the only write this download
		// attempts is the one recording its success.
		Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9, ObservedAt: now},
	}}, map[string]*fakeSession{"1": {}})
	m.SetPersist(func(config.Accounts) error { return errors.New("read-only config dir") })

	var warned []error
	res, err := m.Download(context.Background(), "1:abc", Options{
		Dir:    "/tmp",
		OnWarn: func(err error) { warned = append(warned, err) },
	})
	if err != nil {
		t.Fatalf("Download() error = %v, want the download to succeed", err)
	}
	if res.FilePath == "" {
		t.Fatalf("result = %+v, want the saved file", res)
	}
	if len(warned) != 1 {
		t.Fatalf("warnings = %v, want exactly one persist warning", warned)
	}
	if !strings.Contains(warned[0].Error(), "could not save account state") {
		t.Fatalf("warning = %q, want it to name the failed save", warned[0])
	}
}
