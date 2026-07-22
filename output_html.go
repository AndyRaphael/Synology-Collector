package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// The HTML report is a human-readable companion to the KV/JSON output: a single
// self-contained, styled page written to a file via --html-file. It never goes
// to stdout (which stays machine-parseable) and is rendered from the same
// *Report the other formats use, so it stays in sync as new modules are added.
//
// A precomputed view model (htmlView) does all the formatting and severity
// mapping in Go, leaving the template to iterate and print. That keeps escaping
// trivially correct (html/template auto-escapes every NAS-provided string) and
// makes the renderer easy to unit-test and to extend section by section.

// humanBytes formats a byte count with decimal (base-1000) units, matching how
// Synology labels drive/volume capacity. Non-positive values render as an em dash.
func humanBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	const unit = 1000.0
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f, i := float64(n), 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// humanUptime turns a second count into a compact "3d 4h 12m" form.
func humanUptime(sec int64) string {
	if sec <= 0 {
		return "—"
	}
	d, h, m := sec/86400, (sec%86400)/3600, (sec%3600)/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// reasonSuffix renders a collector state reason as a sentence tail.
func reasonSuffix(reason string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		return ": " + reason + "."
	}
	return "."
}

// humanTime formats a timestamp for human-facing views (the HTML report and the
// SUMMARY line): "2026-07-21 18:31 UTC". The machine-readable KV keys
// (LAST_SUCCESS, COLLECTED_AT) deliberately keep full RFC3339 instead.
func humanTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04 MST")
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return humanTime(*t)
}

// statusClass maps the overall STATUS word to a CSS class.
func statusClass(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "OK":
		return "ok"
	case "WARNING":
		return "warn"
	case "CRITICAL":
		return "crit"
	default:
		return "err"
	}
}

func sevClass(s Severity) string {
	switch s {
	case SevWarning:
		return "warn"
	case SevCritical:
		return "crit"
	default:
		return "ok"
	}
}

func usageClass(pct float64, warn, crit int) string {
	switch {
	case pct >= float64(crit):
		return "crit"
	case pct >= float64(warn):
		return "warn"
	default:
		return "ok"
	}
}

// badgeLabel makes a raw DSM status word presentable: "sys_partition_normal"
// becomes "Sys partition normal".
func badgeLabel(status string) string {
	s := strings.TrimSpace(status)
	if s == "" {
		return "Unknown"
	}
	return capitalizeASCII(strings.ReplaceAll(s, "_", " "))
}

func poolBadge(status string) htmlBadge {
	sev, _ := statusSeverity("", status, poolStatusSeverity)
	return htmlBadge{Label: badgeLabel(status), Class: sevClass(sev)}
}

func driveBadge(status string) htmlBadge {
	sev, _ := statusSeverity("", status, driveStatusSeverity)
	return htmlBadge{Label: badgeLabel(status), Class: sevClass(sev)}
}

// taskBadge summarizes a task's monitored outcome into a single pill.
func taskBadge(t *ABBTask) htmlBadge {
	switch {
	case !t.Enabled:
		return htmlBadge{"Disabled", "muted"}
	case t.Excluded:
		return htmlBadge{"Excluded", "muted"}
	case t.Failed:
		return htmlBadge{"Failed", "warn"}
	case t.Overdue:
		return htmlBadge{"Overdue", "warn"}
	case t.Cancelled:
		return htmlBadge{"Cancelled", "warn"}
	case t.Unknown:
		return htmlBadge{"Indeterminate", "warn"}
	default:
		return htmlBadge{"OK", "ok"}
	}
}

// --- view model -------------------------------------------------------------

type htmlBadge struct{ Label, Class string }

// htmlBar is a usage bar. Width is a trusted CSS length (e.g. "68%") built from
// an internal 0..100 integer, so it is safe to emit into a style attribute.
type htmlBar struct {
	Class string
	Width template.CSS
}

func newBar(pct float64, class string) htmlBar {
	return htmlBar{Class: class, Width: template.CSS(strconv.Itoa(pctInt(pct)) + "%")}
}

type htmlKV struct{ Label, Value string }

type htmlStat struct {
	Label, Value, Class string // Class tints the number (warn/crit) when non-empty
}

type htmlPool struct {
	ID, Type, Usage string
	Bar             htmlBar
	Status          htmlBadge
}

type htmlVolume struct {
	Name, FS, UsageText string
	Bar                 *htmlBar // nil when capacity is unknown
	Status              htmlBadge
}

type htmlDisk struct {
	Name, Model, Serial, Size, Temp string
	Status                          htmlBadge
}

type htmlTask struct {
	Name, Source, MaxAge, LastSuccess string
	Status                            htmlBadge
}

type htmlCheck struct {
	Name, Message string
	Sev           htmlBadge
}

type htmlView struct {
	Title, Status, StatusClass, Summary, ErrorMsg string

	System     []htmlKV
	SystemNote string

	HasStorage  bool
	StorageNote string
	Pools       []htmlPool
	Volumes     []htmlVolume
	Disks       []htmlDisk

	HasABB         bool
	ABBNote        string
	ABBStats       []htmlStat
	ABBLastSuccess string // formatted, or a sentinel (never / N/A / Unknown)
	Tasks          []htmlTask

	Checks []htmlCheck

	CollectedAt, Version, Host string
	DurationMs                 int64
}

func countStat(label string, n int, warnWhenPositive bool) htmlStat {
	s := htmlStat{Label: label, Value: strconv.Itoa(n)}
	if warnWhenPositive && n > 0 {
		s.Class = "warn"
	}
	return s
}

// buildHTMLView flattens a *Report into display-ready values. It is nil-safe for
// every optional section so an error report (no system/storage/abb) still renders.
func buildHTMLView(r *Report) htmlView {
	v := htmlView{
		Status:      r.Status,
		StatusClass: statusClass(r.Status),
		Summary:     r.Summary,
		ErrorMsg:    r.Error,
		CollectedAt: humanTime(r.CollectedAt),
		Version:     r.CollectorVersion,
		Host:        r.Host,
		DurationMs:  r.DurationMs,
	}

	v.Title = "Synology NAS"
	if r.System != nil {
		if name := firstNonEmpty(r.System.Hostname, r.System.Model); name != "" {
			v.Title = name
		}
	}

	// System.
	switch {
	case r.System != nil && r.System.State == StateOK:
		v.System = []htmlKV{
			{"Model", orDash(r.System.Model)},
			{"DSM version", orDash(firstNonEmpty(r.System.VersionFull, r.System.VersionShort))},
			{"Hostname", orDash(r.System.Hostname)},
			{"Serial", orDash(r.System.Serial)},
			{"Uptime", humanUptime(r.System.UptimeSec)},
		}
	case r.System != nil:
		v.SystemNote = "System information unavailable" + reasonSuffix(r.System.StateReason)
	}

	// Storage.
	if r.Storage != nil {
		v.HasStorage = true
		if r.Storage.State != StateOK {
			v.StorageNote = "Storage information unavailable" + reasonSuffix(r.Storage.StateReason)
		} else {
			for _, p := range r.Storage.Pools {
				v.Pools = append(v.Pools, htmlPool{
					ID:     p.ID,
					Type:   orDash(p.DeviceType),
					Usage:  humanBytes(p.SizeUsed) + " of " + humanBytes(p.SizeTotal),
					Bar:    newBar(p.UsedPct, "accent"),
					Status: poolBadge(p.Status),
				})
			}
			for _, vol := range r.Storage.Volumes {
				row := htmlVolume{Name: volLabel(vol), FS: orDash(vol.FsType), Status: poolBadge(vol.Status)}
				if vol.CapacityKnown {
					row.UsageText = fmt.Sprintf("%d%% · %s of %s", pctInt(vol.UsedPct), humanBytes(vol.SizeUsed), humanBytes(vol.SizeTotal))
					bar := newBar(vol.UsedPct, usageClass(vol.UsedPct, r.Config.VolWarnPct, r.Config.VolCritPct))
					row.Bar = &bar
				} else {
					row.UsageText = "capacity unknown"
				}
				v.Volumes = append(v.Volumes, row)
			}
			for _, d := range r.Storage.Disks {
				temp := "—"
				if d.TempC > 0 {
					temp = strconv.FormatInt(d.TempC, 10) + "°C"
				}
				v.Disks = append(v.Disks, htmlDisk{
					Name:   d.Name,
					Model:  orDash(d.Model),
					Serial: orDash(d.Serial),
					Size:   humanBytes(d.SizeTotal),
					Temp:   temp,
					Status: driveBadge(d.Status),
				})
			}
		}
	}

	// Active Backup for Business.
	if r.ABB != nil {
		v.HasABB = true
		switch r.ABB.State {
		case StateNotInstalled:
			v.ABBNote = "Active Backup for Business is not installed."
		case StateUnavailable, StateError:
			v.ABBNote = "Active Backup information unavailable" + reasonSuffix(r.ABB.StateReason)
		default: // ok / partial
			if r.ABB.State == StatePartial {
				v.ABBNote = "Coverage is partial" + reasonSuffix(r.ABB.StateReason)
			}
			v.ABBStats = []htmlStat{
				countStat("Tasks", r.ABB.Total, false),
				countStat("Monitored", r.ABB.Monitored, false),
				countStat("Failed", r.ABB.Failed, true),
				countStat("Overdue", r.ABB.Overdue, true),
				countStat("Cancelled", r.ABB.Cancelled, true),
				countStat("Disabled", r.ABB.Disabled, false),
				countStat("Excluded", r.ABB.Excluded, false),
			}
			// Last success is a timestamp, not a count — render it on its own line
			// below the tiles rather than as an oversized number card.
			v.ABBLastSuccess = kvLastSuccess(r.ABB)
			if r.ABB.LastSuccessState == LSKnown && r.ABB.LastSuccess != nil {
				v.ABBLastSuccess = humanTime(*r.ABB.LastSuccess)
			}
			for i := range r.ABB.Tasks {
				t := &r.ABB.Tasks[i]
				v.Tasks = append(v.Tasks, htmlTask{
					Name:        t.Name,
					Source:      orDash(t.SourceType),
					MaxAge:      orDash(t.EffectiveMaxAge),
					LastSuccess: fmtTimePtr(t.LastSuccess),
					Status:      taskBadge(t),
				})
			}
		}
	}

	// Checks.
	for _, c := range r.Checks {
		v.Checks = append(v.Checks, htmlCheck{
			Name:    c.Name,
			Message: c.Message,
			Sev:     htmlBadge{Label: c.Severity.String(), Class: sevClass(c.Severity)},
		})
	}

	return v
}

var htmlReportTemplate = template.Must(template.New("report").Parse(htmlReportSrc))

// renderHTML writes a self-contained HTML report for r to w.
func renderHTML(w io.Writer, r *Report) error {
	return htmlReportTemplate.Execute(w, buildHTMLView(r))
}

// writeRenderedFile renders to a new file at path (created/truncated), closing it
// even on a render error and surfacing a flush error on close.
func writeRenderedFile(path string, render func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := render(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeHTMLReport writes the self-contained styled page (for a browser or share).
func writeHTMLReport(path string, r *Report) error {
	return writeRenderedFile(path, func(w io.Writer) error { return renderHTML(w, r) })
}

// writeHTMLEmbed writes the inline-styled fragment (for a NinjaOne WYSIWYG field,
// or any rich-text field that strips <style>/<script>).
func writeHTMLEmbed(path string, r *Report) error {
	return writeRenderedFile(path, func(w io.Writer) error { return renderHTMLEmbed(w, r) })
}

// --- inline-styled fragment for WYSIWYG / rich-text fields -------------------

// WYSIWYG editors (NinjaOne's included) strip <style> blocks and <script>, so
// this variant carries every rule as an inline style attribute and lays out with
// tables. Colors are light-theme only — these fields render on a light ground.

func classFG(class string) string {
	switch class {
	case "ok":
		return "#1a7f37"
	case "warn":
		return "#9a6700"
	case "crit", "err":
		return "#cf222e"
	default:
		return "#57606a"
	}
}

func classBG(class string) string {
	switch class {
	case "ok":
		return "#dafbe1"
	case "warn":
		return "#fff8c5"
	case "crit", "err":
		return "#ffebe9"
	default:
		return "#eaeef2"
	}
}

// embedFuncs return trusted (template.CSS) inline-style snippets built from a
// fixed palette, so the CSS escaper passes them through verbatim.
var embedFuncs = template.FuncMap{
	"badgeCSS": func(c string) template.CSS {
		return template.CSS("display:inline-block;padding:1px 8px;border-radius:10px;font-size:12px;font-weight:600;color:" + classFG(c) + ";background:" + classBG(c))
	},
	"bannerCSS": func(c string) template.CSS {
		return template.CSS("padding:10px 14px;border-radius:8px;border:1px solid " + classFG(c) + ";background:" + classBG(c))
	},
	"wordCSS": func(c string) template.CSS {
		return template.CSS("font-size:18px;font-weight:700;color:" + classFG(c))
	},
	"numCSS": func(c string) template.CSS {
		if c == "" {
			return template.CSS("font-weight:700;font-size:15px")
		}
		return template.CSS("font-weight:700;font-size:15px;color:" + classFG(c))
	},
	"thCSS": func() template.CSS {
		return template.CSS("padding:5px 8px;text-align:left;color:#57606a;font-size:11px;font-weight:600;border-bottom:1px solid #d0d7de")
	},
	"tdCSS": func() template.CSS {
		return template.CSS("padding:5px 8px;border-bottom:1px solid #eaeef2;vertical-align:top")
	},
	"mutedCSS": func() template.CSS {
		return template.CSS("padding:5px 8px;border-bottom:1px solid #eaeef2;color:#57606a;vertical-align:top")
	},
}

var htmlEmbedTemplate = template.Must(template.New("embed").Funcs(embedFuncs).Parse(htmlEmbedSrc))

// renderHTMLEmbed writes the inline-styled HTML fragment for r to w, folded to
// pure ASCII so it survives an embedding field that assumes a non-UTF-8 charset.
func renderHTMLEmbed(w io.Writer, r *Report) error {
	var buf bytes.Buffer
	if err := htmlEmbedTemplate.Execute(&buf, buildHTMLView(r)); err != nil {
		return err
	}
	_, err := io.WriteString(w, asciiFold(buf.String()))
	return err
}

// asciiFold replaces every non-ASCII rune with an HTML numeric character
// reference. The fragment is dropped into rich-text fields that carry no
// <meta charset> and are frequently read as Windows-1252 (PowerShell 5.1's
// default file encoding), which mangles UTF-8 bytes into mojibake (·→Â·,
// °→Â°, —→â€"). Numeric entities are pure ASCII and render identically under
// any charset — and this also covers non-ASCII task names.
func asciiFold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 128 {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "&#%d;", r)
		}
	}
	return b.String()
}

const htmlReportSrc = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — Synology Collector</title>
<style>
:root{
--bg:#f6f8fa;--card:#fff;--text:#1f2328;--muted:#656d76;--border:#d0d7de;
--ok:#1a7f37;--ok-bg:#dafbe1;--warn:#9a6700;--warn-bg:#fff8c5;
--crit:#cf222e;--crit-bg:#ffebe9;--muted-bg:#eaeef2;--accent:#0969da;
}
@media (prefers-color-scheme:dark){:root{
--bg:#0d1117;--card:#161b22;--text:#e6edf3;--muted:#8b949e;--border:#30363d;
--ok:#3fb950;--ok-bg:#12261e;--warn:#d29922;--warn-bg:#272115;
--crit:#f85149;--crit-bg:#25171a;--muted-bg:#21262d;--accent:#4493f8;
}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
.wrap{max-width:900px;margin:0 auto;padding:24px 16px 48px}
.banner{display:flex;align-items:center;gap:20px;padding:20px 24px;margin-bottom:20px;
border:1px solid var(--border);border-radius:12px;background:var(--card)}
.banner .word{font-size:28px;font-weight:800;letter-spacing:.5px}
.banner.ok{background:var(--ok-bg);border-color:var(--ok)}
.banner.warn{background:var(--warn-bg);border-color:var(--warn)}
.banner.crit,.banner.err{background:var(--crit-bg);border-color:var(--crit)}
.banner.ok .word{color:var(--ok)}
.banner.warn .word{color:var(--warn)}
.banner.crit .word,.banner.err .word{color:var(--crit)}
.banner .title{font-size:18px;font-weight:600}
.banner .summary{color:var(--muted)}
.card{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:18px 20px;margin-bottom:18px}
.card h2{margin:0 0 14px;font-size:16px}
.card h3{margin:18px 0 8px;font-size:12px;text-transform:uppercase;letter-spacing:.4px;color:var(--muted)}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--border);vertical-align:middle}
th{color:var(--muted);font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.3px}
tr:last-child td{border-bottom:none}
.t{color:var(--muted);white-space:nowrap}
.badge{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;font-weight:600}
.badge.ok{color:var(--ok);background:var(--ok-bg)}
.badge.warn{color:var(--warn);background:var(--warn-bg)}
.badge.crit,.badge.err{color:var(--crit);background:var(--crit-bg)}
.badge.muted{color:var(--muted);background:var(--muted-bg)}
.kvgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px 20px}
.kvgrid div{display:flex;flex-direction:column}
.kvgrid span{color:var(--muted);font-size:12px}
.kvgrid b{font-weight:600}
.stats{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px}
@media (max-width:620px){.stats{grid-template-columns:repeat(2,minmax(0,1fr))}}
.laststamp{color:var(--muted);font-size:13px;margin:12px 0 2px}
.laststamp b{color:var(--text);font-weight:600}
.stat{background:var(--bg);border:1px solid var(--border);border-radius:8px;padding:10px 12px}
.stat .n{font-size:20px;font-weight:700}
.stat .n.warn{color:var(--warn)}
.stat .n.crit{color:var(--crit)}
.stat .l{color:var(--muted);font-size:12px}
.bar{height:8px;min-width:90px;background:var(--muted-bg);border-radius:999px;overflow:hidden}
.bar>i{display:block;height:100%;border-radius:999px}
.bar>i.ok{background:var(--ok)}
.bar>i.warn{background:var(--warn)}
.bar>i.crit{background:var(--crit)}
.bar>i.accent{background:var(--accent)}
.note{color:var(--muted);font-style:italic;margin:4px 0}
.errbox{background:var(--crit-bg);color:var(--crit);border:1px solid var(--crit);border-radius:8px;padding:12px 14px;margin-bottom:18px}
footer{color:var(--muted);font-size:12px;text-align:center;margin-top:24px}
</style>
</head>
<body>
<div class="wrap">
<div class="banner {{.StatusClass}}">
<div class="word">{{.Status}}</div>
<div>
<div class="title">{{.Title}}</div>
{{if .Summary}}<div class="summary">{{.Summary}}</div>{{end}}
</div>
</div>

{{if .ErrorMsg}}<div class="errbox">{{.ErrorMsg}}</div>{{end}}

{{if .System}}
<div class="card">
<h2>System</h2>
<div class="kvgrid">{{range .System}}<div><span>{{.Label}}</span><b>{{.Value}}</b></div>{{end}}</div>
</div>
{{else if .SystemNote}}<div class="card"><h2>System</h2><p class="note">{{.SystemNote}}</p></div>{{end}}

{{if .HasStorage}}
<div class="card">
<h2>Storage</h2>
{{if .StorageNote}}<p class="note">{{.StorageNote}}</p>{{end}}
{{if .Pools}}
<h3>Storage pools</h3>
<table><thead><tr><th>Pool</th><th>Type</th><th>Usage</th><th></th><th>Status</th></tr></thead><tbody>
{{range .Pools}}<tr><td>{{.ID}}</td><td>{{.Type}}</td><td class="t">{{.Usage}}</td>
<td><div class="bar"><i class="{{.Bar.Class}}" style="width:{{.Bar.Width}}"></i></div></td>
<td><span class="badge {{.Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</tbody></table>
{{end}}
{{if .Volumes}}
<h3>Volumes</h3>
<table><thead><tr><th>Volume</th><th>FS</th><th>Usage</th><th></th><th>Status</th></tr></thead><tbody>
{{range .Volumes}}<tr><td>{{.Name}}</td><td>{{.FS}}</td><td class="t">{{.UsageText}}</td>
<td>{{if .Bar}}<div class="bar"><i class="{{.Bar.Class}}" style="width:{{.Bar.Width}}"></i></div>{{end}}</td>
<td><span class="badge {{.Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</tbody></table>
{{end}}
{{if .Disks}}
<h3>Drives</h3>
<table><thead><tr><th>Drive</th><th>Model</th><th>Serial</th><th>Size</th><th>Temp</th><th>Status</th></tr></thead><tbody>
{{range .Disks}}<tr><td>{{.Name}}</td><td>{{.Model}}</td><td>{{.Serial}}</td><td>{{.Size}}</td><td>{{.Temp}}</td>
<td><span class="badge {{.Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</tbody></table>
{{end}}
</div>
{{end}}

{{if .HasABB}}
<div class="card">
<h2>Active Backup for Business</h2>
{{if .ABBNote}}<p class="note">{{.ABBNote}}</p>{{end}}
{{if .ABBStats}}<div class="stats">{{range .ABBStats}}<div class="stat"><div class="n {{.Class}}">{{.Value}}</div><div class="l">{{.Label}}</div></div>{{end}}</div>{{end}}
{{if .ABBLastSuccess}}<p class="laststamp">Last monitored success · <b>{{.ABBLastSuccess}}</b></p>{{end}}
{{if .Tasks}}
<table><thead><tr><th>Task</th><th>Source</th><th>Max age</th><th>Last success</th><th>Status</th></tr></thead><tbody>
{{range .Tasks}}<tr><td>{{.Name}}</td><td>{{.Source}}</td><td>{{.MaxAge}}</td><td>{{.LastSuccess}}</td>
<td><span class="badge {{.Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</tbody></table>
{{end}}
</div>
{{end}}

{{if .Checks}}
<div class="card">
<h2>Checks</h2>
<table><thead><tr><th>Severity</th><th>Check</th><th>Detail</th></tr></thead><tbody>
{{range .Checks}}<tr><td><span class="badge {{.Sev.Class}}">{{.Sev.Label}}</span></td><td>{{.Name}}</td><td>{{.Message}}</td></tr>{{end}}
</tbody></table>
</div>
{{end}}

<footer>Collected {{.CollectedAt}} · {{.DurationMs}} ms · collector {{.Version}} · {{.Host}}</footer>
</div>
</body>
</html>
`

const htmlEmbedSrc = `<div style="font-family:Segoe UI,Roboto,Helvetica,Arial,sans-serif;font-size:13px;color:#1f2328;line-height:1.5;">
<div style="{{bannerCSS .StatusClass}};margin-bottom:12px;">
<span style="{{wordCSS .StatusClass}}">{{.Status}}</span>
<span style="font-weight:600;margin-left:8px;">{{.Title}}</span>
{{if .Summary}}<div style="color:#57606a;margin-top:4px;">{{.Summary}}</div>{{end}}
</div>
{{if .ErrorMsg}}<div style="{{bannerCSS "err"}};color:#cf222e;margin-bottom:12px;">{{.ErrorMsg}}</div>{{end}}
{{if .System}}
<div style="font-weight:600;margin:12px 0 6px;">System</div>
<table style="border-collapse:collapse;">
{{range .System}}<tr><td style="padding:3px 16px 3px 0;color:#57606a;">{{.Label}}</td><td style="padding:3px 0;font-weight:600;">{{.Value}}</td></tr>{{end}}
</table>
{{else if .SystemNote}}<div style="color:#57606a;font-style:italic;">{{.SystemNote}}</div>{{end}}
{{if .HasStorage}}
{{if .StorageNote}}<div style="color:#57606a;font-style:italic;margin-top:8px;">{{.StorageNote}}</div>{{end}}
{{if .Pools}}
<div style="font-weight:600;margin:12px 0 6px;">Storage pools</div>
<table style="width:100%;border-collapse:collapse;">
<tr><td style="{{thCSS}}">Pool</td><td style="{{thCSS}}">Type</td><td style="{{thCSS}}">Usage</td><td style="{{thCSS}}">Status</td></tr>
{{range .Pools}}<tr><td style="{{tdCSS}}">{{.ID}}</td><td style="{{tdCSS}}">{{.Type}}</td><td style="{{mutedCSS}}">{{.Usage}}</td><td style="{{tdCSS}}"><span style="{{badgeCSS .Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</table>
{{end}}
{{if .Volumes}}
<div style="font-weight:600;margin:12px 0 6px;">Volumes</div>
<table style="width:100%;border-collapse:collapse;">
<tr><td style="{{thCSS}}">Volume</td><td style="{{thCSS}}">FS</td><td style="{{thCSS}}">Usage</td><td style="{{thCSS}}">Status</td></tr>
{{range .Volumes}}<tr><td style="{{tdCSS}}">{{.Name}}</td><td style="{{tdCSS}}">{{.FS}}</td><td style="{{mutedCSS}}">{{.UsageText}}</td><td style="{{tdCSS}}"><span style="{{badgeCSS .Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</table>
{{end}}
{{if .Disks}}
<div style="font-weight:600;margin:12px 0 6px;">Drives</div>
<table style="width:100%;border-collapse:collapse;">
<tr><td style="{{thCSS}}">Drive</td><td style="{{thCSS}}">Model</td><td style="{{thCSS}}">Serial</td><td style="{{thCSS}}">Size</td><td style="{{thCSS}}">Temp</td><td style="{{thCSS}}">Status</td></tr>
{{range .Disks}}<tr><td style="{{tdCSS}}">{{.Name}}</td><td style="{{tdCSS}}">{{.Model}}</td><td style="{{tdCSS}}">{{.Serial}}</td><td style="{{tdCSS}}">{{.Size}}</td><td style="{{tdCSS}}">{{.Temp}}</td><td style="{{tdCSS}}"><span style="{{badgeCSS .Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</table>
{{end}}
{{end}}
{{if .HasABB}}
<div style="font-weight:600;margin:12px 0 6px;">Active Backup for Business</div>
{{if .ABBNote}}<div style="color:#57606a;font-style:italic;">{{.ABBNote}}</div>{{end}}
{{if .ABBStats}}<div style="color:#57606a;margin:4px 0;">{{range .ABBStats}}{{.Label}} <span style="{{numCSS .Class}}">{{.Value}}</span>&nbsp;&nbsp;&nbsp;{{end}}</div>{{end}}
{{if .ABBLastSuccess}}<div style="color:#57606a;margin:4px 0;">Last monitored success · <b style="color:#1f2328;">{{.ABBLastSuccess}}</b></div>{{end}}
{{if .Tasks}}
<table style="width:100%;border-collapse:collapse;margin-top:6px;">
<tr><td style="{{thCSS}}">Task</td><td style="{{thCSS}}">Source</td><td style="{{thCSS}}">Max age</td><td style="{{thCSS}}">Last success</td><td style="{{thCSS}}">Status</td></tr>
{{range .Tasks}}<tr><td style="{{tdCSS}}">{{.Name}}</td><td style="{{tdCSS}}">{{.Source}}</td><td style="{{tdCSS}}">{{.MaxAge}}</td><td style="{{tdCSS}}">{{.LastSuccess}}</td><td style="{{tdCSS}}"><span style="{{badgeCSS .Status.Class}}">{{.Status.Label}}</span></td></tr>{{end}}
</table>
{{end}}
{{end}}
{{if .Checks}}
<div style="font-weight:600;margin:12px 0 6px;">Checks</div>
<table style="width:100%;border-collapse:collapse;">
<tr><td style="{{thCSS}}">Severity</td><td style="{{thCSS}}">Check</td><td style="{{thCSS}}">Detail</td></tr>
{{range .Checks}}<tr><td style="{{tdCSS}}"><span style="{{badgeCSS .Sev.Class}}">{{.Sev.Label}}</span></td><td style="{{tdCSS}}">{{.Name}}</td><td style="{{mutedCSS}}">{{.Message}}</td></tr>{{end}}
</table>
{{end}}
<div style="color:#57606a;font-size:12px;margin-top:14px;">Collected {{.CollectedAt}} · {{.DurationMs}} ms · collector {{.Version}} · {{.Host}}</div>
</div>
`
