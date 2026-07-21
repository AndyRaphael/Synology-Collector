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

const (
	versionPageLimit = 200
	versionCap       = 1000
)

// ABBOutcome is the normalized result of a backup version or task run.
type ABBOutcome string

const (
	OutcomeSuccess   ABBOutcome = "success"
	OutcomeFailed    ABBOutcome = "failed"
	OutcomeCancelled ABBOutcome = "cancelled"
	OutcomeRunning   ABBOutcome = "running"
	OutcomeUnknown   ABBOutcome = "unknown"
)

func (o ABBOutcome) terminal() bool {
	return o == OutcomeSuccess || o == OutcomeFailed || o == OutcomeCancelled
}

// abbStatusMap maps normalized DSM status/result strings to outcomes. Matching is
// EXACT on the normalized token — substring matching is unsafe ("broken" contains
// "ok", "incomplete" contains "complete"). Unrecognized values become
// OutcomeUnknown and are surfaced (never silently treated as success). After the
// first live --debug run, correct real enum values here and nowhere else.
var abbStatusMap = map[string]ABBOutcome{
	"complete":   OutcomeSuccess,
	"completed":  OutcomeSuccess,
	"finish":     OutcomeSuccess,
	"finished":   OutcomeSuccess,
	"success":    OutcomeSuccess,
	"successful": OutcomeSuccess,
	"done":       OutcomeSuccess,
	"ok":         OutcomeSuccess,

	"fail":    OutcomeFailed,
	"failed":  OutcomeFailed,
	"failure": OutcomeFailed,
	"error":   OutcomeFailed,
	"broken":  OutcomeFailed,
	"crash":   OutcomeFailed,
	"crashed": OutcomeFailed,

	"cancel":      OutcomeCancelled,
	"canceled":    OutcomeCancelled,
	"cancelled":   OutcomeCancelled,
	"suspend":     OutcomeCancelled,
	"suspended":   OutcomeCancelled,
	"interrupt":   OutcomeCancelled,
	"interrupted": OutcomeCancelled,
	"abort":       OutcomeCancelled,
	"aborted":     OutcomeCancelled,

	"running":    OutcomeRunning,
	"backingup":  OutcomeRunning,
	"backing_up": OutcomeRunning,
	"waiting":    OutcomeRunning,
	"pending":    OutcomeRunning,
	"preparing":  OutcomeRunning,
	"processing": OutcomeRunning,
}

// abbNumericStatus maps DSM's numeric Active Backup status codes, observed on
// DSM 7.x: version restore points carry status 3 (success); a task's last_result
// carries 2 (success), 4 (completed with errors), and 5 (failed). Code 1 is the
// standard "in progress". This map is only a HINT for the latest-attempt outcome —
// the authoritative signal is version alignment / error counts (see
// lastResultEvent), which does not depend on getting every code right. Codes not
// listed stay unknown and are surfaced.
var abbNumericStatus = map[string]ABBOutcome{
	"1": OutcomeRunning,
	"2": OutcomeSuccess,
	"3": OutcomeSuccess,
	"4": OutcomeFailed,
	"5": OutcomeFailed,
}

func classifyABBStatus(raw string) ABBOutcome {
	key := strings.ToLower(strings.TrimSpace(raw))
	key = strings.ReplaceAll(key, "-", "_")
	if key == "" {
		return OutcomeUnknown
	}
	if o, ok := abbStatusMap[key]; ok {
		return o
	}
	if o, ok := abbNumericStatus[key]; ok {
		return o
	}
	return OutcomeUnknown
}

// ABBEvent is one backup version reduced to what the checks need.
type ABBEvent struct {
	Time      time.Time  `json:"time"`
	Outcome   ABBOutcome `json:"outcome"`
	RawStatus string     `json:"raw_status,omitempty"`
}

// ABBTask is the per-task evaluation result.
type ABBTask struct {
	TaskID          int64      `json:"task_id"`
	Name            string     `json:"name"`
	SourceType      string     `json:"source_type,omitempty"`
	Enabled         bool       `json:"enabled"`
	Excluded        bool       `json:"excluded"`
	Monitored       bool       `json:"monitored"`
	EffectiveMaxAge string     `json:"effective_max_age"`
	VersionsSeen    int        `json:"versions_seen"`
	HistoryComplete bool       `json:"history_complete"`
	LatestAttempt   *ABBEvent  `json:"latest_attempt,omitempty"`
	LatestTerminal  *ABBEvent  `json:"latest_terminal,omitempty"`
	LastSuccess     *time.Time `json:"last_success,omitempty"`
	Failed          bool       `json:"failed"`
	Cancelled       bool       `json:"cancelled"`
	Overdue         bool       `json:"overdue"`
	Unknown         bool       `json:"unknown"`
	Note            string     `json:"note,omitempty"`

	versionFetchFailed bool // internal: version history was inaccessible
}

// LastSuccessState captures the four distinct meanings LAST_SUCCESS can carry.
type LastSuccessState int

const (
	LSNone    LastSuccessState = iota // N/A: no monitored tasks / ABB not applicable
	LSKnown                           // a timestamp is available
	LSNever                           // monitored, complete histories, no success ever
	LSUnknown                         // indeterminate (truncation or fetch failure)
)

// ABBInfo is the Active Backup for Business collector result.
type ABBInfo struct {
	State              CollectorState   `json:"state"`
	StateReason        string           `json:"state_reason,omitempty"`
	Tasks              []ABBTask        `json:"tasks"`
	Total              int              `json:"total"`
	Monitored          int              `json:"monitored"`
	Disabled           int              `json:"disabled"`
	Excluded           int              `json:"excluded"`
	Failed             int              `json:"failed"`
	Overdue            int              `json:"overdue"`
	Cancelled          int              `json:"cancelled"`
	Unknown            int              `json:"unknown"`
	LastSuccess        *time.Time       `json:"last_success,omitempty"`
	LastSuccessState   LastSuccessState `json:"-"`
	Notes              []string         `json:"notes,omitempty"`
	UnmatchedSelectors []string         `json:"unmatched_selectors,omitempty"`
}

type rawABBVersion struct {
	TimeEnd    FlexInt64  `json:"time_end"`
	TimeStart  FlexInt64  `json:"time_start"`
	CreateTime FlexInt64  `json:"create_time"`
	Status     FlexString `json:"status"`
	Result     FlexString `json:"result"`
}

func (v rawABBVersion) rawStatus() string {
	return firstNonEmpty(string(v.Status), string(v.Result))
}

func (v rawABBVersion) outcome() ABBOutcome {
	return classifyABBStatus(v.rawStatus())
}

// orderTime picks the best available timestamp for ordering: end, then start,
// then creation. A running backup often has no time_end but does have time_start.
func (v rawABBVersion) orderTime(now time.Time) (time.Time, bool) {
	for _, cand := range []int64{int64(v.TimeEnd), int64(v.TimeStart), int64(v.CreateTime)} {
		if t, ok := parseEpoch(cand, now); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// collectABB is the top-level ABB collector. A network error is returned as a
// fatal error; an ambiguous task selector is also fatal (a usage error → exit 3).
func collectABB(ctx context.Context, c *Client, cfg *Config, now time.Time) (*ABBInfo, error) {
	info := &ABBInfo{Tasks: []ABBTask{}}

	// 1. Install state. 102/103 (missing API) alone does not prove "not installed".
	if !c.HasAPI("SYNO.ActiveBackup.Task") {
		pkgs, err := collectPackages(ctx, c)
		if err != nil {
			if k, _ := kindOf(err); k == ErrNetwork {
				return nil, err
			}
			info.State = StateUnavailable
			info.StateReason = "Active Backup API not advertised and the package list is inaccessible"
			return info, nil
		}
		if pkgs != nil && !pkgs["ActiveBackup"] {
			info.State = StateNotInstalled
			info.StateReason = "Active Backup for Business is not installed"
			return info, nil
		}
		info.State = StateUnavailable
		info.StateReason = "Active Backup package present but its API is not accessible (check account privileges)"
		return info, nil
	}

	ver, verr := c.pickVersion("SYNO.ActiveBackup.Task")
	if verr != nil {
		info.State = StateUnavailable
		info.StateReason = verr.Error()
		return info, nil
	}

	// 2. Task list, preferring the enriched form; retry plain on a bad-param code.
	params := url.Values{}
	params.Set("load_status", "true")
	params.Set("load_result", "true")
	data, err := c.apiCall(ctx, "SYNO.ActiveBackup.Task", ver, "list", params)
	if err != nil && codeOf(err) == 402 {
		data, err = c.apiCall(ctx, "SYNO.ActiveBackup.Task", ver, "list", nil)
	}
	if err != nil {
		if k, _ := kindOf(err); k == ErrNetwork {
			return nil, err
		}
		info.State = StateError
		info.StateReason = "cannot list Active Backup tasks: " + err.Error()
		return info, nil
	}

	// Require the tasks field to be present and be a JSON array. A missing or
	// null field (e.g. an API schema change) must be an error, not a false
	// "zero healthy tasks". A present empty array is legitimate.
	taskRaws, err := decodeArrayField(data, "tasks")
	if err != nil {
		info.State = StateError
		info.StateReason = "cannot parse Active Backup task list: " + err.Error()
		return info, nil
	}
	pre := make([]abbRawTask, 0, len(taskRaws))
	for _, tRaw := range taskRaws {
		pre = append(pre, parseABBTask(tRaw))
	}

	// 3. Resolve selectors against the concrete task set (ambiguity is fatal).
	refs := make([]taskRef, 0, len(pre))
	for _, p := range pre {
		if p.hasID {
			refs = append(refs, taskRef{id: p.id, name: p.name})
		}
	}
	excludedIDs, overrides, unmatched, selErr := resolveTaskSelectors(cfg, refs)
	if selErr != nil {
		return nil, selErr
	}
	info.UnmatchedSelectors = unmatched

	// 4. Evaluate each task. Monitored status is resolved BEFORE any history
	// fetch so disabled/excluded tasks are skipped (their failures cannot alert,
	// and a timeout fetching them must not abort the run).
	for _, p := range pre {
		task := ABBTask{
			TaskID:          p.id,
			Name:            p.name,
			SourceType:      p.sourceType,
			Enabled:         p.enabled,
			EffectiveMaxAge: cfg.BackupMaxAge.String(),
		}
		if p.hasID {
			task.Excluded = excludedIDs[p.id]
			if d, ok := overrides[p.id]; ok {
				task.EffectiveMaxAge = d.String()
			}
		}
		task.Monitored = task.Enabled && !task.Excluded

		// No usable task_id → history cannot be fetched. If the task would be
		// monitored, count it as inaccessible so coverage reflects the gap
		// (all-invalid monitored tasks then yield ABB error, not a false OK).
		if !p.hasID {
			task.Unknown = true
			if p.idInvalid {
				task.Note = "task_id present but not a valid positive integer; cannot evaluate history"
			} else {
				task.Note = "task has no task_id; cannot evaluate history"
			}
			if task.Monitored {
				task.versionFetchFailed = true
			}
			info.Tasks = append(info.Tasks, task)
			continue
		}

		// Skip history for tasks that cannot alert; retain their list metadata.
		if !task.Monitored {
			task.Note = "not monitored (disabled or excluded); backup history not collected"
			info.Tasks = append(info.Tasks, task)
			continue
		}

		maxAge := cfg.BackupMaxAge
		if d, ok := overrides[p.id]; ok {
			maxAge = d
		}

		scan := c.scanVersions(ctx, p.id, now)
		if scan.err != nil {
			if k, _ := kindOf(scan.err); k == ErrNetwork {
				return nil, scan.err
			}
			task.versionFetchFailed = true
			task.Unknown = true
			task.Note = "version history query failed: " + scan.err.Error()
			info.Tasks = append(info.Tasks, task)
			continue
		}

		task.VersionsSeen = scan.count
		task.HistoryComplete = scan.historyComplete
		var newestSuccess time.Time
		if scan.lastSuccess != nil {
			newestSuccess = scan.lastSuccess.Time
			task.LastSuccess = &newestSuccess
		}

		// The latest ATTEMPT outcome comes from the task's last_result: a failed
		// attempt creates no version, so failures live only there. Its success or
		// failure is judged by evidence (version alignment / error counts), not the
		// raw status code. Fall back to the freshest terminal version when
		// last_result is absent.
		if p.lastResult != nil && p.lastResult.present {
			task.LatestAttempt = lastResultEvent(p.lastResult, newestSuccess, now)
			if task.LatestAttempt.Outcome.terminal() {
				task.LatestTerminal = task.LatestAttempt
			}
		} else {
			task.LatestAttempt = scan.latestAttempt
			task.LatestTerminal = scan.latestTerminal
		}
		if task.LatestTerminal != nil {
			switch task.LatestTerminal.Outcome {
			case OutcomeFailed:
				task.Failed = true
			case OutcomeCancelled:
				task.Cancelled = true
			}
		}
		// String-only fallback when there is neither a last_result nor any version.
		if (p.lastResult == nil || !p.lastResult.present) && scan.count == 0 &&
			p.statusHint != "" && classifyABBStatus(p.statusHint) == OutcomeFailed {
			task.Failed = true
		}

		// Overdue / never / indeterminate, driven only by classified successes.
		switch {
		case scan.lastSuccess != nil:
			if now.Sub(scan.lastSuccess.Time) > maxAge {
				task.Overdue = true
			}
		case scan.historyComplete:
			// Complete history with no success ever → overdue by design (a task
			// alerts until its first backup lands).
			task.Overdue = true
			if task.Note == "" {
				task.Note = "no successful backup on record"
			}
		default:
			// Truncated history without a discovered success proves nothing: an
			// unseen record beyond the cap could be a recent success.
			task.Unknown = true
			task.Note = fmt.Sprintf("history truncated at %d versions without a success; last success indeterminate", versionCap)
		}

		// Surface enum drift on the latest attempt specifically (not deep history).
		if task.LatestAttempt != nil && task.LatestAttempt.Outcome == OutcomeUnknown {
			task.Unknown = true
			if task.Note == "" {
				task.Note = "latest backup status unrecognized: " + task.LatestAttempt.RawStatus
			}
		}

		info.Tasks = append(info.Tasks, task)
	}

	aggregateABB(info, now)
	return info, nil
}

// aggregateABB computes counts and state over the monitored set (enabled and not
// excluded). Disabled and excluded tasks can neither alert nor refresh LAST_SUCCESS.
func aggregateABB(info *ABBInfo, now time.Time) {
	info.Total = len(info.Tasks)
	monitoredWithHistory := 0
	monitoredFetchFailed := 0

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
		if t.versionFetchFailed {
			monitoredFetchFailed++
		} else {
			monitoredWithHistory++
		}
		if t.Failed {
			info.Failed++
		}
		if t.Overdue {
			info.Overdue++
		}
		if t.Cancelled {
			info.Cancelled++
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
	}

	// LAST_SUCCESS semantics.
	switch {
	case info.Monitored == 0:
		info.LastSuccessState = LSNone
	case info.LastSuccess != nil:
		info.LastSuccessState = LSKnown
	default:
		allDeterminate := true
		for i := range info.Tasks {
			t := &info.Tasks[i]
			if !t.Monitored {
				continue
			}
			if t.versionFetchFailed || !t.HistoryComplete {
				allDeterminate = false
				break
			}
		}
		if allDeterminate {
			info.LastSuccessState = LSNever
		} else {
			info.LastSuccessState = LSUnknown
		}
	}

	// Coverage state.
	switch {
	case info.Monitored > 0 && monitoredWithHistory == 0:
		info.State = StateError
		info.StateReason = "every monitored Active Backup task's version history was inaccessible"
	case monitoredFetchFailed > 0:
		info.State = StatePartial
		info.StateReason = fmt.Sprintf("%d monitored task(s) had inaccessible version history", monitoredFetchFailed)
	default:
		info.State = StateOK
	}
}

// abbLastResult is the task's most recent backup attempt (from load_result). It
// is the authoritative latest-attempt outcome: a failed attempt never creates a
// version (versions are successful restore points), so failures are only visible
// here.
type abbLastResult struct {
	present      bool
	statusRaw    string
	timeEnd      int64
	errorCount   int64
	successCount int64
}

// abbRawTask holds one task decoded defensively from the list response.
type abbRawTask struct {
	raw        map[string]json.RawMessage
	id         int64
	hasID      bool // true only when task_id is present AND a valid positive integer
	idInvalid  bool // task_id present but unparseable/non-positive, or record undecodable
	name       string
	sourceType string
	enabled    bool
	statusHint string
	lastResult *abbLastResult
}

// parseABBTask extracts fields individually from the task object so that a single
// oddly-typed field (e.g. enabled as 0/1 or a string) cannot silently wipe out the
// entire record. task_id is accepted only when present and a positive integer.
func parseABBTask(tRaw json.RawMessage) abbRawTask {
	t := abbRawTask{enabled: true}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(tRaw, &m); err != nil {
		t.idInvalid = true
		t.name = "unparseable-task"
		return t
	}
	t.raw = m

	if raw, ok := m["task_id"]; ok {
		var fi FlexInt64
		if err := json.Unmarshal(raw, &fi); err == nil && int64(fi) > 0 {
			t.id = int64(fi)
			t.hasID = true
		} else {
			t.idInvalid = true
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
	t.sourceType = flexStr(m["source_type"])

	for _, k := range []string{"enabled", "is_enabled", "enable"} {
		if raw, ok := m[k]; ok {
			t.enabled = t.enabled && parseFlexBool(raw, true)
		}
	}

	// last_result (an object) is the latest attempt's real outcome.
	if raw, ok := m["last_result"]; ok && len(raw) > 0 && string(raw) != "null" {
		var lr struct {
			Status       FlexString `json:"status"`
			TimeEnd      FlexInt64  `json:"time_end"`
			ErrorCount   FlexInt64  `json:"error_count"`
			SuccessCount FlexInt64  `json:"success_count"`
		}
		if err := json.Unmarshal(raw, &lr); err == nil {
			t.lastResult = &abbLastResult{
				present:      true,
				statusRaw:    strings.TrimSpace(string(lr.Status)),
				timeEnd:      int64(lr.TimeEnd),
				errorCount:   int64(lr.ErrorCount),
				successCount: int64(lr.SuccessCount),
			}
		}
	}

	// statusHint is a string-only fallback for tasks with neither versions nor a
	// last_result object.
	t.statusHint = firstNonEmpty(flexStr(m["status"]), flexStr(m["last_backup_result"]))
	return t
}

// resultVersionTolerance is how close a task's last_result end time must be to its
// newest restore point to be considered the same (successful) run. A completed
// backup writes its version a few seconds before the activity ends.
const resultVersionTolerance = 15 * time.Minute

// lastResultEvent converts a task's last_result into a latest-attempt event. The
// outcome is decided by EVIDENCE, not the numeric status code (which varies and is
// unreliable): a run errored if error_count>0; it succeeded if a device succeeded
// or its completion aligns with a fresh restore point; a run that finished after
// the newest restore point yet produced none of its own did not succeed. The
// status code is only a fallback when there is no version data.
func lastResultEvent(lr *abbLastResult, newestSuccess time.Time, now time.Time) *ABBEvent {
	ev := &ABBEvent{RawStatus: lr.statusRaw}
	t, hasTime := parseEpoch(lr.timeEnd, now)
	if hasTime {
		ev.Time = t
	}
	hint := classifyABBStatus(lr.statusRaw)

	switch {
	case hint == OutcomeRunning:
		ev.Outcome = OutcomeRunning
	case lr.errorCount > 0:
		ev.Outcome = OutcomeFailed
	case lr.successCount > 0:
		ev.Outcome = OutcomeSuccess
	case hasTime && !newestSuccess.IsZero() && t.After(newestSuccess.Add(resultVersionTolerance)):
		ev.Outcome = OutcomeFailed // ran after the newest restore point, produced none of its own
	case hasTime && !newestSuccess.IsZero():
		ev.Outcome = OutcomeSuccess // completion aligns with a restore point
	case hint != OutcomeUnknown:
		ev.Outcome = hint
	default:
		ev.Outcome = OutcomeUnknown
	}
	return ev
}

// versionScan accumulates running maxima while paging so the full history is
// never held in memory.
type versionScan struct {
	count           int
	historyComplete bool
	latestAttempt   *ABBEvent
	latestTerminal  *ABBEvent
	lastSuccess     *ABBEvent
	err             error
}

// scanVersions pages a task's version history to its end or the safety cap,
// making no assumption about API ordering.
func (c *Client) scanVersions(ctx context.Context, taskID int64, now time.Time) versionScan {
	var vs versionScan
	ver, verr := c.pickVersion("SYNO.ActiveBackup.Version")
	if verr != nil {
		vs.err = verr
		return vs
	}
	offset := 0
	for {
		params := url.Values{}
		params.Set("task_id", strconv.FormatInt(taskID, 10))
		params.Set("offset", strconv.Itoa(offset))
		params.Set("limit", strconv.Itoa(versionPageLimit))
		data, err := c.apiCall(ctx, "SYNO.ActiveBackup.Version", ver, "list", params)
		if err != nil {
			vs.err = err
			return vs
		}
		// Require the versions field to be present and an array. A missing/null
		// field must be an error (→ task unknown), not a false "never succeeded".
		verRaws, derr := decodeArrayField(data, "versions")
		if derr != nil {
			vs.err = &DSMError{Kind: ErrParse, API: "SYNO.ActiveBackup.Version", Msg: "cannot parse version list: " + derr.Error()}
			return vs
		}
		n := len(verRaws)
		for _, vr := range verRaws {
			var rv rawABBVersion
			if err := json.Unmarshal(vr, &rv); err != nil {
				continue // skip a single malformed version record
			}
			t, ok := rv.orderTime(now)
			ev := &ABBEvent{Outcome: rv.outcome(), RawStatus: rv.rawStatus()}
			if !ok {
				continue // cannot order a timestamp-less record
			}
			ev.Time = t
			if vs.latestAttempt == nil || t.After(vs.latestAttempt.Time) {
				vs.latestAttempt = ev
			}
			if ev.Outcome.terminal() && (vs.latestTerminal == nil || t.After(vs.latestTerminal.Time)) {
				vs.latestTerminal = ev
			}
			if ev.Outcome == OutcomeSuccess && (vs.lastSuccess == nil || t.After(vs.lastSuccess.Time)) {
				vs.lastSuccess = ev
			}
		}
		vs.count += n
		offset += n
		if n < versionPageLimit {
			vs.historyComplete = true
			return vs
		}
		if vs.count >= versionCap {
			vs.historyComplete = false
			return vs
		}
	}
}

// collectPackages returns the set of installed package ids, used to confirm ABB
// install state when the API itself is not advertised.
func collectPackages(ctx context.Context, c *Client) (map[string]bool, error) {
	ver, err := c.pickVersion("SYNO.Core.Package")
	if err != nil {
		return nil, err
	}
	data, err := c.apiCall(ctx, "SYNO.Core.Package", ver, "list", nil)
	if err != nil {
		return nil, err
	}
	// Require the packages field present and an array. A missing field means we
	// cannot confirm install state → caller treats it as unavailable, not
	// not-installed.
	pkgRaws, err := decodeArrayField(data, "packages")
	if err != nil {
		return nil, &DSMError{Kind: ErrParse, API: "SYNO.Core.Package", Msg: "cannot parse package list: " + err.Error()}
	}
	set := make(map[string]bool, len(pkgRaws))
	for _, pr := range pkgRaws {
		var p struct {
			ID   FlexString `json:"id"`
			Name FlexString `json:"name"`
		}
		if err := json.Unmarshal(pr, &p); err != nil {
			continue
		}
		if id := strings.TrimSpace(string(p.ID)); id != "" {
			set[id] = true
		}
	}
	return set, nil
}

type taskRef struct {
	id   int64
	name string
}

// resolveTaskSelectors turns --exclude-task / --task-max-age selectors into a set
// of excluded ids and a map of per-task overrides. An ambiguous name match is a
// fatal usage error; selectors matching nothing are returned as unmatched.
func resolveTaskSelectors(cfg *Config, refs []taskRef) (excluded map[int64]bool, overrides map[int64]time.Duration, unmatched []string, err error) {
	excluded = map[int64]bool{}
	overrides = map[int64]time.Duration{}

	for _, sel := range cfg.ExcludeTasks {
		ids, matched, e := matchSelector(sel, refs)
		if e != nil {
			return nil, nil, nil, e
		}
		if !matched {
			unmatched = append(unmatched, sel.Raw)
			continue
		}
		for id := range ids {
			excluded[id] = true
		}
	}
	for _, ov := range cfg.TaskMaxAge {
		ids, matched, e := matchSelector(ov.Sel, refs)
		if e != nil {
			return nil, nil, nil, e
		}
		if !matched {
			unmatched = append(unmatched, ov.Sel.Raw)
			continue
		}
		for id := range ids {
			overrides[id] = ov.MaxAge
		}
	}
	return excluded, overrides, unmatched, nil
}

func matchSelector(sel Selector, refs []taskRef) (map[int64]bool, bool, error) {
	ids := map[int64]bool{}
	switch sel.Kind {
	case SelID:
		for _, t := range refs {
			if t.id == sel.ID {
				ids[t.id] = true
			}
		}
	case SelName:
		matches := matchByName(sel.Name, refs)
		if len(matches) > 1 {
			return nil, false, &DSMError{Kind: ErrAPI, API: "selector",
				Msg: fmt.Sprintf("task selector %q matches %d tasks (ids %s); use id:N to disambiguate", sel.Raw, len(matches), joinIDs(matches))}
		}
		for _, id := range matches {
			ids[id] = true
		}
	case SelAuto:
		matches := matchByName(sel.Name, refs)
		if len(matches) > 1 {
			return nil, false, &DSMError{Kind: ErrAPI, API: "selector",
				Msg: fmt.Sprintf("task selector %q matches %d tasks by name (ids %s); use id:N to disambiguate", sel.Raw, len(matches), joinIDs(matches))}
		}
		if len(matches) == 1 {
			ids[matches[0]] = true
		} else if n, perr := strconv.ParseInt(sel.Name, 10, 64); perr == nil {
			for _, t := range refs {
				if t.id == n {
					ids[t.id] = true
				}
			}
		}
	}
	return ids, len(ids) > 0, nil
}

func matchByName(name string, refs []taskRef) []int64 {
	var out []int64
	for _, t := range refs {
		if t.name == name {
			out = append(out, t.id)
		}
	}
	return out
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}
