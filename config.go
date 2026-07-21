package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// errShowVersion is a sentinel returned by parseConfig when --version is passed,
// letting main() print the version and exit 0 without treating it as an error.
var errShowVersion = errors.New("version requested")

// SelKind distinguishes how a task selector should be matched.
type SelKind int

const (
	// SelAuto matches by exact task name first, then falls back to numeric task ID.
	SelAuto SelKind = iota
	// SelID matches strictly by numeric task ID (id: prefix).
	SelID
	// SelName matches strictly by exact task name (name: prefix), unambiguous
	// even when a task is named with digits.
	SelName
)

// Selector identifies one or more ABB tasks for --task-max-age / --exclude-task.
type Selector struct {
	Kind SelKind
	Raw  string // original selector text, used in messages
	ID   int64  // populated when Kind == SelID
	Name string // populated when Kind == SelName or SelAuto
}

// TaskOverride is a per-task freshness window from --task-max-age.
type TaskOverride struct {
	Sel    Selector
	MaxAge time.Duration
}

// Config holds all resolved runtime configuration. Password is never marshaled;
// the JSON echo (see ConfigEcho in output.go) deliberately omits it.
type Config struct {
	RawHost      string // as supplied, for diagnostics
	Host         string // normalized base URL, e.g. https://192.168.1.20:5001
	Scheme       string // "https" or "http"
	Username     string
	Password     string
	VolWarnPct   int
	VolCritPct   int
	BackupMaxAge time.Duration
	TaskMaxAge   []TaskOverride
	ExcludeTasks []Selector
	Timeout      time.Duration
	AllowHTTP    bool
	InsecureTLS  bool
	CAFile       string
	TLSPin       string // normalized lowercase hex SHA-256, or ""
	Format       string // kv | json | both
	Debug        bool
}

// stringList accumulates a repeatable flag's values.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseConfig builds a Config from CLI args, environment, and (for --password-file -)
// stdin. It is pure with respect to these injected inputs so tests can drive it
// without touching the real process environment.
func parseConfig(args []string, getenv func(string) string, stdin io.Reader, stderr io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("synologycollector", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		host         = fs.String("host", "", "DSM base URL, e.g. https://192.168.1.20:5001 (env DSM_HOST)")
		username     = fs.String("username", "", "DSM username (env DSM_USERNAME)")
		password     = fs.String("password", "", "DSM password (discouraged: visible in process listings; prefer --password-file or DSM_PASSWORD)")
		passwordFile = fs.String("password-file", "", "read password from a file, or '-' for stdin")
		volWarn      = fs.Int("vol-warn", 80, "volume usage warning threshold (percent)")
		volCrit      = fs.Int("vol-crit", 90, "volume usage critical threshold (percent)")
		backupMaxAge = fs.Duration("backup-max-age", 24*time.Hour, "max age of last successful backup before a task is overdue")
		timeout      = fs.Duration("timeout", 90*time.Second, "overall run timeout")
		allowHTTP    = fs.Bool("allow-http", false, "permit cleartext http:// (sends credentials unencrypted)")
		insecure     = fs.Bool("insecure-skip-verify", false, "disable TLS certificate verification (last resort)")
		caFile       = fs.String("ca-file", "", "PEM CA bundle for TLS verification")
		tlsPin       = fs.String("tls-pin", "", "SHA-256 fingerprint of the server leaf certificate (hex; alternate verification mode for self-signed certs)")
		format       = fs.String("format", "both", "output format: kv | json | both")
		debug        = fs.Bool("debug", false, "include raw API payloads in JSON and verbose diagnostics on stderr")
		showVersion  = fs.Bool("version", false, "print version and exit")
	)
	var taskMaxAge stringList
	var excludeTask stringList
	fs.Var(&taskMaxAge, "task-max-age", "per-task freshness override SELECTOR=DURATION, repeatable (SELECTOR = id:N, name:X, or a bare name)")
	fs.Var(&excludeTask, "exclude-task", "exclude a task from the monitored set, repeatable (same SELECTOR forms)")

	if err := fs.Parse(args); err != nil {
		return nil, err // includes flag.ErrHelp, handled by caller
	}
	if *showVersion {
		return nil, errShowVersion
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg := &Config{
		VolWarnPct:   *volWarn,
		VolCritPct:   *volCrit,
		BackupMaxAge: *backupMaxAge,
		Timeout:      *timeout,
		AllowHTTP:    *allowHTTP,
		InsecureTLS:  *insecure,
		CAFile:       *caFile,
		Debug:        *debug,
		Format:       strings.ToLower(strings.TrimSpace(*format)),
	}

	// Host (flag, then env).
	cfg.RawHost = *host
	if cfg.RawHost == "" {
		cfg.RawHost = getenv("DSM_HOST")
	}
	baseURL, scheme, err := normalizeHost(cfg.RawHost, cfg.AllowHTTP)
	if err != nil {
		return nil, err
	}
	cfg.Host = baseURL
	cfg.Scheme = scheme

	// Username (flag, then env).
	cfg.Username = *username
	if cfg.Username == "" {
		cfg.Username = getenv("DSM_USERNAME")
	}
	if cfg.Username == "" {
		return nil, errors.New("username is required (--username or DSM_USERNAME)")
	}

	// Password (flag > file/stdin > env).
	pw, err := resolvePassword(*password, *passwordFile, getenv, stdin)
	if err != nil {
		return nil, err
	}
	cfg.Password = pw

	// Thresholds.
	if cfg.VolWarnPct < 1 || cfg.VolWarnPct > 100 {
		return nil, fmt.Errorf("--vol-warn must be 1..100, got %d", cfg.VolWarnPct)
	}
	if cfg.VolCritPct < 1 || cfg.VolCritPct > 100 {
		return nil, fmt.Errorf("--vol-crit must be 1..100, got %d", cfg.VolCritPct)
	}
	if cfg.VolWarnPct >= cfg.VolCritPct {
		return nil, fmt.Errorf("--vol-warn (%d) must be less than --vol-crit (%d)", cfg.VolWarnPct, cfg.VolCritPct)
	}

	// Durations.
	if cfg.BackupMaxAge <= 0 {
		return nil, fmt.Errorf("--backup-max-age must be positive, got %s", cfg.BackupMaxAge)
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive, got %s", cfg.Timeout)
	}

	// Format.
	switch cfg.Format {
	case "kv", "json", "both":
	default:
		return nil, fmt.Errorf("--format must be kv, json, or both, got %q", cfg.Format)
	}

	// TLS pin.
	if *tlsPin != "" {
		pin, err := normalizePin(*tlsPin)
		if err != nil {
			return nil, err
		}
		cfg.TLSPin = pin
	}

	// Task selectors.
	cfg.TaskMaxAge, err = parseTaskOverrides(taskMaxAge)
	if err != nil {
		return nil, err
	}
	cfg.ExcludeTasks, err = parseSelectors(excludeTask)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// normalizeHost turns user input (full URL, bare host, or host:port) into a
// canonical base URL and scheme. Bare hosts always use https on port 5001; only
// an explicit http:// scheme uses http (port 5000) and requires allowHTTP.
func normalizeHost(raw string, allowHTTP bool) (baseURL, scheme string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("host is required (--host or DSM_HOST)")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid host %q: %v", raw, err)
	}
	scheme = strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return "", "", errors.New("refusing to send credentials over http; pass --allow-http to override")
		}
	default:
		return "", "", fmt.Errorf("unsupported scheme %q (use http or https)", u.Scheme)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "", "", fmt.Errorf("invalid host %q: missing hostname", raw)
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "5001"
		} else {
			port = "5000"
		}
	}
	path := strings.TrimRight(u.Path, "/")
	baseURL = scheme + "://" + net.JoinHostPort(hostname, port) + path
	return baseURL, scheme, nil
}

// resolvePassword applies precedence flag > file/stdin > env. A trailing newline
// from a file or stdin is trimmed so `echo secret | ... --password-file -` works.
func resolvePassword(flagVal, fileVal string, getenv func(string) string, stdin io.Reader) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if fileVal != "" {
		var b []byte
		var err error
		if fileVal == "-" {
			b, err = io.ReadAll(stdin)
			if err != nil {
				return "", fmt.Errorf("reading password from stdin: %v", err)
			}
		} else {
			b, err = os.ReadFile(fileVal)
			if err != nil {
				return "", fmt.Errorf("reading password file: %v", err)
			}
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if v := getenv("DSM_PASSWORD"); v != "" {
		return v, nil
	}
	return "", errors.New("password is required (--password, --password-file, or DSM_PASSWORD)")
}

// normalizePin validates and canonicalizes a certificate fingerprint to 64 lowercase
// hex characters (SHA-256), tolerating colons/spaces and an optional sha256: prefix.
func normalizePin(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	p = strings.TrimPrefix(p, "sha256:")
	p = strings.NewReplacer(":", "", " ", "", "-", "").Replace(p)
	if len(p) != 64 {
		return "", fmt.Errorf("--tls-pin must be a 64-hex-character SHA-256 fingerprint, got %d characters", len(p))
	}
	for _, r := range p {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return "", fmt.Errorf("--tls-pin contains a non-hex character %q", r)
		}
	}
	return p, nil
}

func parseSelector(raw string) (Selector, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Selector{}, errors.New("empty task selector")
	}
	switch {
	case strings.HasPrefix(raw, "id:"):
		v := strings.TrimSpace(raw[len("id:"):])
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Selector{}, fmt.Errorf("invalid task id selector %q: %v", raw, err)
		}
		return Selector{Kind: SelID, Raw: raw, ID: id}, nil
	case strings.HasPrefix(raw, "name:"):
		name := raw[len("name:"):]
		if name == "" {
			return Selector{}, fmt.Errorf("empty name in selector %q", raw)
		}
		return Selector{Kind: SelName, Raw: raw, Name: name}, nil
	default:
		return Selector{Kind: SelAuto, Raw: raw, Name: raw}, nil
	}
}

func parseSelectors(raws []string) ([]Selector, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]Selector, 0, len(raws))
	for _, r := range raws {
		s, err := parseSelector(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseTaskOverrides(raws []string) ([]TaskOverride, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]TaskOverride, 0, len(raws))
	for _, s := range raws {
		// Duration never contains '=', so split on the last '=' to permit '=' in a name.
		eq := strings.LastIndex(s, "=")
		if eq < 0 {
			return nil, fmt.Errorf("--task-max-age %q must be SELECTOR=DURATION", s)
		}
		sel, err := parseSelector(s[:eq])
		if err != nil {
			return nil, err
		}
		dur, err := time.ParseDuration(s[eq+1:])
		if err != nil {
			return nil, fmt.Errorf("--task-max-age %q: invalid duration: %v", s, err)
		}
		if dur <= 0 {
			return nil, fmt.Errorf("--task-max-age %q: duration must be positive", s)
		}
		out = append(out, TaskOverride{Sel: sel, MaxAge: dur})
	}
	return out, nil
}
