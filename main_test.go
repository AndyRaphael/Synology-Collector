package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEngineWritesNothingToStdStreams enforces the interactive-UI boundary: the
// collection engine must route all diagnostics through debugf and never touch the
// process's stdout/stderr or the global logger.
func TestEngineWritesNothingToStdStreams(t *testing.T) {
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr

	srv := httptest.NewTLSServer(defaultScenario().handler())
	defer srv.Close()
	cfg := testConfig(srv.URL)
	_ = collect(context.Background(), cfg, testClock, func(string, ...any) {})

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = origOut, origErr
	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)
	if len(outBytes) > 0 || len(errBytes) > 0 {
		t.Errorf("engine wrote to std streams: stdout=%q stderr=%q", outBytes, errBytes)
	}
}

func TestHappyPath(t *testing.T) {
	r := runScenario(t, defaultScenario(), nil)
	if r.Status != "OK" || r.ExitCode != ExitOK {
		t.Fatalf("status=%s exit=%d, want OK/0; summary=%q", r.Status, r.ExitCode, r.Summary)
	}
	if got := kvValue(t, r, "NAS"); got != "DS723+" {
		t.Errorf("NAS=%q, want DS723+", got)
	}
	if got := kvValue(t, r, "DSM"); got != "7.2.2" {
		t.Errorf("DSM=%q, want 7.2.2", got)
	}
	if got := kvValue(t, r, "VOLUME_USAGE"); got != "68%" {
		t.Errorf("VOLUME_USAGE=%q, want 68%%", got)
	}
	if got := kvValue(t, r, "DRIVES"); got != "2" {
		t.Errorf("DRIVES=%q, want 2", got)
	}
	if got := kvValue(t, r, "ABB_TASKS"); got != "3" {
		t.Errorf("ABB_TASKS=%q, want 3", got)
	}
	if got := kvValue(t, r, "ABB_MONITORED"); got != "3" {
		t.Errorf("ABB_MONITORED=%q, want 3", got)
	}
	if got := kvValue(t, r, "SYSTEM_HEALTH"); got != "Normal" {
		t.Errorf("SYSTEM_HEALTH=%q, want Normal", got)
	}
	if got := kvValue(t, r, "STORAGE_POOL"); got != "Healthy" {
		t.Errorf("STORAGE_POOL=%q, want Healthy", got)
	}
}

func TestAuthErrors(t *testing.T) {
	t.Run("bad-credentials-400", func(t *testing.T) {
		s := defaultScenario()
		s.authCode = 400
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3", r.ExitCode)
		}
		if !strings.Contains(strings.ToLower(r.Error), "invalid username or password") {
			t.Errorf("error=%q, want credentials message", r.Error)
		}
	})
	t.Run("2fa-403", func(t *testing.T) {
		s := defaultScenario()
		s.authCode = 403
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3", r.ExitCode)
		}
		if !strings.Contains(strings.ToLower(r.Error), "service account") {
			t.Errorf("error=%q, want 2FA/service-account message", r.Error)
		}
	})
	t.Run("login-returns-html", func(t *testing.T) {
		s := defaultScenario()
		s.authHTML = true
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3", r.ExitCode)
		}
	})
}

func TestStorageSeverities(t *testing.T) {
	poolDegraded := `{
      "storagePools":[{"id":"pool_1","status":"degraded","size":{"total":"100","used":"40"}}],
      "volumes":[{"id":"volume_1","status":"normal","size":{"total":"100","used":"40"}}],
      "disks":[{"id":"sata1","name":"Drive 1","status":"normal"}]
    }`
	vol92 := `{
      "storagePools":[{"id":"pool_1","status":"normal","size":{"total":"100","used":"92"}}],
      "volumes":[{"id":"volume_1","status":"normal","size":{"total":"100","used":"92"}}],
      "disks":[{"id":"sata1","name":"Drive 1","status":"normal"}]
    }`
	vol85 := `{
      "storagePools":[{"id":"pool_1","status":"normal","size":{"total":"100","used":"85"}}],
      "volumes":[{"id":"volume_1","status":"normal","size":{"total":"100","used":"85"}}],
      "disks":[{"id":"sata1","name":"Drive 1","status":"normal"}]
    }`
	driveWarn := `{
      "storagePools":[{"id":"pool_1","status":"normal","size":{"total":"100","used":"40"}}],
      "volumes":[{"id":"volume_1","status":"normal","size":{"total":"100","used":"40"}}],
      "disks":[{"id":"sata1","name":"Drive 1","status":"warning"},{"id":"sata2","name":"Drive 2","status":"normal"}]
    }`

	cases := []struct {
		name       string
		storage    string
		wantStatus string
		wantExit   int
		checkKey   string
		checkVal   string
	}{
		{"pool-degraded", poolDegraded, "CRITICAL", ExitCritical, "STORAGE_POOL", "Degraded"},
		{"volume-92-critical", vol92, "CRITICAL", ExitCritical, "VOLUME_USAGE", "92%"},
		{"volume-85-warning", vol85, "WARNING", ExitWarning, "VOLUME_USAGE", "85%"},
		{"drive-warning", driveWarn, "WARNING", ExitWarning, "DRIVE_WARNINGS", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultScenario()
			s.storage = tc.storage
			r := runScenario(t, s, nil)
			if r.Status != tc.wantStatus || r.ExitCode != tc.wantExit {
				t.Fatalf("status=%s exit=%d, want %s/%d; summary=%q", r.Status, r.ExitCode, tc.wantStatus, tc.wantExit, r.Summary)
			}
			if got := kvValue(t, r, tc.checkKey); got != tc.checkVal {
				t.Errorf("%s=%q, want %q", tc.checkKey, got, tc.checkVal)
			}
		})
	}
}

func TestStorageEmptyData(t *testing.T) {
	t.Run("no-volumes-is-fatal", func(t *testing.T) {
		s := defaultScenario()
		s.storage = `{"storagePools":[{"id":"pool_1","status":"normal"}],"volumes":[],"disks":[{"id":"sata1","status":"normal"}]}`
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3", r.ExitCode)
		}
	})
	t.Run("no-disks-is-warning", func(t *testing.T) {
		s := defaultScenario()
		s.storage = `{"storagePools":[{"id":"pool_1","status":"normal","size":{"total":"100","used":"10"}}],"volumes":[{"id":"volume_1","status":"normal","size":{"total":"100","used":"10"}}],"disks":[]}`
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitWarning {
			t.Fatalf("exit=%d, want 1 (drive_data_missing); summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "DRIVES"); got != "0" {
			t.Errorf("DRIVES=%q, want 0", got)
		}
		if got := kvValue(t, r, "DRIVE_WARNINGS"); got != "Unknown" {
			t.Errorf("DRIVE_WARNINGS=%q, want Unknown", got)
		}
	})
}

func TestABBFailedAndOverdue(t *testing.T) {
	t.Run("failed-latest-terminal", func(t *testing.T) {
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(recent, "failed"))},
			2: {versionsPage(versionJSON(recent, "complete"))},
			3: {versionsPage(versionJSON(recent, "complete"))},
		}
		r := runScenario(t, s, nil)
		if r.Status != "WARNING" || r.ExitCode != ExitWarning {
			t.Fatalf("status=%s exit=%d, want WARNING/1", r.Status, r.ExitCode)
		}
		if got := kvValue(t, r, "ABB_FAILED"); got != "1" {
			t.Errorf("ABB_FAILED=%q, want 1", got)
		}
	})
	t.Run("overdue-stale-success", func(t *testing.T) {
		stale := testNow.Add(-40 * time.Hour).Unix()
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(stale, "complete"))},
			2: {versionsPage(versionJSON(recent, "complete"))},
			3: {versionsPage(versionJSON(recent, "complete"))},
		}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitWarning {
			t.Fatalf("exit=%d, want 1; summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "ABB_OVERDUE"); got != "1" {
			t.Errorf("ABB_OVERDUE=%q, want 1", got)
		}
	})
	t.Run("running-does-not-mask-failure", func(t *testing.T) {
		failedAt := testNow.Add(-3 * time.Hour).Unix()
		runningAt := testNow.Add(-1 * time.Hour).Unix()
		s := defaultScenario()
		// Newest attempt is running; the freshest TERMINAL outcome is failed.
		s.versions = map[int64][]string{
			1: {versionsPage(
				`{"time_start":`+itoa(runningAt)+`,"status":"running"}`,
				versionJSON(failedAt, "failed"),
			)},
			2: {versionsPage(versionJSON(testNow.Add(-2*time.Hour).Unix(), "complete"))},
			3: {versionsPage(versionJSON(testNow.Add(-2*time.Hour).Unix(), "complete"))},
		}
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "ABB_FAILED"); got != "1" {
			t.Errorf("ABB_FAILED=%q, want 1 (running must not mask prior failure)", got)
		}
	})
	t.Run("cancelled-latest-terminal-warns", func(t *testing.T) {
		at := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(at, "cancelled"), versionJSON(testNow.Add(-30*time.Hour).Unix(), "complete"))},
			2: {versionsPage(versionJSON(at, "complete"))},
			3: {versionsPage(versionJSON(at, "complete"))},
		}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitWarning {
			t.Fatalf("exit=%d, want 1; summary=%q", r.ExitCode, r.Summary)
		}
		if !hasCheck(r, "abb_cancelled") {
			t.Errorf("expected abb_cancelled check; got summary %q", r.Summary)
		}
	})
}

func TestABBInstallState(t *testing.T) {
	t.Run("not-installed-is-ok", func(t *testing.T) {
		s := defaultScenario()
		apis := defaultApis()
		delete(apis, "SYNO.ActiveBackup.Task")
		s.apis = apis
		s.packages = []string{"FileStation"} // ABB package absent
		r := runScenario(t, s, nil)
		if r.Status != "OK" || r.ExitCode != ExitOK {
			t.Fatalf("status=%s exit=%d, want OK/0", r.Status, r.ExitCode)
		}
		if got := kvValue(t, r, "ABB_STATE"); got != "NOT_INSTALLED" {
			t.Errorf("ABB_STATE=%q, want NOT_INSTALLED", got)
		}
		if got := kvValue(t, r, "ABB_TASKS"); got != "0" {
			t.Errorf("ABB_TASKS=%q, want 0", got)
		}
		if got := kvValue(t, r, "LAST_SUCCESS"); got != "N/A" {
			t.Errorf("LAST_SUCCESS=%q, want N/A", got)
		}
	})
	t.Run("api-absent-but-package-present-is-unavailable", func(t *testing.T) {
		s := defaultScenario()
		apis := defaultApis()
		delete(apis, "SYNO.ActiveBackup.Task")
		s.apis = apis
		s.packages = []string{"ActiveBackup"} // present
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3", r.ExitCode)
		}
		if got := kvValue(t, r, "ABB_STATE"); got != "UNAVAILABLE" {
			t.Errorf("ABB_STATE=%q, want UNAVAILABLE", got)
		}
	})
	t.Run("task-list-permission-denied-is-error", func(t *testing.T) {
		s := defaultScenario()
		s.taskListErrCode = 105
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3", r.ExitCode)
		}
	})
}

func TestABBPartialCoverage(t *testing.T) {
	t.Run("one-task-history-fails-is-partial-warning", func(t *testing.T) {
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(recent, "complete"))},
			3: {versionsPage(versionJSON(recent, "complete"))},
		}
		s.versionErrCode = map[int64]int{2: 105}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitWarning {
			t.Fatalf("exit=%d, want 1; summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "ABB_STATE"); got != "PARTIAL" {
			t.Errorf("ABB_STATE=%q, want PARTIAL", got)
		}
	})
	t.Run("all-histories-fail-is-error", func(t *testing.T) {
		s := defaultScenario()
		s.versionErrCode = map[int64]int{1: 105, 2: 105, 3: 105}
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3; summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "ABB_STATE"); got != "ERROR" {
			t.Errorf("ABB_STATE=%q, want ERROR", got)
		}
	})
}

func TestABBTruncatedHistory(t *testing.T) {
	// 1000-version cap reached with no success: LAST_SUCCESS must be Unknown (not
	// never) and the task must NOT be overdue.
	fullPage := make([]string, versionPageLimit)
	for i := range fullPage {
		fullPage[i] = versionJSON(testNow.Add(-time.Duration(i)*time.Hour).Unix(), "running")
	}
	page := versionsPage(fullPage...)
	pages := make([]string, 6) // 6*200 = 1200 > cap
	for i := range pages {
		pages[i] = page
	}
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"BIG","enabled":true}]}`
	s.versions = map[int64][]string{1: pages}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "LAST_SUCCESS"); got != "Unknown" {
		t.Errorf("LAST_SUCCESS=%q, want Unknown (truncated history)", got)
	}
	if got := kvValue(t, r, "ABB_OVERDUE"); got != "0" {
		t.Errorf("ABB_OVERDUE=%q, want 0 (cannot prove overdue on truncated history)", got)
	}
}

func TestABBNeverSucceeded(t *testing.T) {
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"NEW","enabled":true}]}`
	s.versions = map[int64][]string{1: {`{"versions":[]}`}}
	r := runScenario(t, s, nil)
	if got := kvValue(t, r, "ABB_OVERDUE"); got != "1" {
		t.Errorf("ABB_OVERDUE=%q, want 1 (never succeeded)", got)
	}
	if got := kvValue(t, r, "LAST_SUCCESS"); got != "never" {
		t.Errorf("LAST_SUCCESS=%q, want never", got)
	}
}

func TestABBDisabledExcluded(t *testing.T) {
	t.Run("all-disabled-monitored-zero", func(t *testing.T) {
		s := defaultScenario()
		s.tasks = `{"tasks":[
          {"task_id":1,"task_name":"A","enabled":false},
          {"task_id":2,"task_name":"B","enabled":false}
        ]}`
		s.versions = map[int64][]string{}
		r := runScenario(t, s, nil)
		if got := kvValue(t, r, "ABB_MONITORED"); got != "0" {
			t.Errorf("ABB_MONITORED=%q, want 0", got)
		}
		if got := kvValue(t, r, "ABB_DISABLED"); got != "2" {
			t.Errorf("ABB_DISABLED=%q, want 2", got)
		}
		if got := kvValue(t, r, "LAST_SUCCESS"); got != "N/A" {
			t.Errorf("LAST_SUCCESS=%q, want N/A", got)
		}
	})
	t.Run("excluded-task-does-not-alert", func(t *testing.T) {
		// Task 2 has failed, but is excluded → no alert.
		recent := testNow.Add(-2 * time.Hour).Unix()
		s := defaultScenario()
		s.versions = map[int64][]string{
			1: {versionsPage(versionJSON(recent, "complete"))},
			2: {versionsPage(versionJSON(recent, "failed"))},
			3: {versionsPage(versionJSON(recent, "complete"))},
		}
		r := runScenario(t, s, func(c *Config) {
			c.ExcludeTasks = []Selector{{Kind: SelID, Raw: "id:2", ID: 2}}
		})
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 (excluded failure); summary=%q", r.ExitCode, r.Summary)
		}
		if got := kvValue(t, r, "ABB_EXCLUDED"); got != "1" {
			t.Errorf("ABB_EXCLUDED=%q, want 1", got)
		}
	})
}

func TestABBLoadStatusRetry(t *testing.T) {
	s := defaultScenario()
	s.taskListRejectLoad = true // rejects load_status=true with 402, plain list works
	r := runScenario(t, s, nil)
	if r.Status != "OK" || r.ExitCode != ExitOK {
		t.Fatalf("status=%s exit=%d, want OK/0 (should retry plain list)", r.Status, r.ExitCode)
	}
	if got := kvValue(t, r, "ABB_TASKS"); got != "3" {
		t.Errorf("ABB_TASKS=%q, want 3", got)
	}
}

func TestDSMVersionNegotiation(t *testing.T) {
	t.Run("dsm6-auth-v3", func(t *testing.T) {
		s := defaultScenario()
		s.authMaxVersion = 3 // DSM6-era
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitOK {
			t.Fatalf("exit=%d, want 0 (auth should negotiate v3); err=%q", r.ExitCode, r.Error)
		}
	})
	t.Run("auth-too-old-no-overlap", func(t *testing.T) {
		s := defaultScenario()
		s.authMaxVersion = 2 // below client floor of 3
		r := runScenario(t, s, nil)
		if r.ExitCode != ExitError {
			t.Fatalf("exit=%d, want 3 (no compatible auth version)", r.ExitCode)
		}
		if !strings.Contains(r.Error, "compatible version") {
			t.Errorf("error=%q, want no-compatible-version message", r.Error)
		}
	})
}

func TestUnreachableHost(t *testing.T) {
	srv := httptest.NewTLSServer(defaultScenario().handler())
	url := srv.URL
	srv.Close() // now nothing is listening
	cfg := testConfig(url)
	r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 (unreachable)", r.ExitCode)
	}
}

func TestOversizeResponse(t *testing.T) {
	s := defaultScenario()
	s.oversize = true
	r := runScenario(t, s, nil)
	if r.ExitCode != ExitError {
		t.Fatalf("exit=%d, want 3 (oversize)", r.ExitCode)
	}
	if !strings.Contains(strings.ToLower(r.Error), "exceeds") {
		t.Errorf("error=%q, want size-limit message", r.Error)
	}
}

func TestErrorRenderingHonorsFormat(t *testing.T) {
	s := defaultScenario()
	s.authCode = 400
	for _, format := range []string{"kv", "json", "both"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewTLSServer(s.handler())
			t.Cleanup(srv.Close)
			cfg := testConfig(srv.URL)
			cfg.Format = format
			r := collect(context.Background(), cfg, testClock, func(string, ...any) {})
			var buf strings.Builder
			if err := render(&buf, cfg.Format, r); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			hasKV := strings.Contains(out, "STATUS=ERROR")
			hasJSON := strings.Contains(out, `"status": "ERROR"`)
			switch format {
			case "kv":
				if !hasKV || hasJSON {
					t.Errorf("kv format leaked JSON or missing KV:\n%s", out)
				}
			case "json":
				if hasKV || !hasJSON {
					t.Errorf("json format leaked KV or missing JSON:\n%s", out)
				}
				if !json.Valid([]byte(out)) {
					t.Errorf("json format is not valid JSON:\n%s", out)
				}
			case "both":
				if !hasKV || !hasJSON || !strings.Contains(out, "\n---\n") {
					t.Errorf("both format missing a section:\n%s", out)
				}
			}
		})
	}
}

func TestUnicodeTaskName(t *testing.T) {
	recent := testNow.Add(-2 * time.Hour).Unix()
	s := defaultScenario()
	s.tasks = `{"tasks":[{"task_id":1,"task_name":"Bücher-Sicherung 日本語","enabled":true}]}`
	s.versions = map[int64][]string{1: {versionsPage(versionJSON(recent, "failed"))}}
	r := runScenario(t, s, nil)
	if !strings.Contains(r.Summary, "Bücher-Sicherung 日本語") {
		t.Errorf("summary should preserve unicode task name, got %q", r.Summary)
	}
	// And the KV line must not contain control characters or newlines within it.
	line := kvValue(t, r, "SUMMARY")
	if strings.ContainsAny(line, "\n\r\t") {
		t.Errorf("SUMMARY KV line contains control chars: %q", line)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func hasCheck(r *Report, name string) bool {
	for _, c := range r.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}
