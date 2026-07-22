package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HyperOutcome is the normalized outcome of a Hyper Backup task's last run,
// derived from its last_bkp_result. Hyper Backup keeps no queryable success
// history (unlike Active Backup's version list), so a task's freshness and
// failure state come from this single last-result value plus its live activity.
type HyperOutcome string

const (
	HyperSuccess     HyperOutcome = "success"
	HyperRunning     HyperOutcome = "running"
	HyperFailed      HyperOutcome = "failed"
	HyperPartial     HyperOutcome = "partial"
	HyperIntegrity   HyperOutcome = "integrity_failed"
	HyperDestMissing HyperOutcome = "dest_missing"
	HyperCancelled   HyperOutcome = "cancelled"
	HyperSuspended   HyperOutcome = "suspended"
	HyperNone        HyperOutcome = "none" // configured but never backed up
	HyperUnknown     HyperOutcome = "unknown"
)

// hyperResultMap maps a normalized last_bkp_result token to an outcome. Matching
// is EXACT on the normalized token (lowercased, '-'→'_'), never substring —
// "backingup" must not be read as "backup...failed". Tokens not listed become
// HyperUnknown and are surfaced (never silently healthy). Confirm the real tokens
// on a live NAS with --debug and fold any new ones in here and nowhere else.
var hyperResultMap = map[string]HyperOutcome{
	// completed successfully
	"done": HyperSuccess, "success": HyperSuccess, "successful": HyperSuccess,
	"finish": HyperSuccess, "finished": HyperSuccess, "complete": HyperSuccess,
	"completed": HyperSuccess, "ok": HyperSuccess,

	// in progress — a run of ANY length is healthy activity (a large sync or an
	// integrity check can take days), so these are never overdue or failed.
	"backingup": HyperRunning, "backing_up": HyperRunning, "backup": HyperRunning,
	"resuming": HyperRunning, "resume": HyperRunning, "detect": HyperRunning,
	"detecting": HyperRunning, "waiting": HyperRunning, "processing": HyperRunning,
	"preparing": HyperRunning, "mounting": HyperRunning,
	"version_deleting": HyperRunning, "preparing_version_delete": HyperRunning,

	// configured but no backup has ever completed
	"none": HyperNone,

	// genuine failure of the backup run
	"fail": HyperFailed, "failed": HyperFailed, "error": HyperFailed,
	"crash": HyperFailed, "crashed": HyperFailed, "abort": HyperFailed,
	"aborted": HyperFailed, "version_delete_failed": HyperFailed,

	// finished but not everything succeeded
	"partial": HyperPartial,

	// backup-integrity (checksum) verification found a problem — the stored
	// backup may be unrestorable, distinct from a run that failed to complete.
	"cksum_failed": HyperIntegrity, "checksum_failed": HyperIntegrity,
	"failed_checking": HyperIntegrity, "verify_failed": HyperIntegrity,
	"integrity_failed": HyperIntegrity,

	// the backup destination could not be reached
	"dest_missing": HyperDestMissing, "destination_missing": HyperDestMissing,
	"target_missing": HyperDestMissing,

	// intentionally stopped
	"cancel": HyperCancelled, "canceled": HyperCancelled, "cancelled": HyperCancelled,
	"discard": HyperCancelled, "discarded": HyperCancelled,
	"suspend": HyperSuspended, "suspended": HyperSuspended,
}

// hyperRunningStates is the set of live status/state tokens meaning the task is
// actively working right now — a backup or a backup-integrity check. While a task
// is running it is never reported failed or overdue; this is what keeps a
// multi-day sync or a multi-day integrity check from raising a false alarm.
var hyperRunningStates = map[string]bool{
	"backup": true, "backingup": true, "backing_up": true,
	"detect": true, "detecting": true, "waiting": true,
	"resuming": true, "resume": true, "restoring": true, "restore": true,
	"processing": true, "preparing": true, "mounting": true,
	"checking": true, "verifying": true, "integrity_check": true,
	"integrity_checking": true, "version_deleting": true,
	"preparing_version_delete": true,
}

// hyperIntegrityStates identify a running integrity/verification check, so the
// report can label the activity precisely instead of "backup in progress".
var hyperIntegrityStates = map[string]bool{
	"checking": true, "verifying": true,
	"integrity_check": true, "integrity_checking": true,
}

func normalizeToken(raw string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "-", "_")
}

func classifyHyperResult(raw string) HyperOutcome {
	k := normalizeToken(raw)
	if k == "" {
		return HyperUnknown
	}
	if o, ok := hyperResultMap[k]; ok {
		return o
	}
	return HyperUnknown
}

func hyperStateRunning(raw string) bool   { return hyperRunningStates[normalizeToken(raw)] }
func hyperStateIntegrity(raw string) bool { return hyperIntegrityStates[normalizeToken(raw)] }

// HyperTask is the per-task evaluation result.
type HyperTask struct {
	TaskID          string       `json:"task_id"`
	Name            string       `json:"name"`
	Target          string       `json:"target,omitempty"`
	Enabled         bool         `json:"enabled"`
	Excluded        bool         `json:"excluded"`
	Monitored       bool         `json:"monitored"`
	EffectiveMaxAge string       `json:"effective_max_age"`
	State           string       `json:"state,omitempty"`
	Status          string       `json:"status,omitempty"`
	LastResultRaw   string       `json:"last_result_raw,omitempty"`
	LastResult      HyperOutcome `json:"last_result,omitempty"`
	LastBackupEnd   *time.Time   `json:"last_backup_end,omitempty"`
	LastSuccess     *time.Time   `json:"last_success,omitempty"`
	NextBackup      *time.Time   `json:"next_backup,omitempty"`
	Progress        string       `json:"progress,omitempty"`
	Running         bool         `json:"running"`
	RunningNote     string       `json:"running_note,omitempty"`
	Failed          bool         `json:"failed"`
	Partial         bool         `json:"partial"`
	Integrity       bool         `json:"integrity_failed"`
	DestMissing     bool         `json:"dest_missing"`
	Cancelled       bool         `json:"cancelled"`
	Suspended       bool         `json:"suspended"`
	Overdue         bool         `json:"overdue"`
	Unknown         bool         `json:"unknown"`
	Note            string       `json:"note,omitempty"`

	statusFetchFailed bool // internal: live status was inaccessible
}

// HyperInfo is the Hyper Backup collector result.
type HyperInfo struct {
	State              CollectorState   `json:"state"`
	StateReason        string           `json:"state_reason,omitempty"`
	Tasks              []HyperTask      `json:"tasks"`
	Total              int              `json:"total"`
	Monitored          int              `json:"monitored"`
	Disabled           int              `json:"disabled"`
	Excluded           int              `json:"excluded"`
	Running            int              `json:"running"`
	Failed             int              `json:"failed"` // failed + partial runs
	Overdue            int              `json:"overdue"`
	Integrity          int              `json:"integrity_failed"`
	DestMissing        int              `json:"dest_missing"`
	Cancelled          int              `json:"cancelled"`
	Suspended          int              `json:"suspended"`
	Unknown            int              `json:"unknown"`
	LastSuccess        *time.Time       `json:"last_success,omitempty"`
	LastSuccessState   LastSuccessState `json:"-"`
	Notes              []string         `json:"notes,omitempty"`
	UnmatchedSelectors []string         `json:"unmatched_selectors,omitempty"`
}

// brokenCount is the population reported as HB_FAILED: tasks whose backup is
// actually broken — a failed/partial run, a failed integrity check, or an
// unreachable destination. Cancelled/suspended/overdue/unknown are surfaced
// separately so an intentional pause is not conflated with a broken backup.
func (h *HyperInfo) brokenCount() int { return h.Failed + h.Integrity + h.DestMissing }

// collectHyper is the top-level Hyper Backup collector. A network error is fatal
// (exit 3); an ambiguous task selector is also fatal (a usage error → exit 3).
func collectHyper(ctx context.Context, c *Client, cfg *Config, now time.Time) (*HyperInfo, error) {
	info := &HyperInfo{Tasks: []HyperTask{}}

	// 1. Install state. A missing API alone does not prove "not installed".
	if !c.HasAPI("SYNO.Backup.Task") {
		pkgs, err := collectPackages(ctx, c)
		if err != nil {
			if k, _ := kindOf(err); k == ErrNetwork {
				return nil, err
			}
			info.State = StateUnavailable
			info.StateReason = "Hyper Backup API not advertised and the package list is inaccessible"
			return info, nil
		}
		if pkgs != nil && !pkgs["HyperBackup"] {
			info.State = StateNotInstalled
			info.StateReason = "Hyper Backup is not installed"
			return info, nil
		}
		info.State = StateUnavailable
		info.StateReason = "Hyper Backup package present but its API is not accessible (check account privileges)"
		return info, nil
	}

	ver, verr := c.pickVersion("SYNO.Backup.Task")
	if verr != nil {
		info.State = StateUnavailable
		info.StateReason = verr.Error()
		return info, nil
	}

	// 2. Task list.
	data, err := c.apiCall(ctx, "SYNO.Backup.Task", ver, "list", nil)
	if err != nil {
		if k, _ := kindOf(err); k == ErrNetwork {
			return nil, err
		}
		info.State = StateError
		info.StateReason = "cannot list Hyper Backup tasks: " + err.Error()
		return info, nil
	}
	taskRaws, err := decodeHyperTaskList(data)
	if err != nil {
		info.State = StateError
		info.StateReason = "cannot parse Hyper Backup task list: " + err.Error()
		return info, nil
	}
	pre := make([]hyperRawTask, 0, len(taskRaws))
	for _, tRaw := range taskRaws {
		pre = append(pre, parseHyperTask(tRaw))
	}

	// 3. Resolve exclusion selectors (ambiguity is fatal).
	refs := make([]hyperRef, 0, len(pre))
	for _, p := range pre {
		if p.hasID {
			refs = append(refs, hyperRef{id: p.id, idNum: p.idNum, idNumOK: p.idNumOK, name: p.name})
		}
	}
	excluded, unmatched, selErr := resolveHyperExclusions(cfg, refs)
	if selErr != nil {
		return nil, selErr
	}
	info.UnmatchedSelectors = unmatched

	// 4. Evaluate each task. Monitored status is resolved before any status fetch
	// so disabled/excluded tasks are skipped (their state cannot alert).
	for _, p := range pre {
		task := HyperTask{
			TaskID:          p.id,
			Name:            p.name,
			Target:          p.target,
			Enabled:         p.enabled,
			EffectiveMaxAge: cfg.HyperBackupMaxAge.String(),
			State:           p.fields.state,
			Status:          p.fields.status,
		}
		if p.hasID {
			task.Excluded = excluded[p.id]
		}
		task.Monitored = task.Enabled && !task.Excluded

		if !p.hasID {
			task.Unknown = true
			task.Note = "task has no usable task_id; cannot query status"
			if task.Monitored {
				task.statusFetchFailed = true
			}
			info.Tasks = append(info.Tasks, task)
			continue
		}
		if !task.Monitored {
			task.Note = "not monitored (disabled or excluded); status not collected"
			info.Tasks = append(info.Tasks, task)
			continue
		}

		st, stErr := c.hyperStatus(ctx, ver, p.id)
		if stErr != nil {
			if k, _ := kindOf(stErr); k == ErrNetwork {
				return nil, stErr
			}
			task.statusFetchFailed = true
			task.Unknown = true
			task.Note = "status query failed: " + stErr.Error()
			info.Tasks = append(info.Tasks, task)
			continue
		}
		evaluateHyperTask(&task, mergeHyperFields(p.fields, st), cfg, now)
		info.Tasks = append(info.Tasks, task)
	}

	aggregateHyper(info)
	return info, nil
}

// evaluateHyperTask classifies one monitored task from its merged live fields.
// The order is deliberate: a running task short-circuits to healthy activity
// (never failed, never overdue) regardless of how long it has run; only an idle
// task is judged on its last result and freshness.
func evaluateHyperTask(task *HyperTask, f hyperFields, cfg *Config, now time.Time) {
	task.State = f.state
	task.Status = f.status
	task.LastResultRaw = f.result
	task.Progress = f.progress
	if t, ok := parseHyperTime(f.endTime, now); ok {
		task.LastBackupEnd = &t
	}
	if t, ok := parseHyperTime(f.nextTime, now); ok {
		task.NextBackup = &t
	}
	// last_bkp_success_time is the authoritative last SUCCESS — what DSM's own UI
	// shows — and is reported independently of how the most recent run ended.
	successT, haveSuccess := parseHyperTime(f.successTime, now)

	outcome := HyperUnknown
	if f.resultPresent {
		outcome = classifyHyperResult(f.result)
	}
	task.LastResult = outcome

	// Running now (from the live status/state, or an in-progress result value)
	// suppresses every failure/overdue signal. A multi-day sync or integrity check
	// is exactly this case.
	if hyperStateRunning(f.state) || hyperStateRunning(f.status) || outcome == HyperRunning {
		task.Running = true
		task.RunningNote = hyperRunningNote(f)
		return
	}

	if !f.resultPresent {
		task.Unknown = true
		task.Note = "Hyper Backup did not report a last result (check account privileges or API version)"
		return
	}

	// Fall back to the last run's end time as the success time only when the run
	// itself succeeded (some payloads omit last_bkp_success_time).
	if !haveSuccess && outcome == HyperSuccess && task.LastBackupEnd != nil {
		successT, haveSuccess = *task.LastBackupEnd, true
	}
	// Freshness is judged on the last SUCCESS, independent of the last run's
	// outcome: a task whose latest run failed but succeeded recently is failed, not
	// also overdue; one that has not succeeded within the window is overdue even if
	// its last run "completed".
	if haveSuccess {
		ls := successT
		task.LastSuccess = &ls
		if now.Sub(successT) > cfg.HyperBackupMaxAge {
			task.Overdue = true
		}
	}

	switch outcome {
	case HyperSuccess:
		if !haveSuccess {
			task.Note = "last backup succeeded but reported no completion time; freshness indeterminate"
		}
	case HyperNone:
		task.Overdue = true
		task.Note = "no successful backup on record"
	case HyperFailed:
		task.Failed = true
	case HyperPartial:
		task.Partial = true
		task.Note = "last backup completed with errors (partial)"
	case HyperIntegrity:
		task.Integrity = true
		task.Note = "backup-integrity check failed; the backup data may be corrupt"
	case HyperDestMissing:
		task.DestMissing = true
		task.Note = "backup destination is unreachable"
	case HyperCancelled:
		task.Cancelled = true
		task.Note = "last backup was cancelled"
	case HyperSuspended:
		task.Suspended = true
		task.Note = "task is suspended"
	default: // HyperUnknown
		task.Unknown = true
		task.Note = "last backup result unrecognized: " + f.result
	}
}

// hyperRunningNote describes what a running task is doing, distinguishing an
// integrity check and version housekeeping from an ordinary backup so the report
// explains why a long-running task is not alarming.
func hyperRunningNote(f hyperFields) string {
	switch {
	case hyperStateIntegrity(f.state) || hyperStateIntegrity(f.status):
		return "backup-integrity check in progress"
	case isVersionDeleting(f.state) || isVersionDeleting(f.status):
		return "removing old backup versions"
	default:
		note := "backup in progress"
		if p := strings.TrimSpace(f.progress); p != "" {
			note += " (" + p + ")"
		}
		return note
	}
}

func isVersionDeleting(raw string) bool {
	k := normalizeToken(raw)
	return k == "version_deleting" || k == "preparing_version_delete"
}

// aggregateHyper computes counts and coverage state over the monitored set
// (enabled and not excluded).
func aggregateHyper(info *HyperInfo) {
	info.Total = len(info.Tasks)
	monitoredWithData := 0
	monitoredFetchFailed := 0
	allNever := true // every determinate monitored task has explicitly never succeeded

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
		if t.statusFetchFailed {
			monitoredFetchFailed++
		} else {
			monitoredWithData++
		}
		if t.Running {
			info.Running++
		}
		if t.Failed || t.Partial {
			info.Failed++
		}
		if t.Integrity {
			info.Integrity++
		}
		if t.DestMissing {
			info.DestMissing++
		}
		if t.Cancelled {
			info.Cancelled++
		}
		if t.Suspended {
			info.Suspended++
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
		// "never" holds only if every determinate monitored task explicitly
		// reported none; a failed/unknown/running task hides the true last success.
		if t.statusFetchFailed || t.LastResult != HyperNone {
			allNever = false
		}
	}

	switch {
	case info.Monitored == 0:
		info.LastSuccessState = LSNone
	case info.LastSuccess != nil:
		info.LastSuccessState = LSKnown
	case monitoredFetchFailed == 0 && allNever:
		info.LastSuccessState = LSNever
	default:
		info.LastSuccessState = LSUnknown
	}

	switch {
	case info.Monitored > 0 && monitoredWithData == 0:
		info.State = StateError
		info.StateReason = "every monitored Hyper Backup task's status was inaccessible"
	case monitoredFetchFailed > 0:
		info.State = StatePartial
		info.StateReason = fmt.Sprintf("%d monitored task(s) had inaccessible status", monitoredFetchFailed)
	default:
		info.State = StateOK
	}
}

// hyperFields is the health-relevant subset of a task's live status, drawn from
// either the list entry or the per-task status call (merged, status wins).
// Timestamps are kept as their raw strings — Hyper Backup reports them as
// "2006/01/02 15:04:05" wall-clock strings, not Unix epochs — and parsed lazily
// via parseHyperTime.
type hyperFields struct {
	state         string
	status        string
	result        string
	resultPresent bool
	successTime   string // last_bkp_success_time: the authoritative last SUCCESS
	endTime       string // last_bkp_end_time / last_bkp_time: when the last run ended
	nextTime      string // next_bkp_time
	progress      string
}

func parseHyperFields(m map[string]json.RawMessage) hyperFields {
	f := hyperFields{
		state:       flexStr(m["state"]),
		status:      flexStr(m["status"]),
		progress:    flexStr(m["last_bkp_progress"]),
		successTime: flexStr(m["last_bkp_success_time"]),
		endTime:     firstNonEmpty(flexStr(m["last_bkp_end_time"]), flexStr(m["last_bkp_time"])),
		nextTime:    flexStr(m["next_bkp_time"]),
	}
	// resultPresent distinguishes an explicit "none" (never backed up) from the
	// field being absent (API/permission variance → indeterminate, not "never").
	for _, k := range []string{"last_bkp_result", "last_result"} {
		if raw, ok := m[k]; ok {
			f.result = flexStr(raw)
			f.resultPresent = true
			break
		}
	}
	return f
}

// mergeHyperFields overlays the per-task status response over the list entry:
// any value the status call actually returned wins.
func mergeHyperFields(base, over hyperFields) hyperFields {
	if over.state != "" {
		base.state = over.state
	}
	if over.status != "" {
		base.status = over.status
	}
	if over.resultPresent {
		base.result = over.result
		base.resultPresent = true
	}
	if over.progress != "" {
		base.progress = over.progress
	}
	if over.successTime != "" {
		base.successTime = over.successTime
	}
	if over.endTime != "" {
		base.endTime = over.endTime
	}
	if over.nextTime != "" {
		base.nextTime = over.nextTime
	}
	return base
}

// hyperTimeLayouts are the wall-clock formats Hyper Backup uses for its timestamp
// strings (no timezone — NAS local time). They are parsed as UTC so the same
// wall-clock digits are echoed back, matching what DSM's own UI shows, rather than
// being shifted by the collector host's timezone.
var hyperTimeLayouts = []string{
	"2006/01/02 15:04:05",
	"2006/01/02 15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

// parseHyperTime parses a Hyper Backup timestamp. DSM returns these as
// "2006/01/02 15:04:05" strings (some fields omit the seconds); a numeric Unix
// epoch is still accepted as a fallback for other DSM shapes. ok is false for an
// empty or unrecognized value.
func parseHyperTime(raw string, now time.Time) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return parseEpoch(n, now)
	}
	for _, layout := range hyperTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// hyperRawTask holds one task decoded defensively from the list response.
type hyperRawTask struct {
	id      string // raw task_id string (what the status API expects)
	hasID   bool
	idNum   int64 // numeric form, when task_id is a plain integer
	idNumOK bool
	name    string
	target  string
	enabled bool
	fields  hyperFields
}

// parseHyperTask extracts fields individually so a single oddly-typed field
// cannot wipe the whole record. task_id may arrive as a string or a number; the
// raw string form is kept as the id and a numeric form is derived for selectors.
func parseHyperTask(tRaw json.RawMessage) hyperRawTask {
	t := hyperRawTask{enabled: true}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(tRaw, &m); err != nil {
		t.name = "unparseable-task"
		return t
	}
	if raw, ok := m["task_id"]; ok {
		if id := flexStr(raw); id != "" {
			t.id = id
			t.hasID = true
			if n, err := strconv.ParseInt(id, 10, 64); err == nil {
				t.idNum = n
				t.idNumOK = true
			}
		}
	}
	t.name = firstNonEmpty(flexStr(m["task_name"]), flexStr(m["name"]))
	if t.name == "" {
		if t.hasID {
			t.name = "task_" + t.id
		} else {
			t.name = "unnamed"
		}
	}
	t.target = firstNonEmpty(flexStr(m["target_type"]), flexStr(m["type"]), flexStr(m["vault_type"]))
	// Hyper Backup tasks may omit an enabled flag entirely (always scheduled); the
	// default therefore stays true.
	for _, k := range []string{"enabled", "is_enabled", "enable"} {
		if raw, ok := m[k]; ok {
			t.enabled = t.enabled && parseFlexBool(raw, true)
		}
	}
	t.fields = parseHyperFields(m)
	return t
}

// decodeHyperTaskList returns the task array, tolerating both field names DSM has
// used ("task_list" on current builds, "tasks" on others).
func decodeHyperTaskList(data json.RawMessage) ([]json.RawMessage, error) {
	arr, err := decodeArrayField(data, "task_list")
	if err == nil {
		return arr, nil
	}
	if arr2, err2 := decodeArrayField(data, "tasks"); err2 == nil {
		return arr2, nil
	}
	return nil, err
}

// hyperStatus fetches one task's live status. blOnline=false avoids a slow live
// probe of the (possibly remote/unreachable) destination — the last-run result
// already reflects a missing destination. A 402 (bad additional param on an older
// DSM) is retried without the additional fields.
func (c *Client) hyperStatus(ctx context.Context, ver int, id string) (hyperFields, error) {
	params := url.Values{}
	params.Set("task_id", id)
	params.Set("blOnline", "false")
	params.Set("additional", `["last_bkp_time","last_bkp_end_time","last_bkp_success_time","next_bkp_time","last_bkp_result","is_modified","last_bkp_progress"]`)
	data, err := c.apiCall(ctx, "SYNO.Backup.Task", ver, "status", params)
	if err != nil && codeOf(err) == 402 {
		plain := url.Values{}
		plain.Set("task_id", id)
		data, err = c.apiCall(ctx, "SYNO.Backup.Task", ver, "status", plain)
	}
	if err != nil {
		return hyperFields{}, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return hyperFields{}, &DSMError{Kind: ErrParse, API: "SYNO.Backup.Task", Msg: "cannot parse task status: " + err.Error()}
	}
	return parseHyperFields(m), nil
}

// hyperRef is a task identity used for exclusion-selector matching.
type hyperRef struct {
	id      string
	idNum   int64
	idNumOK bool
	name    string
}

// resolveHyperExclusions turns --exclude-hyperbackup-task selectors into a set of
// excluded task ids. An ambiguous name match is a fatal usage error; selectors
// matching nothing are returned as unmatched.
func resolveHyperExclusions(cfg *Config, refs []hyperRef) (excluded map[string]bool, unmatched []string, err error) {
	excluded = map[string]bool{}
	for _, sel := range cfg.ExcludeHyperTasks {
		ids, matched, e := matchHyperSelector(sel, refs)
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

func matchHyperSelector(sel Selector, refs []hyperRef) (map[string]bool, bool, error) {
	ids := map[string]bool{}
	byName := func(name string) []hyperRef {
		var out []hyperRef
		for _, r := range refs {
			if r.name == name {
				out = append(out, r)
			}
		}
		return out
	}
	selNum := strconv.FormatInt(sel.ID, 10)
	switch sel.Kind {
	case SelID:
		for _, r := range refs {
			if r.id == selNum || (r.idNumOK && r.idNum == sel.ID) {
				ids[r.id] = true
			}
		}
	case SelName:
		matches := byName(sel.Name)
		if len(matches) > 1 {
			return nil, false, &DSMError{Kind: ErrAPI, API: "selector",
				Msg: fmt.Sprintf("Hyper Backup task selector %q matches %d tasks; use id:N to disambiguate", sel.Raw, len(matches))}
		}
		for _, r := range matches {
			ids[r.id] = true
		}
	case SelAuto:
		matches := byName(sel.Name)
		if len(matches) > 1 {
			return nil, false, &DSMError{Kind: ErrAPI, API: "selector",
				Msg: fmt.Sprintf("Hyper Backup task selector %q matches %d tasks by name; use id:N to disambiguate", sel.Raw, len(matches))}
		}
		if len(matches) == 1 {
			ids[matches[0].id] = true
		} else {
			for _, r := range refs {
				if r.id == sel.Name {
					ids[r.id] = true
				}
			}
		}
	}
	return ids, len(ids) > 0, nil
}
