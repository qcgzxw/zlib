package cli

import (
	"errors"
	"fmt"
	"github.com/heartleo/zlib/internal/zlib"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func inlineStep(m inlineForm, msg tea.Msg) inlineForm {
	next, _ := m.Update(msg)
	return next.(inlineForm)
}

func newResendForm(deadline time.Time) inlineForm {
	form := newInlineForm("Verify your email", "Verify", []formField{
		{label: "Code", hint: "code", validate: requiredField("code")},
	})
	form.resend = true
	form.resendDeadline = deadline
	return form
}

// Tab has to reach the resend button; before it only reached the action.
func TestResendButtonIsATabStop(t *testing.T) {
	form := newResendForm(time.Now().Add(-time.Second))
	form.focus(0)
	if form.cursor != 0 {
		t.Fatalf("cursor = %d, want the code field", form.cursor)
	}
	form = inlineStep(form, tea.KeyPressMsg{Code: tea.KeyTab})
	if form.cursor != 1 {
		t.Fatalf("cursor after tab = %d, want the action button", form.cursor)
	}
	form = inlineStep(form, tea.KeyPressMsg{Code: tea.KeyTab})
	if form.cursor != 2 {
		t.Fatalf("cursor after second tab = %d, want the resend button", form.cursor)
	}
	form = inlineStep(form, tea.KeyPressMsg{Code: tea.KeyTab})
	if form.cursor != 0 {
		t.Fatalf("cursor after third tab = %d, want it to wrap to the code field", form.cursor)
	}
	// shift+tab from the first field wraps to the resend button.
	form = inlineStep(form, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if form.cursor != 2 {
		t.Fatalf("cursor after shift+tab = %d, want the resend button", form.cursor)
	}
}

// A resend needs no code, so the required-field validation must not block it.
func TestResendSkipsValidationWhenReady(t *testing.T) {
	form := newResendForm(time.Now().Add(-time.Second))
	form.focus(2)
	form = inlineStep(form, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !form.accepted {
		t.Fatal("accepted = false, want the form to end on a resend")
	}
	if !form.fields[0].resendChosen {
		t.Fatal("resendChosen = false, want the resend flag set")
	}
}

func TestResendBlockedDuringCooldown(t *testing.T) {
	form := newResendForm(time.Now().Add(45 * time.Second))
	form.focus(2)
	form = inlineStep(form, tea.KeyPressMsg{Code: tea.KeyEnter})
	if form.accepted || form.fields[0].resendChosen {
		t.Fatal("a resend during the cooldown must not end the form")
	}
	if !strings.Contains(form.message, "resend the code in") {
		t.Errorf("message = %q, want the countdown notice", form.message)
	}

	// The accelerator is gated the same way.
	form.message = ""
	form = inlineStep(form, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if form.accepted {
		t.Fatal("ctrl+r during the cooldown must not end the form")
	}

	// The view shows the remaining seconds and drops the accelerator hint.
	if view := form.View().Content; !strings.Contains(view, "Resend code (") {
		t.Errorf("view does not show the countdown: %q", view)
	}
}

// The tick loop keeps the view counting down and stops once the deadline has
// passed, at which point the button renders as enabled again.
func TestResendCountdownReenables(t *testing.T) {
	form := newResendForm(time.Now().Add(30 * time.Second))
	if remaining := form.resendRemaining(); remaining <= 0 {
		t.Fatalf("resendRemaining() = %d, want a positive countdown", remaining)
	}
	if _, cmd := form.Update(resendTickMsg{}); cmd == nil {
		t.Fatal("countdown stopped while the cooldown was still running")
	}

	form.resendDeadline = time.Now().Add(-time.Second)
	if remaining := form.resendRemaining(); remaining != 0 {
		t.Fatalf("resendRemaining() after the deadline = %d, want 0", remaining)
	}
	if _, cmd := form.Update(resendTickMsg{}); cmd != nil {
		t.Error("countdown kept ticking after the cooldown expired")
	}
	if strings.Contains(form.View().Content, "Resend code (") {
		t.Error("view still shows a countdown after it expired")
	}
}

// A rejected code must return to the prompt, not end the command. The first
// implementation returned on every rejection because it gated the retry on the
// --code flag being set, and reset the error message every iteration.
func TestVerifyRegistrationCodeRepromptsOnRejection(t *testing.T) {
	var prompts int
	var promptsAfterError []string
	entered := []string{"1111", "2222"}
	verify := func(code string) error {
		if code == "2222" {
			return nil
		}
		return fmt.Errorf("%w: Invalid code, please try again", zlib.ErrCodeRejected)
	}
	code, err := verifyRegistrationCode("",
		func(lastError error, notice, previous string) (string, bool, error) {
			if lastError != nil {
				promptsAfterError = append(promptsAfterError, lastError.Error())
			}
			value := entered[prompts]
			prompts++
			return value, false, nil
		},
		func(c string) error { return verify(c) },
		func() error { t.Error("resend() called, want the code retried"); return nil },
	)
	if err != nil {
		t.Fatalf("verifyRegistrationCode() error = %v, want the second code to be accepted", err)
	}
	if code != "2222" {
		t.Errorf("code = %q, want the accepted one", code)
	}
	if prompts != 2 {
		t.Fatalf("prompts = %d, want one per attempt", prompts)
	}
	if len(promptsAfterError) != 1 || !strings.Contains(promptsAfterError[0], "Invalid code") {
		t.Errorf("prompt messages = %v, want the server's rejection carried into the second prompt", promptsAfterError)
	}
}

// A rejected --code value falls back to the prompt instead of exiting.
func TestVerifyRegistrationCodeRepromptsAfterFlagValue(t *testing.T) {
	code, err := verifyRegistrationCode("0000",
		func(_ error, _ string, previous string) (string, bool, error) {
			if previous != "0000" {
				t.Errorf("prefill = %q, want the rejected value", previous)
			}
			return "8890", false, nil
		},
		func(c string) error {
			if c == "0000" {
				return fmt.Errorf("%w: Invalid code, please try again", zlib.ErrCodeRejected)
			}
			return nil
		},
		func() error { return nil },
	)
	if err != nil || code != "8890" {
		t.Fatalf("verifyRegistrationCode() = %q, %v; want the prompted code to be accepted", code, err)
	}
}

// Resend re-sends and re-prompts; other failures abort.
func TestVerifyRegistrationCodeResendAndFatal(t *testing.T) {
	sends := 0
	prompts := 0
	_, err := verifyRegistrationCode("",
		func(error, string, string) (string, bool, error) {
			prompts++
			return "", prompts == 1, nil
		},
		func(string) error { return nil },
		func() error { sends++; return nil },
	)
	if err != nil || sends != 1 {
		t.Fatalf("resend: sends = %d, err = %v; want one resend then an accepted prompt", sends, err)
	}

	fatal := errors.New("network down")
	if _, err := verifyRegistrationCode("",
		func(error, string, string) (string, bool, error) { return "1111", false, nil },
		func(string) error { return fatal },
		func() error { return nil },
	); !errors.Is(err, fatal) {
		t.Errorf("error = %v, want the non-retryable failure returned", err)
	}
}
