package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTLSPin(t *testing.T) {
	srv := httptest.NewTLSServer(defaultScenario().handler())
	defer srv.Close()

	sum := sha256.Sum256(srv.Certificate().Raw)
	goodPin := hex.EncodeToString(sum[:])

	t.Run("matching-pin-connects", func(t *testing.T) {
		cfg := testConfig(srv.URL)
		cfg.InsecureTLS = false
		cfg.TLSPin = goodPin
		r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 with matching pin; err=%q", r.ExitCode, r.Error)
		}
	})

	t.Run("mismatching-pin-refused", func(t *testing.T) {
		cfg := testConfig(srv.URL)
		cfg.InsecureTLS = false
		cfg.TLSPin = strings.Repeat("0", 64)
		r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 with mismatching pin", r.ExitCode)
		}
	})
}

func TestDefaultTLSRejectsSelfSigned(t *testing.T) {
	srv := httptest.NewTLSServer(defaultScenario().handler())
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.InsecureTLS = false // default verification against a self-signed cert
	r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 (self-signed cert should fail default verification)", r.ExitCode)
	}
}

func TestRedirectRefused(t *testing.T) {
	// A server that 302-redirects the login POST cross-path must not be followed.
	redir := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("api") == "SYNO.API.Info" {
			writeSuccess(w, discoveryData(defaultApis()))
			return
		}
		http.Redirect(w, r, "https://evil.example/steal", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()

	cfg := testConfig(redir.URL)
	r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 (redirect must be refused)", r.ExitCode)
	}
}

func TestDiscoveryFailureFatal(t *testing.T) {
	s := defaultScenario()
	s.discoveryFails = true
	r := runScenario(t, s, nil)
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 (discovery failure fatal)", r.ExitCode)
	}
}

func TestPickVersionIntersection(t *testing.T) {
	c := &Client{apis: map[string]apiEndpoint{
		"SYNO.API.Auth": {Path: "entry.cgi", MinVersion: 1, MaxVersion: 3},
	}}
	// Client supports [3,7], DSM [1,3] -> pick 3.
	v, err := c.pickVersion("SYNO.API.Auth")
	if err != nil || v != 3 {
		t.Fatalf("pickVersion=%d err=%v, want 3", v, err)
	}

	c.apis["SYNO.API.Auth"] = apiEndpoint{Path: "entry.cgi", MinVersion: 1, MaxVersion: 2}
	if _, err := c.pickVersion("SYNO.API.Auth"); err == nil {
		t.Fatalf("expected no-overlap error for DSM max 2 vs client min 3")
	}
}

func TestAuthCodeMessages(t *testing.T) {
	cases := map[int]string{
		400: "invalid username or password",
		401: "disabled",
		403: "service account",
		407: "auto-block",
	}
	for code, want := range cases {
		if msg := authCodeMessage(code); !strings.Contains(strings.ToLower(msg), want) {
			t.Errorf("authCodeMessage(%d)=%q, want substring %q", code, msg, want)
		}
	}
}
