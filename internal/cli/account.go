package cli

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	accounts "github.com/heartleo/zlib/internal/account"
	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
	"github.com/spf13/cobra"
)

var accountCmd = &cobra.Command{
	Use:   "account",
	Short: "Manage the saved account pool",
	Long:  "List, refresh, select and remove the Z-Library accounts downloads rotate through.",
}

var accountListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the saved accounts and their quota",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := accounts.Open()
		if err != nil {
			return err
		}
		pool := manager.Snapshot()
		if jsonFlag(cmd) {
			return printJSONTo(cmd.OutOrStdout(), accountListJSON(pool))
		}
		if len(pool.Accounts) == 0 {
			lipgloss.Fprintf(cmd.OutOrStdout(), "No accounts saved. Run `zlib login`.\n")
			return nil
		}
		for _, acct := range pool.Accounts {
			marker := " "
			if pool.DefaultAccount != "" && acct.UserID == pool.DefaultAccount {
				marker = "*"
			}
			quota := "quota unknown"
			if acct.Quota.Known {
				quota = fmt.Sprintf("%d/%d left", acct.Quota.Remaining, acct.Quota.Allowed)
			}
			extra := ""
			if !acct.Quota.ResetAt.IsZero() {
				extra += " resets " + acct.Quota.ResetAt.Format("2006-01-02 15:04")
			}
			if !acct.LastUsedAt.IsZero() {
				extra += " last used " + acct.LastUsedAt.Format("2006-01-02 15:04")
			}
			if acct.NeedsRelogin {
				extra += " needs re-login"
			}
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s %s  %s  %s%s\n", marker, acct.Name(), acct.Domain, quota, extra)
		}
		return nil
	},
}

var accountSyncCmd = &cobra.Command{
	Use:   "sync [email...]",
	Short: "Refresh the cached quota from the server",
	RunE: func(cmd *cobra.Command, args []string) error {
		manager, err := accounts.Open()
		if err != nil {
			return err
		}
		if len(manager.Snapshot().Accounts) == 0 {
			lipgloss.Fprintf(cmd.OutOrStdout(), "No accounts saved. Run `zlib login`.\n")
			return nil
		}
		refreshed, err := manager.Sync(cmd.Context(), args)
		if err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Refreshed %d account(s).\n", colorGreen(symbolSuccess), refreshed)
		return nil
	},
}

var accountUseCmd = &cobra.Command{
	Use:   "use <account>",
	Short: "Choose the account reads and pinned downloads use",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		acct, err := pool.Resolve(args[0])
		if err != nil {
			return err
		}
		if strings.TrimSpace(acct.UserID) == "" {
			// Upsert matches on the user id, so logging in again would append a
			// second entry with the same email instead of repairing this one.
			name := acct.Name()
			if name == "" {
				name = "<account>"
			}
			return fmt.Errorf("this account has no user id; remove it with `zlib account remove %s` and log in again", name)
		}
		pool.DefaultAccount = acct.UserID
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		if err := syncSessionMirror(pool); err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Default account is now %s.\n", colorGreen(symbolSuccess), acct.Name())
		return nil
	},
}

var accountRemoveCmd = &cobra.Command{
	Use:   "remove <account>",
	Short: "Remove a saved account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		index, err := pool.ResolveIndex(args[0])
		if err != nil {
			return err
		}
		// Delete by position: two entries can share a user id in a hand-edited
		// file, and removing by id would drop the one the user did not name.
		acct := pool.Accounts[index]
		pool.RemoveAt(index)
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		if len(pool.Accounts) == 0 {
			if err := syncSessionMirror(pool); err != nil {
				return err
			}
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s Removed %s. No accounts left; run `zlib login`.\n", colorFaint(symbolSuccess), acct.Name())
			return nil
		}
		if err := syncSessionMirror(pool); err != nil {
			return err
		}
		if pool.DefaultAccount == "" {
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s Removed %s. No saved account can be named as the default; run `zlib login`.\n", colorFaint(symbolSuccess), acct.Name())
			return nil
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Removed %s. Default is now %s.\n", colorGreen(symbolSuccess), acct.Name(), defaultName(pool))
		return nil
	},
}

// accountListJSONDoc is the --json shape of `account list`.
type accountListJSONDoc struct {
	Strategy  string               `json:"strategy"`
	ResetTime string               `json:"reset_time"`
	Default   string               `json:"default,omitempty"`
	Accounts  []accountListJSONRow `json:"accounts"`
}

type accountListJSONRow struct {
	Email      string `json:"email,omitempty"`
	Label      string `json:"label,omitempty"`
	UserID     string `json:"user_id"`
	Domain     string `json:"domain"`
	Known      bool   `json:"quota_known"`
	Allowed    int    `json:"daily_allowed"`
	Remaining  int    `json:"daily_remaining"`
	ResetAt    string `json:"reset_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	Default    bool   `json:"default"`
	NeedsLogin bool   `json:"needs_relogin,omitempty"`
}

func accountListJSON(pool config.Accounts) accountListJSONDoc {
	doc := accountListJSONDoc{
		Strategy:  pool.Strategy,
		ResetTime: pool.ResetTime,
		Default:   pool.DefaultAccount,
		// An empty pool must encode as [] so consumers can iterate the field
		// without a null check.
		Accounts: []accountListJSONRow{},
	}
	for _, acct := range pool.Accounts {
		row := accountListJSONRow{
			Email:      acct.Email,
			Label:      acct.Label,
			UserID:     acct.UserID,
			Domain:     acct.Domain,
			Known:      acct.Quota.Known,
			Allowed:    acct.Quota.Allowed,
			Remaining:  acct.Quota.Remaining,
			Default:    pool.DefaultAccount != "" && acct.UserID == pool.DefaultAccount,
			NeedsLogin: acct.NeedsRelogin,
		}
		if !acct.Quota.ResetAt.IsZero() {
			row.ResetAt = acct.Quota.ResetAt.Format(time.RFC3339)
		}
		if !acct.LastUsedAt.IsZero() {
			row.LastUsedAt = acct.LastUsedAt.Format(time.RFC3339)
		}
		doc.Accounts = append(doc.Accounts, row)
	}
	return doc
}

// syncSessionMirror keeps session.json pointing at the current default, so
// commands that still read it (profile, favorites, kindle) do not keep using a
// removed account. When the pool names no resolvable default — a hand-edited
// file, or entries without a user id — the mirror follows the first entry that
// carries cookies, because those commands only need cookies and a domain.
// Only a pool with nothing to mirror drops the file.
func syncSessionMirror(pool config.Accounts) error {
	if len(pool.Accounts) == 0 {
		_, err := removeMirror()
		return err
	}
	if pool.DefaultAccount != "" {
		if acct, err := pool.Resolve(pool.DefaultAccount); err == nil {
			return saveMirror(acct)
		}
	}
	for _, acct := range pool.Accounts {
		if len(acct.Cookies) > 0 {
			return saveMirror(acct)
		}
	}
	_, err := removeMirror()
	return err
}

// saveMirror writes one account's session to session.json.
func saveMirror(acct config.Account) error {
	return config.SaveSession(config.Session{Cookies: acct.Cookies, Domain: acct.Domain, Mode: string(zlib.ClientModeEAPI)})
}

// defaultName is the display label of the pool's default account, falling back
// to the stored id when the pool cannot resolve it.
func defaultName(pool config.Accounts) string {
	if acct, err := pool.Resolve(pool.DefaultAccount); err == nil {
		return acct.Name()
	}
	return pool.DefaultAccount
}

// saveAccount stores one account in the pool. It never steals the default
// unless asked to; logging into a second account is a step towards a pool, not
// a request to switch identity. An entry without a user id is refused, because
// every other rule here identifies accounts by it.
func saveAccount(acct config.Account, use bool) error {
	if strings.TrimSpace(acct.UserID) == "" {
		return fmt.Errorf("refusing to save an account without an id")
	}
	pool, err := config.LoadAccounts()
	if err != nil {
		return err
	}
	pool.Upsert(acct)
	// Repair a dangling default before writing: SaveAccounts would persist it
	// and the session mirror would then fail on a file that already changed.
	pool.EnsureDefault()
	if use {
		pool.DefaultAccount = acct.UserID
	}
	if err := config.SaveAccounts(pool); err != nil {
		return err
	}
	return syncSessionMirror(pool)
}

// accountConfigCmd changes the stored strategy and reset time, so neither is
// reachable only by hand-editing accounts.json.
var accountConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Show or change the selection strategy and reset time",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		pool, err := config.LoadAccounts()
		if err != nil {
			return err
		}
		strategy, _ := cmd.Flags().GetString("strategy")
		resetTime, _ := cmd.Flags().GetString("reset-time")
		if strategy == "" && resetTime == "" {
			lipgloss.Fprintf(cmd.OutOrStdout(), "strategy: %s\nreset time: %s\n", pool.Strategy, pool.ResetTime)
			return nil
		}
		if strategy != "" {
			if !config.ValidStrategy(strategy) {
				return fmt.Errorf("unknown strategy %q: use %s or %s", strategy, config.StrategyBalanced, config.StrategyRoundRobin)
			}
			pool.Strategy = strategy
		}
		if resetTime != "" {
			if _, ok := accounts.NextReset(resetTime, time.Now()); !ok {
				return fmt.Errorf("invalid reset time %q: use HH:MM", resetTime)
			}
			pool.ResetTime = resetTime
		}
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s strategy: %s, reset time: %s\n", colorGreen(symbolSuccess), pool.Strategy, pool.ResetTime)
		return nil
	},
}

// jsonFlag reports whether the command was asked for machine-readable output.
// Commands that do not declare the flag fall through to plain text, so callers
// need no per-command knowledge.
func jsonFlag(cmd *cobra.Command) bool {
	flag := cmd.Flags().Lookup("json")
	if flag == nil {
		return false
	}
	value, _ := cmd.Flags().GetBool("json")
	return value
}

func init() {
	accountListCmd.Flags().Bool("json", false, "Print accounts as JSON")
	accountConfigCmd.Flags().String("strategy", "", "balanced or round_robin")
	accountConfigCmd.Flags().String("reset-time", "", "Local wall-clock reset time, HH:MM")
	accountCmd.AddCommand(accountListCmd, accountSyncCmd, accountUseCmd, accountRemoveCmd, accountConfigCmd)
	rootCmd.AddCommand(accountCmd)
}
