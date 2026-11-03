package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/heartleo/zlib/internal/config"
	"github.com/heartleo/zlib/internal/zlib"
)

func TestLoginDefaultHintExplainsAnUnchangedDefault(t *testing.T) {
	pool := config.Accounts{DefaultAccount: "1", Accounts: []config.Account{
		{UserID: "1", Email: "one@example.com", Domain: "https://example.com"},
		{UserID: "2", Email: "two@example.com", Domain: "https://example.com"},
	}}
	if got := loginDefaultHint(pool, "2", false); !strings.Contains(got, "one@example.com") || !strings.Contains(got, "--use") {
		t.Fatalf("hint = %q, want the surviving default and the --use remedy", got)
	}
	for _, tt := range []struct {
		name   string
		pool   config.Accounts
		userID string
		use    bool
	}{
		{name: "use switched", pool: pool, userID: "2", use: true},
		{name: "the account is already the default", pool: pool, userID: "1"},
		{name: "empty pool", pool: config.Accounts{}, userID: "2"},
	} {
		if got := loginDefaultHint(tt.pool, tt.userID, tt.use); got != "" {
			t.Fatalf("%s: hint = %q, want none", tt.name, got)
		}
	}
}

func TestResolveLoginDomain(t *testing.T) {
	tests := []struct {
		name       string
		flagDomain string
		domainEnv  string
		want       string
		wantErr    string
	}{
		{name: "explicit domain", flagDomain: "https://z-lib.gl/", domainEnv: "https://z-lib.sk", want: "https://z-lib.gl"},
		{name: "environment domain", domainEnv: "https://z-lib.gd", want: "https://z-lib.gd"},
		{name: "missing domain", wantErr: zlib.EnvDomain},
		{name: "invalid domain", domainEnv: "https://untrusted.example", wantErr: "EAPI requires"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(zlib.EnvDomain, tt.domainEnv)
			got, err := resolveLoginDomain(tt.flagDomain)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveLoginDomain() error = %v, want mention of %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLoginDomain() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveLoginDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRememberLoginEmailPreservesOtherPreferences(t *testing.T) {
	isolateConfigHome(t)
	if err := config.SaveConfig(config.Config{Theme: "dracula"}); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	if err := rememberLoginEmail("reader@example.com"); err != nil {
		t.Fatalf("rememberLoginEmail() error = %v", err)
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.LastLoginEmail != "reader@example.com" {
		t.Errorf("LastLoginEmail = %q, want %q", cfg.LastLoginEmail, "reader@example.com")
	}
	if cfg.Theme != "dracula" {
		t.Errorf("Theme = %q, want %q", cfg.Theme, "dracula")
	}
	if got := rememberedLoginEmail(); got != "reader@example.com" {
		t.Errorf("rememberedLoginEmail() = %q, want %q", got, "reader@example.com")
	}

	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(strings.ToLower(string(data)), "password") {
		t.Fatalf("config must not persist a password: %s", data)
	}
}

func TestRememberLoginEmailIgnoresBlankEmail(t *testing.T) {
	isolateConfigHome(t)
	if err := config.SaveConfig(config.Config{LastLoginEmail: "reader@example.com"}); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	if err := rememberLoginEmail("  "); err != nil {
		t.Fatalf("rememberLoginEmail() error = %v", err)
	}
	if got := rememberedLoginEmail(); got != "reader@example.com" {
		t.Errorf("rememberedLoginEmail() = %q, want existing email", got)
	}
}

func TestResolveEAPIDomainKeepsDirectOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != eapiProbePath {
			t.Errorf("probed %q, want %q", r.URL.Path, eapiProbePath)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":1,"domains":[]}`))
	}))
	defer server.Close()

	got, err := resolveEAPIDomain(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("resolveEAPIDomain() error = %v", err)
	}
	if got != server.URL {
		t.Errorf("resolveEAPIDomain() = %q, want %q", got, server.URL)
	}
}

func TestResolveEAPIDomainRejectsRedirectOffAllowlist(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":1,"domains":[]}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirector.Close()

	_, err := resolveEAPIDomain(context.Background(), redirector.URL)
	if err == nil {
		t.Fatal("resolveEAPIDomain() accepted a redirect to a host outside the allowlist")
	}
	if !strings.Contains(err.Error(), "not an allowed Z-Library domain") {
		t.Errorf("resolveEAPIDomain() error = %v, want an allowlist rejection", err)
	}
}

func TestResolveEAPIDomainRejectsUnhealthyOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := resolveEAPIDomain(context.Background(), server.URL)
	if err == nil {
		t.Fatal("resolveEAPIDomain() accepted an origin that does not serve the EAPI")
	}
	if !strings.Contains(err.Error(), "not usable for EAPI") {
		t.Errorf("resolveEAPIDomain() error = %v, want an EAPI usability rejection", err)
	}
}
