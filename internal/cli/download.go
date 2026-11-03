package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	keys "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	accounts "github.com/heartleo/zlib/internal/account"
	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
	"github.com/spf13/cobra"
)

var downloadCmd = &cobra.Command{
	Use:   "download <book-id>",
	Short: "Download a book by ID",
	Long:  "Download a book by its id:hash ID from search, favorites, or history results.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := destDirFlag(cmd)
		if err != nil {
			return err
		}
		sendToKindle, _ := cmd.Flags().GetBool("send-to-kindle")
		return runPoolDownload(cmd, sendToKindle, func(manager *accounts.Manager) downloadModel {
			return newDownloadModel(args[0], dir, manager)
		})
	},
}

// downloadResult is the --json document a finished download prints.
type downloadResult struct {
	ID       string        `json:"id,omitempty"`
	Name     string        `json:"name,omitempty"`
	Path     string        `json:"path"`
	Size     int64         `json:"size"`
	Account  string        `json:"account,omitempty"`
	Attempts int           `json:"attempts,omitempty"`
	Kindle   *kindleResult `json:"kindle,omitempty"`
}

// addPoolFlags registers the account-pool flags on a command that downloads.
// Every command reaching runPoolDownload must call it: reading an unregistered
// flag returns an empty string silently, so a missing call would make
// --account a no-op instead of an error.
func addPoolFlags(cmd *cobra.Command) {
	cmd.Flags().String("account", "", "Pin the download to one saved account")
	cmd.Flags().String("strategy", "", "Selection strategy for this run: balanced or round_robin")
}

// runPoolDownload builds the download model over the account pool and runs it.
// The pool opens here rather than when the command starts: the listing paths of
// these commands never download, so an unreadable accounts.json must not break
// `zlib search --json` or `zlib history`.
func runPoolDownload(cmd *cobra.Command, sendToKindle bool, build func(*accounts.Manager) downloadModel) error {
	// Reading an unregistered flag returns "" rather than an error, so a command
	// wired here without addPoolFlags would quietly ignore --account.
	if cmd.Flags().Lookup("account") == nil || cmd.Flags().Lookup("strategy") == nil {
		return fmt.Errorf("internal error: %s is missing the pool flags", cmd.Name())
	}
	manager, err := loadManager()
	if err != nil {
		return err
	}
	m := build(manager)
	m.pinAccount, _ = cmd.Flags().GetString("account")
	m.strategy, _ = cmd.Flags().GetString("strategy")
	return runDownload(m, sendToKindle, cmd)
}

// kindleResult reports the Send to Kindle step of a download.
type kindleResult struct {
	Sent  bool   `json:"sent"`
	To    string `json:"to,omitempty"`
	Error string `json:"error,omitempty"`
}

// runDownload executes the download described by m — through the bubbletea
// progress UI on an interactive terminal, or a plain path otherwise (see
// isInteractive) — then reports the saved file and optionally delivers it to
// Kindle. Shared by the download, search, history, favorites and feed
// commands. With --json the plain path always runs, progress goes to stderr,
// and stdout carries only the downloadResult document.
func runDownload(m downloadModel, sendToKindle bool, cmd *cobra.Command) error {
	jsonOut := false
	if cmd.Flags().Lookup("json") != nil {
		jsonOut, _ = cmd.Flags().GetBool("json")
	}
	if !jsonOut {
		dir, proceed, err := chooseDownloadDirectory(cmd, m.dir)
		if err != nil {
			return err
		}
		if !proceed {
			return errCancelled
		}
		m.dir = dir
	}
	var dm downloadModel
	var err error
	if jsonOut || !isInteractive() {
		dm, err = m.runPlain(os.Stderr)
	} else {
		dm, err = runDownloadProgram(m)
	}
	if err != nil {
		return err
	}
	if dm.state == stateCancelled {
		return errCancelled
	}
	if dm.err != nil {
		return dm.err
	}

	path := dm.savedPath
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	result := downloadResult{
		ID:       dm.id,
		Name:     dm.bookName,
		Path:     path,
		Size:     dm.savedSize,
		Account:  dm.accountName,
		Attempts: dm.attempts,
	}
	if !jsonOut {
		lipgloss.Fprintln(os.Stdout)
		lipgloss.Fprintf(os.Stdout, "%s Saved to: %s (%d bytes)\n",
			colorGreen(symbolSuccess),
			colorGreen(path),
			dm.savedSize,
		)
		if dm.attempts > 1 {
			lipgloss.Fprintf(os.Stdout, "%s Served by %s after %d attempts.\n",
				colorFaint(symbolInfo), safeText(dm.accountName), dm.attempts)
		}
	}
	var deliveryErr error
	if sendToKindle {
		to, err := sendDownloadedFileToKindle(path, !jsonOut)
		result.Kindle = &kindleResult{Sent: err == nil, To: to}
		if err != nil {
			result.Kindle.Error = err.Error()
			deliveryErr = &kindleDeliveryError{path: path, err: err}
		}
	}
	if jsonOut {
		if err := printJSON(result); err != nil {
			return err
		}
	}
	return deliveryErr
}

// runDownloadProgram runs the download through the bubbletea progress UI.
func runDownloadProgram(m downloadModel) (downloadModel, error) {
	result, err := tea.NewProgram(m).Run()
	if err != nil {
		return downloadModel{}, err
	}
	return result.(downloadModel), nil
}

// runPlain performs the download without the bubbletea UI, for non-terminal
// output and --json. It mirrors the flow of the UI model and records the result
// on the returned model. Status and progress lines go to status (stderr),
// keeping stdout for the result. Ctrl+C cancels the transfer, so the library can
// clean up its partial file before the command exits.
//
// The trap covers the fetch as well, since the manager fetches and downloads in
// one call: a second Ctrl+C during a hung fetch does not reach the process. That
// is accepted rather than restructuring the signal handling.
func (m downloadModel) runPlain(status io.Writer) (downloadModel, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	progress := plainProgress{w: status}
	headerPrinted := false
	printHeader := func(name string) {
		if headerPrinted || name == "" {
			return
		}
		// The title comes from the server; bookCell strips control sequences.
		fmt.Fprintf(status, "Downloading: %s\n", bookCell(name))
		m.bookName = name
		headerPrinted = true
	}
	if m.directURL != "" {
		// A known URL carries its title from the caller, not from the server.
		printHeader(m.bookName)
	}

	result, err := m.manager.Download(ctx, m.id, accounts.Options{
		Dir:      m.dir,
		URL:      m.directURL,
		Account:  m.pinAccount,
		Strategy: m.strategy,
		Progress: progress.report,
		OnBook:   printHeader,
		OnAttempt: func(acct config.Account, attempt int, reason error) {
			if attempt > 1 {
				fmt.Fprintf(status, "  %s\n", rotationNote(m.accountName, acct.Name(), reason))
				progress.reported = 0
			}
			m.accountName = acct.Name()
		},
		OnWarn: func(warn error) {
			fmt.Fprintf(status, "  %s\n", safeText(warn.Error()))
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			m.state = stateCancelled
			return m, nil
		}
		return m, err
	}
	if result.BookName != "" {
		m.bookName = result.BookName
	}
	m.savedPath = result.FilePath
	m.savedSize = result.Size
	m.accountName = result.Account.Name()
	m.attempts = result.Attempts
	m.state = stateComplete
	return m, nil
}

// rotationReason words why the previous account was dropped. The manager
// rotates on an exhausted quota and on an expired session, so blaming the quota
// every time would misreport half the rotations.
func rotationReason(account string, reason error) string {
	name := bookCell(account)
	switch {
	case errors.Is(reason, zlib.ErrDailyLimitReached):
		return "Quota exhausted on " + name
	case errors.Is(reason, zlib.ErrSessionExpired):
		return "Session expired for " + name
	default:
		return "Switching from " + name
	}
}

// rotationNote is the one wording both paths report a rotation with, so the
// plain log and the full-screen status line cannot drift apart.
func rotationNote(from, to string, reason error) string {
	return rotationReason(from, reason) + "; trying " + bookCell(to)
}

// plainProgress prints download progress as plain lines a log or an agent can
// follow: one line per 10% when the size is known, otherwise one per
// plainProgressChunk bytes.
//
// It lives beside runPlain and rotationReason because they are the plain path;
// if this file grows again, those three are the natural split.
type plainProgress struct {
	w        io.Writer
	reported int64 // last percent step, or bytes when the total is unknown
}

const plainProgressChunk = 5 << 20

func (p *plainProgress) report(written, total int64) {
	const mb = 1 << 20
	if total > 0 {
		step := written * 100 / total / 10 * 10
		if step > p.reported {
			p.reported = step
			fmt.Fprintf(p.w, "  %d%% (%.1f MB / %.1f MB)\n", step, float64(written)/mb, float64(total)/mb)
		}
		return
	}
	if written-p.reported >= plainProgressChunk {
		p.reported = written
		fmt.Fprintf(p.w, "  %.1f MB\n", float64(written)/mb)
	}
}

func init() {
	addDestDirFlag(downloadCmd)
	addPoolFlags(downloadCmd)
	downloadCmd.Flags().Bool("send-to-kindle", false, "Send the downloaded book to Kindle")
	downloadCmd.Flags().Bool("json", false, "Print the saved file as JSON; progress goes to stderr")
}

// — bubbletea model —

type downloadState int

const (
	stateFetching downloadState = iota
	stateDownloading
	stateComplete
	stateCancelled
)

type downloadModel struct {
	width, height  int
	id             string
	dir            string
	manager        *accounts.Manager
	accountName    string
	attempts       int
	pinAccount     string
	strategy       string
	state          downloadState
	bookName       string
	directURL      string // set to skip fetch and download immediately
	note           string // latest rotation or quota warning, shown while downloading
	spinner        spinner.Model
	progress       progress.Model
	written        int64
	total          int64
	savedPath      string
	savedSize      int64
	stayOnComplete bool
	completeOffset int
	completeAction int
	sendToKindle   bool
	kindleMessage  string
	err            error
	downloadCh     chan tea.Msg
	cancel         context.CancelFunc
}

type bookNameMsg struct{ name string }
type attemptMsg struct {
	account string
	attempt int
	reason  error
}
type startedMsg struct {
	ch     chan tea.Msg
	cancel context.CancelFunc
}
type progressMsg struct{ written, total int64 }
type warnMsg struct{ err error }
type downloadDoneMsg struct {
	path     string
	size     int64
	account  string
	attempts int
}
type errMsg struct{ err error }

func newDownloadModel(id, dir string, manager *accounts.Manager) downloadModel {
	m := downloadModel{id: id, dir: dir, manager: manager}
	m.spinner = spinner.New()
	m.spinner.Spinner = spinner.Dot
	m.spinner.Style = lipgloss.NewStyle().Foreground(currentTheme.Accent)
	m.progress = progress.New(progress.WithDefaultBlend(), progress.WithWidth(40))
	return m
}

// newDirectDownloadModel skips the fetch phase and downloads from a known URL.
func newDirectDownloadModel(bookName, downloadURL, dir string, manager *accounts.Manager) downloadModel {
	m := newDownloadModel("", dir, manager)
	m.bookName = bookName
	m.directURL = downloadURL
	return m
}

func (m downloadModel) Init() tea.Cmd {
	if m.state == stateComplete {
		return nil
	}
	return tea.Batch(m.spinner.Tick, m.startCmd())
}

// startCmd hands the message channel to Update through startedMsg; Update then
// streams from it with waitForMsg. Building the channel inside the command
// keeps the model copy-free while still letting Update own the cancel func.
func (m downloadModel) startCmd() tea.Cmd {
	return func() tea.Msg {
		ch := make(chan tea.Msg, 64)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			defer close(ch)
			emit := func(msg tea.Msg) {
				select {
				case ch <- msg:
				case <-ctx.Done():
				}
			}
			result, err := m.manager.Download(ctx, m.id, accounts.Options{
				Dir:      m.dir,
				URL:      m.directURL,
				Account:  m.pinAccount,
				Strategy: m.strategy,
				Progress: func(written, total int64) { emit(progressMsg{written, total}) },
				OnBook:   func(name string) { emit(bookNameMsg{name}) },
				OnAttempt: func(acct config.Account, attempt int, reason error) {
					emit(attemptMsg{account: acct.Name(), attempt: attempt, reason: reason})
				},
				OnWarn: func(err error) { emit(warnMsg{err}) },
			})
			if err != nil {
				emit(errMsg{err})
				return
			}
			emit(downloadDoneMsg{
				path:     result.FilePath,
				size:     result.Size,
				account:  result.Account.Name(),
				attempts: result.Attempts,
			})
		}()
		return startedMsg{ch: ch, cancel: cancel}
	}
}

// waitForMsg returns a Cmd that reads the next message from the download channel.
func waitForMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

// nextMsg keeps the download's message chain alive. Update only ever sees the
// next manager message because the branch that handled the previous one returns
// a command that reads one: a branch returning a nil command freezes the UI on
// the first attempt message and leaves the download goroutine blocked on its
// buffer. A nil channel — a model driven without startCmd, as several view tests
// are — stays idle instead of blocking forever.
func (m downloadModel) nextMsg() tea.Cmd {
	if m.downloadCh == nil {
		return nil
	}
	return waitForMsg(m.downloadCh)
}

func (m downloadModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.progress.SetWidth(max(1, min(60, msg.Width-8)))
		return m, nil

	case errMsg:
		m.err = msg.err
		return m, tea.Quit

	case startedMsg:
		m.downloadCh = msg.ch
		m.cancel = msg.cancel
		// A known URL already has its title, so the transfer is all that is left.
		// Otherwise the model keeps its "Fetching book" spinner until the server
		// names the book.
		if m.directURL != "" {
			m.state = stateDownloading
		}
		return m, m.nextMsg()

	case bookNameMsg:
		// A rotation fetches the metadata again; when the next account answers
		// without a title, the one already on screen is better than the id.
		if msg.name != "" {
			m.bookName = msg.name
		}
		if m.state == stateFetching {
			m.state = stateDownloading
		}
		return m, m.nextMsg()

	case attemptMsg:
		if msg.attempt > 1 {
			m.attempts = msg.attempt
			m.written, m.total = 0, 0
			// Same wording as the plain path, and the bar resets with the
			// counters: the next attempt must not start at the previous
			// account's percentage.
			m.note = rotationNote(m.accountName, msg.account, msg.reason)
			m.accountName = msg.account
			return m, tea.Batch(m.progress.SetPercent(0), m.nextMsg())
		}
		m.accountName = msg.account
		return m, m.nextMsg()

	case warnMsg:
		m.note = safeText(msg.err.Error())
		return m, m.nextMsg()

	case tea.KeyPressMsg:
		if m.state == stateComplete && m.stayOnComplete {
			if !m.canSendToKindle() {
				m.completeAction = 0
			}
			switch msg.String() {
			case "enter":
				m.sendToKindle = m.completeAction == 1 && m.canSendToKindle()
				return m, tea.Quit
			case "esc", "q", "ctrl+c":
				m.sendToKindle = false
				return m, tea.Quit
			case "left", "shift+tab":
				if m.canSendToKindle() {
					m.completeAction = (m.completeAction + 1) % 2
				}
			case "right", "tab":
				if m.canSendToKindle() {
					m.completeAction = (m.completeAction + 1) % 2
				}
			case "up", "k":
				m.completeOffset--
			case "down", "j":
				m.completeOffset++
			case "pgup":
				m.completeOffset -= max(1, m.height-6)
			case "pgdown":
				m.completeOffset += max(1, m.height-6)
			}
			m.completeOffset = min(max(0, m.completeOffset), m.maxCompleteOffset())
			return m, nil
		}
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			if m.cancel != nil {
				m.cancel()
			}
			m.state = stateCancelled
			return m, tea.Quit
		}

	case progressMsg:
		m.written = msg.written
		m.total = msg.total
		var pct float64
		if m.total > 0 {
			pct = float64(m.written) / float64(m.total)
		}
		progressCmd := m.progress.SetPercent(pct)
		return m, tea.Batch(progressCmd, m.nextMsg())

	case downloadDoneMsg:
		m.savedPath = msg.path
		if path, err := filepath.Abs(msg.path); err == nil {
			m.savedPath = path
		}
		m.savedSize = msg.size
		m.state = stateComplete
		m.accountName, m.attempts = msg.account, msg.attempts
		if m.stayOnComplete {
			return m, nil
		}
		return m, tea.Quit

	case progress.FrameMsg:
		pm, cmd := m.progress.Update(msg)
		m.progress = pm
		return m, cmd

	case spinner.TickMsg:
		if m.state == stateFetching {
			sm, cmd := m.spinner.Update(msg)
			m.spinner = sm
			return m, cmd
		}
	}
	return m, nil
}

func (m downloadModel) View() tea.View {
	if m.state == stateComplete && m.stayOnComplete {
		return m.completeView()
	}
	return screenView(m.width, m.height, "Download", bookCell(m.downloadTitle()), m.viewContent(), "Directory: "+bookCell(m.dir), uiKey("esc", "cancel"))
}

// downloadTitle names the book in the UI. Metadata can arrive without a title,
// so the id the user asked for stands in for it rather than an empty heading.
func (m downloadModel) downloadTitle() string {
	if m.bookName != "" {
		return m.bookName
	}
	if m.id != "" {
		return m.id
	}
	return "this book"
}

func (m downloadModel) completeLines() []string {
	path := strings.NewReplacer("\r", " ", "\n", " ").Replace(ansi.Strip(m.savedPath))
	return append([]string{"Saved to:"}, strings.Split(ansi.Hardwrap(path, max(1, m.width-2), false), "\n")...)
}

func (m downloadModel) maxCompleteOffset() int {
	return max(0, len(m.completeLines())-m.completePathRows())
}

func (m downloadModel) canSendToKindle() bool {
	return slices.Contains(kindleAllowedTypes, strings.ToLower(filepath.Ext(m.savedPath)))
}

func (m downloadModel) completeActions() []string {
	if m.canSendToKindle() {
		return []string{"Back", "Send to Kindle"}
	}
	return []string{"Back"}
}

func (m downloadModel) completePathRows() int {
	return max(1, m.height-6-len(actionButtonRows(m.completeActions(), m.completeAction+1, m.width))-1)
}

func (m downloadModel) completeView() tea.View {
	lines := m.completeLines()
	buttons := actionButtonRows(m.completeActions(), m.completeAction+1, m.width)
	visible := m.completePathRows()
	start := min(max(0, m.completeOffset), max(0, len(lines)-visible))
	status := m.kindleMessage
	if status == "" && len(lines) > visible {
		status = fmt.Sprintf("Path lines %d–%d of %d", start+1, min(start+visible, len(lines)), len(lines))
	}
	bindings := []keys.Binding{uiKey("enter", "back")}
	if m.canSendToKindle() {
		bindings = []keys.Binding{uiKey("←/→", "select"), uiKey("enter", "open")}
	}
	if len(lines) > visible {
		bindings = append(bindings, uiKey("↑/↓", "scroll"))
	}
	body := append([]string{}, lines[start:min(len(lines), start+visible)]...)
	body = append(body, "")
	body = append(body, buttons...)
	return screenView(m.width, m.height, "Download complete", bookCell(m.downloadTitle()), strings.Join(body, "\n"), status,
		bindings...)
}

func (m downloadModel) viewContent() string {
	switch m.state {
	case stateFetching:
		return fmt.Sprintf("  %s Fetching book %s...\n",
			m.spinner.View(), colorCyan(m.id))
	case stateDownloading, stateComplete, stateCancelled:
		writtenMB := float64(m.written) / 1024 / 1024
		totalMB := float64(m.total) / 1024 / 1024
		var sizeInfo string
		if m.total > 0 {
			sizeInfo = fmt.Sprintf("  %.1f MB / %.1f MB", writtenMB, totalMB)
		} else {
			sizeInfo = fmt.Sprintf("  %.1f MB", writtenMB)
		}
		bar := m.progress.View()
		if m.state == stateComplete {
			bar = m.progress.ViewAs(1.0)
		}
		out := fmt.Sprintf("  Downloading: %s\n  %s%s\n",
			bookCell(m.downloadTitle()), bar, colorFaint(sizeInfo))
		if m.note != "" {
			out += fmt.Sprintf("\n  %s\n", colorFaint(m.note))
		}
		if m.state == stateCancelled {
			out += fmt.Sprintf("\n  %s %s\n",
				colorRed(symbolError),
				colorRed("Download cancelled (incomplete file removed)"))
		}
		return out
	}
	return ""
}
