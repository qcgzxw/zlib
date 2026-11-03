package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	accounts "github.com/heartleo/zlib/internal/account"
	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
	"github.com/spf13/cobra"
)

// captureOutput swaps os.Stdout and os.Stderr for files while fn runs.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		outFile.Close()
		errFile.Close()
	}()
	fn()
	out, _ := os.ReadFile(outFile.Name())
	errOut, _ := os.ReadFile(errFile.Name())
	return string(out), string(errOut)
}

func newDownloadTestServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="Dune.epub"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func downloadTestCommand(t *testing.T, jsonOut bool) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	addDestDirFlag(cmd)
	cmd.Flags().Bool("json", false, "")
	if jsonOut {
		if err := cmd.Flags().Set("json", "true"); err != nil {
			t.Fatal(err)
		}
	}
	return cmd
}

func TestDownloadJSONPrintsOnlyTheResultOnStdout(t *testing.T) {
	isolateConfigHome(t)
	body := bytes.Repeat([]byte("x"), 4096)
	server := newDownloadTestServer(t, body)
	client := zlib.NewClient(zlib.WithDomain(server.URL))
	client.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "key"})
	dir := t.TempDir()
	// A relative --dir must still be reported as an absolute path.
	t.Chdir(dir)

	var runErr error
	stdout, stderr := captureOutput(t, func() {
		m := newDirectDownloadModel("Dune", server.URL+"/files/dune", ".", newManagerForClient(t, client))
		runErr = runDownload(m, false, downloadTestCommand(t, true))
	})
	if runErr != nil {
		t.Fatalf("runDownload error = %v", runErr)
	}

	var result downloadResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	want, _ := filepath.Abs(filepath.Join(dir, "Dune.epub"))
	if result.Path != want || result.Size != int64(len(body)) || result.Name != "Dune" || result.Kindle != nil {
		t.Fatalf("result = %+v, want path %s", result, want)
	}
	if !strings.Contains(stderr, "Downloading: Dune") || !strings.Contains(stderr, "100%") {
		t.Fatalf("progress missing from stderr: %q", stderr)
	}
}

// history --download fetches from a known URL; its JSON must still carry the
// id:hash reference like every other download.
func TestHistoryDirectDownloadJSONKeepsReference(t *testing.T) {
	isolateConfigHome(t)
	server := newDownloadTestServer(t, []byte("book"))
	client := zlib.NewClient(zlib.WithDomain(server.URL))
	client.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "key"})
	item := zlib.DownloadHistoryItem{ID: "7", Hash: "h1", Name: "Dune", DownloadURL: server.URL + "/files/dune"}

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runDownload(historyDirectDownload(item, item.Name, t.TempDir(), newManagerForClient(t, client)), false, downloadTestCommand(t, true))
	})
	var result downloadResult
	if runErr != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.ID != "7:h1" {
		t.Fatalf("error = %v, stdout = %q", runErr, stdout)
	}
}

func TestDownloadReportsKindleFailureAfterSavingAsPartial(t *testing.T) {
	isolateConfigHome(t)
	server := newDownloadTestServer(t, []byte("book"))
	client := zlib.NewClient(zlib.WithDomain(server.URL))
	client.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "key"})
	dir := t.TempDir()

	var runErr error
	stdout, _ := captureOutput(t, func() {
		m := newDirectDownloadModel("Dune", server.URL+"/files/dune", dir, newManagerForClient(t, client))
		runErr = runDownload(m, true, downloadTestCommand(t, true))
	})
	var delivery *kindleDeliveryError
	if !errors.As(runErr, &delivery) || exitCode(runErr) != exitPartial {
		t.Fatalf("error = %v, want a kindle delivery error", runErr)
	}
	var result downloadResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if result.Kindle == nil || result.Kindle.Sent || !strings.Contains(result.Kindle.Error, "kindle config not found") {
		t.Fatalf("kindle = %+v", result.Kindle)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
}

func TestPlainDownloadTextKeepsProgressOffStdout(t *testing.T) {
	isolateConfigHome(t)
	server := newDownloadTestServer(t, []byte("book"))
	client := zlib.NewClient(zlib.WithDomain(server.URL))
	client.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "key"})
	dir := t.TempDir()

	var runErr error
	stdout, stderr := captureOutput(t, func() {
		m := newDirectDownloadModel("Dune", server.URL+"/files/dune", dir, newManagerForClient(t, client))
		runErr = runDownload(m, false, downloadTestCommand(t, false))
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if strings.Contains(stdout, "Downloading") || !strings.Contains(stdout, "Saved to: "+filepath.Join(dir, "Dune.epub")) {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "Downloading: Dune") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestPlainDownloadStripsControlSequencesFromTitle(t *testing.T) {
	isolateConfigHome(t)
	server := newDownloadTestServer(t, []byte("book"))
	client := zlib.NewClient(zlib.WithDomain(server.URL))
	client.SetCookies(map[string]string{"remix_userid": "1", "remix_userkey": "key"})

	_, stderr := captureOutput(t, func() {
		m := newDirectDownloadModel("Dune\x1b]0;owned\x07\x1b[2J", server.URL+"/files/dune", t.TempDir(), newManagerForClient(t, client))
		if err := runDownload(m, false, downloadTestCommand(t, true)); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(stderr, "\x1b") || !strings.Contains(stderr, "Downloading: Dune") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestPlainProgressReportsEachTenPercentOnce(t *testing.T) {
	var out bytes.Buffer
	p := plainProgress{w: &out}
	for written := int64(0); written <= 1000; written += 7 {
		p.report(written, 1000)
	}
	p.report(1000, 1000)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 10 || !strings.HasPrefix(lines[0], "  10%") || !strings.HasPrefix(lines[9], "  100%") {
		t.Fatalf("progress lines = %q", lines)
	}

	out.Reset()
	p = plainProgress{w: &out}
	for written := int64(0); written <= 12<<20; written += 1 << 20 {
		p.report(written, 0)
	}
	if got := strings.Count(out.String(), "\n"); got != 2 {
		t.Fatalf("unknown-size progress = %q", out.String())
	}
}

// Every command that downloads must register the pool flags itself: reading an
// unregistered flag returns "" instead of an error, so a missing registration
// would silently ignore --account. The set is derived from the commands that
// offer --send-to-kindle, so a new download command is covered automatically.
func TestEveryDownloadingCommandAcceptsThePoolFlags(t *testing.T) {
	checked := 0
	for _, cmd := range rootCmd.Commands() {
		if cmd.Flags().Lookup("send-to-kindle") == nil {
			continue
		}
		checked++
		for _, flag := range []string{"account", "strategy"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Fatalf("%s does not accept --%s", cmd.Name(), flag)
			}
		}
	}
	if checked < 6 {
		t.Fatalf("checked %d commands, want at least 6", checked)
	}
}

// runPoolDownload refuses a command that never registered the flags, so the
// silent path above cannot come back unnoticed.
func TestRunPoolDownloadRejectsACommandWithoutPoolFlags(t *testing.T) {
	err := runPoolDownload(&cobra.Command{Use: "probe"}, false, func(*accounts.Manager) downloadModel {
		return downloadModel{}
	})
	if err == nil || !strings.Contains(err.Error(), "missing the pool flags") {
		t.Fatalf("error = %v, want a pool-flags guard", err)
	}
}

// A fresh install has no pool. `zlib download` must exit 3 like `zlib profile`
// does, not 1: scripts branch on the exit code to send the user to login.
func TestDownloadWithoutAccountsExitsNotLoggedIn(t *testing.T) {
	isolateConfigHome(t)

	_, err := executeRootForTest(t, "download", "1:abc", "--json")
	if err == nil {
		t.Fatal("download with an empty pool must fail")
	}
	if got := exitCode(err); got != exitAuth {
		t.Fatalf("exit code = %d, want %d (error = %v)", got, exitAuth, err)
	}
	if got := ansi.Strip(formatCLIError(err)); got != "✗ Error: not logged in. Run `zlib login` first." {
		t.Fatalf("formatted = %q", got)
	}
}

// A successful download whose state write failed must warn in both paths. In the
// TUI stderr is unreadable, so the note line is the only route for it.
func TestPersistFailureWarningReachesBothPaths(t *testing.T) {
	oneAccount := config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID: "1", Email: "one@example.com", Domain: "https://example.com",
			Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9},
		}},
	}
	sessions := func() map[string]*fakeDownloadSession {
		return map[string]*fakeDownloadSession{"1": {}}
	}
	// A read-only config directory: the download works, the quota cache cannot
	// record it.
	failPersist := func(manager *accounts.Manager) *accounts.Manager {
		manager.SetPersist(func(config.Accounts) error { return errors.New("read-only config dir") })
		return manager
	}

	var status bytes.Buffer
	plain := newDownloadModel("1:abc", t.TempDir(), failPersist(newFakeManagerWith(t, oneAccount, sessions())))
	if _, err := plain.runPlain(&status); err != nil {
		t.Fatalf("runPlain() error = %v", err)
	}
	if !strings.Contains(status.String(), "could not save account state") {
		t.Fatalf("status = %q, want the persist warning", status.String())
	}

	ui := driveDownload(t, newDownloadModel("1:abc", t.TempDir(), failPersist(newFakeManagerWith(t, oneAccount, sessions()))))
	if !strings.Contains(ui.note, "could not save account state") {
		t.Fatalf("status line = %q, want the persist warning", ui.note)
	}
	if ui.state != stateComplete {
		t.Fatalf("state = %d, want the download to finish despite the failed write", ui.state)
	}
}

// The manager rotates on an exhausted quota and on an expired session; the
// status line has to say which, or a relogin problem looks like a quota one.
func TestRotationReasonNamesWhatDroppedTheAccount(t *testing.T) {
	cases := []struct {
		reason error
		want   string
	}{
		{zlib.ErrDailyLimitReached, "Quota exhausted on one@example.com"},
		{zlib.ErrSessionExpired, "Session expired for one@example.com"},
		{errors.New("network reset"), "Switching from one@example.com"},
		{nil, "Switching from one@example.com"},
	}
	for _, tc := range cases {
		if got := rotationReason("one@example.com", tc.reason); got != tc.want {
			t.Fatalf("rotationReason(%v) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestPlainRotationLineNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason error
		want   string
	}{
		{"quota", zlib.ErrDailyLimitReached, "Quota exhausted on one@example.com; trying two@example.com"},
		{"session", zlib.ErrSessionExpired, "Session expired for one@example.com; trying two@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newFakeManager(t, map[string]*fakeDownloadSession{
				"1": {err: tc.reason},
				"2": {},
			})
			var status bytes.Buffer
			model := newDownloadModel("1:abc", t.TempDir(), manager)
			if _, err := model.runPlain(&status); err != nil {
				t.Fatalf("runPlain() error = %v", err)
			}
			if !strings.Contains(status.String(), tc.want) {
				t.Fatalf("status = %q, want %q", status.String(), tc.want)
			}
		})
	}
}

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

// A failed quota refresh is not fatal, but it must not be invisible either.
func TestQuotaRefreshWarningReachesBothPaths(t *testing.T) {
	oneAccount := config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID: "1", Email: "one@example.com", Domain: "https://example.com",
			Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9},
		}},
	}
	sessions := func() map[string]*fakeDownloadSession {
		return map[string]*fakeDownloadSession{"1": {profileErr: errors.New("profile unavailable")}}
	}

	var status bytes.Buffer
	plain := newDownloadModel("1:abc", t.TempDir(), newFakeManagerWith(t, oneAccount, sessions()))
	if _, err := plain.runPlain(&status); err != nil {
		t.Fatalf("runPlain() error = %v", err)
	}
	if !strings.Contains(status.String(), "profile unavailable") {
		t.Fatalf("status = %q, want the refresh warning", status.String())
	}

	ui := driveDownload(t, newDownloadModel("1:abc", t.TempDir(), newFakeManagerWith(t, oneAccount, sessions())))
	if !strings.Contains(ui.note, "profile unavailable") {
		t.Fatalf("status line = %q, want the refresh warning", ui.note)
	}
}

// Ctrl+C during the transfer is a cancel, not a failure: runDownload reports
// errCancelled and leaves the partial file cleanup to the library.
func TestPlainDownloadCancelReportsCancelled(t *testing.T) {
	isolateConfigHome(t)
	session := &fakeDownloadSession{block: true, started: make(chan struct{})}
	manager := newFakeManagerWith(t, config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID: "1", Email: "one@example.com", Domain: "https://example.com",
			Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9},
		}},
	}, map[string]*fakeDownloadSession{"1": session})

	done := make(chan error, 1)
	captureOutput(t, func() {
		go func() {
			done <- runDownload(newDownloadModel("1:abc", t.TempDir(), manager), false, downloadTestCommand(t, true))
		}()
		<-session.started
		// The equivalent of Ctrl+C: runPlain traps os.Interrupt for the transfer.
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Skipf("cannot signal this process: %v", err)
		}
		if err := proc.Signal(os.Interrupt); err != nil {
			t.Skipf("cannot deliver an interrupt on this platform: %v", err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, errCancelled) {
				t.Fatalf("error = %v, want errCancelled", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("download did not stop after the interrupt")
		}
	})
}

// The JSON document names the serving account and the attempt count, and never
// carries the credentials the pool holds.
func TestDownloadJSONReportsServingAccountWithoutCredentials(t *testing.T) {
	isolateConfigHome(t)
	manager := newFakeManagerWith(t, config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com",
				Cookies: map[string]string{"remix_userid": "1", "remix_userkey": "FAKE-KEY-ONE"},
				Quota:   config.Quota{Known: true, Allowed: 10, Remaining: 9}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com",
				Cookies: map[string]string{"remix_userid": "2", "remix_userkey": "FAKE-KEY-TWO"},
				Quota:   config.Quota{Known: true, Allowed: 10, Remaining: 5}},
		},
	}, map[string]*fakeDownloadSession{
		"1": {err: zlib.ErrDailyLimitReached},
		"2": {},
	})

	var runErr error
	stdout, _ := captureOutput(t, func() {
		runErr = runDownload(newDownloadModel("1:abc", t.TempDir(), manager), false, downloadTestCommand(t, true))
	})
	if runErr != nil {
		t.Fatalf("runDownload() error = %v", runErr)
	}
	var result downloadResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if result.Account != "two@example.com" || result.Attempts != 2 {
		t.Fatalf("result = account %q, attempts %d", result.Account, result.Attempts)
	}
	for _, secret := range []string{"FAKE-KEY", "remix_userid", "remix_userkey", "Cookies", "cookies"} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("stdout leaked %q: %s", secret, stdout)
		}
	}
}

// driveDownload runs a download model the way bubbletea's event loop does: run
// the command Update returned, feed the message back, repeat. A branch that
// returns a nil command — bookNameMsg or attemptMsg, say — shows up here as a
// chain that goes idle while the transfer is unfinished. The spinner ticker is
// left out (it never ends, and this starts from startCmd rather than Init).
func driveDownload(t *testing.T, m downloadModel) downloadModel {
	t.Helper()
	var model tea.Model = m
	queue := []tea.Cmd{m.startCmd()}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 2000 {
			t.Fatalf("download message chain did not finish after %d steps", steps)
		}
		cmd := queue[0]
		queue = queue[1:]
		msg := cmd()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		next, nextCmd := model.Update(msg)
		model = next
		if nextCmd != nil {
			queue = append(queue, nextCmd)
			continue
		}
		// Only the download's own messages have to keep the chain alive. A
		// progress animation that settles returns no command by design.
		switch msg.(type) {
		case startedMsg, bookNameMsg, attemptMsg, warnMsg, progressMsg:
			t.Fatalf("message chain went idle in state %d after %T before the download finished",
				model.(downloadModel).state, msg)
		}
	}
	return model.(downloadModel)
}

// The interactive path has to keep reading after every message: the manager
// sends an attempt message first, so a branch that returns no command strands
// the download before it starts.
func TestDownloadUIStreamsTheRotationIntoTheModel(t *testing.T) {
	cases := []struct {
		name  string
		build func(dir string, manager *accounts.Manager) downloadModel
		want  string // the title the model must end up showing
	}{
		{"reference", func(dir string, manager *accounts.Manager) downloadModel {
			return newDownloadModel("1:abc", dir, manager)
		}, "Book"},
		{"url", func(dir string, manager *accounts.Manager) downloadModel {
			return newDirectDownloadModel("Fixture", "https://example.com/eapi/book/1/abc/file", dir, manager)
		}, "Fixture"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := newFakeManager(t, map[string]*fakeDownloadSession{
				"1": {err: zlib.ErrDailyLimitReached},
				"2": {},
			})
			dm := driveDownload(t, tc.build(t.TempDir(), manager))
			if dm.state != stateComplete || dm.accountName != "two@example.com" || dm.attempts != 2 {
				t.Fatalf("model = state %d, account %q, attempts %d", dm.state, dm.accountName, dm.attempts)
			}
			if dm.savedPath == "" || dm.bookName != tc.want {
				t.Fatalf("model = path %q, title %q", dm.savedPath, dm.bookName)
			}
			if want := "Quota exhausted on one@example.com; trying two@example.com"; !strings.Contains(dm.note, want) {
				t.Fatalf("status line = %q, want %q", dm.note, want)
			}
			if !strings.Contains(dm.View().Content, "Quota exhausted on one@example.com") {
				t.Fatal("the rotation is not rendered")
			}
		})
	}
}

type fakeDownloadSession struct {
	err        error
	profileErr error
	block      bool
	started    chan struct{}
}

func (f *fakeDownloadSession) FetchBookContext(ctx context.Context, ref string) (zlib.Book, error) {
	return zlib.Book{Name: "Book", DownloadURL: "https://example.com/eapi/book/1/abc/file"}, nil
}

func (f *fakeDownloadSession) DownloadWithContext(ctx context.Context, url, dir string, progress func(written, total int64)) (zlib.DownloadResult, error) {
	if f.block {
		close(f.started)
		<-ctx.Done()
		return zlib.DownloadResult{}, ctx.Err()
	}
	if f.err != nil {
		return zlib.DownloadResult{}, f.err
	}
	if progress != nil {
		progress(10, 10)
	}
	return zlib.DownloadResult{FilePath: filepath.Join(dir, "book.epub"), Size: 10}, nil
}

func (f *fakeDownloadSession) GetProfileContext(ctx context.Context) (zlib.UserProfile, error) {
	return zlib.UserProfile{}, f.profileErr
}

// newFakeManager builds a pool whose first balanced pick is the failing
// account, so the test observes exactly one rotation.
func newFakeManager(t *testing.T, sessions map[string]*fakeDownloadSession) *accounts.Manager {
	t.Helper()
	return newFakeManagerWith(t, config.Accounts{
		Strategy:       config.StrategyBalanced,
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{
			{UserID: "1", Email: "one@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 9}},
			{UserID: "2", Email: "two@example.com", Domain: "https://example.com", Quota: config.Quota{Known: true, Allowed: 10, Remaining: 5}},
		},
	}, sessions)
}

func newFakeManagerWith(t *testing.T, data config.Accounts, sessions map[string]*fakeDownloadSession) *accounts.Manager {
	t.Helper()
	manager := accounts.NewManager(data, nil)
	manager.SetSessionFactory(func(acct config.Account) (accounts.Session, error) {
		return sessions[acct.UserID], nil
	})
	manager.SetPersist(func(config.Accounts) error { return nil })
	return manager
}

// newManagerForClient wraps an existing client in a one-account pool, for the
// tests that download from an httptest server.
func newManagerForClient(t *testing.T, client *zlib.Client) *accounts.Manager {
	t.Helper()
	data := config.Accounts{
		ResetTime:      "05:00",
		DefaultAccount: "1",
		Accounts: []config.Account{{
			UserID: "1",
			Email:  "reader@example.com",
			Domain: "https://example.com",
			Quota:  config.Quota{Known: true, Allowed: 10, Remaining: 10},
		}},
	}
	manager := accounts.NewManager(data, nil)
	manager.SetSessionFactory(func(config.Account) (accounts.Session, error) { return client, nil })
	manager.SetPersist(func(config.Accounts) error { return nil })
	return manager
}

// Metadata can arrive without a title. The screen still has to say which book
// is downloading instead of leaving the heading and the line empty.
func TestDownloadViewNamesAnUntitledBook(t *testing.T) {
	m := newDownloadModel("115066162:e9f13b", t.TempDir(), nil)
	m.state = stateDownloading
	if got := ansi.Strip(m.viewContent()); !strings.Contains(got, "Downloading: 115066162:e9f13b") {
		t.Fatalf("viewContent = %q", got)
	}
	if got := ansi.Strip(m.View().Content); !strings.Contains(got, "115066162:e9f13b") {
		t.Fatalf("view = %q", got)
	}

	// A direct-URL download has no id, so the wording must still read as one.
	direct := newDirectDownloadModel("", "https://example.com/file", t.TempDir(), nil)
	direct.state = stateDownloading
	if got := ansi.Strip(direct.viewContent()); !strings.Contains(got, "Downloading: this book") {
		t.Fatalf("direct viewContent = %q", got)
	}
}

// A rotation fetches the metadata again. If the next account answers without a
// title, the title already on screen must survive instead of flipping back to
// the id, and the model must still leave the fetching state.
func TestDownloadKeepsTheTitleAcrossARotation(t *testing.T) {
	m := newDownloadModel("115066162:e9f13b", t.TempDir(), nil)
	m.state = stateFetching

	model, _ := m.Update(bookNameMsg{"Dune"})
	m = model.(downloadModel)
	if m.bookName != "Dune" || m.state != stateDownloading {
		t.Fatalf("first metadata: bookName=%q state=%v", m.bookName, m.state)
	}

	model, _ = m.Update(bookNameMsg{""})
	m = model.(downloadModel)
	if m.bookName != "Dune" {
		t.Fatalf("empty metadata from the next attempt cleared the title: %q", m.bookName)
	}
	if got := ansi.Strip(m.viewContent()); !strings.Contains(got, "Downloading: Dune") {
		t.Fatalf("viewContent = %q", got)
	}
}
