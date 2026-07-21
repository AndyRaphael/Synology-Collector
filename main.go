package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses configuration, drives collection, renders output, and returns the
// process exit code. It is the only place stdout/stderr and the wall clock are
// bound; the collection engine below takes them as parameters.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args, os.Getenv, os.Stdin, stderr)
	if err != nil {
		switch {
		case errors.Is(err, errShowVersion):
			fmt.Fprintf(stdout, "synologycollector %s\n", version)
			return ExitOK
		case errors.Is(err, flag.ErrHelp):
			return ExitOK
		}
		// Usage/config error: human message on stderr, machine-readable ERROR block
		// on stdout so an RMM still captures something parseable. The requested
		// format is honored even though full config parsing failed.
		fmt.Fprintf(stderr, "error: %v\n", err)
		r := minimalErrorReport(err.Error())
		_ = render(stdout, requestedFormat(args), r)
		return ExitError
	}

	debugf := func(string, ...any) {}
	if cfg.Debug {
		debugf = func(format string, a ...any) {
			fmt.Fprintf(stderr, "[debug] "+format+"\n", a...)
		}
	}

	report := collect(context.Background(), cfg, time.Now, debugf)

	if err := render(stdout, cfg.Format, report); err != nil {
		// Failing to deliver the report is itself a collector failure.
		fmt.Fprintf(stderr, "error writing output: %v\n", err)
		return ExitError
	}
	return report.ExitCode
}

// requestedFormat recovers the --format value directly from args so that config
// errors (where full parsing did not complete) still honor the requested format.
// An absent or invalid value falls back to "both".
func requestedFormat(args []string) string {
	norm := func(s string) string {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "kv":
			return "kv"
		case "json":
			return "json"
		default:
			return "both"
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--format" || a == "-format":
			if i+1 < len(args) {
				return norm(args[i+1])
			}
		case strings.HasPrefix(a, "--format="):
			return norm(strings.TrimPrefix(a, "--format="))
		case strings.HasPrefix(a, "-format="):
			return norm(strings.TrimPrefix(a, "-format="))
		}
	}
	return "both"
}

// collect is the engine: it performs the whole DSM interaction and returns a fully
// populated Report (including Status and ExitCode). It writes to neither stdout nor
// stderr — diagnostics go through debugf — so a future interactive UI can reuse it.
func collect(ctx context.Context, cfg *Config, clock func() time.Time, debugf func(string, ...any)) *Report {
	now := clock()
	r := &Report{
		SchemaVersion:    schemaVersion,
		CollectorVersion: version,
		CollectedAt:      now,
		Host:             cfg.Host,
		Config:           newConfigEcho(cfg),
		Checks:           []CheckResult{},
	}
	defer func() { r.DurationMs = clock().Sub(now).Milliseconds() }()

	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	client, err := newClient(cfg, debugf)
	if err != nil {
		return finishError(r, err.Error())
	}

	if err := client.Discover(runCtx); err != nil {
		return finishError(r, "discovery failed: "+err.Error())
	}
	if err := client.Login(runCtx, cfg.Username, cfg.Password); err != nil {
		return finishError(r, err.Error())
	}
	defer func() {
		logoutCtx, lc := context.WithTimeout(context.Background(), 5*time.Second)
		defer lc()
		client.Logout(logoutCtx)
	}()

	sys, ferr := collectSystem(runCtx, client)
	if ferr != nil {
		return finishError(r, "NAS became unreachable during system collection: "+ferr.Error())
	}
	r.System = sys

	st, ferr := collectStorage(runCtx, client)
	if ferr != nil {
		return finishError(r, "NAS became unreachable during storage collection: "+ferr.Error())
	}
	r.Storage = st

	abb, ferr := collectABB(runCtx, client, cfg, now)
	if ferr != nil {
		return finishError(r, ferr.Error())
	}
	r.ABB = abb

	checks := evaluate(cfg, sys, st, abb)
	r.Checks = checks
	if cfg.Debug {
		r.Raw = client.RawPayloads()
	}

	// Coverage contract: some collection gaps mean no meaningful health statement.
	if msg, isErr := coverageError(st, abb); isErr {
		r.Status = "ERROR"
		r.ExitCode = ExitError
		r.Error = sanitizeInline(msg)
		r.Summary = sanitizeInline(msg)
		return r
	}

	sev := overallSeverity(checks)
	r.Status = severityStatus(sev)
	r.ExitCode = severityExitCode(sev)
	r.Summary = buildSummary(checks, st, abb)
	return r
}

func finishError(r *Report, msg string) *Report {
	r.Status = "ERROR"
	r.ExitCode = ExitError
	r.Error = sanitizeInline(msg)
	r.Summary = sanitizeInline(msg)
	if r.Checks == nil {
		r.Checks = []CheckResult{}
	}
	return r
}

// minimalErrorReport builds an ERROR report for failures that occur before a
// Config exists (usage/config errors).
func minimalErrorReport(msg string) *Report {
	return &Report{
		SchemaVersion:    schemaVersion,
		CollectorVersion: version,
		CollectedAt:      time.Now(),
		Status:           "ERROR",
		ExitCode:         ExitError,
		Error:            sanitizeInline(msg),
		Summary:          sanitizeInline(msg),
		Checks:           []CheckResult{},
	}
}
