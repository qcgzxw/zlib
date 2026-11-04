package zlib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// The web registration form goes through endpoints outside the EAPI: the
// browser first asks /papi/user/verification/send-code for an email code, then
// posts the code together with the credentials to /rpc.php, whose JSON answer
// carries the remix credentials directly.
const (
	papiSendCodePath = "/papi/user/verification/send-code"
	rpcRegisterPath  = "/rpc.php"

	// registerRX is the fixed rx form value the web registration form sends.
	// Every captured request carries the same constant, so it is replayed
	// verbatim rather than derived.
	registerRX = "215"
)

// ErrRegisterFailed wraps every failure of the registration flow.
var ErrRegisterFailed = errors.New("zlibrary: registration failed")

// RegisterOptions carries the identity a new account is created with. Name
// defaults to the local part of the email when empty.
type RegisterOptions struct {
	Email    string
	Password string
	Name     string
}

// SendRegistrationCode asks the upstream to email a verification code for a
// new account. The code itself arrives out of band; VerifyRegistration
// completes the flow with it.
func (c *Client) SendRegistrationCode(ctx context.Context, opts RegisterOptions) error {
	_, err := c.sendRegistrationCode(ctx, opts)
	return err
}

func (c *Client) sendRegistrationCode(ctx context.Context, opts RegisterOptions) (string, error) {
	var body strings.Builder
	writer := multipart.NewWriter(&body)
	for name, values := range c.registrationForm(opts) {
		if err := writer.WriteField(name, strings.Join(values, "")); err != nil {
			return "", fmt.Errorf("%w: %w", ErrRegisterFailed, err)
		}
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrRegisterFailed, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.domain+papiSendCodePath, strings.NewReader(body.String()))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRegisterFailed, err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", defaultUserAgent)

	responseBody, err := c.doRegistrationRequest(req)
	if err != nil {
		return "", err
	}
	var result struct {
		Success int    `json:"success"`
		Error   string `json:"error"`
	}
	if err := decodeEAPIJSON(responseBody, &result); err != nil {
		return "", fmt.Errorf("%w: invalid send-code response: %v", ErrRegisterFailed, err)
	}
	if result.Success != 1 {
		if result.Error == "" {
			result.Error = "the code was not sent"
		}
		return "", fmt.Errorf("%w: %s", ErrRegisterFailed, result.Error)
	}
	return writer.Boundary(), nil
}

// IsWrongCodeError reports whether err was caused by the verification code
// being rejected, so an interactive caller can re-prompt instead of aborting.
// It matches ErrCodeRejected rather than a concrete type, so any error that
// declares itself retryable is honoured.
func IsWrongCodeError(err error) bool {
	return errors.Is(err, ErrCodeRejected)
}

type codeMismatchError struct{ message string }

func (e *codeMismatchError) Error() string { return e.message }

func (e *codeMismatchError) Unwrap() []error { return []error{ErrRegisterFailed, ErrCodeRejected} }

// VerifyRegistration submits the emailed code and, on success, installs the
// returned remix credentials as the client's session.
func (c *Client) VerifyRegistration(ctx context.Context, opts RegisterOptions, code string) error {
	values := c.registrationForm(opts)
	values.Set("isModal", "true")
	values.Set("verifyCode", code)
	values.Set("gg_json_mode", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.domain+rpcRegisterPath, strings.NewReader(values.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRegisterFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	responseBody, err := c.doRegistrationRequest(req)
	if err != nil {
		return err
	}

	var result struct {
		Errors   json.RawMessage `json:"errors"`
		Error    string          `json:"error"`
		Response struct {
			UserID  json.Number `json:"user_id"`
			UserKey string      `json:"user_key"`
			Error   string      `json:"error"`
			Message string      `json:"message"`
		} `json:"response"`
	}
	if err := decodeEAPIJSON(responseBody, &result); err != nil {
		return fmt.Errorf("%w: invalid rpc response: %v", ErrRegisterFailed, err)
	}
	// rpc.php reports a rejected code in whichever field the endpoint feels
	// like: an errors array, a single error string, or a message inside the
	// response. Every shape counts as a rejection the user can retry.
	if texts := rpcErrorTexts(result.Errors, result.Error, result.Response.Error, result.Response.Message); len(texts) > 0 {
		return &codeMismatchError{message: fmt.Sprintf("%s: %s", ErrRegisterFailed, strings.Join(texts, "; "))}
	}
	if result.Response.UserID == "" || result.Response.UserKey == "" {
		// No credentials and no stated reason. That is still a rejection the
		// user can retry with another code, so it is classified as one; the
		// body goes into the message because the shape is undocumented and
		// otherwise unguessable.
		return &codeMismatchError{message: fmt.Sprintf("%s: the code was not accepted; response was %s", ErrRegisterFailed, rpcBodySnippet(responseBody))}
	}

	c.SetMode(ClientModeEAPI)
	c.SetCookies(map[string]string{
		"remix_userid":  result.Response.UserID.String(),
		"remix_userkey": result.Response.UserKey,
	})
	return nil
}

// rpcErrorTexts collects the non-empty error messages out of the shapes
// rpc.php uses for a rejection: an errors array, an errors object keyed by
// field, a single string, or the loose strings passed alongside it.
func rpcErrorTexts(raw json.RawMessage, loose ...string) []string {
	var texts []string
	if len(raw) > 0 && string(raw) != "null" {
		var list []string
		var single string
		var object map[string]any
		switch {
		case json.Unmarshal(raw, &list) == nil:
			texts = list
		case json.Unmarshal(raw, &single) == nil:
			texts = []string{single}
		case json.Unmarshal(raw, &object) == nil:
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if value, ok := object[key].(string); ok {
					texts = append(texts, value)
				}
			}
		}
	}
	texts = append(texts, loose...)
	cleaned := texts[:0]
	for _, text := range texts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return cleaned
}

// rpcBodySnippet renders an undocumented response body for an error message:
// control characters stripped, length capped.
func rpcBodySnippet(body []byte) string {
	text := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(body))
	text = strings.Join(strings.Fields(text), " ")
	const limit = 200
	if len(text) > limit {
		text = text[:limit] + "..."
	}
	return text
}

// registrationForm builds the shared identity fields both registration
// requests send.
func (c *Client) registrationForm(opts RegisterOptions) url.Values {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		local := opts.Email
		if at := strings.Index(local, "@"); at > 0 {
			local = local[:at]
		}
		name = local
	}
	return url.Values{
		"email":       {strings.TrimSpace(opts.Email)},
		"password":    {opts.Password},
		"name":        {name},
		"rx":          {registerRX},
		"action":      {"registration"},
		"redirectUrl": {""},
	}
}

// doRegistrationRequest performs req and returns the body, applying the same
// bot-protection and status checks the EAPI transport uses.
func (c *Client) doRegistrationRequest(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegisterFailed, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegisterFailed, err)
	}
	if isBotProtectionResponse(resp.StatusCode, resp.Header, responseBody) {
		return nil, &BotProtectionError{StatusCode: resp.StatusCode, URL: resp.Request.URL.String()}
	}
	// rpc.php reports a bad verification code with HTTP 500 and the error text
	// in the JSON body, so the body is handed back even on a non-2xx status.
	if err := validateStatus(resp); err != nil && !looksLikeRPCJSON(responseBody) {
		return nil, fmt.Errorf("%w: %w", ErrRegisterFailed, err)
	}
	return responseBody, nil
}

// looksLikeRPCJSON reports whether body is the {"errors":[...],"response":...}
// envelope rpc.php answers with, so its error text can be surfaced.
func looksLikeRPCJSON(body []byte) bool {
	var envelope struct {
		Errors   []string `json:"errors"`
		Response any      `json:"response"`
	}
	return json.Unmarshal(body, &envelope) == nil && (envelope.Errors != nil || envelope.Response != nil)
}
