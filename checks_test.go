package main

import (
	"testing"
	"time"
)

func testCfg() *Config {
	return &Config{VolWarnPct: 80, VolCritPct: 90, BackupMaxAge: 0}
}

func TestPoolStatusSeverity(t *testing.T) {
	cases := []struct {
		status string
		want   Severity
	}{
		{"normal", SevOK},
		{"healthy", SevOK},
		{"attention", SevWarning},
		{"rebuilding", SevWarning},
		{"degraded", SevCritical},
		{"crashed", SevCritical},
		{"some_new_status", SevWarning}, // unrecognized -> warning
		{"", SevWarning},
	}
	for _, tc := range cases {
		got, _ := statusSeverity("Pool", tc.status, poolStatusSeverity)
		if got != tc.want {
			t.Errorf("pool status %q -> %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestDriveStatusSeverity(t *testing.T) {
	cases := []struct {
		status string
		want   Severity
	}{
		{"normal", SevOK},
		{"warning", SevWarning},
		{"abnormal", SevWarning},
		{"failing", SevCritical},
		{"crashed", SevCritical},
		{"mystery", SevWarning},
	}
	for _, tc := range cases {
		got, _ := statusSeverity("Drive", tc.status, driveStatusSeverity)
		if got != tc.want {
			t.Errorf("drive status %q -> %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestVolumeUsageSeverity(t *testing.T) {
	cfg := testCfg()
	cases := []struct {
		pct  float64
		want Severity
	}{
		{50, SevOK},
		{79.9, SevOK},
		{80, SevWarning},
		{89.9, SevWarning},
		{90, SevCritical},
		{99, SevCritical},
	}
	for _, tc := range cases {
		got, _ := volumeUsageSeverity(cfg, Volume{ID: "v", UsedPct: tc.pct})
		if got != tc.want {
			t.Errorf("usage %.1f%% -> %v, want %v", tc.pct, got, tc.want)
		}
	}
}

func TestOverallSeverity(t *testing.T) {
	checks := []CheckResult{
		{Name: "a", Severity: SevOK},
		{Name: "b", Severity: SevWarning},
		{Name: "c", Severity: SevOK},
	}
	if got := overallSeverity(checks); got != SevWarning {
		t.Errorf("overall=%v, want WARNING", got)
	}
	checks = append(checks, CheckResult{Name: "d", Severity: SevCritical})
	if got := overallSeverity(checks); got != SevCritical {
		t.Errorf("overall=%v, want CRITICAL", got)
	}
}

func TestEvaluateDeepHistoryUnknownDoesNotFireABBUnknown(t *testing.T) {
	// A monitored task with a fresh success but an unknown status buried in
	// history must NOT raise abb_unknown.
	ls := testNow.Add(-1 * time.Hour)
	abb := &ABBInfo{
		State:            StateOK,
		Total:            1,
		Monitored:        1,
		LastSuccess:      &ls,
		LastSuccessState: LSKnown,
		Tasks: []ABBTask{{
			TaskID: 1, Name: "T", Enabled: true, Monitored: true,
			HistoryComplete: true, LastSuccess: &ls,
		}},
	}
	st := &StorageInfo{State: StateOK, Volumes: []Volume{{ID: "volume_1", Status: "normal"}}, Disks: []Disk{{Name: "d1", Status: "normal"}}}
	checks := evaluate(testCfg(), &SystemInfo{State: StateOK}, st, abb)
	for _, c := range checks {
		if c.Name == "abb_unknown" {
			t.Errorf("abb_unknown should not fire for deep-history unknowns")
		}
	}
}
