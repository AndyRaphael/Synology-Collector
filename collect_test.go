package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseUptime(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"230:40:35", 830435}, // real DSM value: 230h 40m 35s
		{"1:00:00", 3600},
		{"0:00:00", 0},
		{" 12:30:00 ", 45000}, // surrounding whitespace tolerated
		{"", 0},
		{"garbage", 0},
		{"12:30", 0},    // too few fields
		{"-1:00:00", 0}, // negative rejected
		{"a:b:c", 0},    // non-numeric rejected
	}
	for _, tc := range cases {
		if got := parseUptime(tc.in); got != tc.want {
			t.Errorf("parseUptime(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The NAS hostname comes from the storage payload (storageMachineInfo.nameStr),
// not system info, and uptime from the up_time string — both must land on System.
func TestSystemHostnameBackfillAndUptime(t *testing.T) {
	r := runScenario(t, defaultScenario(), nil)
	if r.System == nil {
		t.Fatal("System is nil")
	}
	if r.System.Hostname != "nas01" {
		t.Errorf("Hostname = %q, want nas01 (backfilled from storageMachineInfo.nameStr)", r.System.Hostname)
	}
	if r.System.UptimeSec != 830435 {
		t.Errorf("UptimeSec = %d, want 830435 (parsed from up_time %q)", r.System.UptimeSec, "230:40:35")
	}
}

func TestClassifyABBStatus(t *testing.T) {
	cases := []struct {
		in   string
		want ABBOutcome
	}{
		{"complete", OutcomeSuccess},
		{"completed", OutcomeSuccess},
		{"SUCCESS", OutcomeSuccess},
		{"ok", OutcomeSuccess},
		{"failed", OutcomeFailed},
		{"error", OutcomeFailed},
		{"broken", OutcomeFailed}, // must NOT be success despite containing "ok" as a substring elsewhere
		{"cancelled", OutcomeCancelled},
		{"suspended", OutcomeCancelled},
		{"running", OutcomeRunning},
		{"backing_up", OutcomeRunning},
		{"backing-up", OutcomeRunning},
		{"incomplete", OutcomeUnknown}, // must NOT be success despite containing "complete"
		// Numeric DSM status codes (observed on real DSM 7.x).
		{"3", OutcomeSuccess},
		{"2", OutcomeSuccess}, // last_result success (confirmed on a just-completed backup)
		{"5", OutcomeFailed},
		{"4", OutcomeFailed},
		{"1", OutcomeRunning},
		{"", OutcomeUnknown},
		{"weird_new_status", OutcomeUnknown},
		{"99", OutcomeUnknown}, // an unmapped numeric code stays unknown
	}
	for _, tc := range cases {
		if got := classifyABBStatus(tc.in); got != tc.want {
			t.Errorf("classifyABBStatus(%q)=%s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestFlexInt64(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{`123`, 123},
		{`"123"`, 123},
		{`"3985729650688"`, 3985729650688},
		{`null`, 0},
		{`""`, 0},
		{`1.0`, 1},
	}
	for _, tc := range cases {
		var f FlexInt64
		if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
			t.Errorf("FlexInt64(%s) error: %v", tc.in, err)
			continue
		}
		if int64(f) != tc.want {
			t.Errorf("FlexInt64(%s)=%d, want %d", tc.in, int64(f), tc.want)
		}
	}
}

func TestFlexString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"hello"`, "hello"},
		{`5`, "5"},
		{`true`, "true"},
		{`null`, ""},
	}
	for _, tc := range cases {
		var f FlexString
		if err := json.Unmarshal([]byte(tc.in), &f); err != nil {
			t.Errorf("FlexString(%s) error: %v", tc.in, err)
			continue
		}
		if string(f) != tc.want {
			t.Errorf("FlexString(%s)=%q, want %q", tc.in, string(f), tc.want)
		}
	}
}

func TestParseEpoch(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	t.Run("seconds", func(t *testing.T) {
		want := time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC)
		got, ok := parseEpoch(want.Unix(), now)
		if !ok || !got.Equal(want) {
			t.Errorf("parseEpoch seconds=%v ok=%v, want %v", got, ok, want)
		}
	})
	t.Run("milliseconds", func(t *testing.T) {
		want := time.Date(2026, 7, 21, 2, 0, 0, 0, time.UTC)
		got, ok := parseEpoch(want.UnixMilli(), now)
		if !ok || !got.Equal(want) {
			t.Errorf("parseEpoch millis=%v ok=%v, want %v", got, ok, want)
		}
	})
	t.Run("zero-invalid", func(t *testing.T) {
		if _, ok := parseEpoch(0, now); ok {
			t.Errorf("zero epoch should be invalid")
		}
	})
	t.Run("negative-invalid", func(t *testing.T) {
		if _, ok := parseEpoch(-5, now); ok {
			t.Errorf("negative epoch should be invalid")
		}
	})
	t.Run("far-future-invalid", func(t *testing.T) {
		if _, ok := parseEpoch(now.Add(72*time.Hour).Unix(), now); ok {
			t.Errorf("far-future epoch should be invalid")
		}
	})
}

func TestVersionOrderTimeFallback(t *testing.T) {
	now := testNow
	// Running version with only time_start must still be orderable.
	var v rawABBVersion
	if err := json.Unmarshal([]byte(`{"time_start":`+itoa(now.Add(-time.Hour).Unix())+`,"status":"running"}`), &v); err != nil {
		t.Fatal(err)
	}
	got, ok := v.orderTime(now)
	if !ok {
		t.Fatalf("orderTime should fall back to time_start")
	}
	if !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("orderTime=%v, want %v", got, now.Add(-time.Hour))
	}
}

func TestExtractVersionShort(t *testing.T) {
	cases := map[string]string{
		"DSM 7.2.2-72806 Update 3": "7.2.2",
		"DSM 6.2.4-25556":          "6.2.4",
		"7.1":                      "7.1",
		"garbage":                  "",
	}
	for in, want := range cases {
		if got := extractVersionShort(in); got != want {
			t.Errorf("extractVersionShort(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestMatchSelectorAmbiguous(t *testing.T) {
	refs := []taskRef{
		{id: 1, name: "Backup"},
		{id: 2, name: "Backup"}, // duplicate name
	}
	_, _, err := matchSelector(Selector{Kind: SelName, Raw: "name:Backup", Name: "Backup"}, refs)
	if err == nil {
		t.Fatalf("ambiguous name selector should error")
	}
	// id: form disambiguates.
	ids, matched, err := matchSelector(Selector{Kind: SelID, Raw: "id:2", ID: 2}, refs)
	if err != nil || !matched || !ids[2] {
		t.Errorf("id selector should match task 2 unambiguously: ids=%v matched=%v err=%v", ids, matched, err)
	}
}

func TestMatchSelectorAutoNumericFallback(t *testing.T) {
	refs := []taskRef{{id: 42, name: "Daily"}}
	// Bare "42" matches no name → falls back to numeric id 42.
	ids, matched, err := matchSelector(Selector{Kind: SelAuto, Raw: "42", Name: "42"}, refs)
	if err != nil || !matched || !ids[42] {
		t.Errorf("auto numeric fallback failed: ids=%v matched=%v err=%v", ids, matched, err)
	}
}
