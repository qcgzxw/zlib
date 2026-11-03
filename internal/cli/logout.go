package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/heartleo/zlib/internal/config"
	"github.com/spf13/cobra"
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove saved accounts",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		selectorFlag, _ := cmd.Flags().GetString("account")
		all, _ := cmd.Flags().GetBool("all")
		pool, err := config.LoadAccounts()
		if err != nil {
			// session.json is still credentials, so a pool file this build
			// cannot read must not be the reason they stay on disk. The common
			// trigger is a rollback from a newer build, which is exactly when
			// the user needs logout to work.
			_, mirrorErr := removeMirror()
			// The pool itself holds credentials, and leaving it in place leaves
			// every later command with the same error. Moving it aside is what
			// turns this from a dead end into a fresh start.
			moved, renameErr := quarantineAccounts()
			if renameErr == nil && moved != "" {
				lipgloss.Fprintf(cmd.OutOrStdout(), "%s Moved the unreadable accounts file to %s.\n", colorFaint(symbolInfo), moved)
			}
			if mirrorErr != nil || renameErr != nil {
				return errors.Join(err, mirrorErr, renameErr)
			}
			return err
		}
		if len(pool.Accounts) == 0 {
			// The pool can be emptied by something else — a hand-edited
			// accounts.json, or a command that failed after clearing it — and
			// session.json would then stay on disk holding credentials logout
			// was asked to remove.
			removed, err := removeMirror()
			if err != nil {
				return err
			}
			if !removed {
				lipgloss.Fprintf(cmd.OutOrStdout(), "Not logged in.\n")
				return nil
			}
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s Removed the saved session.\n", colorFaint(symbolSuccess))
			return nil
		}
		if all {
			pool.Accounts = nil
			pool.DefaultAccount = ""
			if err := config.SaveAccounts(pool); err != nil {
				return err
			}
			if err := syncSessionMirror(pool); err != nil {
				return err
			}
			lipgloss.Fprintf(cmd.OutOrStdout(), "%s Logged out.\n", colorFaint(symbolSuccess))
			return nil
		}
		// A bare `zlib logout` means the default account, as it always has.
		selector := strings.TrimSpace(selectorFlag)
		if selector == "" {
			selector = pool.DefaultAccount
		}
		if selector == "" {
			// Entries without a user id cannot be named, so no default was
			// chosen and there is nothing for a bare logout to remove.
			return fmt.Errorf("no default account; pass --account <email, label or user id>, or --all")
		}
		// Remove by position: a hand-edited file can hold two entries with the
		// same user id, and removing by id would drop the wrong one.
		index, err := pool.ResolveIndex(selector)
		if err != nil {
			return err
		}
		name := pool.Accounts[index].Name()
		pool.RemoveAt(index)
		if err := config.SaveAccounts(pool); err != nil {
			return err
		}
		if err := syncSessionMirror(pool); err != nil {
			return err
		}
		lipgloss.Fprintf(cmd.OutOrStdout(), "%s Logged out %s.\n", colorGreen(symbolSuccess), name)
		return nil
	},
}

// clearSavedSession removes every saved account and the session mirror.
func clearSavedSession() (bool, error) {
	pool, err := config.LoadAccounts()
	if err != nil {
		// Same reasoning as the command: an unreadable pool must not keep the
		// credentials on disk, and the corrupt file is still reported.
		removed, mirrorErr := removeMirror()
		moved, renameErr := quarantineAccounts()
		if mirrorErr != nil || renameErr != nil {
			return removed, errors.Join(err, mirrorErr, renameErr)
		}
		if moved != "" {
			// Only the shell calls this, and it shows the error on one line, so
			// the move has to travel in it: nothing here owns a writer the
			// full-screen program is not drawing over.
			return removed, fmt.Errorf("%w; moved the unreadable accounts file to %s", err, moved)
		}
		return removed, err
	}
	// The mirror can outlive the pool: a session.json from before the pool
	// existed, or one whose cookies are empty. The TUI asks this function
	// whether there was anything to clear.
	had := len(pool.Accounts) > 0
	// Drop the mirror first. Removing it is the step that can fail, and failing
	// after the pool was already emptied would report "could not log out" for a
	// logout that had in fact happened.
	removed, err := removeMirror()
	if err != nil {
		return had, err
	}
	pool.Accounts = nil
	pool.DefaultAccount = ""
	return had || removed, config.SaveAccounts(pool)
}

// removeMirror deletes session.json and reports whether a file was there.
// Logout is the command that promises credentials are gone, so it has to run
// even when the pool no longer names an account.
func removeMirror() (bool, error) {
	err := os.Remove(config.SessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// quarantineAccounts moves an accounts.json this build cannot read aside. It is
// a rename, never a delete: the file holds credentials, so whatever the user had
// stays recoverable, and the next command starts from an empty pool instead of
// the same unreadable file. An older .broken copy is replaced, which is fine —
// that one was unreadable too.
func quarantineAccounts() (string, error) {
	path := config.AccountsPath()
	target := path + ".broken"
	if err := os.Rename(path, target); err != nil {
		// The loader can fail for a file that is not there (a failed read of a
		// directory, say); there is then nothing to move.
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return target, nil
}

func init() {
	logoutCmd.Flags().String("account", "", "Remove one account by email, label or user id")
	logoutCmd.Flags().Bool("all", false, "Remove every saved account")
	logoutCmd.MarkFlagsMutuallyExclusive("all", "account")
}
