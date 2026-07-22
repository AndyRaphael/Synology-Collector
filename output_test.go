package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestKVOrder(t *testing.T) {
	r := runScenario(t, defaultScenario(), nil)
	out := renderKV(r)
	wantOrder := []string{
		"STATUS=", "NAS=", "DSM=", "SYSTEM_HEALTH=", "STORAGE_POOL=", "VOLUME_USAGE=",
		"DRIVES=", "DRIVE_WARNINGS=", "ABB_STATE=", "ABB_TASKS=", "ABB_MONITORED=",
		"ABB_DISABLED=", "ABB_EXCLUDED=", "ABB_FAILED=", "ABB_OVERDUE=", "LAST_SUCCESS=",
		"HB_STATE=", "HB_TASKS=", "HB_MONITORED=", "HB_DISABLED=", "HB_EXCLUDED=",
		"HB_RUNNING=", "HB_FAILED=", "HB_OVERDUE=", "HB_LAST_SUCCESS=",
		"SUMMARY=", "HOST=", "COLLECTED_AT=", "COLLECTOR_VERSION=",
	}
	idx := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if idx < len(wantOrder) && strings.HasPrefix(line, wantOrder[idx]) {
			idx++
		}
	}
	if idx != len(wantOrder) {
		t.Errorf("KV keys out of order or missing (matched %d/%d):\n%s", idx, len(wantOrder), out)
	}
}

func TestErrorKVHasErrorLine(t *testing.T) {
	s := defaultScenario()
	s.authCode = 400
	r := runScenario(t, s, nil)
	out := renderKV(r)
	if !strings.Contains(out, "STATUS=ERROR\n") {
		t.Errorf("missing STATUS=ERROR:\n%s", out)
	}
	if !strings.Contains(out, "ERROR=") {
		t.Errorf("missing ERROR= line:\n%s", out)
	}
	// Minimal parseable keys must still be present.
	for _, k := range []string{"HOST=", "COLLECTED_AT=", "COLLECTOR_VERSION=", "SUMMARY="} {
		if !strings.Contains(out, k) {
			t.Errorf("error KV missing %q:\n%s", k, out)
		}
	}
}

func TestNoPasswordLeak(t *testing.T) {
	const secret = "sup3r-s3cret-p@ss"
	s := defaultScenario()
	r := runScenario(t, s, func(c *Config) {
		c.Password = secret
		c.Debug = true // exercise raw retention path too
	})

	// Render every surface and scan for the password.
	var buf strings.Builder
	if err := render(&buf, "both", r); err != nil {
		t.Fatal(err)
	}
	jsonBytes, _ := json.Marshal(r)
	surfaces := []string{buf.String(), string(jsonBytes), r.Error, r.Summary}
	for _, sfc := range surfaces {
		if strings.Contains(sfc, secret) {
			t.Errorf("password leaked into output surface: %q", sfc)
		}
	}
}

func TestConfigEchoHasNoPasswordField(t *testing.T) {
	cfg := testConfig("https://nas:5001")
	cfg.Password = "leaky"
	echo := newConfigEcho(cfg)
	b, _ := json.Marshal(echo)
	if strings.Contains(strings.ToLower(string(b)), "password") {
		t.Errorf("ConfigEcho JSON mentions password: %s", b)
	}
	if strings.Contains(string(b), "leaky") {
		t.Errorf("ConfigEcho JSON contains the password value: %s", b)
	}
}

func TestSanitizeInline(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"clean", "clean"},
		{"has\nnewline", "has newline"},
		{"tab\tsep", "tab sep"},
		{"bell\x07here", "bellhere"},
		{"Bücher 日本語", "Bücher 日本語"}, // printable UTF-8 preserved
		{"  trim  ", "trim"},
	}
	for _, tc := range cases {
		if got := sanitizeInline(tc.in); got != tc.want {
			t.Errorf("sanitizeInline(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTLSVerifyEcho(t *testing.T) {
	cases := []struct {
		mutate func(*Config)
		want   string
	}{
		{func(c *Config) {}, "default"},
		{func(c *Config) { c.InsecureTLS = true }, "insecure"},
		{func(c *Config) { c.CAFile = "/x.pem" }, "ca-file"},
		{func(c *Config) { c.TLSPin = strings.Repeat("a", 64) }, "pinned"},
	}
	for _, tc := range cases {
		cfg := &Config{Host: "h", BackupMaxAge: time.Hour, Timeout: time.Minute, Format: "both"}
		tc.mutate(cfg)
		if got := newConfigEcho(cfg).TLSVerify; got != tc.want {
			t.Errorf("TLSVerify=%q, want %q", got, tc.want)
		}
	}
}

func TestJSONReportSchemaVersion(t *testing.T) {
	r := runScenario(t, defaultScenario(), nil)
	b, _ := json.Marshal(r)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"].(float64) != 1 {
		t.Errorf("schema_version=%v, want 1", m["schema_version"])
	}
}
