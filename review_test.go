package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---- [P1] #1: missing ABB arrays must not read as empty-healthy ----

func TestABBMissingArrays(t *testing.T) {
	t.Run("tasks-object-empty-is-error", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{}` // no "tasks" field at all
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 for missing tasks field", r.ExitCode)
		}
	})
	t.Run("tasks-null-is-error", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":null}`
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 for tasks:null", r.ExitCode)
		}
	})
	t.Run("tasks-empty-array-is-valid", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":[]}`
		s.versions = map[int64][]string{}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 for a genuinely empty task list; summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "ABB_TASKS"); got != "0" {
			t.Errorf("ABB_TASKS=%q, want 0", got)
		}
	})
	t.Run("packages-missing-field-is-unavailable-not-not-installed", func(t *testing.T) {
		s := defaultScenario()
		apis := defaultApis()
		delete(apis, "SYNO.ActiveBackup.Task") // force the package-list check
		s.apis = apis
		s.packageBody = `{}` // packages field absent
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "ABB_STATE"); got != "UNAVAILABLE" {
			t.Errorf("ABB_STATE=%q, want UNAVAILABLE (must not claim NOT_INSTALLED)", got)
		}
	})
	t.Run("versions-missing-field-is-unknown-not-never", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":[{"task_id":1,"task_name":"T","enabled":true}]}`
		s.versions = map[int64][]string{1: {`{}`}} // no "versions" field
		r := runScenario(t, s, nil)
		// A missing versions field is a parse error → task inaccessible → ABB error,
		// never a false "never succeeded" / overdue.
		if got := kvValue(t, r, "LAST_SUCCESS"); got == "never" {
			t.Errorf("LAST_SUCCESS=never on a missing versions field; must be Unknown/error")
		}
		if got := kvValue(t, r, "ABB_OVERDUE"); got == "1" {
			t.Errorf("task marked overdue on a missing versions field")
		}
	})
}

// ---- [P1] #2: invalid debug payloads must not break JSON output ----

func TestDebugInvalidPayloadStillValidJSON(t *testing.T) {
	t.Run("json-format-with-html-system", func(t *testing.T) {
		s := defaultScenario()
		s.systemHTML = true
		srv := httptest.NewTLSServer(s.handler())
		defer srv.Close()
		cfg := testConfig(srv.URL)
		cfg.Debug = true
		cfg.Format = "json"
		r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
		var buf strings.Builder
		if err := render(&buf, cfg.Format, r); err != nil {
			t.Fatalf("render error: %v", err)
		}
		if !json.Valid([]byte(buf.String())) {
			t.Errorf("json output is not valid JSON:\n%s", buf.String())
		}
	})
	t.Run("both-format-with-malformed-storage", func(t *testing.T) {
		s := defaultScenario()
		s.storageHTML = true
		srv := httptest.NewTLSServer(s.handler())
		defer srv.Close()
		cfg := testConfig(srv.URL)
		cfg.Debug = true
		cfg.Format = "both"
		r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
		var buf strings.Builder
		if err := render(&buf, cfg.Format, r); err != nil {
			t.Fatalf("render error: %v", err)
		}
		out := buf.String()
		parts := strings.SplitN(out, "\n---\n", 2)
		if len(parts) != 2 {
			t.Fatalf("both output missing separator:\n%s", out)
		}
		if !strings.Contains(parts[0], "STATUS=") {
			t.Errorf("KV section malformed:\n%s", parts[0])
		}
		if !json.Valid([]byte(parts[1])) {
			t.Errorf("JSON section is not valid JSON:\n%s", parts[1])
		}
	})
	t.Run("raw-text-is-bounded", func(t *testing.T) {
		body := []byte(strings.Repeat("<x>", 100000))
		got := boundedText(body)
		if len([]byte(got)) > 8192+32 {
			t.Errorf("boundedText did not bound length: got %d bytes", len(got))
		}
	})
}

// ---- [P1] #3: missing/invalid volume capacity is not healthy 0% ----

func TestVolumeCapacityUnknown(t *testing.T) {
	cases := map[string]string{
		"missing-size":  `{"id":"volume_1","status":"normal"}`,
		"missing-total": `{"id":"volume_1","status":"normal","size":{"used":"10"}}`,
		"zero-total":    `{"id":"volume_1","status":"normal","size":{"total":"0","used":"0"}}`,
		"negative-used": `{"id":"volume_1","status":"normal","size":{"total":"100","used":"-5"}}`,
		"used-gt-total": `{"id":"volume_1","status":"normal","size":{"total":"100","used":"140"}}`,
	}
	for name, vol := range cases {
		t.Run(name, func(t *testing.T) {
			s := defaultScenario()
			s.storage = `{"storagePools":[{"id":"pool_1","status":"normal","size":{"total":"100","used":"10"}}],
			              "volumes":[` + vol + `],
			              "disks":[{"id":"sata1","name":"Drive 1","status":"normal"}]}`
			r := runScenario(t, s, nil)
			if got := kvValue(t, r, "VOLUME_USAGE"); got != "Unknown" {
				t.Errorf("VOLUME_USAGE=%q, want Unknown (missing/invalid capacity must not read as 0%%)", got)
			}
			if r.Status == "OK" {
				t.Errorf("status OK with unknown volume capacity; want at least WARNING")
			}
		})
	}
}

// ---- [P1] #5: output write failure returns exit 3 ----

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("simulated write failure") }

func TestRenderFailureExits3(t *testing.T) {
	run3 := func(t *testing.T, storage string) {
		srv := httptest.NewTLSServer(func() *dsmScenario {
			s := defaultScenario()
			if storage != "" {
				s.storage = storage
			}
			return s
		}().handler())
		defer srv.Close()
		t.Setenv("DSM_HOST", srv.URL)
		t.Setenv("DSM_USERNAME", "svc")
		t.Setenv("DSM_PASSWORD", "pw")
		code := run([]string{"--insecure-skip-verify"}, failWriter{}, io.Discard)
		if code != ExitError {
			t.Fatalf("code=%d, want 3 when stdout write fails", code)
		}
	}
	t.Run("healthy-report", func(t *testing.T) { run3(t, "") })
	t.Run("critical-report", func(t *testing.T) {
		run3(t, `{"storagePools":[{"id":"pool_1","status":"crashed","size":{"total":"100","used":"40"}}],
		          "volumes":[{"id":"volume_1","status":"normal","size":{"total":"100","used":"40"}}],
		          "disks":[{"id":"sata1","status":"normal"}]}`)
	})
}

// ---- [P2] #6: malformed / missing task IDs ----

func TestMalformedTaskIDs(t *testing.T) {
	t.Run("nonnumeric-id-is-inaccessible", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":[{"task_id":"invalid","task_name":"Bad","enabled":true}]}`
		s.versions = map[int64][]string{}
		r := runScenario(t, s, nil)
		// A single monitored task with no usable id → all monitored history
		// inaccessible → ABB error, exit 3.
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 (invalid id, no accessible history); summary=%q", r.ExitCode, r.Summary)
		}
	})
	t.Run("zero-id-is-invalid", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":[{"task_id":0,"task_name":"Zero","enabled":true}]}`
		s.versions = map[int64][]string{}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 (task_id 0 is not valid)", r.ExitCode)
		}
		if s.versionCalledFor(0) {
			t.Errorf("must not query version history for task_id 0")
		}
	})
	t.Run("enabled-as-numeric-and-string", func(t *testing.T) {
		// enabled:0 (disabled), is_enabled:"false" (disabled), enable:1 (enabled).
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.tasks = `{"tasks":[
			{"task_id":1,"task_name":"NumDisabled","enabled":0},
			{"task_id":2,"task_name":"StrDisabled","is_enabled":"false"},
			{"task_id":3,"task_name":"NumEnabled","enable":1}
		]}`
		s.versions = map[int64][]string{
			3: {versionsPage(versionJSON(recent, "complete"))},
		}
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "ABB_DISABLED"); got != "2" {
			t.Errorf("ABB_DISABLED=%q, want 2 (numeric 0 and string \"false\")", got)
		}
		if got := kvValue(t, r, "ABB_MONITORED"); got != "1" {
			t.Errorf("ABB_MONITORED=%q, want 1", got)
		}
	})
	t.Run("all-monitored-invalid-is-error", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":[
			{"task_id":"x","task_name":"A","enabled":true},
			{"task_name":"B","enabled":true}
		]}`
		s.versions = map[int64][]string{}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 (all monitored tasks have invalid ids)", r.ExitCode)
		}
	})
	t.Run("missing-id-with-healthy-peer-is-partial", func(t *testing.T) {
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.tasks = `{"tasks":[
			{"task_id":1,"task_name":"Good","enabled":true},
			{"task_name":"NoID","enabled":true}
		]}`
		s.versions = map[int64][]string{1: {versionsPage(versionJSON(recent, "complete"))}}
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "ABB_STATE"); got != "PARTIAL" {
			t.Errorf("ABB_STATE=%q, want PARTIAL (one task lacks a usable id)", got)
		}
		if r.ExitCode != ExitWarning {
			t.Errorf("exit=%d, want 1", r.ExitCode)
		}
	})
	t.Run("duplicate-ids-do-not-crash", func(t *testing.T) {
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.tasks = `{"tasks":[
			{"task_id":1,"task_name":"A","enabled":true},
			{"task_id":1,"task_name":"B","enabled":true}
		]}`
		s.versions = map[int64][]string{1: {versionsPage(versionJSON(recent, "complete"))}}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 for duplicate ids; summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "ABB_TASKS"); got != "2" {
			t.Errorf("ABB_TASKS=%q, want 2 (both retained)", got)
		}
	})
}

// Real-DSM shape: version restore points carry numeric status 3 (success) and
// the task's last_result reports the latest attempt as failed. Mirrors the live
// NAS that surfaced the "never" bug.
func TestABBNumericStatusAndLastResult(t *testing.T) {
	successAt := testNow.Add(-44 * 24 * time.Hour).Unix() // ~44 days ago
	attemptAt := testNow.Add(-2 * time.Hour).Unix()
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"andreas-Default","enabled":true,` +
		`"last_result":{"status":5,"error_count":0,"success_count":0,"time_end":` + itoa(attemptAt) + `}}]}`
	s.versions = map[int64][]string{
		1: {`{"versions":[{"status":3,"time_end":` + itoa(successAt) + `,"time_start":` + itoa(successAt-100) + `}]}`},
	}
	r := runScenario(t, s, nil)

	if got := kvValue(t, r, "LAST_SUCCESS"); got == "never" || got == "Unknown" {
		t.Errorf("LAST_SUCCESS=%q; numeric status 3 must be recognized as a real success", got)
	}
	if got := kvValue(t, r, "ABB_FAILED"); got != "1" {
		t.Errorf("ABB_FAILED=%q, want 1 (last_result status 5)", got)
	}
	if got := kvValue(t, r, "ABB_OVERDUE"); got != "1" {
		t.Errorf("ABB_OVERDUE=%q, want 1 (last success 44 days ago)", got)
	}
	if hasCheck(r, "abb_unknown") {
		t.Errorf("abb_unknown must not fire once numeric statuses are recognized")
	}
}

// A just-completed successful backup: last_result end time aligns with a fresh
// restore point, so the latest attempt is success (not cancelled/failed) even
// though the raw status code is an ambiguous "2". Mirrors the live re-run.
func TestABBLatestAttemptSuccessByVersionAlignment(t *testing.T) {
	justNow := testNow.Add(-1 * time.Minute).Unix()    // fresh restore point
	resultEnd := testNow.Add(-30 * time.Second).Unix() // attempt ended just after
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"WeaveDocs","enabled":true,` +
		`"last_result":{"status":2,"error_count":0,"success_count":0,"time_end":` + itoa(resultEnd) + `}}]}`
	s.versions = map[int64][]string{
		1: {`{"versions":[{"status":3,"time_end":` + itoa(justNow) + `}]}`},
	}
	r := runScenario(t, s, nil)
	if r.Status != "OK" || r.ExitCode != ExitOK {
		t.Fatalf("status=%s exit=%d, want OK/0 for a just-succeeded backup; summary=%q", r.Status, r.ExitCode, r.Summary)
	}
	if got := kvValue(t, r, "ABB_FAILED"); got != "0" {
		t.Errorf("ABB_FAILED=%q, want 0", got)
	}
	if len(r.ABB.Tasks) != 1 || r.ABB.Tasks[0].Cancelled {
		t.Errorf("task incorrectly marked cancelled: %+v", r.ABB.Tasks[0])
	}
	if got := r.ABB.Tasks[0].LatestAttempt.Outcome; got != OutcomeSuccess {
		t.Errorf("latest_attempt outcome=%s, want success", got)
	}
}

// last_result with a device error (status 4, error_count>0, success_count==0) is
// a failure even though "partial" status codes are ambiguous.
func TestABBLastResultErrorCountIsFailure(t *testing.T) {
	successAt := testNow.Add(-10 * 24 * time.Hour).Unix()
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"Docker","enabled":true,` +
		`"last_result":{"status":4,"error_count":1,"success_count":0,"time_end":` + itoa(testNow.Add(-time.Hour).Unix()) + `}}]}`
	s.versions = map[int64][]string{
		1: {`{"versions":[{"status":3,"time_end":` + itoa(successAt) + `}]}`},
	}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "ABB_FAILED"); got != "1" {
		t.Errorf("ABB_FAILED=%q, want 1 (error_count>0, success_count==0)", got)
	}
}

// [P1] #4: the NinjaOne wrapper must not pass the password on the command line.
func TestNinjaWrapperDoesNotPassPasswordArg(t *testing.T) {
	b, err := os.ReadFile("examples/ninjaone.ps1")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Contains(src, "'--password',") {
		t.Errorf("ninjaone.ps1 passes --password on the command line; it should use the DSM_PASSWORD env var")
	}
	if !strings.Contains(src, "env:DSM_PASSWORD") {
		t.Errorf("ninjaone.ps1 should authenticate via the DSM_PASSWORD environment variable")
	}
}

// ---- [P2] #7: disabled/excluded tasks skip history ----

func TestUnmonitoredTasksSkipHistory(t *testing.T) {
	t.Run("disabled-task-history-not-called", func(t *testing.T) {
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.tasks = `{"tasks":[
			{"task_id":1,"task_name":"Live","enabled":true},
			{"task_id":2,"task_name":"Off","enabled":false}
		]}`
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(recent, "complete"))},
		}
		s.versionErrCode = map[int64]int{2: 105} // would fail if queried
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 (disabled task must not affect run); summary=%q", r.ExitCode, r.Summary)
		}
		if s.versionCalledFor(2) {
			t.Errorf("version history was queried for a disabled task")
		}
	})
	t.Run("excluded-task-history-not-called", func(t *testing.T) {
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.tasks = `{"tasks":[
			{"task_id":1,"task_name":"Live","enabled":true},
			{"task_id":2,"task_name":"Skip","enabled":true}
		]}`
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(recent, "complete"))},
		}
		s.versionErrCode = map[int64]int{2: 105}
		r := runScenario(t, s, func(c *Config) {
			c.ExcludeTasks = []Selector{{Kind: SelID, Raw: "id:2", ID: 2}}
		})
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 (excluded task must not affect run); summary=%q", r.ExitCode, r.Summary)
		}
		if s.versionCalledFor(2) {
			t.Errorf("version history was queried for an excluded task")
		}
	})
}

// ---- [P2] #8: config errors honor requested format ----

func TestConfigErrorHonorsFormat(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantKV   bool
		wantJSON bool
	}{
		{"kv", []string{"--format", "kv", "--vol-warn", "95", "--vol-crit", "90"}, true, false},
		{"json", []string{"--format", "json", "--vol-warn", "95", "--vol-crit", "90"}, false, true},
		{"both", []string{"--format", "both", "--vol-warn", "95", "--vol-crit", "90"}, true, true},
		{"invalid-format-falls-back-both", []string{"--format", "yaml"}, true, true},
		{"missing-format-arg-falls-back-both", []string{"--format"}, true, true},
		{"equals-form-json", []string{"--format=json"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := requestedFormat(tc.args)
			var buf strings.Builder
			_ = render(&buf, got, minimalErrorReport("boom"))
			out := buf.String()
			hasKV := strings.Contains(out, "STATUS=ERROR")
			hasJSON := strings.Contains(out, `"status": "ERROR"`)
			if hasKV != tc.wantKV || hasJSON != tc.wantJSON {
				t.Errorf("format %q: hasKV=%v hasJSON=%v, want KV=%v JSON=%v\n%s", got, hasKV, hasJSON, tc.wantKV, tc.wantJSON, out)
			}
		})
	}
}

// ---- [P2] #9: paginated debug responses do not overwrite each other ----

func TestDebugPaginationKeysDistinct(t *testing.T) {
	full := make([]string, versionPageLimit)
	for i := range full {
		full[i] = versionJSON(testNow.Add(-time.Duration(i)*time.Minute).Unix(), "complete")
	}
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"P","enabled":true}]}`
	// Exactly one full page then a terminating empty page.
	s.versions = map[int64][]string{1: {versionsPage(full...), `{"versions":[]}`}}
	srv := httptest.NewTLSServer(s.handler())
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.Debug = true
	r := collect(context.Background(), cfg, testClock, func(string, ...any) {})

	var offset0, offset200 bool
	for k := range r.Raw {
		if strings.Contains(k, "SYNO.ActiveBackup.Version") && strings.Contains(k, "task_1") {
			if strings.Contains(k, "offset_0") {
				offset0 = true
			}
			if strings.Contains(k, "offset_200") {
				offset200 = true
			}
		}
	}
	if !offset0 || !offset200 {
		t.Errorf("expected distinct raw keys per page; offset_0=%v offset_200=%v; keys=%v", offset0, offset200, rawKeys(r))
	}
}

func rawKeys(r *Report) []string {
	var ks []string
	for k := range r.Raw {
		ks = append(ks, k)
	}
	return ks
}
