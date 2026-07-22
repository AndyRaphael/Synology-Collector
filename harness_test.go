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

	// Hyper Backup (SYNO.Backup.Task). Not advertised by default; set
	// hyperAdvertise to expose the API so the collector reads it.
	hyperAdvertise              bool
	hyperTasks                  string            // task-list data (default {"task_list":[]})
	hyperTaskListErrCode        int               // DSM code for the list call
	hyperStatus                 map[string]string // task_id -> status data
	hyperStatusErrCode          map[string]int    // task_id -> DSM code for the status call
	hyperStatusRejectAdditional bool              // 402 when the status call passes `additional`

	// Active Backup SaaS (SYNO.ActiveBackupOffice365 / SYNO.ActiveBackupGSuite). Not
	// advertised by default; set the advertise flag to expose the API.
	m365Advertise       bool
	m365Tasks           string // list_tasks data (default {"tasks":[]})
	m365TaskListErrCode int    // DSM code for the list_tasks call
	gwsAdvertise        bool
	gwsTasks            string // list_tasks data (default {"tasks":[]})
	gwsTaskListErrCode  int    // DSM code for the list_tasks call

	packages       []string
	packageErrCode int
	packageMissing bool   // Core.Package not advertised
	packageBody    string // raw data override for Core.Package.list (e.g. "{}")

	oversize bool // return a >8MB body for storage

	mu               sync.Mutex
	versionCalls     []int64  // task_ids for which a version page was requested
	hyperStatusCalls []string // task_ids for which a Hyper Backup status was requested
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

func (s *dsmScenario) recordHyperStatusCall(tid string) {
	s.mu.Lock()
	s.hyperStatusCalls = append(s.hyperStatusCalls, tid)
	s.mu.Unlock()
}

func (s *dsmScenario) hyperStatusCalledFor(tid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.hyperStatusCalls {
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

// Mirrors real DSM: no hostname field, and uptime as the up_time "H:M:S" string.
const defaultSystem = `{"model":"DS723+","firmware_ver":"DSM 7.2.2-72806 Update 3","serial":"2340ABC","up_time":"230:40:35"}`

// One healthy pool, one volume at 68%, two healthy drives. Sizes as strings to
// exercise FlexInt64.
const defaultStorage = `{
  "storagePools":[{"id":"pool_1","status":"normal","device_type":"raid_1","size":{"total":"3900000000000","used":"2652000000000"}}],
  "volumes":[{"id":"volume_1","display_name":"volume_1","status":"normal","fs_type":"btrfs","size":{"total":"3900000000000","used":"2652000000000"}}],
  "disks":[
    {"id":"sata1","name":"Drive 1","model":"HAT5300-4T","status":"normal","temp":34,"size_total":"4000000000000"},
    {"id":"sata2","name":"Drive 2","model":"HAT5300-4T","status":"normal","temp":35,"size_total":"4000000000000"}
  ],
  "env":{"status":{"system_crashed":false},"storageMachineInfo":[
    {"nameStr":"expansion-1","modelName":"DX517","order":1},
    {"nameStr":"nas01","modelName":"DS723+","order":0}
  ]}
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

// hyperListData wraps Hyper Backup task-list entries in the DSM envelope shape.
func hyperListData(entries ...string) string {
	return `{"task_list":[` + strings.Join(entries, ",") + `]}`
}

func hyperListEntry(id, name string) string {
	return fmt.Sprintf(`{"task_id":%q,"task_name":%q}`, id, name)
}

// hyperStatusJSON builds a per-task status response; zero timestamps are omitted.
func hyperStatusJSON(state, status, result string, lastEnd, nextTime int64) string {
	parts := []string{fmt.Sprintf(`"state":%q,"status":%q,"last_bkp_result":%q`, state, status, result)}
	if lastEnd != 0 {
		parts = append(parts, fmt.Sprintf(`"last_bkp_end_time":%d`, lastEnd))
	}
	if nextTime != 0 {
		parts = append(parts, fmt.Sprintf(`"next_bkp_time":%d`, nextTime))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// saasListData wraps Active Backup SaaS task entries in the list_tasks data shape.
func saasListData(entries ...string) string {
	return `{"tasks":[` + strings.Join(entries, ",") + `]}`
}

// saasTaskEntry builds one M365/GWS task entry: liveStatus is `status` (4=running),
// resultStatus is `task_status` (2=success), errCode is task_status_error_code, and
// lastExec is last_execution_time (0 omitted).
func saasTaskEntry(id int, name string, liveStatus, resultStatus, errCode int, lastExec int64) string {
	parts := []string{fmt.Sprintf(`"task_id":%d,"task_name":%q,"status":%d,"task_status":%d,"task_status_error_code":%d`,
		id, name, liveStatus, resultStatus, errCode)}
	if lastExec != 0 {
		parts = append(parts, fmt.Sprintf(`"last_execution_time":%d`, lastExec))
	}
	return "{" + strings.Join(parts, ",") + "}"
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
	if s.hyperAdvertise {
		m["SYNO.Backup.Task"] = apiAdv{"entry.cgi", 1, 1}
	}
	if s.m365Advertise {
		m["SYNO.ActiveBackupOffice365"] = apiAdv{"entry.cgi", 1, 1}
	}
	if s.gwsAdvertise {
		m["SYNO.ActiveBackupGSuite"] = apiAdv{"entry.cgi", 1, 1}
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
		case "SYNO.Backup.Task":
			if method == "status" {
				tid := r.FormValue("task_id")
				s.recordHyperStatusCall(tid)
				if s.hyperStatusRejectAdditional && r.FormValue("additional") != "" {
					writeError(w, 402)
					return
				}
				if code, ok := s.hyperStatusErrCode[tid]; ok {
					writeError(w, code)
					return
				}
				if body, ok := s.hyperStatus[tid]; ok {
					writeSuccess(w, body)
					return
				}
				writeSuccess(w, `{}`) // no fields → indeterminate
				return
			}
			if s.hyperTaskListErrCode != 0 {
				writeError(w, s.hyperTaskListErrCode)
				return
			}
			writeSuccess(w, orDefault(s.hyperTasks, `{"task_list":[]}`))
		case "SYNO.ActiveBackupOffice365":
			if s.m365TaskListErrCode != 0 {
				writeError(w, s.m365TaskListErrCode)
				return
			}
			writeSuccess(w, orDefault(s.m365Tasks, `{"tasks":[]}`))
		case "SYNO.ActiveBackupGSuite":
			if s.gwsTaskListErrCode != 0 {
				writeError(w, s.gwsTaskListErrCode)
				return
			}
			writeSuccess(w, orDefault(s.gwsTasks, `{"tasks":[]}`))
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
		Host:              host,
		Username:          "svc",
		Password:          "secret-pw",
		VolWarnPct:        80,
		VolCritPct:        90,
		BackupMaxAge:      24 * time.Hour,
		HyperBackupMaxAge: 7 * 24 * time.Hour,
		SaaSBackupMaxAge:  48 * time.Hour,
		Timeout:           30 * time.Second,
		InsecureTLS:       true,
		Format:            "both",
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
