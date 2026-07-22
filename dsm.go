package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RawPayload is a retained (--debug) API response. A body is stored as JSON only
// when it actually parses; otherwise it is kept as bounded text, so an HTML or
// malformed response can never make the final report fail to marshal.
type RawPayload struct {
	ValidJSON bool            `json:"valid_json"`
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
}

// flexStr decodes a raw JSON value to a trimmed string via FlexString, tolerating
// strings, numbers, bools, or null. Absent/undecodable yields "".
func flexStr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fs FlexString
	if err := json.Unmarshal(raw, &fs); err != nil {
		return ""
	}
	return strings.TrimSpace(string(fs))
}

// parseFlexBool decodes a raw JSON value that DSM may express as bool, 0/1, or a
// quoted string. Absent or unrecognized yields def.
func parseFlexBool(raw json.RawMessage, def bool) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return def
	}
	switch strings.ToLower(strings.Trim(s, `"`)) {
	case "true", "1", "yes", "on", "enabled":
		return true
	case "false", "0", "no", "off", "disabled":
		return false
	default:
		return def
	}
}

// decodeArrayField unmarshals data as a JSON object and returns the raw elements
// of the named array field. It errors when data is not an object, the field is
// absent, or the field is present but not a JSON array (e.g. null). A present
// empty array is legitimate and returns (empty slice, nil) — this is what lets the
// collector distinguish "zero configured items" from "field missing/renamed".
func decodeArrayField(data json.RawMessage, field string) ([]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("expected a JSON object: %w", err)
	}
	raw, ok := obj[field]
	if !ok {
		return nil, fmt.Errorf("missing %q field", field)
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' {
		return nil, fmt.Errorf("%q is not a JSON array", field)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("%q is not a decodable array: %w", field, err)
	}
	return arr, nil
}

// maxBodyBytes caps a single API response. We read one extra byte to detect
// truncation rather than silently accepting a clipped payload.
const maxBodyBytes = 8 << 20

// ErrorKind classifies failures so the pipeline can decide fatal (exit 3) vs
// per-collector degradation.
type ErrorKind int

const (
	// ErrNetwork covers dial/TLS/timeout/redirect/non-2xx — the NAS could not be
	// reached or did not speak HTTP properly. Always fatal.
	ErrNetwork ErrorKind = iota
	// ErrAuth covers login failures. Always fatal.
	ErrAuth
	// ErrAPI covers a well-formed DSM response with success:false (missing API,
	// permission denied, bad params). Collector-scoped post-login.
	ErrAPI
	// ErrParse covers a body that is not the expected JSON envelope.
	ErrParse
)

func (k ErrorKind) String() string {
	switch k {
	case ErrNetwork:
		return "network"
	case ErrAuth:
		return "auth"
	case ErrAPI:
		return "api"
	case ErrParse:
		return "parse"
	default:
		return "unknown"
	}
}

// DSMError is the single error type returned by the client.
type DSMError struct {
	Kind ErrorKind
	Code int    // DSM numeric code, or HTTP status for network errors, 0 if none
	API  string // API/method that failed
	Msg  string // human-readable message (never contains secrets)
	Err  error  // wrapped cause, if any
}

func (e *DSMError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "dsm error"
}

func (e *DSMError) Unwrap() error { return e.Err }

func kindOf(err error) (ErrorKind, bool) {
	var de *DSMError
	if errors.As(err, &de) {
		return de.Kind, true
	}
	return 0, false
}

func codeOf(err error) int {
	var de *DSMError
	if errors.As(err, &de) {
		return de.Code
	}
	return 0
}

// FlexInt64 unmarshals a JSON number, a numeric string, or null into int64.
// DSM returns byte sizes as strings on some models and as numbers on others.
type FlexInt64 int64

func (f *FlexInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		*f = 0
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = FlexInt64(i)
		return nil
	}
	if fl, err := strconv.ParseFloat(s, 64); err == nil {
		*f = FlexInt64(int64(fl))
		return nil
	}
	return fmt.Errorf("cannot parse %q as integer", s)
}

// FlexString unmarshals a JSON string, number, bool, or null into a string,
// so a field whose type varies across DSM versions never breaks decoding.
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = ""
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*f = FlexString(str)
		return nil
	}
	*f = FlexString(s)
	return nil
}

// parseEpoch interprets a Unix timestamp that may be in seconds or milliseconds.
// ok is false for non-positive values or timestamps implausibly far in the future
// (clock skew or a millisecond value misread as seconds).
func parseEpoch(v int64, now time.Time) (t time.Time, ok bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	// A seconds value above ~1e11 is the year 5138; treat such values as millis.
	if v > 100_000_000_000 {
		t = time.UnixMilli(v).UTC()
	} else {
		t = time.Unix(v, 0).UTC()
	}
	if t.After(now.Add(48 * time.Hour)) {
		return time.Time{}, false
	}
	return t, true
}

// apiEndpoint is one entry from SYNO.API.Info discovery.
type apiEndpoint struct {
	Path       string
	MinVersion int
	MaxVersion int
}

// clientAPIVersions declares the response shapes this collector knows how to
// parse, per API. The negotiated request version is the max of the intersection
// with the DSM-advertised [minVersion, maxVersion].
var clientAPIVersions = map[string][2]int{
	"SYNO.API.Auth":             {3, 7},
	"SYNO.Core.System":          {1, 2},
	"SYNO.Core.Package":         {1, 2},
	"SYNO.Storage.CGI.Storage":  {1, 1},
	"SYNO.ActiveBackup.Task":    {1, 1},
	"SYNO.ActiveBackup.Version": {1, 1},
	"SYNO.Backup.Task":          {1, 1},
}

// Client is a stateful DSM Web API client. It writes to neither stdout nor stderr;
// all diagnostics flow through the injected debugf callback so the engine stays
// reusable by a future interactive UI.
type Client struct {
	baseURL string
	hc      *http.Client
	sid     string
	apis    map[string]apiEndpoint
	raw     map[string]RawPayload
	retain  bool
	debugf  func(string, ...any)
}

type apiResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// newClient builds a DSM client with the TLS and redirect policy from cfg.
func newClient(cfg *Config, debugf func(string, ...any)) (*Client, error) {
	tc, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig: tc,
		Proxy:           http.ProxyFromEnvironment,
	}
	hc := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second, // per-request; overall budget comes from ctx
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A 30x would resend the credential-bearing POST to the redirect
			// target, possibly cross-host. Never follow.
			return errors.New("refusing HTTP redirect (would resend credentials)")
		},
	}
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	c := &Client{
		baseURL: cfg.Host,
		hc:      hc,
		apis:    map[string]apiEndpoint{},
		raw:     map[string]RawPayload{},
		retain:  cfg.Debug,
		debugf:  debugf,
	}
	// Seed the discovery endpoint: SYNO.API.Info answers at query.cgi and needs
	// no session, so it works before discovery populates the rest.
	c.apis["SYNO.API.Info"] = apiEndpoint{Path: "query.cgi", MinVersion: 1, MaxVersion: 1}
	return c, nil
}

// buildTLSConfig implements the four TLS modes. Precedence: pin > ca-file >
// insecure > default (full verification).
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.TLSPin != "":
		// Alternate verification mode. Go runs chain + hostname verification
		// before VerifyPeerCertificate, which would reject a self-signed cert
		// first; so we disable the default chain check and instead REQUIRE the
		// leaf certificate to match the pinned SHA-256 fingerprint.
		pin := cfg.TLSPin
		tc.InsecureSkipVerify = true
		tc.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("tls: server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if subtle.ConstantTimeCompare([]byte(got), []byte(pin)) != 1 {
				return fmt.Errorf("tls: server certificate fingerprint %s does not match --tls-pin", got)
			}
			return nil
		}
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading --ca-file: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in --ca-file %q", cfg.CAFile)
		}
		tc.RootCAs = pool
	case cfg.InsecureTLS:
		tc.InsecureSkipVerify = true
	}
	return tc, nil
}

// apiCall is the single generic request path. All typed wrappers build on it.
func (c *Client) apiCall(ctx context.Context, api string, version int, method string, params url.Values) (json.RawMessage, error) {
	cgiPath := "entry.cgi"
	if ep, ok := c.apis[api]; ok && ep.Path != "" {
		cgiPath = ep.Path
	}
	endpoint := c.baseURL + "/webapi/" + cgiPath

	form := url.Values{}
	form.Set("api", api)
	form.Set("version", strconv.Itoa(version))
	form.Set("method", method)
	if c.sid != "" {
		form.Set("_sid", c.sid)
	}
	for k, vs := range params {
		for _, v := range vs {
			form.Add(k, v)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &DSMError{Kind: ErrNetwork, API: api, Msg: "building request", Err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &DSMError{Kind: ErrNetwork, API: api, Msg: describeNetErr(err), Err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, &DSMError{Kind: ErrNetwork, API: api, Msg: "reading response body", Err: err}
	}
	if len(body) > maxBodyBytes {
		return nil, &DSMError{Kind: ErrParse, API: api, Msg: fmt.Sprintf("response exceeds %d bytes", maxBodyBytes)}
	}

	c.retainRaw(rawKey(api, method, params), body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &DSMError{Kind: ErrNetwork, API: api, Code: resp.StatusCode,
			Msg: fmt.Sprintf("HTTP %d from %s", resp.StatusCode, cgiPath)}
	}

	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, &DSMError{Kind: ErrParse, API: api,
			Msg: fmt.Sprintf("response from %s was not JSON (got %q)", cgiPath, snippet(body)), Err: err}
	}
	if !env.Success {
		code := 0
		if env.Error != nil {
			code = env.Error.Code
		}
		return nil, &DSMError{Kind: ErrAPI, API: api, Code: code,
			Msg: fmt.Sprintf("%s.%s failed (DSM code %d)", api, method, code)}
	}
	c.debugf("%s.%s v%d ok", api, method, version)
	return env.Data, nil
}

// rawKey names a retained response uniquely: including task_id and offset keeps
// each version page distinct (otherwise later pages overwrite earlier ones), and
// the load marker distinguishes the enriched task-list request from a plain retry.
func rawKey(api, method string, params url.Values) string {
	k := api + "." + method
	if tid := params.Get("task_id"); tid != "" {
		k += ".task_" + tid
	}
	if off := params.Get("offset"); off != "" {
		k += ".offset_" + off
	}
	if params.Get("load_status") != "" {
		k += ".loaded"
	}
	return k
}

// retainRaw stores a response body for --debug output. Auth responses are never
// retained (they carry the session id). Non-JSON bodies are kept as bounded text
// so an HTML/malformed response can never make the final report fail to marshal.
func (c *Client) retainRaw(key string, body []byte) {
	if !c.retain {
		return
	}
	if strings.HasPrefix(key, "SYNO.API.Auth.") {
		return
	}
	if json.Valid(body) {
		c.raw[key] = RawPayload{ValidJSON: true, JSON: append(json.RawMessage(nil), body...)}
	} else {
		c.raw[key] = RawPayload{ValidJSON: false, Text: boundedText(body)}
	}
}

// boundedText caps and sanitizes a non-JSON body for safe inclusion in the report.
func boundedText(b []byte) string {
	const max = 8192
	s := string(b)
	if len(s) > max {
		s = s[:max] + "...(truncated)"
	}
	return sanitizeInline(s)
}

// RawPayloads returns the retained raw responses (empty unless --debug).
func (c *Client) RawPayloads() map[string]RawPayload { return c.raw }

// Discover populates the API table from SYNO.API.Info. Failure is fatal.
func (c *Client) Discover(ctx context.Context) error {
	params := url.Values{}
	params.Set("query", "ALL")
	data, err := c.apiCall(ctx, "SYNO.API.Info", 1, "query", params)
	if err != nil {
		var de *DSMError
		if errors.As(err, &de) {
			return de
		}
		return &DSMError{Kind: ErrNetwork, API: "SYNO.API.Info", Msg: err.Error(), Err: err}
	}
	var raw map[string]struct {
		Path       string `json:"path"`
		MinVersion int    `json:"minVersion"`
		MaxVersion int    `json:"maxVersion"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return &DSMError{Kind: ErrParse, API: "SYNO.API.Info", Msg: "cannot parse API discovery response", Err: err}
	}
	for name, ep := range raw {
		path := ep.Path
		if path == "" {
			path = "entry.cgi"
		}
		c.apis[name] = apiEndpoint{Path: path, MinVersion: ep.MinVersion, MaxVersion: ep.MaxVersion}
	}
	c.debugf("discovered %d APIs", len(c.apis))
	return nil
}

// HasAPI reports whether discovery advertised the named API.
func (c *Client) HasAPI(name string) bool {
	_, ok := c.apis[name]
	return ok
}

// pickVersion negotiates the request version for an API.
func (c *Client) pickVersion(api string) (int, error) {
	cl, ok := clientAPIVersions[api]
	if !ok {
		return 0, fmt.Errorf("no client version range declared for %s", api)
	}
	clMin, clMax := cl[0], cl[1]
	dsmMin, dsmMax := clMin, clMax
	if ep, ok := c.apis[api]; ok {
		if ep.MinVersion > 0 {
			dsmMin = ep.MinVersion
		}
		if ep.MaxVersion > 0 {
			dsmMax = ep.MaxVersion
		}
	}
	lo := max(clMin, dsmMin)
	hi := min(clMax, dsmMax)
	if lo > hi {
		return 0, &DSMError{Kind: ErrAPI, API: api,
			Msg: fmt.Sprintf("no compatible version for %s (DSM advertises %d-%d, collector supports %d-%d)",
				api, dsmMin, dsmMax, clMin, clMax)}
	}
	return hi, nil
}

// Login authenticates and stores the session id. Failure is always fatal (ErrAuth).
func (c *Client) Login(ctx context.Context, username, password string) error {
	ver, err := c.pickVersion("SYNO.API.Auth")
	if err != nil {
		return &DSMError{Kind: ErrAuth, API: "SYNO.API.Auth",
			Msg: "cannot authenticate: " + err.Error(), Err: err}
	}
	params := url.Values{}
	params.Set("account", username)
	params.Set("passwd", password)
	params.Set("format", "sid")
	params.Set("session", "SynologyCollector")
	data, err := c.apiCall(ctx, "SYNO.API.Auth", ver, "login", params)
	if err != nil {
		return mapAuthError(err)
	}
	var out struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.SID == "" {
		return &DSMError{Kind: ErrAuth, API: "SYNO.API.Auth",
			Msg: "login response contained no session id", Err: err}
	}
	c.sid = out.SID
	c.debugf("login ok (auth v%d)", ver)
	return nil
}

// Logout best-effort ends the session. Callers pass a fresh short-lived context
// so logout still runs even if the main run context has expired.
func (c *Client) Logout(ctx context.Context) {
	if c.sid == "" {
		return
	}
	ver, err := c.pickVersion("SYNO.API.Auth")
	if err != nil {
		ver = 6
	}
	params := url.Values{}
	params.Set("session", "SynologyCollector")
	if _, err := c.apiCall(ctx, "SYNO.API.Auth", ver, "logout", params); err != nil {
		c.debugf("logout failed (ignored): %v", err)
	}
	c.sid = ""
}

// mapAuthError converts a login failure into a friendly ErrAuth, preserving
// network/parse causes.
func mapAuthError(err error) error {
	var de *DSMError
	if !errors.As(err, &de) {
		return &DSMError{Kind: ErrAuth, API: "SYNO.API.Auth", Msg: err.Error(), Err: err}
	}
	if de.Kind == ErrNetwork || de.Kind == ErrParse {
		return de
	}
	return &DSMError{Kind: ErrAuth, API: "SYNO.API.Auth", Code: de.Code, Msg: authCodeMessage(de.Code)}
}

func authCodeMessage(code int) string {
	switch code {
	case 400:
		return "invalid username or password"
	case 401:
		return "account disabled"
	case 402:
		return "permission denied (account lacks required privileges)"
	case 403:
		return "two-factor authentication is required — use a dedicated service account with 2FA disabled"
	case 404:
		return "two-factor authentication code failed"
	case 407:
		return "sign-in blocked: source IP is on the DSM auto-block list"
	case 0:
		return "authentication failed"
	default:
		return fmt.Sprintf("authentication failed (DSM code %d)", code)
	}
}

// describeNetErr produces a concise, secret-free message for a transport error.
// The endpoint URL never carries credentials (they travel in the POST body).
func describeNetErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "run timed out before the request completed"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// snippet returns a short, single-line, control-char-free preview of a body,
// used to diagnose wrong-port HTML responses.
func snippet(b []byte) string {
	const n = 120
	s := string(b)
	if len(s) > n {
		s = s[:n] + "..."
	}
	return sanitizeInline(s)
}
