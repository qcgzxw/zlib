package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
	"github.com/spf13/cobra"
)

// registerResendCooldown is how long the resend button stays disabled after a
// code was sent, so the flow cannot flood the address it registers.
const registerResendCooldown = 60 * time.Second

// registerCmd creates a new Z-Library account through the web registration
// flow: email + password + an emailed verification code, then the session is
// saved exactly like a login.
var registerCmd = &cobra.Command{
	Use:   "register",
	Short: "Create a new account",
	Long:  `Create a new Z-Library account. A verification code is emailed to the address given; enter it when prompted and the new session is saved locally.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		email, _ := cmd.Flags().GetString("email")
		password, _ := cmd.Flags().GetString("password")
		name, _ := cmd.Flags().GetString("name")
		domainFlag, _ := cmd.Flags().GetString("domain")
		domain, err := resolveLoginDomain(domainFlag)
		if err != nil {
			return err
		}

		promptNeeded := email == "" || password == ""
		if promptNeeded {
			fields, accepted, err := runInlineForm(newInlineForm("Create an account", "Register", []formField{
				{label: "Email", value: email, hint: "The verification code is emailed here", validate: requiredField("email")},
				{label: "Password", value: password, secret: true, hint: "Password is not saved", validate: requiredField("password")},
			}))
			if err != nil {
				return err
			}
			if !accepted {
				return nil
			}
			email, password = fields[0].value, fields[1].value
		}

		resolved, err := resolveEAPIDomain(cmd.Context(), domain)
		if err != nil {
			return err
		}
		if resolved != domain {
			lipgloss.Fprintf(os.Stdout, "%s %s redirects to %s; registering there.\n", colorCyan(symbolInfo), domain, resolved)
			domain = resolved
		}

		opts := zlib.RegisterOptions{Email: email, Password: password, Name: name}
		c := zlib.NewClient()
		c.SetDomain(domain)

		var lastSent time.Time
		sendCode := func() error {
			lipgloss.Fprintf(os.Stdout, "%s Sending verification code to %s...\n", colorCyan(symbolInfo), email)
			if err := c.SendRegistrationCode(cmd.Context(), opts); err != nil {
				return err
			}
			lastSent = time.Now()
			return nil
		}
		if err := sendCode(); err != nil {
			return err
		}

		// promptCode asks for the verification code. A rejected code is not
		// fatal: the server keeps the registration pending, so the prompt
		// re-opens with the server's message and, in the TUI, a choice to
		// resend the email and try again. The resend button stays disabled
		// until the cooldown measured from the last send has passed.
		promptCode := func(lastError error, notice, previous string) (string, bool, error) {
			message := ""
			switch {
			case lastError != nil:
				// The sentinel prefix is noise in a form message.
				message = colorRed(strings.TrimPrefix(lastError.Error(), zlib.ErrRegisterFailed.Error()+": "))
			case notice != "":
				message = colorGreen(notice)
			}
			form := newInlineForm("Verify your email", "Verify", []formField{
				{label: "Code", value: previous, hint: "The code emailed to " + email, validate: requiredField("code")},
			})
			form.resend = true
			form.resendDeadline = lastSent.Add(registerResendCooldown)
			form.message = message
			fields, accepted, err := runInlineForm(form)
			if err != nil || !accepted {
				return "", false, err
			}
			if fields[len(fields)-1].resendChosen {
				return "", true, nil
			}
			return fields[0].value, false, nil
		}

		// The verification loop owns the whole retry state. lastError, notice
		// and rejected have to live outside the loop body, or each attempt
		// would start with a blank prompt and lose the server's message.
		initial, _ := cmd.Flags().GetString("code")
		_, err = verifyRegistrationCode(initial,
			promptCode,
			func(code string) error { return c.VerifyRegistration(cmd.Context(), opts, code) },
			sendCode,
		)
		if errors.Is(err, errPromptCancelled) {
			return nil
		}
		if err != nil {
			return err
		}

		userID := strings.TrimSpace(c.Cookies()["remix_userid"])
		if userID == "" {
			return fmt.Errorf("registration response carried no account id")
		}
		label, _ := cmd.Flags().GetString("label")
		use, _ := cmd.Flags().GetBool("use")
		if err := saveAccount(config.Account{
			UserID:  userID,
			Email:   strings.TrimSpace(email),
			Label:   strings.TrimSpace(label),
			Domain:  c.Domain(),
			Cookies: c.Cookies(),
		}, use); err != nil {
			return err
		}
		if err := rememberLoginEmail(email); err != nil {
			lipgloss.Fprintf(cmd.ErrOrStderr(), "Could not remember login email: %v\n", err)
		}

		lipgloss.Fprintf(os.Stdout, "%s Account created and logged in.%s\n", colorGreen(symbolSuccess), loginDefaultHint(poolForHint(), userID, use))
		reportPersistedDomain(cmd.OutOrStdout(), cmd.ErrOrStderr(), c.Domain())
		return nil
	},
}

func init() {
	registerCmd.Flags().String("email", "", "Account email")
	registerCmd.Flags().String("password", "", "Account password")
	registerCmd.Flags().String("name", "", "Display name (defaults to the email local part)")
	registerCmd.Flags().String("code", "", "Verification code (skips the prompt when already received)")
	registerCmd.Flags().String("domain", "", "Registration domain (falls back to $ZLIB_DOMAIN)")
	registerCmd.Flags().String("label", "", "Display name for the account (optional)")
	registerCmd.Flags().Bool("use", false, "Make the new account the default")
}

// errPromptCancelled means the user closed the code prompt, which is a clean
// exit rather than a failure.
var errPromptCancelled = errors.New("verification prompt cancelled")

// verifyRegistrationCode drives the code prompt until the server accepts a
// code. A rejected code returns to the prompt with the server's message and
// the rejected value prefilled; a resend request re-sends the email and starts
// the prompt over; any other error is returned, because a network failure or
// an anti-bot page will not be fixed by typing another code.
//
// The state (lastError, notice, rejected) lives outside the loop on purpose:
// declaring it inside would reset the prompt on every attempt and drop the
// message the user needs to see.
func verifyRegistrationCode(
	initial string,
	prompt func(lastError error, notice, previous string) (code string, resend bool, err error),
	verify func(code string) error,
	resend func() error,
) (string, error) {
	code := initial
	var lastError error
	notice, rejected := "", ""
	for {
		if strings.TrimSpace(code) == "" {
			entered, wantsResend, err := prompt(lastError, notice, rejected)
			if err != nil {
				return "", err
			}
			if wantsResend {
				if err := resend(); err != nil {
					return "", err
				}
				lastError, rejected, notice = nil, "", "A new code was sent."
				continue
			}
			code = entered
		}

		err := verify(strings.TrimSpace(code))
		if err == nil {
			return code, nil
		}
		if !zlib.IsWrongCodeError(err) {
			return "", err
		}
		lastError, rejected, notice, code = err, strings.TrimSpace(code), "", ""
	}
}
