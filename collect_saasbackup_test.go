package main

import (
	"fmt"
	"testing"
	"time"
)

// Classification is evidence-based, NOT from the task_status code — that code is
// inconsistent across the products (Microsoft 365 reports 2, Google Workspace 1 for
// the same "done" state). A task that has a dated run with no error counters is a
// success whatever its task_status code says.
func TestSaaSSuccessByEvidence(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	for _, code := range []int{1, 2, 99} {
		s := m365Scenario()
		s.m365Tasks = saasListData(saasTaskEntry(1, "T", 1, code, 0, recent))
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "M365_STATE"); got != "OK" {
			t.Errorf("task_status=%d: M365_STATE=%q, want OK", code, got)
		}
		if got := kvValue(t, r, "M365_FAILED"); got != "0" {
			t.Errorf("task_status=%d: M365_FAILED=%q, want 0", code, got)
		}
		if got := kvValue(t, r, "M365_OVERDUE"); got != "0" {
			t.Errorf("task_status=%d: M365_OVERDUE=%q, want 0 (recent success)", code, got)
		}
		if got := kvValue(t, r, "M365_LAST_SUCCESS"); got == "never" || got == "Unknown" || got == "N/A" {
			t.Errorf("task_status=%d: M365_LAST_SUCCESS=%q, want a timestamp", code, got)
		}
		if r.ExitCode != ExitOK {
			t.Errorf("task_status=%d: exit=%d, want 0", code, r.ExitCode)
		}
	}
}

// Regression for the live-NAS shape: Google Workspace reports task_status 1 (not 2)
// for a healthy task and omits task_status_error_code entirely. It must classify as a
// success, never "indeterminate".
func TestGWSHealthyRealShape(t *testing.T) {
	recent := testNow.Add(-17 * time.Hour)
	s := defaultScenario()
	s.gwsAdvertise = true
	s.gwsTasks = fmt.Sprintf(`{"tasks":[{"task_id":1,"task_name":"Backup raphaelhome.com","status":1,"task_status":1,"success_count":5,"attention_count":0,"error_mail":0,"error_drive":0,"last_execution_time":%d}]}`, recent.Unix())
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "GWS_STATE"); got != "OK" {
		t.Fatalf("GWS_STATE=%q, want OK; summary=%q", got, r.Summary)
	}
	if hasCheck(r, "gws_unknown") {
		t.Errorf("gws_unknown must not fire for a healthy task_status=1 task")
	}
	if got := kvValue(t, r, "GWS_LAST_SUCCESS"); got != recent.UTC().Format(time.RFC3339) {
		t.Errorf("GWS_LAST_SUCCESS=%q, want the run time %q", got, recent.UTC().Format(time.RFC3339))
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
}

// Live-NAS shape: an M365 task whose last_execution_time is months old, with no
// errors, is a stale success → overdue. This is the tool working as intended — it
// flags a backup that has genuinely stopped completing.
func TestM365RealShapeStaleIsOverdue(t *testing.T) {
	stale := testNow.Add(-180 * 24 * time.Hour) // ~6 months ago
	s := m365Scenario()
	s.m365Tasks = fmt.Sprintf(`{"tasks":[{"task_id":1,"task_name":"Backup raphaelhome.com","status":1,"task_status":2,"task_status_error_code":0,"success_count":5,"attention_count":0,"progress_list":[],"last_execution_time":%d}]}`, stale.Unix())
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_STATE"); got != "OK" {
		t.Fatalf("M365_STATE=%q, want OK", got)
	}
	if got := kvValue(t, r, "M365_FAILED"); got != "0" {
		t.Errorf("M365_FAILED=%q, want 0 (no errors)", got)
	}
	if got := kvValue(t, r, "M365_OVERDUE"); got != "1" {
		t.Errorf("M365_OVERDUE=%q, want 1 (last success ~6 months ago)", got)
	}
	if got := kvValue(t, r, "M365_LAST_SUCCESS"); got != stale.UTC().Format(time.RFC3339) {
		t.Errorf("M365_LAST_SUCCESS=%q, want %q", got, stale.UTC().Format(time.RFC3339))
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
}

// Not advertised and package absent → not installed, which is N/A (never an error),
// and must not affect the run's exit code.
func TestSaaSNotInstalled(t *testing.T) {
	r := runScenario(t, defaultScenario(), nil) // default apis advertise neither SaaS API
	for _, p := range []string{"M365", "GWS"} {
		if got := kvValue(t, r, p+"_STATE"); got != "NOT_INSTALLED" {
			t.Errorf("%s_STATE=%q, want NOT_INSTALLED", p, got)
		}
		if got := kvValue(t, r, p+"_LAST_SUCCESS"); got != "N/A" {
			t.Errorf("%s_LAST_SUCCESS=%q, want N/A", p, got)
		}
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0 (not-installed SaaS backups must not alter health)", r.ExitCode)
	}
}

func m365Scenario() *dsmScenario {
	s := defaultScenario()
	s.m365Advertise = true
	return s
}

// A healthy idle task: live status 1 (idle), task_status 2 (success), a recent
// execution → OK with a real last-success timestamp.
func TestSaaSHealthy(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	s := m365Scenario()
	s.m365Tasks = saasListData(saasTaskEntry(1, "Contoso", 1, 2, 0, recent))
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "M365_STATE"); got != "OK" {
		t.Fatalf("M365_STATE=%q, want OK; summary=%q", got, r.Summary)
	}
	if got := kvValue(t, r, "M365_MONITORED"); got != "1" {
		t.Errorf("M365_MONITORED=%q, want 1", got)
	}
	if got := kvValue(t, r, "M365_FAILED"); got != "0" {
		t.Errorf("M365_FAILED=%q, want 0", got)
	}
	if got := kvValue(t, r, "M365_OVERDUE"); got != "0" {
		t.Errorf("M365_OVERDUE=%q, want 0", got)
	}
	if got := kvValue(t, r, "M365_LAST_SUCCESS"); got == "never" || got == "Unknown" || got == "N/A" {
		t.Errorf("M365_LAST_SUCCESS=%q, want a real timestamp", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
}

// The central caveat (as with Hyper Backup): a task backing up RIGHT NOW is never
// overdue or failed, however old its last completion is. M365 backs up continuously.
func TestSaaSRunningSuppressesOverdue(t *testing.T) {
	stale := testNow.Add(-10 * 24 * time.Hour).Unix() // 10 days ago (> 48h window)
	s := m365Scenario()
	s.m365Tasks = saasListData(saasTaskEntry(1, "Continuous", 4, 2, 0, stale)) // status 4 = running
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "M365_RUNNING"); got != "1" {
		t.Errorf("M365_RUNNING=%q, want 1", got)
	}
	if got := kvValue(t, r, "M365_OVERDUE"); got != "0" {
		t.Errorf("M365_OVERDUE=%q, want 0 (a running task is never overdue)", got)
	}
	if got := kvValue(t, r, "M365_FAILED"); got != "0" {
		t.Errorf("M365_FAILED=%q, want 0 (a running task is never failed)", got)
	}
	if r.ExitCode != ExitOK {
		t.Fatalf("exit=%d, want 0 for a running backup; summary=%q", r.ExitCode, r.Summary)
	}
	if len(r.M365.Tasks) != 1 || !r.M365.Tasks[0].Running {
		t.Fatalf("task not marked running: %+v", r.M365.Tasks)
	}
}

// A non-empty progress_list is a live-activity signal even when the numeric status
// is not one we recognize as running, and it likewise suppresses overdue.
func TestSaaSProgressListSuppressesOverdue(t *testing.T) {
	stale := testNow.Add(-10 * 24 * time.Hour).Unix()
	s := m365Scenario()
	s.m365Tasks = fmt.Sprintf(`{"tasks":[{"task_id":1,"task_name":"Busy","status":1,"task_status":2,"last_execution_time":%d,"progress_list":[{"phase":"drive"}]}]}`, stale)
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_RUNNING"); got != "1" {
		t.Errorf("M365_RUNNING=%q, want 1 (non-empty progress_list)", got)
	}
	if got := kvValue(t, r, "M365_OVERDUE"); got != "0" {
		t.Errorf("M365_OVERDUE=%q, want 0", got)
	}
}

// An idle task whose last success is past the window IS overdue.
func TestSaaSOverdueWhenIdle(t *testing.T) {
	stale := testNow.Add(-3 * 24 * time.Hour).Unix() // 3 days ago > 48h
	s := m365Scenario()
	s.m365Tasks = saasListData(saasTaskEntry(1, "Weekly", 1, 2, 0, stale))
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_OVERDUE"); got != "1" {
		t.Fatalf("M365_OVERDUE=%q, want 1 (idle, last success 3 days ago > 48h)", got)
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
	if !hasCheck(r, "m365_overdue") {
		t.Errorf("expected m365_overdue check to fire")
	}
}

// A task-level error code is an authoritative failure, independent of task_status.
func TestSaaSTaskLevelErrorFails(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	s := m365Scenario()
	s.m365Tasks = saasListData(saasTaskEntry(1, "Broken", 1, 2, 5, recent)) // error code 5, task_status still 2
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_FAILED"); got != "1" {
		t.Errorf("M365_FAILED=%q, want 1 (task_status_error_code != 0)", got)
	}
	if !hasCheck(r, "m365_failed") {
		t.Errorf("expected m365_failed check to fire")
	}
	if r.ExitCode != ExitWarning {
		t.Errorf("exit=%d, want 1", r.ExitCode)
	}
	// A failed task yields no success time and is therefore not also overdue.
	if got := kvValue(t, r, "M365_OVERDUE"); got != "0" {
		t.Errorf("M365_OVERDUE=%q, want 0 (a failed task is flagged failed, not overdue)", got)
	}
}

// Per-service errors or attention items → a partial (attention) state: a warning
// counted in the broken total, but distinct from a task-level failure.
func TestSaaSAttentionIsPartial(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	cases := map[string]string{
		"attention_count": fmt.Sprintf(`{"tasks":[{"task_id":1,"task_name":"A","status":1,"task_status":2,"attention_count":2,"last_execution_time":%d}]}`, recent),
		"per_service":     fmt.Sprintf(`{"tasks":[{"task_id":1,"task_name":"A","status":1,"task_status":2,"error_mail":1,"error_drive":0,"last_execution_time":%d}]}`, recent),
	}
	for name, tasks := range cases {
		t.Run(name, func(t *testing.T) {
			s := m365Scenario()
			s.m365Tasks = tasks
			r := runScenario(t, s, nil)
			if got := kvValue(t, r, "M365_FAILED"); got != "1" {
				t.Errorf("M365_FAILED=%q, want 1 (attention counts as broken)", got)
			}
			if !hasCheck(r, "m365_failed") {
				t.Errorf("expected m365_failed check to fire")
			}
			if len(r.M365.Tasks) != 1 || !r.M365.Tasks[0].Partial {
				t.Errorf("task not marked partial: %+v", r.M365.Tasks)
			}
			if r.ExitCode != ExitWarning {
				t.Errorf("exit=%d, want 1", r.ExitCode)
			}
		})
	}
}

// A task with no dated execution → never backed up → overdue with a "never" last
// success.
func TestSaaSNeverBackedUp(t *testing.T) {
	s := m365Scenario()
	s.m365Tasks = saasListData(saasTaskEntry(1, "Fresh", 1, 0, 0, 0)) // no last_execution_time
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_LAST_SUCCESS"); got != "never" {
		t.Errorf("M365_LAST_SUCCESS=%q, want never", got)
	}
	if got := kvValue(t, r, "M365_OVERDUE"); got != "1" {
		t.Errorf("M365_OVERDUE=%q, want 1", got)
	}
}

// enable_schedule:false is NORMAL for M365 continuous backup and must NOT be read as
// "disabled" — the task stays monitored.
func TestSaaSScheduleDisabledStillMonitored(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	s := m365Scenario()
	s.m365Tasks = fmt.Sprintf(`{"tasks":[{"task_id":1,"task_name":"Continuous","status":1,"task_status":2,"enable_schedule":false,"last_execution_time":%d}]}`, recent)
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_MONITORED"); got != "1" {
		t.Errorf("M365_MONITORED=%q, want 1 (enable_schedule:false is not 'disabled')", got)
	}
	if got := kvValue(t, r, "M365_DISABLED"); got != "0" {
		t.Errorf("M365_DISABLED=%q, want 0", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
}

// Excluding a failing task removes it from the monitored set, silencing its alert.
func TestSaaSExcludeTask(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	s := m365Scenario()
	s.m365Tasks = saasListData(
		saasTaskEntry(1, "Keep", 1, 2, 0, recent),
		saasTaskEntry(2, "Noisy", 1, 2, 9, recent), // error code 9 → would fail
	)
	r := runScenario(t, s, func(c *Config) {
		c.ExcludeM365Tasks = []Selector{{Kind: SelName, Raw: "name:Noisy", Name: "Noisy"}}
	})
	if got := kvValue(t, r, "M365_EXCLUDED"); got != "1" {
		t.Errorf("M365_EXCLUDED=%q, want 1", got)
	}
	if got := kvValue(t, r, "M365_FAILED"); got != "0" {
		t.Errorf("M365_FAILED=%q, want 0 (the failing task is excluded)", got)
	}
	if r.ExitCode != ExitOK {
		t.Errorf("exit=%d, want 0", r.ExitCode)
	}
}

// The task list being inaccessible is a hard error (exit 3), per the coverage
// contract, since no meaningful statement can be made about the SaaS backups.
func TestSaaSTaskListErrorIsError(t *testing.T) {
	s := m365Scenario()
	s.m365TaskListErrCode = 105
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_STATE"); got != "ERROR" {
		t.Errorf("M365_STATE=%q, want ERROR", got)
	}
	if r.ExitCode != ExitError {
		t.Errorf("exit=%d, want 3", r.ExitCode)
	}
}

// A missing "tasks" field (schema drift) must be an error, not a false empty-healthy.
func TestSaaSMissingTasksFieldIsError(t *testing.T) {
	s := m365Scenario()
	s.m365Tasks = `{}` // no "tasks" field
	r := runScenario(t, s, nil)
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 for a missing tasks field", r.ExitCode)
	}
}

// The package is present but the API is not advertised (e.g. account privileges) →
// unavailable, which is a coverage gap (exit 3), not a false "not installed".
func TestSaaSPackagePresentButAPIInaccessible(t *testing.T) {
	s := defaultScenario()
	s.packages = []string{"ActiveBackup", "ActiveBackup-Office365"} // installed, but API not advertised
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "M365_STATE"); got != "UNAVAILABLE" {
		t.Errorf("M365_STATE=%q, want UNAVAILABLE (installed but API inaccessible)", got)
	}
	if r.ExitCode != ExitError {
		t.Errorf("exit=%d, want 3 (a present-but-unreadable backup is a monitoring gap)", r.ExitCode)
	}
}

// The Google Workspace flavor runs the same shared code down a distinct API and KV
// prefix, and its checks carry the gws_ prefix.
func TestGWSHealthyAndFailure(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	t.Run("healthy", func(t *testing.T) {
		s := defaultScenario()
		s.gwsAdvertise = true
		s.gwsTasks = saasListData(saasTaskEntry(1, "Acme Drive", 1, 2, 0, recent))
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "GWS_STATE"); got != "OK" {
			t.Fatalf("GWS_STATE=%q, want OK; summary=%q", got, r.Summary)
		}
		if got := kvValue(t, r, "GWS_MONITORED"); got != "1" {
			t.Errorf("GWS_MONITORED=%q, want 1", got)
		}
		if r.ExitCode != ExitOK {
			t.Errorf("exit=%d, want 0", r.ExitCode)
		}
	})
	t.Run("failure-uses-gws-prefix", func(t *testing.T) {
		s := defaultScenario()
		s.gwsAdvertise = true
		s.gwsTasks = saasListData(saasTaskEntry(1, "Acme Drive", 1, 2, 7, recent))
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "GWS_FAILED"); got != "1" {
			t.Errorf("GWS_FAILED=%q, want 1", got)
		}
		if !hasCheck(r, "gws_failed") {
			t.Errorf("expected gws_failed check (not m365_failed)")
		}
		if hasCheck(r, "m365_failed") {
			t.Errorf("m365_failed must not fire for a Google Workspace failure")
		}
	})
}

// last_execution_time is a Unix epoch; a healthy task's KV last-success reflects it.
func TestSaaSEpochLastSuccess(t *testing.T) {
	at := testNow.Add(-90 * time.Minute)
	s := m365Scenario()
	s.m365Tasks = saasListData(saasTaskEntry(1, "T", 1, 2, 0, at.Unix()))
	r := runScenario(t, s, nil)
	want := at.UTC().Format(time.RFC3339)
	if got := kvValue(t, r, "M365_LAST_SUCCESS"); got != want {
		t.Errorf("M365_LAST_SUCCESS=%q, want %q (from last_execution_time epoch)", got, want)
	}
}
