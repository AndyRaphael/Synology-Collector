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

With `--format both` (default), the KV block is printed first, then a line
containing exactly `---`, then indented JSON.

```
STATUS=OK
NAS=DS723+
DSM=7.2.2
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
SUMMARY=1 Active Backup task(s) overdue: WS-05 (last success 2026-07-19T02:14:00Z)
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

## KV keys and units

| Key | Values / units |
|-----|----------------|
| `STATUS` | `OK` \| `WARNING` \| `CRITICAL` \| `ERROR` |
| `ERROR` | present only when `STATUS=ERROR` |
| `NAS` | model string (e.g. `DS723+`), or `Unknown` |
| `DSM` | short version (e.g. `7.2.2`), or `Unknown` |
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
| `LAST_SUCCESS` | newest monitored success (RFC3339 UTC) \| `never` \| `Unknown` \| `N/A` |
| `SUMMARY` | one-line human summary |
| `HOST` | normalized base URL |
| `COLLECTED_AT` | RFC3339 UTC run time |
| `COLLECTOR_VERSION` | build version |

`LAST_SUCCESS` distinguishes: a timestamp (a success is known); `never` (all
monitored tasks have complete history and none ever succeeded); `Unknown`
(indeterminate — history was truncated or a fetch failed); `N/A` (no monitored
tasks, or ABB not installed).

## JSON document

The JSON document carries `schema_version` (currently `1`), a secret-free config
echo, typed `system`/`storage`/`abb` sections each with a `state`, the full
`checks` array, and — with `--debug` — the raw API payloads under `raw`.
