package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Severity is the outcome of a single check. Order matters: the overall status is
// the maximum severity across all checks.
type Severity int

const (
	SevOK Severity = iota
	SevWarning
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevOK:
		return "OK"
	case SevWarning:
		return "WARNING"
	case SevCritical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

func (s Severity) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// CheckResult is one evaluated condition. Message is reused verbatim in SUMMARY.
type CheckResult struct {
	Name     string   `json:"name"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Value    string   `json:"value,omitempty"`
}

// poolStatusSeverity maps DSM pool/volume status words to severities. Unrecognized
// words become WARNING (surfaced with the raw value), never silently OK.
var poolStatusSeverity = map[string]Severity{
	"normal": SevOK, "healthy": SevOK, "good": SevOK, "online": SevOK,

	"attention": SevWarning, "warning": SevWarning, "repairing": SevWarning,
	"rebuilding": SevWarning, "migrating": SevWarning, "expanding": SevWarning,
	"converting": SevWarning, "background_activity": SevWarning, "syncing": SevWarning,
	"scrubbing": SevWarning,

	"degraded": SevCritical, "degrade": SevCritical, "crashed": SevCritical,
	"danger": SevCritical, "error": SevCritical, "missing": SevCritical,
	"deleting": SevCritical, "unknown": SevWarning,
}

// driveStatusSeverity maps DSM disk status words to severities. DSM reports the
// drive's DSM-system-partition state (a RAID1 OS mirror spread across every
// drive) as a distinct dimension from SMART/allocation health, so a perfectly
// healthy drive can read "sys_partition_normal" instead of "normal" — both are
// healthy. The failure counterpart (a failed/damaged system partition) is left
// to the unknown->warning fallback so it still surfaces, never silently OK.
var driveStatusSeverity = map[string]Severity{
	"normal": SevOK, "healthy": SevOK, "good": SevOK, "online": SevOK,
	"sys_partition_normal": SevOK,

	"warning": SevWarning, "attention": SevWarning, "abnormal": SevWarning,
	"degraded": SevWarning, "below": SevWarning,

	"crashed": SevCritical, "failed": SevCritical, "failing": SevCritical,
	"critical": SevCritical, "danger": SevCritical, "dead": SevCritical,
}

// coverageError reports whether the run cannot make a meaningful health statement
// and must exit 3 (STATUS=ERROR), per the coverage contract.
func coverageError(st *StorageInfo, abb *ABBInfo) (string, bool) {
	if st == nil {
		return "storage information is unavailable", true
	}
	if st.State != StateOK {
		return "storage: " + reasonOr(st.StateReason, string(st.State)), true
	}
	if abb != nil {
		switch abb.State {
		case StateError, StateUnavailable:
			return "active backup: " + reasonOr(abb.StateReason, string(abb.State)), true
		}
	}
	return "", false
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	return fallback
}

// evaluate runs every check and returns the results. It never emits to stdout or
// stderr, so the engine stays reusable by a future interactive UI.
func evaluate(cfg *Config, sys *SystemInfo, st *StorageInfo, abb *ABBInfo) []CheckResult {
	var checks []CheckResult
	add := func(name string, sev Severity, msg, val string) {
		checks = append(checks, CheckResult{Name: name, Severity: sev, Message: sanitizeInline(msg), Value: sanitizeInline(val)})
	}

	// System info is non-fatal: a warning, with NAS/DSM reported as Unknown.
	if sys == nil || sys.State != StateOK {
		reason := "system information unavailable"
		if sys != nil && sys.StateReason != "" {
			reason = sys.StateReason
		}
		add("system_info", SevWarning, "System info unavailable: "+reason, string(stateOfSystem(sys)))
	}

	// Storage checks are only meaningful when the collector is OK; a non-OK storage
	// state is handled by coverageError (exit 3) upstream.
	if st != nil && st.State == StateOK {
		for _, p := range st.Pools {
			sev, msg := statusSeverity("Storage pool "+p.ID, p.Status, poolStatusSeverity)
			add("pool_status:"+p.ID, sev, msg, p.Status)
		}
		if len(st.Pools) == 0 {
			add("pools_missing", SevWarning, "No storage pools reported (legacy volume layout?)", "0")
		}
		for _, v := range st.Volumes {
			sev, msg := statusSeverity("Volume "+volLabel(v), v.Status, poolStatusSeverity)
			add("volume_status:"+v.ID, sev, msg, v.Status)
			if !v.CapacityKnown {
				add("volume_usage:"+v.ID, SevWarning,
					fmt.Sprintf("Volume %s capacity data is unavailable or inconsistent", volLabel(v)), "Unknown")
			} else {
				usev, umsg := volumeUsageSeverity(cfg, v)
				add("volume_usage:"+v.ID, usev, umsg, fmt.Sprintf("%d%%", pctInt(v.UsedPct)))
			}
		}
		for _, d := range st.Disks {
			sev, msg := statusSeverity(driveLabel(d.Name), d.Status, driveStatusSeverity)
			add("drive_health:"+d.Name, sev, msg, d.Status)
		}
		if len(st.Disks) == 0 {
			add("drive_data_missing", SevWarning, "No physical drives reported (Virtual DSM or unusual configuration)", "0")
		}
	}

	// ABB checks. Unavailable/error states are handled by coverageError upstream.
	if abb != nil {
		switch abb.State {
		case StateNotInstalled:
			add("abb_installed", SevOK, "Active Backup for Business is not installed", "not_installed")
		case StatePartial:
			add("abb_partial", SevWarning, "Active Backup coverage is partial: "+abb.StateReason, "partial")
		}
		if abb.State == StateOK || abb.State == StatePartial {
			if abb.Failed > 0 {
				add("abb_failed", SevWarning,
					fmt.Sprintf("%d Active Backup task(s) failed: %s", abb.Failed, abbNames(abb, func(t *ABBTask) bool { return t.Monitored && t.Failed })),
					strconv.Itoa(abb.Failed))
			}
			if abb.Overdue > 0 {
				add("abb_overdue", SevWarning, overdueMessage(abb), strconv.Itoa(abb.Overdue))
			}
			if abb.Cancelled > 0 {
				add("abb_cancelled", SevWarning,
					fmt.Sprintf("%d Active Backup task(s) last ended cancelled: %s", abb.Cancelled, abbNames(abb, func(t *ABBTask) bool { return t.Monitored && t.Cancelled })),
					strconv.Itoa(abb.Cancelled))
			}
			if abb.Unknown > 0 {
				add("abb_unknown", SevWarning,
					fmt.Sprintf("%d Active Backup task(s) have an indeterminate status (run --debug and update the classifier): %s", abb.Unknown, abbNames(abb, func(t *ABBTask) bool { return t.Monitored && t.Unknown })),
					strconv.Itoa(abb.Unknown))
			}
		}
		for _, sel := range abb.UnmatchedSelectors {
			add("selector_unmatched", SevWarning, fmt.Sprintf("Task selector %q matched no task", sel), sel)
		}
	}

	return checks
}

func overallSeverity(checks []CheckResult) Severity {
	sev := SevOK
	for _, c := range checks {
		if c.Severity > sev {
			sev = c.Severity
		}
	}
	return sev
}

// statusSeverity maps a status word via table, returning a human message.
func statusSeverity(label, status string, table map[string]Severity) (Severity, string) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return SevWarning, label + " has no reported status"
	}
	if sev, ok := table[status]; ok {
		switch sev {
		case SevOK:
			return SevOK, label + " is healthy"
		case SevWarning:
			return SevWarning, fmt.Sprintf("%s is in a warning state (%s)", label, status)
		default:
			return SevCritical, fmt.Sprintf("%s is in a critical state (%s)", label, status)
		}
	}
	return SevWarning, fmt.Sprintf("%s has an unrecognized status %q", label, status)
}

func volumeUsageSeverity(cfg *Config, v Volume) (Severity, string) {
	pct := pctInt(v.UsedPct)
	switch {
	case v.UsedPct >= float64(cfg.VolCritPct):
		return SevCritical, fmt.Sprintf("Volume %s at %d%% (critical >= %d%%)", volLabel(v), pct, cfg.VolCritPct)
	case v.UsedPct >= float64(cfg.VolWarnPct):
		return SevWarning, fmt.Sprintf("Volume %s at %d%% (warning >= %d%%)", volLabel(v), pct, cfg.VolWarnPct)
	default:
		return SevOK, fmt.Sprintf("Volume %s at %d%%", volLabel(v), pct)
	}
}

func overdueMessage(abb *ABBInfo) string {
	var parts []string
	for i := range abb.Tasks {
		t := &abb.Tasks[i]
		if !(t.Monitored && t.Overdue) {
			continue
		}
		if t.LastSuccess != nil {
			parts = append(parts, fmt.Sprintf("%s (last success %s)", t.Name, humanTime(*t.LastSuccess)))
		} else {
			parts = append(parts, fmt.Sprintf("%s (never)", t.Name))
		}
	}
	return fmt.Sprintf("%d Active Backup task(s) overdue: %s", abb.Overdue, strings.Join(parts, "; "))
}

func abbNames(abb *ABBInfo, pred func(*ABBTask) bool) string {
	var names []string
	for i := range abb.Tasks {
		t := &abb.Tasks[i]
		if pred(t) {
			names = append(names, t.Name)
		}
	}
	return strings.Join(names, ", ")
}

func volLabel(v Volume) string {
	if v.Name != "" {
		return v.Name
	}
	return v.ID
}

// driveLabel prefixes a disk name with "Drive " for readable check messages,
// unless DSM already names the disk "Drive N" — which would otherwise read as
// the doubled "Drive Drive 4". Matching is case-insensitive and only skips the
// prefix when "drive" is a standalone leading word, so a name like "Driver 1"
// still gets its prefix.
func driveLabel(name string) string {
	if name == "" {
		return "Drive"
	}
	if lower := strings.ToLower(name); strings.HasPrefix(lower, "drive") {
		if rest := lower[len("drive"):]; rest == "" || rest[0] < 'a' || rest[0] > 'z' {
			return name
		}
	}
	return "Drive " + name
}

// pctInt rounds a percentage to the nearest whole number, clamped to [0,100].
func pctInt(p float64) int {
	n := int(math.Round(p))
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// buildSummary joins non-OK check messages, or produces a healthy summary using
// the monitored ABB count.
func buildSummary(checks []CheckResult, st *StorageInfo, abb *ABBInfo) string {
	var msgs []string
	for _, c := range checks {
		if c.Severity != SevOK {
			msgs = append(msgs, c.Message)
		}
	}
	if len(msgs) > 0 {
		return truncateRunes(sanitizeInline(strings.Join(msgs, "; ")), 500)
	}

	parts := []string{"NAS healthy"}
	nVol, nDrive := 0, 0
	if st != nil {
		nVol = len(st.Volumes)
		nDrive = len(st.Disks)
	}
	parts = append(parts, fmt.Sprintf("%d volume(s), %d drive(s)", nVol, nDrive))
	if abb != nil {
		switch abb.State {
		case StateNotInstalled:
			parts = append(parts, "Active Backup not installed")
		case StateOK, StatePartial:
			seg := fmt.Sprintf("%d monitored ABB task(s) OK", abb.Monitored)
			if abb.Disabled > 0 {
				seg += fmt.Sprintf("; %d disabled", abb.Disabled)
			}
			if abb.Excluded > 0 {
				seg += fmt.Sprintf("; %d excluded", abb.Excluded)
			}
			parts = append(parts, seg)
		}
	}
	return truncateRunes(sanitizeInline(strings.Join(parts, "; ")), 500)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

func stateOfSystem(sys *SystemInfo) CollectorState {
	if sys == nil {
		return StateError
	}
	return sys.State
}
