# Output & exit codes

The collector communicates results two ways: a machine-parseable output block on
stdout and a process **exit code**. RMM conditions can match on either.

## Exit codes

| Code | Meaning  | When |
|------|----------|------|
| `0`  | healthy  | everything within thresholds |
| `1`  | warning  | ABB task failed/overdue/cancelled, drive warning, volume ≥ warn threshold |
| `2`  | critical | storage pool/volume degraded, drive failed, volume ≥ crit threshold |
| `3`  | error    | DSM auth failed, NAS unreachable, storage or backup data inaccessible |

Exit `3` is the "the collector could not do its job" signal (bad credentials,
unreachable NAS, insufficient privileges). It is distinct from `2` (the NAS is
reachable and reports a genuine problem).

## Output format

The default is `--format kv`, which prints only the ordered `KEY=VALUE` block.
`--format json` prints only the JSON document; `--format both` prints the KV
block, then a line containing exactly `---`, then the indented JSON — shown here:

```
STATUS=OK
NAS=DS723+
DSM=7.2.2
HOSTNAME=jellyflame
UPTIME=9d 14h 40m
SYSTEM_HEALTH=Normal
STORAGE_POOL=Healthy
VOLUME_USAGE=68%
DRIVES=2
DRIVE_WARNINGS=0
ABB_STATE=OK
ABB_TASKS=7
ABB_MONITORED=7
ABB_DISABLED=0
ABB_EXCLUDED=0
ABB_FAILED=0
ABB_OVERDUE=1
LAST_SUCCESS=2026-07-21T02:14:00Z
HB_STATE=OK
HB_TASKS=2
HB_MONITORED=2
HB_DISABLED=0
HB_EXCLUDED=0
HB_RUNNING=1
HB_FAILED=0
HB_OVERDUE=0
HB_LAST_SUCCESS=2026-07-18T01:00:00Z
SUMMARY=1 Active Backup task(s) overdue: WS-05 (last success 2026-07-19 02:14 UTC)
HOST=https://192.168.1.20:5001
COLLECTED_AT=2026-07-21T12:34:56Z
COLLECTOR_VERSION=0.1.0
---
{ ... full JSON report ... }
```

Even on an auth or connectivity failure, a parseable KV block is still printed
(`STATUS=ERROR`, `ERROR=...`, plus `HOST`/`COLLECTED_AT`/`COLLECTOR_VERSION`)
before exit `3`, so RMM conditions can match on text as well as exit code. Error
output honors `--format` exactly (`kv` prints only KV, `json` prints only JSON).

## HTML report

`--format` controls stdout, which stays machine-parseable. When you also want a
human-readable view, add `--html-file <path>` to write a **self-contained**,
styled HTML summary to that file:

```bash
synologycollector --html-file /srv/reports/nas01.html
```

- It is written **in addition to** stdout, independent of `--format` — the KV/
  JSON contract RMM depends on is unchanged.
- The page is a single file with all CSS inlined (no external assets), so it
  opens anywhere and can be dropped on a share, emailed, or attached to a
  ticket. It adapts to the viewer's light/dark theme.
- It renders the same data as the JSON — status banner, system, storage
  (pools/volumes/drives with usage bars), Active Backup tasks, and the full
  check list — but scannable at a glance, which matters more as modules are
  added.
- An error report (unreachable NAS, bad credentials) is rendered too, so the
  file is never stale-looking after a failed run.
- Writing the file is a **convenience, not the health signal**: if the path is
  not writable the collector prints a `warning:` to stderr and continues with
  its normal health-based exit code, so a bad `--html-file` path never masks a
  real NAS problem (or invents one).

### Embedding in a rich-text / WYSIWYG field (`--html-embed-file`)

`--html-file` is a full standalone page with a `<style>` block. Rich-text /
WYSIWYG custom fields (NinjaOne's included) **strip `<style>` and `<script>`**,
so that page would render unstyled in one. `--html-embed-file <path>` writes the
same report as an **inline-styled fragment** (every rule on the element, tables
for layout, no `<style>`/`<script>`) that survives the sanitizer and renders
with its status colors intact:

```bash
synologycollector --html-embed-file /var/reports/nas.fragment.html
```

It's the same opt-in, warn-don't-fail contract as `--html-file`, and the two can
be used together. See [NinjaOne integration](ninjaone.md#step-7--optional-render-the-report-in-a-wysiwyg-field)
for pushing it into a WYSIWYG field.

## KV keys and units

| Key | Values / units |
|-----|----------------|
| `STATUS` | `OK` \| `WARNING` \| `CRITICAL` \| `ERROR` |
| `ERROR` | present only when `STATUS=ERROR` |
| `NAS` | model string (e.g. `DS723+`), or `Unknown` |
| `DSM` | short version (e.g. `7.2.2`), or `Unknown` |
| `HOSTNAME` | NAS hostname (e.g. `jellyflame`), or `Unknown` |
| `UPTIME` | humanized uptime (e.g. `9d 14h 40m`), or `Unknown` |
| `SYSTEM_HEALTH` | `Normal` \| `Warning` \| `Critical` \| `Unknown` (worst of pool/volume/drive) |
| `STORAGE_POOL` | `Healthy`, a capitalized status word, or `Unknown` |
| `VOLUME_USAGE` | highest volume usage as an integer percent with `%` (e.g. `68%`), or `Unknown` |
| `DRIVES` | physical drive count, or `Unknown` |
| `DRIVE_WARNINGS` | count of drives not reporting healthy, or `Unknown` |
| `ABB_STATE` | `OK` \| `PARTIAL` \| `NOT_INSTALLED` \| `UNAVAILABLE` \| `ERROR` |
| `ABB_TASKS` | total tasks configured |
| `ABB_MONITORED` | enabled and not excluded (the population that can alert) |
| `ABB_DISABLED` / `ABB_EXCLUDED` | counts excluded from alerting |
| `ABB_FAILED` / `ABB_OVERDUE` | counts over the monitored set |
| `LAST_SUCCESS` | newest monitored ABB success (RFC3339 UTC) \| `never` \| `Unknown` \| `N/A` |
| `HB_STATE` | `OK` \| `PARTIAL` \| `NOT_INSTALLED` \| `UNAVAILABLE` \| `ERROR` (Hyper Backup) |
| `HB_TASKS` | total Hyper Backup tasks configured |
| `HB_MONITORED` | enabled and not excluded (the population that can alert) |
| `HB_DISABLED` / `HB_EXCLUDED` | counts excluded from alerting |
| `HB_RUNNING` | tasks currently backing up or running an integrity check (healthy activity, never overdue) |
| `HB_FAILED` | tasks with a broken backup: failed/partial run, failed integrity check, or unreachable destination |
| `HB_OVERDUE` | **idle** tasks whose last success is older than `--hyperbackup-max-age` |
| `HB_LAST_SUCCESS` | newest monitored Hyper Backup success (RFC3339 UTC) \| `never` \| `Unknown` \| `N/A` |
| `SUMMARY` | one-line human summary |
| `HOST` | normalized base URL |
| `COLLECTED_AT` | RFC3339 UTC run time |
| `COLLECTOR_VERSION` | build version |

`LAST_SUCCESS` distinguishes: a timestamp (a success is known); `never` (all
monitored tasks have complete history and none ever succeeded); `Unknown`
(indeterminate — history was truncated or a fetch failed); `N/A` (no monitored
tasks, or ABB not installed). `HB_LAST_SUCCESS` follows the same convention for
Hyper Backup.

Hyper Backup's `HB_RUNNING` and `HB_OVERDUE` are deliberately kept apart: a task
that is actively syncing or running a backup-integrity check counts only in
`HB_RUNNING` and is **never** counted as overdue or failed, no matter how long it
has been running. `HB_OVERDUE` therefore means "idle *and* stale" — a task that
has stopped completing on schedule — not "still working." See
[Configuration → Hyper Backup vs. Active Backup freshness](configuration.md#hyper-backup-vs-active-backup-freshness).

## JSON document

The JSON document carries `schema_version` (currently `1`), a secret-free config
echo, typed `system`/`storage`/`abb`/`hyperbackup` sections each with a `state`,
the full `checks` array, and — with `--debug` — the raw API payloads under `raw`.
The `hyperbackup` section lists each task with its raw `state`/`status`, the
classified `last_result`, `last_success`/`next_backup` timestamps, and the
per-task `running`/`overdue`/`integrity_failed`/`dest_missing` flags.
