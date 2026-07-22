package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const schemaVersion = 1

// Exit codes drive RMM alert conditions.
const (
	ExitOK       = 0
	ExitWarning  = 1
	ExitCritical = 2
	ExitError    = 3
)

// ConfigEcho is the secret-free echo of configuration in the JSON report. It has
// no password field at all.
type ConfigEcho struct {
	Host              string `json:"host"`
	Username          string `json:"username"`
	VolWarnPct        int    `json:"vol_warn_pct"`
	VolCritPct        int    `json:"vol_crit_pct"`
	BackupMaxAge      string `json:"backup_max_age"`
	HyperBackupMaxAge string `json:"hyperbackup_max_age"`
	SaaSBackupMaxAge  string `json:"saas_backup_max_age"`
	Timeout           string `json:"timeout"`
	TLSVerify         string `json:"tls_verify"` // default | insecure | pinned | ca-file
	Format            string `json:"format"`
	Debug             bool   `json:"debug"`
}

// Report is the full JSON document.
type Report struct {
	SchemaVersion    int                   `json:"schema_version"`
	CollectorVersion string                `json:"collector_version"`
	CollectedAt      time.Time             `json:"collected_at"`
	DurationMs       int64                 `json:"duration_ms"`
	Host             string                `json:"host"`
	Config           ConfigEcho            `json:"config"`
	Status           string                `json:"status"`
	ExitCode         int                   `json:"exit_code"`
	Summary          string                `json:"summary"`
	Error            string                `json:"error,omitempty"`
	System           *SystemInfo           `json:"system,omitempty"`
	Storage          *StorageInfo          `json:"storage,omitempty"`
	ABB              *ABBInfo              `json:"abb,omitempty"`
	Hyper            *HyperInfo            `json:"hyperbackup,omitempty"`
	M365             *SaaSInfo             `json:"m365,omitempty"`
	GWS              *SaaSInfo             `json:"google_workspace,omitempty"`
	Checks           []CheckResult         `json:"checks"`
	Raw              map[string]RawPayload `json:"raw,omitempty"`
}

func newConfigEcho(cfg *Config) ConfigEcho {
	verify := "default"
	switch {
	case cfg.TLSPin != "":
		verify = "pinned"
	case cfg.CAFile != "":
		verify = "ca-file"
	case cfg.InsecureTLS:
		verify = "insecure"
	}
	return ConfigEcho{
		Host:              cfg.Host,
		Username:          cfg.Username,
		VolWarnPct:        cfg.VolWarnPct,
		VolCritPct:        cfg.VolCritPct,
		BackupMaxAge:      cfg.BackupMaxAge.String(),
		HyperBackupMaxAge: cfg.HyperBackupMaxAge.String(),
		SaaSBackupMaxAge:  cfg.SaaSBackupMaxAge.String(),
		Timeout:           cfg.Timeout.String(),
		TLSVerify:         verify,
		Format:            cfg.Format,
		Debug:             cfg.Debug,
	}
}

func severityStatus(s Severity) string { return s.String() }

func severityExitCode(s Severity) int {
	switch s {
	case SevWarning:
		return ExitWarning
	case SevCritical:
		return ExitCritical
	default:
		return ExitOK
	}
}

// render writes the report honoring --format exactly, on both success and error.
func render(w io.Writer, format string, r *Report) error {
	switch format {
	case "kv":
		_, err := io.WriteString(w, renderKV(r))
		return err
	case "json":
		return writeJSON(w, r)
	default: // both
		if _, err := io.WriteString(w, renderKV(r)); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "---\n"); err != nil {
			return err
		}
		return writeJSON(w, r)
	}
}

func writeJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// renderKV emits the stable, ordered KEY=VALUE block.
func renderKV(r *Report) string {
	sys, st, abb, hb, m365, gws, checks := r.System, r.Storage, r.ABB, r.Hyper, r.M365, r.GWS, r.Checks
	var b strings.Builder
	write := func(k, v string) {
		fmt.Fprintf(&b, "%s=%s\n", k, sanitizeInline(v))
	}

	write("STATUS", r.Status)
	if r.Status == "ERROR" {
		write("ERROR", r.Error)
	}
	write("NAS", kvNAS(sys))
	write("DSM", kvDSM(sys))
	write("HOSTNAME", kvHostname(sys))
	write("UPTIME", kvUptime(sys))
	write("SYSTEM_HEALTH", kvSystemHealth(checks, st))
	write("STORAGE_POOL", kvStoragePool(st))
	write("VOLUME_USAGE", kvVolumeUsage(st))
	write("DRIVES", kvDrives(st))
	write("DRIVE_WARNINGS", kvDriveWarnings(checks, st))

	write("ABB_STATE", kvABBState(abb))
	tasks, monitored, disabled, excluded, failed, overdue := "Unknown", "Unknown", "Unknown", "Unknown", "Unknown", "Unknown"
	if abb != nil && abb.State != StateUnavailable && abb.State != StateError {
		tasks = strconv.Itoa(abb.Total)
		monitored = strconv.Itoa(abb.Monitored)
		disabled = strconv.Itoa(abb.Disabled)
		excluded = strconv.Itoa(abb.Excluded)
		failed = strconv.Itoa(abb.Failed)
		overdue = strconv.Itoa(abb.Overdue)
	}
	write("ABB_TASKS", tasks)
	write("ABB_MONITORED", monitored)
	write("ABB_DISABLED", disabled)
	write("ABB_EXCLUDED", excluded)
	write("ABB_FAILED", failed)
	write("ABB_OVERDUE", overdue)
	write("LAST_SUCCESS", kvLastSuccess(abb))

	write("HB_STATE", kvHyperState(hb))
	hbTasks, hbMon, hbDis, hbExc, hbRun, hbFail, hbOver := "Unknown", "Unknown", "Unknown", "Unknown", "Unknown", "Unknown", "Unknown"
	if hb != nil && hb.State != StateUnavailable && hb.State != StateError {
		hbTasks = strconv.Itoa(hb.Total)
		hbMon = strconv.Itoa(hb.Monitored)
		hbDis = strconv.Itoa(hb.Disabled)
		hbExc = strconv.Itoa(hb.Excluded)
		hbRun = strconv.Itoa(hb.Running)
		hbFail = strconv.Itoa(hb.brokenCount())
		hbOver = strconv.Itoa(hb.Overdue)
	}
	write("HB_TASKS", hbTasks)
	write("HB_MONITORED", hbMon)
	write("HB_DISABLED", hbDis)
	write("HB_EXCLUDED", hbExc)
	write("HB_RUNNING", hbRun)
	write("HB_FAILED", hbFail)
	write("HB_OVERDUE", hbOver)
	write("HB_LAST_SUCCESS", kvHyperLastSuccess(hb))

	writeSaaS(write, m365, "M365")
	writeSaaS(write, gws, "GWS")

	write("SUMMARY", r.Summary)
	write("HOST", r.Host)
	write("COLLECTED_AT", r.CollectedAt.UTC().Format(time.RFC3339))
	write("COLLECTOR_VERSION", r.CollectorVersion)
	return b.String()
}

func kvNAS(sys *SystemInfo) string {
	if sys != nil && sys.Model != "" {
		return sys.Model
	}
	return "Unknown"
}

func kvDSM(sys *SystemInfo) string {
	if sys != nil && sys.VersionShort != "" {
		return sys.VersionShort
	}
	return "Unknown"
}

func kvHostname(sys *SystemInfo) string {
	if sys != nil && sys.Hostname != "" {
		return sys.Hostname
	}
	return "Unknown"
}

func kvUptime(sys *SystemInfo) string {
	if sys != nil && sys.UptimeSec > 0 {
		return humanUptime(sys.UptimeSec)
	}
	return "Unknown"
}

func kvSystemHealth(checks []CheckResult, st *StorageInfo) string {
	if st == nil || st.State != StateOK {
		return "Unknown"
	}
	sev := SevOK
	for _, c := range checks {
		if isStorageHealthCheck(c.Name) && c.Severity > sev {
			sev = c.Severity
		}
	}
	switch sev {
	case SevWarning:
		return "Warning"
	case SevCritical:
		return "Critical"
	default:
		return "Normal"
	}
}

func isStorageHealthCheck(name string) bool {
	return strings.HasPrefix(name, "pool_status:") ||
		strings.HasPrefix(name, "volume_status:") ||
		strings.HasPrefix(name, "volume_usage:") ||
		strings.HasPrefix(name, "drive_health:")
}

func kvStoragePool(st *StorageInfo) string {
	if st == nil || st.State != StateOK || len(st.Pools) == 0 {
		return "Unknown"
	}
	worst := SevOK
	worstStatus := ""
	for _, p := range st.Pools {
		sev, _ := statusSeverity("", p.Status, poolStatusSeverity)
		if sev >= worst {
			worst = sev
			worstStatus = p.Status
		}
	}
	if worst == SevOK {
		return "Healthy"
	}
	return capitalizeASCII(worstStatus)
}

func kvVolumeUsage(st *StorageInfo) string {
	if st == nil || st.State != StateOK {
		return "Unknown"
	}
	maxPct := -1.0
	for _, v := range st.Volumes {
		if v.CapacityKnown && v.UsedPct > maxPct {
			maxPct = v.UsedPct
		}
	}
	if maxPct < 0 {
		return "Unknown" // no volume has trustworthy capacity data
	}
	return fmt.Sprintf("%d%%", pctInt(maxPct))
}

func kvDrives(st *StorageInfo) string {
	if st == nil || st.State != StateOK {
		return "Unknown"
	}
	return strconv.Itoa(len(st.Disks))
}

func kvDriveWarnings(checks []CheckResult, st *StorageInfo) string {
	if st == nil || st.State != StateOK || len(st.Disks) == 0 {
		return "Unknown"
	}
	n := 0
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "drive_health:") && c.Severity != SevOK {
			n++
		}
	}
	return strconv.Itoa(n)
}

func kvABBState(abb *ABBInfo) string {
	if abb == nil {
		return "UNKNOWN"
	}
	switch abb.State {
	case StateOK:
		return "OK"
	case StatePartial:
		return "PARTIAL"
	case StateNotInstalled:
		return "NOT_INSTALLED"
	case StateUnavailable:
		return "UNAVAILABLE"
	case StateError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

func kvLastSuccess(abb *ABBInfo) string {
	if abb == nil {
		return "Unknown"
	}
	switch abb.State {
	case StateNotInstalled:
		return "N/A"
	case StateUnavailable, StateError:
		return "Unknown"
	}
	switch abb.LastSuccessState {
	case LSNone:
		return "N/A"
	case LSKnown:
		if abb.LastSuccess != nil {
			return abb.LastSuccess.UTC().Format(time.RFC3339)
		}
		return "Unknown"
	case LSNever:
		return "never"
	default:
		return "Unknown"
	}
}

func kvHyperState(hb *HyperInfo) string {
	if hb == nil {
		return "UNKNOWN"
	}
	switch hb.State {
	case StateOK:
		return "OK"
	case StatePartial:
		return "PARTIAL"
	case StateNotInstalled:
		return "NOT_INSTALLED"
	case StateUnavailable:
		return "UNAVAILABLE"
	case StateError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

func kvHyperLastSuccess(hb *HyperInfo) string {
	if hb == nil {
		return "Unknown"
	}
	switch hb.State {
	case StateNotInstalled:
		return "N/A"
	case StateUnavailable, StateError:
		return "Unknown"
	}
	switch hb.LastSuccessState {
	case LSNone:
		return "N/A"
	case LSKnown:
		if hb.LastSuccess != nil {
			return hb.LastSuccess.UTC().Format(time.RFC3339)
		}
		return "Unknown"
	case LSNever:
		return "never"
	default:
		return "Unknown"
	}
}

// writeSaaS emits the fixed KV block for one Active Backup SaaS collector, keyed by
// the flavor prefix ("M365" / "GWS"). Every key is always emitted (Unknown sentinels
// when the collector is unavailable) so the KV block stays stable.
func writeSaaS(write func(k, v string), info *SaaSInfo, prefix string) {
	write(prefix+"_STATE", kvSaaSState(info))
	tasks, mon, dis, exc, run, fail, over := "Unknown", "Unknown", "Unknown", "Unknown", "Unknown", "Unknown", "Unknown"
	if info != nil && info.State != StateUnavailable && info.State != StateError {
		tasks = strconv.Itoa(info.Total)
		mon = strconv.Itoa(info.Monitored)
		dis = strconv.Itoa(info.Disabled)
		exc = strconv.Itoa(info.Excluded)
		run = strconv.Itoa(info.Running)
		fail = strconv.Itoa(info.brokenCount())
		over = strconv.Itoa(info.Overdue)
	}
	write(prefix+"_TASKS", tasks)
	write(prefix+"_MONITORED", mon)
	write(prefix+"_DISABLED", dis)
	write(prefix+"_EXCLUDED", exc)
	write(prefix+"_RUNNING", run)
	write(prefix+"_FAILED", fail)
	write(prefix+"_OVERDUE", over)
	write(prefix+"_LAST_SUCCESS", kvSaaSLastSuccess(info))
}

func kvSaaSState(info *SaaSInfo) string {
	if info == nil {
		return "UNKNOWN"
	}
	switch info.State {
	case StateOK:
		return "OK"
	case StatePartial:
		return "PARTIAL"
	case StateNotInstalled:
		return "NOT_INSTALLED"
	case StateUnavailable:
		return "UNAVAILABLE"
	case StateError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

func kvSaaSLastSuccess(info *SaaSInfo) string {
	if info == nil {
		return "Unknown"
	}
	switch info.State {
	case StateNotInstalled:
		return "N/A"
	case StateUnavailable, StateError:
		return "Unknown"
	}
	switch info.LastSuccessState {
	case LSNone:
		return "N/A"
	case LSKnown:
		if info.LastSuccess != nil {
			return info.LastSuccess.UTC().Format(time.RFC3339)
		}
		return "Unknown"
	case LSNever:
		return "never"
	default:
		return "Unknown"
	}
}

func capitalizeASCII(s string) string {
	if s == "" {
		return "Unknown"
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// sanitizeInline makes a value safe for a single KV line and for logs: control
// characters (including CR/LF) are removed, printable UTF-8 (e.g. unicode task
// names) is preserved.
func sanitizeInline(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r == utf8.RuneError:
			// drop invalid/replacement runes
		case unicode.IsControl(r):
			// drop other control characters
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
