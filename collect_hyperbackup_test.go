package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// dsmTime formats a time in Hyper Backup's wall-clock string format (no timezone).
func dsmTime(t time.Time) string { return t.UTC().Format("2006/01/02 15:04:05") }

func TestClassifyHyperResult(t *testing.T) {
	cases := []struct {
		in   string
		want HyperOutcome
	}{
		{"done", HyperSuccess},
		{"success", HyperSuccess},
		{"OK", HyperSuccess},
		{"backingup", HyperRunning},
		{"backing-up", HyperRunning}, // hyphen normalized
		{"resuming", HyperRunning},
		{"version_deleting", HyperRunning}, // housekeeping is healthy activity
		{"none", HyperNone},
		{"failed", HyperFailed},
		{"version_delete_failed", HyperFailed},
		{"partial", HyperPartial},
		{"cksum_failed", HyperIntegrity},
		{"failed_checking", HyperIntegrity},
		{"dest_missing", HyperDestMissing},
		{"cancel", HyperCancelled},
		{"suspend", HyperSuspended},
		{"", HyperUnknown},
		{"brand_new_status", HyperUnknown},
	}
	for _, tc := range cases {
		if got := classifyHyperResult(tc.in); got != tc.want {
			t.Errorf("classifyHyperResult(%q)=%s, want %s", tc.in, got, tc.want)
		}
	}
}

// Not advertised and package absent → not installed, which is N/A (never an
// error), and must not affect the run's exit code.
func TestHyperNotInstalled(t *testing.T) {
	r := runScenario(t, defaultScenario(), nil) // default apis do not advertise Hyper Backup
	if got := kvValue(t, r, "HB_STATE"); got != "NOT_INSTALLED" {
		t.Errorf("HB_STATE=%q, want NOT_INSTALLED", got)
	}
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got != "N/A" {
		t.Errorf("HB_LAST_SUCCESS=%q, want N/A", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0 (not-installed Hyper Backup must not alter health)", r.ExitCode)
	}
}

func hyperScenario() *dsmScenario {
	s := defaultScenario()
	s.hyperAdvertise = true
	return s
}

func TestHyperHealthy(t *testing.T) {
	end := testNow.Add(-3 * 24 * time.Hour).Unix() // 3 days ago, within the 7d window
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "NAS-to-C2"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "done", end, 0)}
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "HB_STATE"); got != "OK" {
		t.Fatalf("HB_STATE=%q, want OK; summary=%q", got, r.Summary)
	}
	if got := kvValue(t, r, "HB_MONITORED"); got != "1" {
		t.Errorf("HB_MONITORED=%q, want 1", got)
	}
	if got := kvValue(t, r, "HB_FAILED"); got != "0" {
		t.Errorf("HB_FAILED=%q, want 0", got)
	}
	if got := kvValue(t, r, "HB_OVERDUE"); got != "0" {
		t.Errorf("HB_OVERDUE=%q, want 0 (3 days is within the 7-day window)", got)
	}
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got == "never" || got == "Unknown" || got == "N/A" {
		t.Errorf("HB_LAST_SUCCESS=%q, want a real timestamp", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
	if !s.hyperStatusCalledFor("1") {
		t.Errorf("status was not queried for task 1")
	}
}

// The central caveat: a task whose last completion is well past the freshness
// window but which is backing up RIGHT NOW must not be overdue or failed.
func TestHyperRunningSuppressesOverdue(t *testing.T) {
	stale := testNow.Add(-10 * 24 * time.Hour).Unix() // 10 days ago (> 7d window)
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "BigClient-5TB"))
	// Actively backing up; last completion is old, but that is irrelevant while running.
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backup", "backingup", "backingup", stale, 0)}
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "HB_RUNNING"); got != "1" {
		t.Errorf("HB_RUNNING=%q, want 1", got)
	}
	if got := kvValue(t, r, "HB_OVERDUE"); got != "0" {
		t.Errorf("HB_OVERDUE=%q, want 0 (a running task is never overdue)", got)
	}
	if got := kvValue(t, r, "HB_FAILED"); got != "0" {
		t.Errorf("HB_FAILED=%q, want 0 (a running task is never failed)", got)
	}
	if r.ExitCode != ExitOK {
		t.Fatalf("exit=%d, want 0 for a long-running sync; summary=%q", r.ExitCode, r.Summary)
	}
	if len(r.Hyper.Tasks) != 1 || !r.Hyper.Tasks[0].Running {
		t.Fatalf("task not marked running: %+v", r.Hyper.Tasks)
	}
}

// A backup-integrity check that has been running for days is likewise healthy
// activity, and the report labels it as an integrity check.
func TestHyperIntegrityCheckRunningNotOverdue(t *testing.T) {
	stale := testNow.Add(-10 * 24 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("7", "Vault-Integrity"))
	s.hyperStatus = map[string]string{"7": hyperStatusJSON("checking", "checking", "done", stale, 0)}
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "HB_RUNNING"); got != "1" {
		t.Errorf("HB_RUNNING=%q, want 1", got)
	}
	if got := kvValue(t, r, "HB_OVERDUE"); got != "0" {
		t.Errorf("HB_OVERDUE=%q, want 0 during a running integrity check", got)
	}
	if r.ExitCode != ExitOK {
		t.Fatalf("exit=%d, want 0 during a multi-day integrity check; summary=%q", r.ExitCode, r.Summary)
	}
	if note := r.Hyper.Tasks[0].RunningNote; !strings.Contains(note, "integrity") {
		t.Errorf("running note = %q, want it to mention the integrity check", note)
	}
}

// An idle task whose last success is past the window IS overdue.
func TestHyperOverdueWhenIdle(t *testing.T) {
	stale := testNow.Add(-10 * 24 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "WeeklyOffsite"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "done", stale, 0)}
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "HB_OVERDUE"); got != "1" {
		t.Fatalf("HB_OVERDUE=%q, want 1 (idle, last success 10 days ago > 7d)", got)
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
	if !strings.Contains(r.Summary, "overdue") {
		t.Errorf("summary should mention overdue: %q", r.Summary)
	}
}

func TestHyperFailureStates(t *testing.T) {
	stale := testNow.Add(-2 * time.Hour).Unix()
	cases := []struct {
		name      string
		result    string
		wantCheck string
		wantKV    string // HB key that should read "1"
	}{
		{"failed", "failed", "hyperbackup_failed", "HB_FAILED"},
		{"partial", "partial", "hyperbackup_failed", "HB_FAILED"},
		{"integrity", "cksum_failed", "hyperbackup_integrity", "HB_FAILED"},
		{"dest_missing", "dest_missing", "hyperbackup_destination", "HB_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hyperScenario()
			s.hyperTasks = hyperListData(hyperListEntry("1", "T"))
			s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", tc.result, stale, 0)}
			r := runScenario(t, s, nil)
			if got := kvValue(t, r, tc.wantKV); got != "1" {
				t.Errorf("%s=%q, want 1", tc.wantKV, got)
			}
			if !hasCheck(r, tc.wantCheck) {
				t.Errorf("expected check %q to fire; checks=%+v", tc.wantCheck, r.Checks)
			}
			if r.ExitCode != ExitWarning {
				t.Errorf("exit=%d, want 1 for result %q", r.ExitCode, tc.result)
			}
		})
	}
}

// A cancelled/suspended task is a warning but is NOT counted as a broken backup
// (HB_FAILED), so an intentional pause is not conflated with a failure.
func TestHyperCancelledNotCountedAsFailed(t *testing.T) {
	stale := testNow.Add(-2 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "Paused"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("suspend", "suspend", "suspend", stale, 0)}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_FAILED"); got != "0" {
		t.Errorf("HB_FAILED=%q, want 0 (suspended is not a broken backup)", got)
	}
	if !hasCheck(r, "hyperbackup_suspended") {
		t.Errorf("expected hyperbackup_suspended check to fire")
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
}

// result "none" → never backed up → overdue with a "never" last success.
func TestHyperNeverBackedUp(t *testing.T) {
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "FreshTask"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "none", 0, 0)}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got != "never" {
		t.Errorf("HB_LAST_SUCCESS=%q, want never", got)
	}
	if got := kvValue(t, r, "HB_OVERDUE"); got != "1" {
		t.Errorf("HB_OVERDUE=%q, want 1", got)
	}
}

// A success with no completion timestamp is neither failed nor overdue, but its
// freshness is indeterminate (not a false "never").
func TestHyperSuccessWithoutTimestamp(t *testing.T) {
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "NoStamp"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "done", 0, 0)}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_OVERDUE"); got != "0" {
		t.Errorf("HB_OVERDUE=%q, want 0", got)
	}
	if got := kvValue(t, r, "HB_FAILED"); got != "0" {
		t.Errorf("HB_FAILED=%q, want 0", got)
	}
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got != "Unknown" {
		t.Errorf("HB_LAST_SUCCESS=%q, want Unknown (succeeded but no timestamp)", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
}

// One task's status fetch failing degrades coverage to PARTIAL (a warning), not a
// false healthy and not a hard error, while the healthy peer is still evaluated.
func TestHyperPartialCoverage(t *testing.T) {
	end := testNow.Add(-1 * 24 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "Good"), hyperListEntry("2", "Unreadable"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "done", end, 0)}
	s.hyperStatusErrCode = map[string]int{"2": 105}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_STATE"); got != "PARTIAL" {
		t.Fatalf("HB_STATE=%q, want PARTIAL", got)
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
}

// Every monitored task's status inaccessible → hard error (exit 3), per the
// coverage contract, since no meaningful Hyper Backup statement can be made.
func TestHyperAllStatusInaccessibleIsError(t *testing.T) {
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "A"), hyperListEntry("2", "B"))
	s.hyperStatusErrCode = map[string]int{"1": 105, "2": 105}
	r := runScenario(t, s, nil)
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 (all monitored status inaccessible); summary=%q", r.ExitCode, r.Summary)
	}
}

// The Hyper Backup task list being inaccessible is a hard error (exit 3).
func TestHyperTaskListErrorIsError(t *testing.T) {
	s := hyperScenario()
	s.hyperTaskListErrCode = 105
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_STATE"); got != "ERROR" {
		t.Errorf("HB_STATE=%q, want ERROR", got)
	}
	if r.ExitCode != ExitError {
		t.Errorf("exit=%d, want 3", r.ExitCode)
	}
}

// A 402 on the enriched status call (older DSM rejecting `additional`) is retried
// without it and still yields a usable result.
func TestHyperStatusAdditionalRetry(t *testing.T) {
	end := testNow.Add(-1 * 24 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperStatusRejectAdditional = true
	s.hyperTasks = hyperListData(hyperListEntry("1", "Legacy"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "done", end, 0)}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_STATE"); got != "OK" {
		t.Fatalf("HB_STATE=%q, want OK after additional-param retry; summary=%q", got, r.Summary)
	}
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got == "Unknown" || got == "never" {
		t.Errorf("HB_LAST_SUCCESS=%q, want a timestamp (retry must recover the fields)", got)
	}
}

// Excluding a failing task removes it from the monitored set, silencing its alert,
// and it is not queried.
func TestHyperExcludeTask(t *testing.T) {
	stale := testNow.Add(-2 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "Keep"), hyperListEntry("2", "Noisy"))
	s.hyperStatus = map[string]string{
		"1": hyperStatusJSON("backupable", "none", "done", stale, 0),
		"2": hyperStatusJSON("backupable", "none", "failed", stale, 0),
	}
	r := runScenario(t, s, func(c *Config) {
		c.ExcludeHyperTasks = []Selector{{Kind: SelName, Raw: "name:Noisy", Name: "Noisy"}}
	})
	if got := kvValue(t, r, "HB_EXCLUDED"); got != "1" {
		t.Errorf("HB_EXCLUDED=%q, want 1", got)
	}
	if got := kvValue(t, r, "HB_FAILED"); got != "0" {
		t.Errorf("HB_FAILED=%q, want 0 (the failing task is excluded)", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
}

// A non-numeric task_id (Hyper Backup ids are strings) is handled end to end.
func TestHyperStringTaskID(t *testing.T) {
	end := testNow.Add(-1 * 24 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("HB_abc", "StringID"))
	s.hyperStatus = map[string]string{"HB_abc": hyperStatusJSON("backupable", "none", "done", end, 0)}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_STATE"); got != "OK" {
		t.Fatalf("HB_STATE=%q, want OK for a string task id; summary=%q", got, r.Summary)
	}
	if !s.hyperStatusCalledFor("HB_abc") {
		t.Errorf("status not queried for the string-id task")
	}
}

// The task-list entry's target_type populates the destination column (mirrors the
// real DSM list shape, where the entry carries target_type directly).
func TestHyperTargetFromTaskList(t *testing.T) {
	end := testNow.Add(-1 * 24 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = `{"task_list":[{"task_id":1,"name":"Synology C2","target_type":"cloud_image","type":"cloud_image:synocloud_swift"}]}`
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "done", end, 0)}
	r := runScenario(t, s, nil)
	if len(r.Hyper.Tasks) != 1 || r.Hyper.Tasks[0].Target != "cloud_image" {
		t.Errorf("task target = %q, want cloud_image", r.Hyper.Tasks[0].Target)
	}
	if r.Hyper.Tasks[0].Name != "Synology C2" {
		t.Errorf("task name = %q, want Synology C2 (from the list's \"name\" field)", r.Hyper.Tasks[0].Name)
	}
}

func TestParseHyperTime(t *testing.T) {
	epoch := testNow.Add(-2 * time.Hour)
	cases := []struct{ in, want string }{
		{"2026/07/22 02:57:32", "2026-07-22T02:57:32Z"},                         // slash, with seconds
		{"2026/07/23 01:10", "2026-07-23T01:10:00Z"},                            // slash, no seconds (next_bkp_time)
		{"2026-07-22 03:09:19", "2026-07-22T03:09:19Z"},                         // dash, with seconds
		{"2026-07-23 01:10", "2026-07-23T01:10:00Z"},                            // dash, no seconds
		{strconv.FormatInt(epoch.Unix(), 10), epoch.UTC().Format(time.RFC3339)}, // numeric epoch fallback
		{"", ""},
		{"not a date", ""},
	}
	for _, tc := range cases {
		got, ok := parseHyperTime(tc.in, testNow)
		if tc.want == "" {
			if ok {
				t.Errorf("parseHyperTime(%q) ok=true, want not-ok", tc.in)
			}
			continue
		}
		if !ok || got.Format(time.RFC3339) != tc.want {
			t.Errorf("parseHyperTime(%q)=%v ok=%v, want %s", tc.in, got, ok, tc.want)
		}
	}
}

// Regression for the live-NAS shape: timestamps are wall-clock strings and
// last_bkp_success_time is the authoritative last success (what the DSM UI shows).
// This is the exact bug from the first --debug run: HB_LAST_SUCCESS was Unknown
// because the string dates were parsed as integer epochs.
func TestHyperStringTimestampsAndSuccessTime(t *testing.T) {
	success := testNow.Add(-9 * time.Hour)
	end := testNow.Add(-8 * time.Hour) // the run ended a bit after it last succeeded
	next := testNow.Add(13 * time.Hour)
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "Synology C2"))
	s.hyperStatus = map[string]string{"1": `{` +
		`"state":"backupable","status":"none","last_bkp_result":"done",` +
		`"last_bkp_success_time":"` + dsmTime(success) + `",` +
		`"last_bkp_end_time":"` + dsmTime(end) + `",` +
		`"last_bkp_time":"` + dsmTime(end) + `",` +
		`"next_bkp_time":"` + next.UTC().Format("2006/01/02 15:04") + `",` +
		`"last_bkp_progress":96,"last_bkp_error_code":4401}`}
	r := runScenario(t, s, nil)

	wantLS := success.UTC().Format(time.RFC3339)
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got != wantLS {
		t.Errorf("HB_LAST_SUCCESS=%q, want %q (from last_bkp_success_time)", got, wantLS)
	}
	task := r.Hyper.Tasks[0]
	if task.NextBackup == nil {
		t.Errorf("NextBackup was not parsed from the no-seconds string")
	}
	if task.Note != "" {
		t.Errorf("note=%q, want empty (freshness is now determinate)", task.Note)
	}
	if task.Overdue {
		t.Errorf("task should not be overdue (last success 9h ago, 7d window)")
	}
	if r.ExitCode != ExitOK {
		t.Fatalf("exit=%d, want 0; summary=%q", r.ExitCode, r.Summary)
	}
}

// last_bkp_success_time gives real success history: a task whose latest run failed
// but which succeeded recently is failed, NOT also overdue.
func TestHyperFailedButRecentSuccessNotOverdue(t *testing.T) {
	success := testNow.Add(-6 * time.Hour)
	end := testNow.Add(-1 * time.Hour)
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "Flaky"))
	s.hyperStatus = map[string]string{"1": `{"state":"backupable","status":"none","last_bkp_result":"failed",` +
		`"last_bkp_success_time":"` + dsmTime(success) + `","last_bkp_end_time":"` + dsmTime(end) + `"}`}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "HB_FAILED"); got != "1" {
		t.Errorf("HB_FAILED=%q, want 1 (last run failed)", got)
	}
	if got := kvValue(t, r, "HB_OVERDUE"); got != "0" {
		t.Errorf("HB_OVERDUE=%q, want 0 (last success 6h ago is within the window)", got)
	}
	if got := kvValue(t, r, "HB_LAST_SUCCESS"); got != success.UTC().Format(time.RFC3339) {
		t.Errorf("HB_LAST_SUCCESS=%q, want the recent success %q", got, success.UTC().Format(time.RFC3339))
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
}

// An unrecognized last_bkp_result surfaces as indeterminate (a warning), never a
// silent healthy.
func TestHyperUnknownResultSurfaces(t *testing.T) {
	stale := testNow.Add(-2 * time.Hour).Unix()
	s := hyperScenario()
	s.hyperTasks = hyperListData(hyperListEntry("1", "Weird"))
	s.hyperStatus = map[string]string{"1": hyperStatusJSON("backupable", "none", "quantum_flux", stale, 0)}
	r := runScenario(t, s, nil)
	if !hasCheck(r, "hyperbackup_unknown") {
		t.Errorf("expected hyperbackup_unknown check for an unrecognized result")
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
}
