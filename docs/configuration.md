# Configuration reference

Credentials and host may be supplied by flag or environment variable. **Prefer
environment variables or `--password-file`** — a password passed as a
command-line argument can appear in process listings and RMM logs.

```
export DSM_HOST="https://192.168.1.20:5001"
export DSM_USERNAME="svc-rmm"
export DSM_PASSWORD="..."      # injected by your RMM's secure field
synologycollector
```

## Flags and environment variables

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--host` | `DSM_HOST` | — | Full URL (`https://host:5001`) or bare `host[:port]`. Bare hosts always use HTTPS on 5001; only an explicit `http://` uses HTTP (5000) and additionally requires `--allow-http`. |
| `--username` | `DSM_USERNAME` | — | DSM account name. |
| `--password` | `DSM_PASSWORD` | — | DSM password. Discouraged as a flag. |
| `--password-file` | — | — | Read the password from a file, or `-` for stdin. |
| `--vol-warn` | — | `80` | Volume usage warning threshold (percent). |
| `--vol-crit` | — | `90` | Volume usage critical threshold (percent). |
| `--backup-max-age` | — | `24h` | A monitored **Active Backup** task with no newer successful backup is overdue. |
| `--task-max-age` | — | — | Per-task Active Backup freshness override, `SELECTOR=DURATION`, repeatable. |
| `--exclude-task` | — | — | Exclude an Active Backup task from the monitored set, repeatable. |
| `--hyperbackup-max-age` | — | `168h` (7d) | A monitored **Hyper Backup** task with no newer successful backup is overdue — but only while it is **idle**. A running task (a large sync or a backup-integrity check, either of which can run for days) is never overdue. Larger than `--backup-max-age` by design. |
| `--exclude-hyperbackup-task` | — | — | Exclude a Hyper Backup task from the monitored set, repeatable (same selector forms). |
| `--timeout` | — | `90s` | Overall run timeout (per-request timeout is 30s). |
| `--allow-http` | — | off | Permit cleartext HTTP (sends credentials unencrypted). |
| `--insecure-skip-verify` | — | off | Disable TLS certificate verification (last resort). |
| `--ca-file` | — | — | PEM CA bundle for TLS verification. |
| `--tls-pin` | — | — | SHA-256 fingerprint of the server certificate (see [Synology NAS setup → TLS](synology-setup.md#tls)). |
| `--format` | — | `kv` | `kv`, `json`, or `both` (KV block, `---`, then JSON). |
| `--html-file` | — | — | Also write a self-contained HTML summary report to this path (see [Output → HTML report](output.md#html-report)). |
| `--html-embed-file` | — | — | Also write an inline-styled HTML fragment for a rich-text / NinjaOne WYSIWYG field (see [Output → Embedding](output.md#embedding-in-a-rich-text--wysiwyg-field---html-embed-file)). |
| `--debug` | — | off | Include raw API payloads in JSON and verbose diagnostics on stderr. |
| `--version` | — | — | Print version and exit. |

For the connection-security flags (`--tls-pin`, `--ca-file`,
`--insecure-skip-verify`, `--allow-http`), see
[Synology NAS setup → TLS](synology-setup.md#tls).

## Task selectors

`--task-max-age`, `--exclude-task`, and `--exclude-hyperbackup-task` accept:

- `id:123` — match by task ID (unambiguous).
- `name:Nightly` — match by exact task name (unambiguous even for numeric names).
- `Nightly` — bare value: match by name first, then fall back to task ID.

If a name matches more than one task, the collector exits `3` and lists the
candidate IDs so you can disambiguate with `id:`. A selector that matches
nothing raises a warning (so a typo is visible, not silent).

```bash
# Weekly server backup: allow 8 days; nightly workstation excluded from alerting.
synologycollector --host nas --username svc --password-file secret.txt \
  --task-max-age "name:Weekly Server=192h" \
  --exclude-task "id:7"
```

## Hyper Backup vs. Active Backup freshness

The two backup modules are monitored independently and have separate freshness
windows because they behave very differently:

- **Active Backup** tasks run on a tight schedule; a missed nightly run is a real
  signal, so `--backup-max-age` defaults to `24h`.
- **Hyper Backup** to a remote destination (Synology C2, S3, an rsync target) can
  take **more than a day for a single sync** on a large dataset, and its
  **backup-integrity check can also run for many hours**. The collector therefore
  (1) never marks a task overdue or failed *while it is running* — a multi-day
  sync or integrity check reports as `RUNNING`, not a problem — and (2) uses the
  larger `--hyperbackup-max-age` (default 7 days) to judge only **idle** tasks.
  Raise it further if your longest cycle plus its integrity check can exceed a
  week between successful completions.

See [Operations → Backup freshness](operations.md#backup-freshness-caveats) for
guidance on choosing freshness windows.
