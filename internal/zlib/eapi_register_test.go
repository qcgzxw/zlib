package zlib

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// happyPathRegisterServer answers both registration endpoints and records the
// requests it saw.
type registerRecorder struct {
	sendCodeForm map[string]string
	sendCodeCT   string
	rpcForm      map[string]string
}

func (rec *registerRecorder) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/papi/user/verification/send-code", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("send-code: ParseMultipartForm() error = %v", err)
		}
		rec.sendCodeForm = map[string]string{}
		for key := range r.MultipartForm.Value {
			rec.sendCodeForm[key] = r.MultipartForm.Value[key][0]
		}
		rec.sendCodeCT = r.Header.Get("Content-Type")
		fmt.Fprint(w, `{"success":1}`)
	})
	mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("rpc: ParseForm() error = %v", err)
		}
		rec.rpcForm = map[string]string{}
		for key := range r.Form {
			rec.rpcForm[key] = r.Form[key][0]
		}
		if r.Form.Get("verifyCode") != "8890" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errors":["Wrong verification code"],"response":{}}`)
			return
		}
		fmt.Fprint(w, `{"errors":[],"response":{"user_id":51337662,"user_key":"d84e40eb8c818c181a07b1e3b1d659cb"}}`)
	})
	return mux
}

func TestRegisterSavesRemixCredentials(t *testing.T) {
	rec := &registerRecorder{}
	server := httptest.NewServer(rec.handler(t))
	defer server.Close()

	opts := RegisterOptions{Email: "reader@example.com", Password: "secret", Name: "reader"}
	c := NewClient(WithDomain(server.URL))
	if err := c.SendRegistrationCode(context.Background(), opts); err != nil {
		t.Fatalf("SendRegistrationCode() error = %v", err)
	}

	for key, want := range map[string]string{"email": "reader@example.com", "password": "secret", "name": "reader", "rx": "215", "action": "registration"} {
		if got := rec.sendCodeForm[key]; got != want {
			t.Errorf("send-code %s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasPrefix(rec.sendCodeCT, "multipart/form-data; boundary=") {
		t.Errorf("send-code Content-Type = %q, want multipart/form-data with a boundary", rec.sendCodeCT)
	}

	if err := c.VerifyRegistration(context.Background(), opts, "8890"); err != nil {
		t.Fatalf("VerifyRegistration() error = %v", err)
	}
	if c.Cookies()["remix_userid"] != "51337662" || c.Cookies()["remix_userkey"] != "d84e40eb8c818c181a07b1e3b1d659cb" {
		t.Errorf("Cookies() = %v, want the rpc remix credentials", c.Cookies())
	}
	for key, want := range map[string]string{"isModal": "true", "verifyCode": "8890", "gg_json_mode": "1", "redirectUrl": ""} {
		if got := rec.rpcForm[key]; got != want {
			t.Errorf("rpc %s = %q, want %q", key, got, want)
		}
	}
}

// A bad code comes back as HTTP 500 with the message in the JSON body; it must
// survive as the error text.
func TestRegisterWrongCodeSurfacesServerError(t *testing.T) {
	rec := &registerRecorder{}
	server := httptest.NewServer(rec.handler(t))
	defer server.Close()

	opts := RegisterOptions{Email: "reader@example.com", Password: "secret", Name: "reader"}
	c := NewClient(WithDomain(server.URL))
	if err := c.SendRegistrationCode(context.Background(), opts); err != nil {
		t.Fatalf("SendRegistrationCode() error = %v", err)
	}
	err := c.VerifyRegistration(context.Background(), opts, "0000")
	if err == nil {
		t.Fatal("VerifyRegistration() error = nil, want the server message")
	}
	if !strings.Contains(err.Error(), "Wrong verification code") {
		t.Errorf("VerifyRegistration() error = %v, want it to carry the server message", err)
	}
	if c.Cookies()["remix_userid"] != "" {
		t.Errorf("Cookies() = %v, want no credentials after a failed verification", c.Cookies())
	}
}

// An empty name falls back to the email's local part.
func TestRegisterDefaultsNameToEmailLocalPart(t *testing.T) {
	rec := &registerRecorder{}
	server := httptest.NewServer(rec.handler(t))
	defer server.Close()

	opts := RegisterOptions{Email: "reader@example.com", Password: "secret"}
	c := NewClient(WithDomain(server.URL))
	if err := c.SendRegistrationCode(context.Background(), opts); err != nil {
		t.Fatalf("SendRegistrationCode() error = %v", err)
	}
	if got := rec.sendCodeForm["name"]; got != "reader" {
		t.Errorf("send-code name = %q, want the email local part", got)
	}
}

// A rejected code is distinguishable from other registration failures, so the
// CLI can re-prompt instead of aborting.
func TestIsWrongCodeError(t *testing.T) {
	rec := &registerRecorder{}
	server := httptest.NewServer(rec.handler(t))
	defer server.Close()

	opts := RegisterOptions{Email: "reader@example.com", Password: "secret", Name: "reader"}
	c := NewClient(WithDomain(server.URL))
	_ = c.SendRegistrationCode(context.Background(), opts)

	err := c.VerifyRegistration(context.Background(), opts, "0000")
	if !IsWrongCodeError(err) {
		t.Errorf("IsWrongCodeError(%v) = false, want true for a rejected code", err)
	}
	if !errors.Is(err, ErrRegisterFailed) {
		t.Errorf("errors.Is(%v, ErrRegisterFailed) = false, want the sentinel chain", err)
	}

	if IsWrongCodeError(context.Canceled) {
		t.Error("IsWrongCodeError(context.Canceled) = true, want false")
	}
}

// rpc.php has no documented error shape: a rejection has been seen as an
// errors array, an errors object and a bare 200 with no credentials. Every
// one of them has to be retryable, not fatal.
func TestVerifyRegistrationRejectionShapes(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"errors array":  {http.StatusInternalServerError, `{"errors":["Wrong verification code"],"response":{}}`, "Wrong verification code"},
		"errors object": {http.StatusOK, `{"errors":{"verifyCode":"Invalid code"},"response":{}}`, "Invalid code"},
		"errors string": {http.StatusOK, `{"errors":"Code expired","response":{}}`, "Code expired"},
		"empty errors":  {http.StatusOK, `{"errors":[],"response":{"params":""}}`, "the code was not accepted"},
		"error field":   {http.StatusOK, `{"error":"Too many attempts","response":{}}`, "Too many attempts"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/papi/user/verification/send-code" {
					fmt.Fprint(w, `{"success":1}`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			opts := RegisterOptions{Email: "reader@example.com", Password: "secret", Name: "reader"}
			c := NewClient(WithDomain(server.URL))
			if err := c.SendRegistrationCode(context.Background(), opts); err != nil {
				t.Fatalf("SendRegistrationCode() error = %v", err)
			}
			err := c.VerifyRegistration(context.Background(), opts, "0000")
			if !IsWrongCodeError(err) {
				t.Fatalf("IsWrongCodeError(%v) = false, want a retryable rejection", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			if c.Cookies()["remix_userid"] != "" {
				t.Errorf("Cookies() = %v, want no session after a rejection", c.Cookies())
			}
		})
	}
}
