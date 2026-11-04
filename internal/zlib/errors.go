package zlib

import (
	"errors"
	"fmt"
)

var (
	ErrLoginFailed    = errors.New("zlibrary: login failed")
	ErrNotLoggedIn    = errors.New("zlibrary: not logged in, call Login first")
	ErrNoDomain       = errors.New("zlibrary: no working domain found")
	ErrEmptyQuery     = errors.New("zlibrary: search query is empty")
	ErrNoID           = errors.New("zlibrary: no book ID provided")
	ErrInvalidProxy   = errors.New("zlibrary: proxy_list must be a non-empty slice")
	ErrParseFailed    = errors.New("zlibrary: failed to parse page")
	ErrSessionExpired = errors.New("zlibrary: session expired or revoked, log in again")
	ErrPhraseMinWords = errors.New("zlibrary: phrase search requires at least 2 words")
	ErrUnsupported    = errors.New("zlibrary: feature unavailable in EAPI")
	ErrDownloadFailed = errors.New("zlibrary: download failed")
	// ErrDailyLimitReached is returned when the account has spent today's
	// download allowance. It is per account, so a caller holding several
	// sessions can retry the same request with another one.
	ErrDailyLimitReached = errors.New("zlibrary: daily download limit reached")
	// ErrSMTPSend wraps every failure to deliver a Send-to-Kindle email after
	// the message was built, so the CLI can show the SMTP cause verbatim.
	ErrSMTPSend = errors.New("zlibrary: smtp send failed")
	// ErrBotProtection is returned when an upstream anti-bot page is served
	// instead of the requested Z-Library response.
	ErrBotProtection = errors.New("zlibrary: blocked by anti-bot protection")
	// ErrRedirectLoop is returned when an upstream keeps redirecting a request
	// beyond the client's redirect limit.
	ErrRedirectLoop = errors.New("zlibrary: too many redirects")
	// ErrChallengeFailed is returned when the JS challenge could not be cleared
	// within the client's attempt budget.
	ErrChallengeFailed = errors.New("zlibrary: could not clear the browser challenge")
	// ErrCodeRejected is returned when a registration verification code was
	// not accepted. It is retryable: the registration stays pending, so a
	// caller can ask for the code again.
	ErrCodeRejected = errors.New("zlibrary: verification code rejected")
	// ErrMirrorUnverified is returned when a domain offered as a user mirror
	// does not itself answer as the Z-Library EAPI.
	ErrMirrorUnverified = errors.New("zlibrary: mirror did not answer as Z-Library")
)

// BotProtectionError describes an anti-bot response returned by the upstream.
// It unwraps to ErrBotProtection so callers can classify it with errors.Is.
type BotProtectionError struct {
	StatusCode int
	URL        string
}

func (e *BotProtectionError) Error() string {
	return fmt.Sprintf("%s (HTTP %d) at %s", ErrBotProtection, e.StatusCode, e.URL)
}

func (e *BotProtectionError) Unwrap() error {
	return ErrBotProtection
}

// HTTPStatusError describes an unexpected non-success HTTP response.
type HTTPStatusError struct {
	StatusCode int
	URL        string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("zlibrary: unexpected HTTP status %d at %s", e.StatusCode, e.URL)
}
