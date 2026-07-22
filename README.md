# Synology Collector

> A single-binary, cross-platform tool that remotely checks a Synology NAS for
> the backup and storage health **SNMP does not expose** — built to run on a
> schedule from an RMM.

Synology Collector queries a Synology NAS through DSM's Web APIs and reports the
signals that matter most for monitoring — above all **Active Backup for Business
(ABB)** and **Hyper Backup** task status, backup freshness, and failed attempts —
plus a few crucial storage and system facts (pool, volume usage, drive health).

It prints an RMM-friendly `KEY=VALUE` block and a full JSON document to stdout,
and returns an exit code that drives alert conditions. No agent, service, or
runtime dependencies — your RMM (NinjaOne in particular, but any RMM) invokes the
binary directly.

## Why not just SNMP?

RMMs like NinjaOne already monitor Synology CPU, RAM, temperatures, and interface
counters over SNMP. This collector deliberately does **not** duplicate those. It
focuses on what SNMP cannot see: Active Backup and Hyper Backup tasks, their
status and history, backup freshness, and failed/overdue backups — plus the
handful of storage/system health signals needed to give an at-a-glance status.

## Features

- **Active Backup for Business coverage** — task status, version history, backup
  freshness, and failed/overdue detection over a well-defined monitored set.
- **Hyper Backup coverage** — per-task result, running/idle state, destination
  reachability, and backup-integrity results. Long-running syncs and multi-day
  integrity checks are reported as *running*, never as false failures or overdue.
- **Storage & system health** — storage pool state, per-volume usage thresholds,
  physical drive health, and DSM/model facts.
- **Two consumable outputs** — a flat `KEY=VALUE` block for RMM field mapping and
  a full JSON document for richer tooling, plus a meaningful process exit code.
- **Secure by default** — credentials via environment or `--password-file`, TLS
  verification on by default with certificate pinning for DSM's self-signed cert,
  and read-only API access.
- **Single static binary** — cross-compiles to Windows, Linux, and macOS with no
  dependencies.

## Quick start

Credentials are read from the environment (or `--password-file`) so the password
never appears on the command line:

```bash
export DSM_HOST="https://192.168.1.20:5001"
export DSM_USERNAME="svc-rmm"
export DSM_PASSWORD="..."      # injected by your RMM's secure field
synologycollector
```

The storage and Active Backup APIs require an **administrators-group** DSM
account — see [Synology NAS setup](docs/synology-setup.md).

Example output (default `--format kv`, trimmed):

```
STATUS=OK
NAS=DS723+
DSM=7.2.2
HOSTNAME=jellyflame
UPTIME=9d 14h 40m
ABB_STATE=OK
ABB_MONITORED=7
ABB_FAILED=0
ABB_OVERDUE=1
LAST_SUCCESS=2026-07-21T02:14:00Z
HB_STATE=OK
HB_MONITORED=2
HB_RUNNING=1
HB_FAILED=0
HB_OVERDUE=0
SUMMARY=1 Active Backup task(s) overdue: WS-05 (last success 2026-07-19 02:14 UTC)
```

Add `--format json` for the full JSON document, or `--format both` for the KV
block followed by a `---` line and then the JSON. For a human-readable view, add
`--html-file <path>` to also write a self-contained, styled HTML summary (see
[Output → HTML report](docs/output.md#html-report)).

## Exit codes

| Code | Meaning  | When |
|------|----------|------|
| `0`  | healthy  | everything within thresholds |
| `1`  | warning  | ABB or Hyper Backup task failed/overdue/cancelled/integrity/destination, drive warning, volume ≥ warn threshold |
| `2`  | critical | storage pool/volume degraded, drive failed, volume ≥ crit threshold |
| `3`  | error    | DSM auth failed, NAS unreachable, storage or backup data inaccessible |

See [Output & exit codes](docs/output.md) for the full contract.

## Documentation

| Guide | What it covers |
|-------|----------------|
| [Installation & building](docs/installation.md) | Prebuilt binaries, automating the "latest" download, building from source, and releasing. |
| [Configuration reference](docs/configuration.md) | Every flag and environment variable, task selectors, thresholds. |
| [Output & exit codes](docs/output.md) | The `KEY=VALUE` block, JSON document, and exit-code contract. |
| [Synology NAS setup](docs/synology-setup.md) | The read-only DSM service account and TLS trust strategy. |
| [NinjaOne integration](docs/ninjaone.md) | The reference NinjaOne wrapper and stale-run detection. |
| [Operations & backup notes](docs/operations.md) | Backup-freshness tuning and Active Backup / Hyper Backup API classification. |

## Roadmap

The collector interface is built so additional Synology backup products slot in
as one file each, reusing the same auth, discovery, evaluation, and output.
Active Backup for Business and Hyper Backup (`SYNO.Backup.Task`) are supported;
next up:

- Active Backup for Microsoft 365 (`SYNO.ActiveBackupOffice365`)
- Active Backup for Google Workspace (`SYNO.ActiveBackupGSuite`)

An optional interactive terminal UI for setup and diagnostics is planned; the
collection engine is already isolated from stdout/stderr so it can be reused
without duplicating logic.
