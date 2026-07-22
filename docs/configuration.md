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
| `--backup-max-age` | — | `24h` | A monitored task with no newer successful backup is overdue. |
| `--task-max-age` | — | — | Per-task freshness override, `SELECTOR=DURATION`, repeatable. |
| `--exclude-task` | — | — | Exclude a task from the monitored set, repeatable. |
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

`--task-max-age` and `--exclude-task` accept:

- `id:123` — match by numeric task ID (unambiguous).
- `name:Nightly` — match by exact task name (unambiguous even for numeric names).
- `Nightly` — bare value: match by name first, then fall back to numeric ID.

If a name matches more than one task, the collector exits `3` and lists the
candidate IDs so you can disambiguate with `id:`. A selector that matches
nothing raises a warning (so a typo is visible, not silent).

```bash
# Weekly server backup: allow 8 days; nightly workstation excluded from alerting.
synologycollector --host nas --username svc --password-file secret.txt \
  --task-max-age "name:Weekly Server=192h" \
  --exclude-task "id:7"
```

See [Operations → Backup freshness](operations.md#backup-freshness-caveats) for
guidance on choosing freshness windows.
