package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Active Backup for Microsoft 365 (SYNO.ActiveBackupOffice365) and Active Backup
// for Google Workspace (SYNO.ActiveBackupGSuite) share the same Active Backup
// engine and an all-but-identical task shape, so one collector serves both,
// parameterized by a SaaSFlavor. Unlike Active Backup for Business there is no
// version-history pagination, and unlike Hyper Backup there is no per-task status
// call: a single `list_tasks` request returns every task with its status inline.

// SaaSFlavor describes one Active Backup SaaS product.
type SaaSFlavor struct {
	Key       string // internal id: "m365" | "gws"
	Label     string // "Active Backup for Microsoft 365"
	Short     string // "Microsoft 365"
	API       string // "SYNO.ActiveBackupOffice365" | "SYNO.ActiveBackupGSuite"
	PackageID string // "ActiveBackup-Office365" | "ActiveBackup-GSuite"
	PkgMatch  string // lowercase package-id substring fallback: "office365" | "gsuite"
	Prefix    string // KV key prefix: "M365" | "GWS"
}

var (
	flavorM365 = SaaSFlavor{
		Key: "m365", Label: "Active Backup for Microsoft 365", Short: "Microsoft 365",
		API: "SYNO.ActiveBackupOffice365", PackageID: "ActiveBackup-Office365",
		PkgMatch: "office365", Prefix: "M365",
	}
	flavorGWS = SaaSFlavor{
		Key: "gws", Label: "Active Backup for Google Workspace", Short: "Google Workspace",
		API: "SYNO.ActiveBackupGSuite", PackageID: "ActiveBackup-GSuite",
		PkgMatch: "gsuite", Prefix: "GWS",
	}
)

// SaaSOutcome is the normalized result of a SaaS backup task's last run.
type SaaSOutcome string

const (
	SaaSSuccess SaaSOutcome = "success"
	SaaSRunning SaaSOutcome = "running"
	SaaSFailed  SaaSOutcome = "failed"
	SaaSPartial SaaSOutcome = "partial" // completed with items needing attention
	SaaSNone    SaaSOutcome = "none"    // configured but never executed
	SaaSUnknown SaaSOutcome = "unknown"
)

// saasLiveRunning is the set of numeric `status` (LIVE run-state) values meaning the
// task is actively backing up right now (4 = running, confirmed on a live NAS: the
// app's cancel path guards on status==4). While a task is running it is never
// reported overdue or failed — this is what keeps M365 continuous backup and a long
// Google Workspace run from raising a false alarm.
//
// The `task_status` (last-run RESULT) code is deliberately NOT used to decide the
// outcome: it is inconsistent across the two products — a healthy, successfully
// backed-up task reports task_status 2 on Microsoft 365 but 1 on Google Workspace,
// and GWS omits task_status_error_code entirely — so classification is evidence-based
// instead (see evaluateSaaSTask). The raw code is still recorded for diagnostics.
var saasLiveRunning = map[int64]bool{
	4: true,
}

// SaaSTask is the per-task evaluation result.
type SaaSTask struct {
	TaskID          int64       `json:"task_id"`
	Name            string      `json:"name"`
	Enabled         bool        `json:"enabled"`
	Excluded        bool        `json:"excluded"`
	Monitored       bool        `json:"monitored"`
	EffectiveMaxAge string      `json:"effective_max_age"`
	LiveStatus      int64       `json:"live_status"`   // raw `status`
	ResultStatus    int64       `json:"result_status"` // raw `task_status`
	ErrorCode       int64       `json:"error_code,omitempty"`
	ErrorItems      int64       `json:"error_items,omitempty"` // summed per-service error_* counts
	Attention       int64       `json:"attention,omitempty"`   // attention_count
	LastResult      SaaSOutcome `json:"last_result,omitempty"`
	LastRun         *time.Time  `json:"last_run,omitempty"`     // last_execution_time
	LastSuccess     *time.Time  `json:"last_success,omitempty"` // last_execution_time when the run succeeded
	Running         bool        `json:"running"`
	RunningNote     string      `json:"running_note,omitempty"`
	Failed          bool        `json:"failed"`
	Partial         bool        `json:"partial"`
	Overdue         bool        `json:"overdue"`
	Unknown         bool        `json:"unknown"`
	Note            string      `json:"note,omitempty"`
}

// SaaSInfo is one SaaS backup collector's result. It carries its Flavor so the
// shared render/checks code can label the two products without branching.
type SaaSInfo struct {
	Flavor             SaaSFlavor       `json:"-"`
	State              CollectorState   `json:"state"`
	StateReason        string           `json:"state_reason,omitempty"`
	Tasks              []SaaSTask       `json:"tasks"`
	Total              int              `json:"total"`
	Monitored          int              `json:"monitored"`
	Disabled           int              `json:"disabled"`
	Excluded           int              `json:"excluded"`
	Running            int              `json:"running"`
	Failed             int              `json:"failed"` // failed + partial (broken/attention)
	Partial            int              `json:"partial"`
	Overdue            int              `json:"overdue"`
	Unknown            int              `json:"unknown"`
	LastSuccess        *time.Time       `json:"last_success,omitempty"`
	LastSuccessState   LastSuccessState `json:"-"`
	Notes              []string         `json:"notes,omitempty"`
	UnmatchedSelectors []string         `json:"unmatched_selectors,omitempty"`
}

// brokenCount is the population reported as {P}_FAILED: tasks whose last backup
// either failed outright or completed with items needing attention.
func (i *SaaSInfo) brokenCount() int { return i.Failed }

// collectSaaS is the shared Active Backup SaaS collector. A network error is fatal
// (exit 3); an ambiguous task selector is also fatal (a usage error → exit 3).
// Every non-fatal condition is encoded in info.State.
func collectSaaS(ctx context.Context, c *Client, flavor SaaSFlavor, exclude []Selector, maxAge time.Duration, now time.Time) (*SaaSInfo, error) {
	info := &SaaSInfo{Flavor: flavor, Tasks: []SaaSTask{}}

	// 1. Install state. A missing API alone does not prove "not installed".
	if !c.HasAPI(flavor.API) {
		pkgs, err := collectPackages(ctx, c)
		if err != nil {
			if k, _ := kindOf(err); k == ErrNetwork {
				return nil, err
			}
			info.State = StateUnavailable
			info.StateReason = flavor.Label + " API not advertised and the package list is inaccessible"
			return info, nil
		}
		if pkgs != nil && !saasPackagePresent(pkgs, flavor) {
			info.State = StateNotInstalled
			info.StateReason = flavor.Label + " is not installed"
			return info, nil
		}
		info.State = StateUnavailable
		info.StateReason = flavor.Label + " package present but its API is not accessible (check account privileges)"
		return info, nil
	}

	ver, verr := c.pickVersion(flavor.API)
	if verr != nil {
		info.State = StateUnavailable
		info.StateReason = verr.Error()
		return info, nil
	}

	// 2. Task list — one call carries every task's status inline.
	data, err := c.apiCall(ctx, flavor.API, ver, "list_tasks", nil)
	if err != nil {
		if k, _ := kindOf(err); k == ErrNetwork {
			return nil, err
		}
		info.State = StateError
		info.StateReason = "cannot list " + flavor.Short + " backup tasks: " + err.Error()
		return info, nil
	}
	taskRaws, err := decodeArrayField(data, "tasks")
	if err != nil {
		info.State = StateError
		info.StateReason = "cannot parse " + flavor.Short + " task list: " + err.Error()
		return info, nil
	}
	pre := make([]saasRawTask, 0, len(taskRaws))
	for _, tRaw := range taskRaws {
		pre = append(pre, parseSaaSTask(tRaw))
	}

	// 3. Resolve exclusion selectors against the concrete task set (ambiguity fatal).
	refs := make([]taskRef, 0, len(pre))
	for _, p := range pre {
		if p.hasID {
			refs = append(refs, taskRef{id: p.id, name: p.name})
		}
	}
	excluded, unmatched, selErr := resolveSaaSExclusions(exclude, refs)
	if selErr != nil {
		return nil, selErr
	}
	info.UnmatchedSelectors = unmatched

	// 4. Evaluate each task. Monitored status is resolved before evaluation so
	// disabled/excluded tasks are not judged (their state cannot alert).
	for _, p := range pre {
		task := SaaSTask{
			TaskID:          p.id,
			Name:            p.name,
			Enabled:         p.enabled,
			EffectiveMaxAge: maxAge.String(),
		}
		if p.hasID {
			task.Excluded = excluded[p.id]
		}
		task.Monitored = task.Enabled && !task.Excluded
		if task.Monitored {
			evaluateSaaSTask(&task, p, maxAge, now)
		} else {
			task.Note = "not monitored (disabled or excluded); status not evaluated"
		}
		info.Tasks = append(info.Tasks, task)
	}

	aggregateSaaS(info)
	return info, nil
}

// evaluateSaaSTask classifies one monitored task from its inline fields using the
// evidence-based Active Backup model — the `task_status` code is NOT trusted (it
// differs across the two products). A running task short-circuits to healthy
// activity; otherwise a task-level error code is a failure, per-service errors or
// attention items are a partial, a task that has a dated run with no error evidence
// is a success, and one that has never run is "never backed up".
func evaluateSaaSTask(task *SaaSTask, p saasRawTask, maxAge time.Duration, now time.Time) {
	task.LiveStatus = p.liveStatus
	task.ResultStatus = p.resultStatus
	task.ErrorCode = p.errorCode
	task.ErrorItems = p.errorItems
	task.Attention = p.attention
	if t, ok := parseEpoch(p.lastExec, now); ok {
		task.LastRun = &t
	}

	// Running now (live status, or an in-progress job) suppresses every failure /
	// overdue signal regardless of how long it has run.
	if saasLiveRunning[p.liveStatus] || p.progressBusy {
		task.Running = true
		task.LastResult = SaaSRunning
		task.RunningNote = "backup in progress"
		return
	}

	switch {
	case p.errorCode != 0:
		task.Failed = true
		task.LastResult = SaaSFailed
		task.Note = fmt.Sprintf("last backup failed (error code %d)", p.errorCode)
	case p.errorItems > 0 || p.attention > 0:
		task.Partial = true
		task.LastResult = SaaSPartial
		task.Note = "last backup completed with items needing attention"
	case task.LastRun != nil:
		// It has a dated run and reported no errors → success. last_execution_time is
		// taken as the last successful backup (there is no separate success field).
		task.LastResult = SaaSSuccess
	default:
		// No dated backup run on record.
		task.LastResult = SaaSNone
		task.Overdue = true
		task.Note = "no successful backup on record"
	}

	// Freshness is judged on the last SUCCESS only. A failed/partial task yields no
	// success time, so it is flagged on its own merits and never also marked overdue.
	if task.LastResult == SaaSSuccess && task.LastRun != nil {
		ls := *task.LastRun
		task.LastSuccess = &ls
		if now.Sub(ls) > maxAge {
			task.Overdue = true
		}
	}
}

// aggregateSaaS computes counts and coverage state over the monitored set (enabled
// and not excluded). A single list call carries every task's data, so — unlike ABB
// and Hyper Backup — there is no per-task fetch that can partially fail: if the list
// parsed (the only way execution reaches here) the collector state is OK.
func aggregateSaaS(info *SaaSInfo) {
	info.Total = len(info.Tasks)
	allNone := true // every determinate monitored task explicitly never backed up
	for i := range info.Tasks {
		t := &info.Tasks[i]
		t.Monitored = t.Enabled && !t.Excluded
		switch {
		case !t.Enabled:
			info.Disabled++
		case t.Excluded:
			info.Excluded++
		}
		if !t.Monitored {
			continue
		}
		info.Monitored++
		if t.Running {
			info.Running++
		}
		if t.Failed || t.Partial {
			info.Failed++
		}
		if t.Partial {
			info.Partial++
		}
		if t.Overdue {
			info.Overdue++
		}
		if t.Unknown {
			info.Unknown++
		}
		if t.LastSuccess != nil {
			if info.LastSuccess == nil || t.LastSuccess.After(*info.LastSuccess) {
				ls := *t.LastSuccess
				info.LastSuccess = &ls
			}
		}
		// "never" holds only if every determinate monitored task reported none; a
		// failed/partial/unknown/running task hides the true last success.
		if t.LastResult != SaaSNone {
			allNone = false
		}
	}

	switch {
	case info.Monitored == 0:
		info.LastSuccessState = LSNone
	case info.LastSuccess != nil:
		info.LastSuccessState = LSKnown
	case allNone:
		info.LastSuccessState = LSNever
	default:
		info.LastSuccessState = LSUnknown
	}

	info.State = StateOK
}

// saasRawTask holds one task decoded defensively from list_tasks.
type saasRawTask struct {
	id           int64
	hasID        bool
	name         string
	enabled      bool
	liveStatus   int64 // status
	resultStatus int64 // task_status
	errorCode    int64 // task_status_error_code
	errorItems   int64 // summed per-service error_* counts
	attention    int64 // attention_count
	lastExec     int64 // last_execution_time (epoch seconds)
	progressBusy bool  // progress_list is non-empty
}

// parseSaaSTask extracts fields individually so a single oddly-typed field cannot
// wipe the whole record.
func parseSaaSTask(tRaw json.RawMessage) saasRawTask {
	t := saasRawTask{enabled: true}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(tRaw, &m); err != nil {
		t.name = "unparseable-task"
		return t
	}
	if raw, ok := m["task_id"]; ok {
		var fi FlexInt64
		if err := json.Unmarshal(raw, &fi); err == nil && int64(fi) > 0 {
			t.id = int64(fi)
			t.hasID = true
		}
	}
	t.name = firstNonEmpty(flexStr(m["task_name"]), flexStr(m["name"]))
	if t.name == "" {
		if t.hasID {
			t.name = fmt.Sprintf("task_%d", t.id)
		} else {
			t.name = "unnamed"
		}
	}
	// These tasks have no top-level enable/disable flag: enable_schedule governs
	// scheduling, NOT whether the task is active (M365 continuous backup runs with
	// enable_schedule=false), so it must NOT be read as "disabled". Default enabled;
	// honor an explicit disable only if a future DSM build adds one.
	for _, k := range []string{"enabled", "is_enabled", "enable"} {
		if raw, ok := m[k]; ok {
			t.enabled = t.enabled && parseFlexBool(raw, true)
		}
	}
	t.liveStatus = flexInt(m["status"])
	t.resultStatus = flexInt(m["task_status"])
	t.errorCode = flexInt(m["task_status_error_code"])
	t.attention = flexInt(m["attention_count"])
	t.lastExec = flexInt(m["last_execution_time"])
	t.errorItems = sumServiceErrors(m)
	t.progressBusy = arrayNonEmpty(m["progress_list"])
	return t
}

// flexInt decodes a raw JSON value to int64 via FlexInt64, tolerating numbers,
// numeric strings, and null. Absent/undecodable yields 0.
func flexInt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var fi FlexInt64
	if err := json.Unmarshal(raw, &fi); err != nil {
		return 0
	}
	return int64(fi)
}

// sumServiceErrors totals every per-service error_* counter on a task (error_mail,
// error_drive, error_site, …). The task-level task_status_error_code is a distinct
// field (different name) and is handled separately.
func sumServiceErrors(m map[string]json.RawMessage) int64 {
	var sum int64
	for k, raw := range m {
		if strings.HasPrefix(k, "error_") {
			sum += flexInt(raw)
		}
	}
	return sum
}

// arrayNonEmpty reports whether raw is a JSON array with at least one element.
// A null, an absent field, or a non-array yields false.
func arrayNonEmpty(raw json.RawMessage) bool {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false
	}
	return len(arr) > 0
}

// saasPackagePresent reports whether the flavor's package is installed. It matches
// the exact package id and, as a self-correcting fallback in case the id drifts
// across DSM versions, any installed id containing the flavor's substring — so a
// slightly-wrong id can never cause a false "not installed".
func saasPackagePresent(pkgs map[string]bool, f SaaSFlavor) bool {
	if pkgs[f.PackageID] {
		return true
	}
	for id := range pkgs {
		if strings.Contains(strings.ToLower(id), f.PkgMatch) {
			return true
		}
	}
	return false
}

// resolveSaaSExclusions turns --exclude-m365-task / --exclude-gws-task selectors
// into a set of excluded task ids, reusing the Active Backup selector matcher. An
// ambiguous name match is a fatal usage error; selectors matching nothing are
// returned as unmatched.
func resolveSaaSExclusions(sels []Selector, refs []taskRef) (excluded map[int64]bool, unmatched []string, err error) {
	excluded = map[int64]bool{}
	for _, sel := range sels {
		ids, matched, e := matchSelector(sel, refs)
		if e != nil {
			return nil, nil, e
		}
		if !matched {
			unmatched = append(unmatched, sel.Raw)
			continue
		}
		for id := range ids {
			excluded[id] = true
		}
	}
	return excluded, unmatched, nil
}
