package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testNow is the fixed clock used by scenario tests so freshness math is
// deterministic.
var testNow = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func testClock() time.Time { return testNow }

// apiAdv is one advertised API in the fake discovery response.
type apiAdv struct {
	path string
	minV int
	maxV int
}

func defaultApis() map[string]apiAdv {
	return map[string]apiAdv{
		"SYNO.API.Auth":             {"entry.cgi", 1, 7},
		"SYNO.Core.System":          {"entry.cgi", 1, 2},
		"SYNO.Core.Package":         {"entry.cgi", 1, 2},
		"SYNO.Storage.CGI.Storage":  {"entry.cgi", 1, 1},
		"SYNO.ActiveBackup.Task":    {"entry.cgi", 1, 1},
		"SYNO.ActiveBackup.Version": {"entry.cgi", 1, 1},
	}
}

// dsmScenario is a programmable fake DSM. Zero values give a healthy happy path
// via defaultScenario(); individual tests override fields.
type dsmScenario struct {
	apis           map[string]apiAdv
	discoveryFails bool
	authCode       int
	authHTML       bool
	authMaxVersion int // override advertised Auth maxVersion (DSM6 sim); 0 = use apis

	system         string
	systemErrCode  int
	systemHTML     bool // system endpoint returns non-JSON HTML
	storage        string
	storageErrCode int
	storageHTML    bool // storage endpoint returns non-JSON HTML

	tasks              string
	taskListErrCode    int
	taskListRejectLoad bool
	versions           map[int64][]string // task_id -> pages (each = versions-array data)
	versionErrCode     map[int64]int

	packages       []string
	packageErrCode int
	packageMissing bool   // Core.Package not advertised
	packageBody    string // raw data override for Core.Package.list (e.g. "{}")

	oversize bool // return a >8MB body for storage

	mu           sync.Mutex
	versionCalls []int64 // task_ids for which a version page was requested
}

func (s *dsmScenario) recordVersionCall(tid int64) {
	s.mu.Lock()
	s.versionCalls = append(s.versionCalls, tid)
	s.mu.Unlock()
}

func (s *dsmScenario) versionCalledFor(tid int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versionCalls {
		if v == tid {
			return true
		}
	}
	return false
}

func defaultScenario() *dsmScenario {
	recent := testNow.Add(-10 * time.Hour).Unix()
	return &dsmScenario{
		packages: []string{"ActiveBackup"},
		versions: map[int64][]string{
			1: {versionsPage(versionJSON(recent, "complete"))},
			2: {versionsPage(versionJSON(recent, "complete"))},
			3: {versionsPage(versionJSON(recent, "complete"))},
		},
	}
}

const defaultSystem = `{"model":"DS723+","firmware_ver":"DSM 7.2.2-72806 Update 3","serial":"2340ABC","hostname":"nas01"}`

// One healthy pool, one volume at 68%, two healthy drives. Sizes as strings to
// exercise FlexInt64.
const defaultStorage = `{
  "storagePools":[{"id":"pool_1","status":"normal","device_type":"raid_1","size":{"total":"3900000000000","used":"2652000000000"}}],
  "volumes":[{"id":"volume_1","display_name":"volume_1","status":"normal","fs_type":"btrfs","size":{"total":"3900000000000","used":"2652000000000"}}],
  "disks":[
    {"id":"sata1","name":"Drive 1","model":"HAT5300-4T","status":"normal","temp":34,"size_total":"4000000000000"},
    {"id":"sata2","name":"Drive 2","model":"HAT5300-4T","status":"normal","temp":35,"size_total":"4000000000000"}
  ]
}`

const defaultTasks = `{"tasks":[
  {"task_id":1,"task_name":"SRV-DC01","source_type":"server","enabled":true},
  {"task_id":2,"task_name":"WS-05","source_type":"pc","enabled":true},
  {"task_id":3,"task_name":"SRV-FILE","source_type":"server","enabled":true}
]}`

func versionsPage(vs ...string) string {
	return `{"versions":[` + strings.Join(vs, ",") + `]}`
}

func versionJSON(timeEnd int64, status string) string {
	return fmt.Sprintf(`{"time_end":%d,"status":%q}`, timeEnd, status)
}

func (s *dsmScenario) apisOrDefault() map[string]apiAdv {
	if s.apis != nil {
		return s.apis
	}
	m := defaultApis()
	if s.authMaxVersion != 0 {
		a := m["SYNO.API.Auth"]
		a.maxV = s.authMaxVersion
		m["SYNO.API.Auth"] = a
	}
	if s.packageMissing {
		delete(m, "SYNO.Core.Package")
	}
	return m
}

func (s *dsmScenario) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		api := r.FormValue("api")
		method := r.FormValue("method")
		switch api {
		case "SYNO.API.Info":
			if s.discoveryFails {
				fmt.Fprint(w, "<html><body>login</body></html>")
				return
			}
			writeSuccess(w, discoveryData(s.apisOrDefault()))
		case "SYNO.API.Auth":
			if method == "logout" {
				writeSuccess(w, "{}")
				return
			}
			if s.authHTML {
				fmt.Fprint(w, "<html><body>login page</body></html>")
				return
			}
			if s.authCode != 0 {
				writeError(w, s.authCode)
				return
			}
			writeSuccess(w, `{"sid":"test-sid"}`)
		case "SYNO.Core.System":
			if s.systemHTML {
				fmt.Fprint(w, "<html><body>not json</body></html>")
				return
			}
			if s.systemErrCode != 0 {
				writeError(w, s.systemErrCode)
				return
			}
			writeSuccess(w, orDefault(s.system, defaultSystem))
		case "SYNO.Storage.CGI.Storage":
			if s.storageHTML {
				fmt.Fprint(w, "<html><body>not json</body></html>")
				return
			}
			if s.oversize {
				fmt.Fprintf(w, `{"success":true,"data":{"blob":%q}}`, strings.Repeat("x", (8<<20)+16))
				return
			}
			if s.storageErrCode != 0 {
				writeError(w, s.storageErrCode)
				return
			}
			writeSuccess(w, orDefault(s.storage, defaultStorage))
		case "SYNO.ActiveBackup.Task":
			if s.taskListRejectLoad && r.FormValue("load_status") != "" {
				writeError(w, 402)
				return
			}
			if s.taskListErrCode != 0 {
				writeError(w, s.taskListErrCode)
				return
			}
			writeSuccess(w, orDefault(s.tasks, defaultTasks))
		case "SYNO.ActiveBackup.Version":
			tid, _ := strconv.ParseInt(r.FormValue("task_id"), 10, 64)
			s.recordVersionCall(tid)
			if code, ok := s.versionErrCode[tid]; ok {
				writeError(w, code)
				return
			}
			offset, _ := strconv.Atoi(r.FormValue("offset"))
			pages := s.versions[tid]
			idx := offset / versionPageLimit
			if idx < len(pages) {
				writeSuccess(w, pages[idx])
			} else {
				writeSuccess(w, `{"versions":[]}`)
			}
		case "SYNO.Core.Package":
			if s.packageErrCode != 0 {
				writeError(w, s.packageErrCode)
				return
			}
			if s.packageBody != "" {
				writeSuccess(w, s.packageBody)
				return
			}
			writeSuccess(w, packagesData(s.packages))
		default:
			writeError(w, 103)
		}
	}
}

func writeSuccess(w http.ResponseWriter, dataJSON string) {
	fmt.Fprintf(w, `{"success":true,"data":%s}`, dataJSON)
}

func writeError(w http.ResponseWriter, code int) {
	fmt.Fprintf(w, `{"success":false,"error":{"code":%d}}`, code)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func discoveryData(apis map[string]apiAdv) string {
	var sb strings.Builder
	sb.WriteString("{")
	first := true
	for name, a := range apis {
		if !first {
			sb.WriteString(",")
		}
		first = false
		fmt.Fprintf(&sb, `%q:{"path":%q,"minVersion":%d,"maxVersion":%d}`, name, a.path, a.minV, a.maxV)
	}
	sb.WriteString("}")
	return sb.String()
}

func packagesData(ids []string) string {
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"name":%q}`, id, id))
	}
	return `{"packages":[` + strings.Join(parts, ",") + `]}`
}

func testConfig(host string) *Config {
	return &Config{
		Host:         host,
		Username:     "svc",
		Password:     "secret-pw",
		VolWarnPct:   80,
		VolCritPct:   90,
		BackupMaxAge: 24 * time.Hour,
		Timeout:      30 * time.Second,
		InsecureTLS:  true,
		Format:       "both",
	}
}

// runScenario starts the fake DSM, runs the full engine against it with the fixed
// test clock, and returns the Report.
func runScenario(t *testing.T, s *dsmScenario, tweak func(*Config)) *Report {
	t.Helper()
	srv := httptest.NewTLSServer(s.handler())
	t.Cleanup(srv.Close)
	cfg := testConfig(srv.URL)
	if tweak != nil {
		tweak(cfg)
	}
	return collect(context.Background(), cfg, testClock, func(string, ...any) {})
}

// kvValue extracts one KEY=VALUE from the rendered KV block.
func kvValue(t *testing.T, r *Report, key string) string {
	t.Helper()
	for _, line := range strings.Split(renderKV(r), "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	t.Fatalf("KV key %q not found in:\n%s", key, renderKV(r))
	return ""
}
