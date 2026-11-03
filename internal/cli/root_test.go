package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/heartleo/zlib/internal/zlib"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestFormatCLIErrorConvertsEOFToFriendlyMessage(t *testing.T) {
	got := formatCLIError(errors.New(`Get "https://untrusted.example/s/%E4%B8%89%E4%BD%93?&page=1": EOF`))
	want := "✗ Error: network request failed. Please check your connection and try again."
	if ansi.Strip(got) != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestFormatCLIErrorConvertsWrappedEOFToFriendlyMessage(t *testing.T) {
	got := formatCLIError(errors.Join(errors.New("failed to fetch book"), io.EOF))
	want := "✗ Error: network request failed. Please check your connection and try again."
	if ansi.Strip(got) != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestFormatCLIErrorLeavesNonNetworkErrorsUntouched(t *testing.T) {
	got := formatCLIError(errors.New("search query is empty"))
	want := "✗ Error: search query is empty"
	if ansi.Strip(got) != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestFormatCLIErrorKeepsSMTPCause(t *testing.T) {
	got := formatCLIError(fmt.Errorf("%w: smtp.163.com:587 (smtp_security starttls): %w", zlib.ErrSMTPSend, io.EOF))
	want := "✗ Error: zlibrary: smtp send failed: smtp.163.com:587 (smtp_security starttls): EOF"
	if ansi.Strip(got) != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func executeRootForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	// Cobra builds the generated `completion` command once and binds it to the
	// writer that was current at that first ExecuteC, so whichever test runs
	// first would keep every later completion script for itself. Dropping the
	// cached command makes ExecuteC rebuild it against this call's writer.
	dropCachedCompletionCmd()
	// Flag values live on the shared command tree, so a test that passes
	// --json or --strategy would otherwise decide what the next test sees no
	// matter which arguments it gives, and shuffling the run order would
	// change the outcome.
	resetCommandFlags(rootCmd)
	t.Cleanup(func() {
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		rootCmd.SetArgs(nil)
	})

	_, err := rootCmd.ExecuteC()
	if errOut.Len() > 0 {
		t.Logf("stderr: %s", errOut.String())
	}
	return out.String(), err
}

// dropCachedCompletionCmd removes the generated `completion` command cobra
// added on an earlier ExecuteC, so the next one builds a fresh copy.
func dropCachedCompletionCmd() {
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "completion" {
			rootCmd.RemoveCommand(sub)
		}
	}
}

// resetCommandFlags restores every flag in the tree to its default value.
func resetCommandFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		// A parse failure here would be a bad default in the command itself,
		// and the next parse reports it as the user-facing flag error.
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
	c.Flags().VisitAll(reset)
	c.PersistentFlags().VisitAll(reset)
	for _, sub := range c.Commands() {
		resetCommandFlags(sub)
	}
}

func TestHiddenCompletionCommandGeneratesZshScript(t *testing.T) {
	out, err := executeRootForTest(t, "completion", "zsh")
	if err != nil {
		t.Fatalf("completion zsh error = %v", err)
	}
	if !strings.Contains(out, "#compdef zlib") {
		t.Fatalf("completion zsh output missing #compdef zlib header")
	}
}

func TestHiddenCompletionCommandDoesNotAppearInHelp(t *testing.T) {
	out, err := executeRootForTest(t, "--help")
	if err != nil {
		t.Fatalf("--help error = %v", err)
	}
	if strings.Contains(out, "completion") {
		t.Fatalf("hidden completion command should not appear in help:\n%s", out)
	}
}

func TestResolveClientDomain(t *testing.T) {
	tests := []struct {
		name          string
		envDomain     string
		sessionDomain string
		want          string
	}{
		{
			name:          "env wins over a stale session domain",
			envDomain:     "https://mirror.invalid",
			sessionDomain: "https://z-lib.sk",
			want:          "https://mirror.invalid",
		},
		{
			name:          "session domain applies when env is unset",
			envDomain:     "",
			sessionDomain: "https://z-lib.sk",
			want:          "https://z-lib.sk",
		},
		{
			name:          "blank env is treated as unset",
			envDomain:     "   ",
			sessionDomain: "https://z-lib.sk",
			want:          "https://z-lib.sk",
		},
		{
			name:          "nothing to apply when both are empty",
			envDomain:     "",
			sessionDomain: "",
			want:          "",
		},
		{
			name:          "env applies when there is no session domain",
			envDomain:     "https://mirror.invalid",
			sessionDomain: "",
			want:          "https://mirror.invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveClientDomain(tt.envDomain, tt.sessionDomain); got != tt.want {
				t.Fatalf("resolveClientDomain(%q, %q) = %q, want %q", tt.envDomain, tt.sessionDomain, got, tt.want)
			}
		})
	}
}
