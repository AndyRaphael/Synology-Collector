package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "—"},
		{-5, "—"},
		{512, "512 B"},
		{2000, "2.0 KB"},
		{12000138625024, "12.0 TB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHumanUptime(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "—"},
		{90, "1m"},
		{3720, "1h 2m"},
		{356400, "4d 3h 0m"},
	}
	for _, tc := range cases {
		if got := humanUptime(tc.sec); got != tc.want {
			t.Errorf("humanUptime(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}

// sampleReport builds a representative WARNING report exercising every section.
func sampleReport() *Report {
	ls := time.Date(2026, 7, 21, 2, 14, 0, 0, time.UTC)
	return &Report{
		CollectorVersion: "test",
		CollectedAt:      time.Date(2026, 7, 21, 12, 34, 56, 0, time.UTC),
		DurationMs:       1234,
		Host:             "https://192.168.1.20:5001",
		Status:           "WARNING",
		Summary:          "1 Active Backup task(s) failed: WS-05",
		Config:           ConfigEcho{VolWarnPct: 80, VolCritPct: 90},
		System: &SystemInfo{
			State: StateOK, Model: "DS923+", VersionFull: "DSM 7.2.2-72806",
			VersionShort: "7.2.2", Hostname: "nas01", Serial: "2380ABC", UptimeSec: 356400,
		},
		Storage: &StorageInfo{
			State: StateOK,
			Pools: []Pool{{ID: "pool_1", Status: "normal", DeviceType: "raid5",
				SizeTotal: 36000000000000, SizeUsed: 24000000000000, UsedPct: 66.6}},
			Volumes: []Volume{{ID: "volume_1", Name: "volume_1", Status: "normal",
				FsType: "btrfs", SizeTotal: 36000000000000, SizeUsed: 24480000000000,
				UsedPct: 68, CapacityKnown: true}},
			Disks: []Disk{{Name: "Drive 4", Model: "ST12000NM001G", Serial: "WL20D820",
				Status: "sys_partition_normal", TempC: 32, SizeTotal: 12000138625024}},
		},
		ABB: &ABBInfo{
			State: StateOK, Total: 2, Monitored: 2, Failed: 1,
			LastSuccess: &ls, LastSuccessState: LSKnown,
			Tasks: []ABBTask{
				{TaskID: 1, Name: "WS-05", SourceType: "pc", Enabled: true, Monitored: true,
					Failed: true, EffectiveMaxAge: "24h0m0s"},
				// A hostile task name proves html/template escapes NAS-provided strings.
				{TaskID: 2, Name: `evil <script>alert(1)</script> & "friends"`, Enabled: true,
					Monitored: true, LastSuccess: &ls, EffectiveMaxAge: "24h0m0s"},
			},
		},
		Hyper: &HyperInfo{
			State: StateOK, Total: 2, Monitored: 2, Running: 1,
			LastSuccess: &ls, LastSuccessState: LSKnown,
			Tasks: []HyperTask{
				// A running integrity check: healthy activity, shown with its note.
				{TaskID: "1", Name: "NAS-to-C2", Target: "C2", Enabled: true, Monitored: true,
					Running: true, RunningNote: "backup-integrity check in progress", EffectiveMaxAge: "168h0m0s"},
				{TaskID: "2", Name: "Offsite-rsync", Target: "rsync", Enabled: true, Monitored: true,
					LastSuccess: &ls, EffectiveMaxAge: "168h0m0s"},
			},
		},
		Checks: []CheckResult{
			{Name: "drive_health:Drive 4", Severity: SevOK, Message: "Drive 4 is healthy"},
			{Name: "abb_failed", Severity: SevWarning, Message: "1 Active Backup task(s) failed: WS-05"},
		},
	}
}

func TestRenderHTMLHealthySections(t *testing.T) {
	var b strings.Builder
	if err := renderHTML(&b, sampleReport()); err != nil {
		t.Fatalf("renderHTML: %v", err)
	}
	out := b.String()

	mustContain := []string{
		"<!doctype html>",
		`<div class="banner warn">`, // status-colored banner
		"WARNING",
		"DS923", "7.2.2", "nas01", // system (the model's "+" is safely encoded as &#43;)
		"12.0 TB",              // disk size, humanized
		"Sys partition normal", // sys_partition_normal presented, not raw
		"32°C",
		"width:68%",                          // volume usage bar rendered (would be ZgotmplZ if unsafe)
		"WS-05",                              // task
		`<div class="stat">`,                 // ABB stat tiles
		"Last monitored success",             // last success on its own line, not a stat tile
		"2026-07-21 02:14 UTC",               // timestamp formatted, not raw RFC3339
		"Hyper Backup",                       // Hyper Backup section heading
		"NAS-to-C2",                          // a Hyper Backup task
		"backup-integrity check in progress", // a running task's activity note
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("rendered HTML missing %q", s)
		}
	}

	// html/template must neutralize the hostile task name.
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("task name was not HTML-escaped — XSS risk")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("expected escaped task name in output")
	}
	// The CSS escaper must never have blanked the bar width.
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("output contains ZgotmplZ — an unsafe value was filtered")
	}
	// Timestamps in the report body are humanized, never raw RFC3339.
	if strings.Contains(out, "T02:14:00Z") {
		t.Error("found raw RFC3339 timestamp in HTML; expected humanized time")
	}
}

func TestRenderHTMLEmbedWYSIWYGSafe(t *testing.T) {
	var b strings.Builder
	if err := renderHTMLEmbed(&b, sampleReport()); err != nil {
		t.Fatalf("renderHTMLEmbed: %v", err)
	}
	out := b.String()

	// WYSIWYG editors strip these, so the fragment must not depend on them.
	if strings.Contains(out, "<style") {
		t.Error("fragment has a <style> block; WYSIWYG fields strip it — styling must be inline")
	}
	if strings.Contains(out, "<script") {
		t.Error("fragment contains <script>")
	}
	// Styling is inline instead (badge background from the palette).
	if !strings.Contains(out, "background:#dafbe1") {
		t.Error("expected inline badge colors in the fragment")
	}
	for _, s := range []string{"WARNING", "DS923", "Sys partition normal", "2026-07-21 02:14 UTC", "Hyper Backup", "NAS-to-C2"} {
		if !strings.Contains(out, s) {
			t.Errorf("fragment missing %q", s)
		}
	}
	// NAS-provided strings are still escaped, and no value was CSS-filtered.
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("task name not escaped in fragment — XSS risk")
	}
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("fragment contains ZgotmplZ — an inline style was filtered")
	}
	// Pure ASCII: no raw UTF-8 bytes that a non-UTF-8 field would turn to mojibake.
	for i := 0; i < len(out); i++ {
		if out[i] > 127 {
			t.Fatalf("fragment has a non-ASCII byte at offset %d; expected numeric entities only", i)
		}
	}
	if !strings.Contains(out, "&#176;") { // the ° in "32°C", folded to an entity
		t.Error("expected the degree sign as a numeric entity (&#176;) in the fragment")
	}
}

func TestRenderHTMLEmbedErrorReportNilSections(t *testing.T) {
	var b strings.Builder
	if err := renderHTMLEmbed(&b, minimalErrorReport("discovery failed")); err != nil {
		t.Fatalf("renderHTMLEmbed(error report): %v", err)
	}
	if !strings.Contains(b.String(), "discovery failed") {
		t.Error("error message missing from fragment")
	}
}

func TestRenderHTMLErrorReportNilSections(t *testing.T) {
	// minimalErrorReport has nil System/Storage/ABB — must render without panic.
	var b strings.Builder
	if err := renderHTML(&b, minimalErrorReport("discovery failed: request timed out")); err != nil {
		t.Fatalf("renderHTML(error report): %v", err)
	}
	out := b.String()
	if !strings.Contains(out, `class="banner err"`) {
		t.Errorf("error report should use the err banner class:\n%s", out)
	}
	if !strings.Contains(out, "discovery failed: request timed out") {
		t.Error("error message missing from HTML")
	}
}

func TestWriteHTMLReportFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.html")
	if err := writeHTMLReport(path, sampleReport()); err != nil {
		t.Fatalf("writeHTMLReport: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.HasPrefix(string(data), "<!doctype html>") {
		t.Errorf("file does not start with doctype:\n%.60s", data)
	}
}

func TestWriteHTMLReportBadPath(t *testing.T) {
	// A path under a non-existent directory must return an error, not panic.
	badPath := filepath.Join(t.TempDir(), "nope", "report.html")
	if err := writeHTMLReport(badPath, sampleReport()); err == nil {
		t.Error("expected error writing to a non-existent directory")
	}
}
