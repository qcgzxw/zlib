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
	// OnWarn receives non-fatal problems, including a failed persist, while the
	// download runs. It is called with the manager's mutex held: like SetPersist,
	// it must not call back into the manager.
	OnWarn func(err error)
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
	// activeWarn is the warning channel of the download in flight, so a failed
	// write reaches the UI that is already showing its other warnings.
	activeWarn    func(error)
	clientOptions []zlib.ClientOption
}

// NewManager builds a manager over already-loaded data. Production code uses
// Open; tests use this to inject a clock and a session factory.
func NewManager(data config.Accounts, now func() time.Time) *Manager {
	data.Normalize()
	// Own the slice and the credential maps: applyResetsLocked writes into the
	// slice, and mutating an account after construction must not reach the pool.
	accounts := make([]config.Account, len(data.Accounts))
	for i, acct := range data.Accounts {
		accounts[i] = cloneAccount(acct)
	}
	data.Accounts = accounts
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

// SetPersist replaces the writer. The callback runs while the manager holds
// its mutex, so it must not call back into the manager (Snapshot, Download,
// Sync, ...) or it will deadlock; it must also not retain the slice it is given.
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

// effectiveDomain is the host a session for this account talks to: ZLIB_DOMAIN
// wins over the stored domain, as it does for every other command. Both the
// client and the mirror rewrite have to agree on it.
func effectiveDomain(acct config.Account) string {
	if env := strings.TrimSpace(os.Getenv(zlib.EnvDomain)); env != "" {
		return env
	}
	return acct.Domain
}

// defaultSession builds a real client for one account. ZLIB_DOMAIN still wins
// over the domain stored with the account, as it does for every other command.
func (m *Manager) defaultSession(acct config.Account) (Session, error) {
	domain := effectiveDomain(acct)
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
	out.Accounts = make([]config.Account, len(m.data.Accounts))
	for i, acct := range m.data.Accounts {
		out.Accounts[i] = cloneAccount(acct)
	}
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
			return cloneAccount(acct), true
		}
	}
	if len(m.data.Accounts) > 0 {
		return cloneAccount(m.data.Accounts[0]), true
	}
	return config.Account{}, false
}

// cloneAccount deep-copies the credential map so a caller can keep or mutate
// what it is handed without touching the pool.
func cloneAccount(acct config.Account) config.Account {
	if acct.Cookies == nil {
		return acct
	}
	cookies := make(map[string]string, len(acct.Cookies))
	for name, value := range acct.Cookies {
		cookies[name] = value
	}
	acct.Cookies = cookies
	return acct
}

// Download runs the failover loop: pick an account, fetch and download with
// it, and move to the next one only when the quota or the session is rejected.
func (m *Manager) Download(ctx context.Context, ref string, opts Options) (Result, error) {
	if strings.TrimSpace(ref) == "" && strings.TrimSpace(opts.URL) == "" {
		return Result{}, errors.New("account: no book reference or URL to download")
	}
	m.mu.Lock()
	m.applyResetsLocked()
	// Hold the download's warning channel for the whole run: persistLocked fires
	// while the mutex is held, and the TUI has no other way to learn about a
	// failed write. The defer clears it so a later Sync does not warn into a
	// download that already finished.
	m.activeWarn = opts.OnWarn
	defer func() {
		m.mu.Lock()
		m.activeWarn = nil
		m.mu.Unlock()
	}()
	strategy, strategyErr := m.strategyLocked(opts.Strategy)
	count := len(m.data.Accounts)
	m.mu.Unlock()
	if strategyErr != nil {
		return Result{}, strategyErr
	}

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
			// The callback formats and logs the account: hand it a copy whose
			// credential map is its own, never the pool's live one.
			opts.OnAttempt(cloneAccount(acct), attempt, reason)
		}
		result, err := m.attempt(ctx, acct, ref, opts)
		if err == nil {
			m.recordSuccess(acct.UserID, strategy)
			result.Account = acct
			// The caller gets the identity, not the credentials: Result is
			// printed, logged and serialized, and the session key must not travel
			// with it. Nilling the field of this copy leaves the pool intact.
			result.Account.Cookies = nil
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
			// A pinned account has no fallback: report the expired session
			// itself rather than claiming the whole pool is empty.
			if opts.Account != "" {
				return Result{}, err
			}
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
		return Result{}, fmt.Errorf("%s: %w", acct.Name(), err)
	}
	m.refreshIfStale(ctx, session, acct.UserID, opts.OnWarn)

	rawURL := strings.TrimSpace(opts.URL)
	original := ""
	name := ""
	if rawURL == "" {
		book, err := session.FetchBookContext(ctx, ref)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", acct.Name(), err)
		}
		if strings.TrimSpace(book.DownloadURL) == "" {
			return Result{}, fmt.Errorf("%s: no download URL available for book %s", acct.Name(), ref)
		}
		rawURL, name = book.DownloadURL, book.Name
		if opts.OnBook != nil {
			// Fire even when the title is empty: the callback means "metadata
			// arrived", and callers use it to prepare the destination.
			opts.OnBook(name)
		}
	} else {
		original = rawURL
		// The rewrite must land on the host the session is really talking to,
		// which ZLIB_DOMAIN may have overridden.
		if rewritten, ok := rewriteHost(rawURL, effectiveDomain(acct)); ok {
			rawURL = rewritten
		}
	}

	downloaded, err := session.DownloadWithContext(ctx, rawURL, opts.Dir, opts.Progress)
	// A host rewrite is a guess: if the mirror does not serve the path, retry
	// once on the original URL before blaming the account. Only a caller-supplied
	// URL has an original to fall back to; a fetched one would retry an empty
	// string.
	if err != nil && original != "" && rawURL != original && errors.Is(err, zlib.ErrDownloadFailed) {
		downloaded, err = session.DownloadWithContext(ctx, original, opts.Dir, opts.Progress)
	}
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", acct.Name(), err)
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
	// An account that was never observed is stale by definition: a quota that
	// is still unknown would otherwise stay unknown until a sync or a rejection.
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
		// Stamp the attempt so a server that never exposes a quota costs one
		// profile request per TTL instead of one per download.
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
// selector is given. One broken account does not abort the run: its failure is
// collected, and reported only when nothing at all could be refreshed.
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
	var failures []error
	for _, acct := range targets {
		session, err := m.newSession(acct)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", acct.Name(), err))
			continue
		}
		profile, err := session.GetProfileContext(ctx)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", acct.Name(), err))
			continue
		}
		if !profile.QuotaKnown {
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
	if refreshed == 0 && len(failures) > 0 {
		// Nothing worked: (0, nil) would read as "everything is current".
		return 0, errors.Join(failures...)
	}
	return refreshed, nil
}

func (m *Manager) strategyLocked(override string) (string, error) {
	trimmed := strings.TrimSpace(override)
	if trimmed == "" {
		return m.data.Strategy, nil
	}
	if config.ValidStrategy(trimmed) {
		return trimmed, nil
	}
	return "", fmt.Errorf("unknown strategy %q; use %q or %q", override, config.StrategyBalanced, config.StrategyRoundRobin)
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
		// A pin bypasses the empty gate and the relogin flag on purpose: the
		// caller asked for this account, so it gets one honest attempt and
		// reports its own error instead of a silent all-exhausted. It is never
		// asked twice: Download returns as soon as a pinned attempt fails.
		return acct, nil
	}
	if len(m.data.Accounts) == 0 {
		// Wrap the sentinel so every surface can exit 3 and name the remedy: the
		// CLI, the shell and the MCP server all word it themselves, so the
		// message itself stays a plain statement of the problem.
		return config.Account{}, fmt.Errorf("%w: no accounts saved", zlib.ErrNotLoggedIn)
	}
	if strategy == config.StrategyRoundRobin {
		size := len(m.data.Accounts)
		start := m.data.RoundRobinIndex % size
		if start < 0 {
			// A hand-edited file can hold a negative index; % keeps the sign, so
			// fold it back into range instead of indexing out of bounds.
			start += size
		}
		for offset := 0; offset < size; offset++ {
			acct := m.data.Accounts[(start+offset)%size]
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
	if acct.Quota.Known {
		if acct.Quota.Remaining > 0 {
			acct.Quota.Remaining--
		}
		// ObservedAt means "the server reported numbers", so it advances only on
		// a known quota. An unknown account keeps its zero value, which is what
		// makes the next attempt ask the server instead of waiting out a TTL.
		acct.Quota.ObservedAt = m.now()
	}
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
	err := m.persist(m.data)
	if err == nil {
		return
	}
	// Prefer the running download's channel: the plain path prints it and the
	// TUI shows it in the note line, so the failure is visible in both.
	if m.activeWarn != nil {
		m.activeWarn(fmt.Errorf("could not save account state: %w", err))
		return
	}
	if m.onPersistError != nil {
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
