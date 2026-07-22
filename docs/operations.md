# Operations & backup notes

Operational guidance for interpreting results and tuning freshness, plus notes
on how the collector reads Synology's Active Backup API.

## Backup freshness caveats

- **Weekly / irregular schedules.** The default `--backup-max-age 24h` will flag
  a weekly job as overdue. Use `--task-max-age "name:Weekly=192h"` (or `id:`) to
  raise the window for specific tasks. See
  [Configuration → Task selectors](configuration.md#task-selectors).
- **Clock skew.** Freshness compares NAS-reported timestamps to the collector
  host's clock. Keep both on NTP; with nightly backups and a 24h window there is
  ample slack.
- **Never-succeeded tasks.** A task with no successful backup on record is
  reported overdue by design — a brand-new task alerts until its first backup
  completes.

## Notes on the Active Backup API

Synology's Active Backup Web API is not officially documented and its status
values vary between versions. The collector classifies every status through a
single table and treats anything unrecognized as *indeterminate* (a warning,
never a silent "healthy"). Run once with `--debug` against your NAS to capture
the real status strings in the JSON `raw` section; if any are unrecognized, they
can be added to the classifier in one place.

## Notes on storage & drive status

Pool, volume, and drive statuses are classified the same way — an exact-match
table with anything unrecognized reported as a *warning* (never silent-healthy).

One benign case worth knowing: DSM keeps a copy of its OS ("system partition")
mirrored across **every** drive, and reports that partition's health as a
separate dimension from the drive's SMART/allocation health. A perfectly healthy
drive therefore sometimes reads `sys_partition_normal` instead of `normal` —
both mean healthy, and the collector treats them identically. A *failed* system
partition (a genuinely different, unrecognized status word) still surfaces as a
warning so it is never missed. If a `--debug` run shows a drive status the
collector doesn't recognize but DSM's Storage Manager calls healthy, add it to
`driveStatusSeverity` in [`checks.go`](../checks.go).
