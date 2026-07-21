package main

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		in        string
		allowHTTP bool
		want      string
		wantErr   bool
	}{
		{"https://192.168.1.20:5001", false, "https://192.168.1.20:5001", false},
		{"192.168.1.20:5001", false, "https://192.168.1.20:5001", false},
		{"192.168.1.20", false, "https://192.168.1.20:5001", false},
		{"nas.local", false, "https://nas.local:5001", false},
		{"https://nas.local/", false, "https://nas.local:5001", false},
		{"https://nas.local/dsm/", false, "https://nas.local:5001/dsm", false},
		{"http://nas.local", true, "http://nas.local:5000", false},
		{"http://nas.local", false, "", true}, // http without --allow-http
		{"[2001:db8::1]:5001", false, "https://[2001:db8::1]:5001", false},
		{"", false, "", true},
		{"ftp://nas", false, "", true},
	}
	for _, tc := range cases {
		got, _, err := normalizeHost(tc.in, tc.allowHTTP)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeHost(%q,%v)=%q, want error", tc.in, tc.allowHTTP, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeHost(%q,%v) error: %v", tc.in, tc.allowHTTP, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeHost(%q,%v)=%q, want %q", tc.in, tc.allowHTTP, got, tc.want)
		}
	}
}

func TestParseConfigEnvAndValidation(t *testing.T) {
	env := map[string]string{
		"DSM_HOST":     "10.0.0.5",
		"DSM_USERNAME": "svc",
		"DSM_PASSWORD": "pw",
	}
	getenv := func(k string) string { return env[k] }

	t.Run("env-fallback", func(t *testing.T) {
		cfg, err := parseConfig(nil, getenv, strings.NewReader(""), &strings.Builder{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "https://10.0.0.5:5001" || cfg.Username != "svc" || cfg.Password != "pw" {
			t.Errorf("env fallback wrong: %+v", cfg)
		}
	})

	t.Run("warn-ge-crit-rejected", func(t *testing.T) {
		_, err := parseConfig([]string{"--vol-warn", "95", "--vol-crit", "90"}, getenv, strings.NewReader(""), &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "less than") {
			t.Errorf("want warn<crit error, got %v", err)
		}
	})

	t.Run("negative-duration-rejected", func(t *testing.T) {
		_, err := parseConfig([]string{"--backup-max-age", "-1h"}, getenv, strings.NewReader(""), &strings.Builder{})
		if err == nil {
			t.Errorf("want positive-duration error")
		}
	})

	t.Run("bad-format-rejected", func(t *testing.T) {
		_, err := parseConfig([]string{"--format", "yaml"}, getenv, strings.NewReader(""), &strings.Builder{})
		if err == nil {
			t.Errorf("want format error")
		}
	})

	t.Run("missing-password", func(t *testing.T) {
		g := func(k string) string {
			if k == "DSM_PASSWORD" {
				return ""
			}
			return env[k]
		}
		_, err := parseConfig(nil, g, strings.NewReader(""), &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "password") {
			t.Errorf("want password-required error, got %v", err)
		}
	})
}

func TestPasswordFromStdin(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "DSM_HOST":
			return "nas"
		case "DSM_USERNAME":
			return "svc"
		}
		return ""
	}
	cfg, err := parseConfig([]string{"--password-file", "-"}, env, strings.NewReader("s3cr3t\n"), &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "s3cr3t" {
		t.Errorf("password=%q, want s3cr3t (trailing newline trimmed)", cfg.Password)
	}
}

func TestNormalizePin(t *testing.T) {
	valid := strings.Repeat("ab", 32) // 64 hex chars
	if _, err := normalizePin("SHA256:" + strings.ToUpper(valid)); err != nil {
		t.Errorf("valid pin rejected: %v", err)
	}
	if _, err := normalizePin(insertColons(valid)); err != nil {
		t.Errorf("colon-separated pin rejected: %v", err)
	}
	if _, err := normalizePin("tooshort"); err == nil {
		t.Errorf("short pin accepted")
	}
	if _, err := normalizePin(strings.Repeat("zz", 32)); err == nil {
		t.Errorf("non-hex pin accepted")
	}
}

func TestParseSelectorForms(t *testing.T) {
	cases := []struct {
		in   string
		kind SelKind
	}{
		{"id:123", SelID},
		{"name:123", SelName},
		{"MyTask", SelAuto},
	}
	for _, tc := range cases {
		s, err := parseSelector(tc.in)
		if err != nil {
			t.Fatalf("parseSelector(%q): %v", tc.in, err)
		}
		if s.Kind != tc.kind {
			t.Errorf("parseSelector(%q).Kind=%v, want %v", tc.in, s.Kind, tc.kind)
		}
	}
}

func TestParseTaskOverrides(t *testing.T) {
	ov, err := parseTaskOverrides([]string{"name:Weekly=168h", "id:5=1h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ov) != 2 || ov[0].MaxAge != 168*time.Hour || ov[1].MaxAge != time.Hour {
		t.Errorf("overrides wrong: %+v", ov)
	}
	if _, err := parseTaskOverrides([]string{"noequals"}); err == nil {
		t.Errorf("want SELECTOR=DURATION error")
	}
	if _, err := parseTaskOverrides([]string{"id:1=-5m"}); err == nil {
		t.Errorf("want positive-duration error")
	}
}

func insertColons(hex string) string {
	var parts []string
	for i := 0; i+2 <= len(hex); i += 2 {
		parts = append(parts, hex[i:i+2])
	}
	return strings.Join(parts, ":")
}
