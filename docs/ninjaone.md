# NinjaOne integration

The collector is RMM-agnostic — it prints a `KEY=VALUE` block and returns an
exit code — but ships with a ready-to-use NinjaOne wrapper,
[`examples/ninjaone.ps1`](../examples/ninjaone.ps1).

## How it fits together

A Synology NAS is not a NinjaOne agent, so it cannot run scripts itself. Run the
wrapper on a **Windows agent at the same site** — any managed server or PC that
can reach the NAS over the network. Two consequences:

- The collector binary and the script live on that agent.
- `Ninja-Property-Get`/`Ninja-Property-Set` read and write **that agent's**
  custom fields, so the Synology data surfaces on the proxy device and your
  conditions target it there. (These cmdlets only work in an agent-run script
  context.)

## What the wrapper does

1. Ensures `synologycollector.exe` is on the agent, **downloading it from GitHub
   Releases on first run** (no separate binary deployment needed).
2. Reads host/credentials from NinjaOne custom fields, falling back to `DSM_*`
   environment variables.
3. Runs the collector and writes the full output to the **activity log** for
   troubleshooting.
4. Optionally maps the `KEY=VALUE` lines to **custom fields** via
   `Ninja-Property-Set` (set `$MapCustomFields = $true`).
5. Exits with the collector's exit code, so a NinjaOne **condition** on script
   result can notify a technician or open a ticket.

## Step 1 — Create the credential custom fields

Custom fields are configured in the Administration module.

1. Go to **Administration → Devices → Global Custom Fields** (or **Organization
   Custom Fields**, depending on the scope you want).
2. Click **Add → Field**.
3. Create the three credential fields:

| Field name (machine name) | Type | Holds |
|---|---|---|
| `dsmHost` | **Text** or **IP Address** | `https://nas:5001` or `host[:port]` |
| `dsmUsername` | **Text** | DSM service-account name |
| `dsmPassword` | **Secure** | DSM service-account password (encrypts the value and masks input) |

> ⚠️ **Critical — Script Permission.** While creating each field, scroll to
> **Script Permission** and set it to **Read Only** (or **Read/Write**). If it is
> left **None**, the PowerShell script is denied access and cannot read the
> value. Credential fields only need **Read Only** (the script reads them); the
> output fields in [Step 5](#step-5--optional-map-output-to-custom-fields) need
> **Read/Write** (the script writes them).

The DSM account must be in the **administrators** group with 2FA disabled — see
[Synology NAS setup](synology-setup.md).

## Step 2 — Assign the fields to your device role

If the fields don't already appear on the agent device, add them to its role:

1. Click the target **Role** to open its configuration.
2. Click **Add Field** and select your three fields (`dsmHost`, `dsmUsername`,
   `dsmPassword`).
3. Set the layout/section where they appear on the device page (e.g. a custom tab
   named **Synology Config**, or under **General**).
4. **Save** your changes.

Then populate the three fields on the proxy agent device (or at organization
scope).

## Step 3 — Add the script

1. Go to **Administration → Library → Automation**, add a **PowerShell** script,
   and paste [`examples/ninjaone.ps1`](../examples/ninjaone.ps1).
2. The script **downloads the collector binary itself** on first run — you do not
   deploy the `.exe` separately. It installs to
   `C:\ProgramData\SynologyCollector\synologycollector.exe` from a pinned GitHub
   Release URL (`$DownloadUrl`).
3. **To upgrade the collector**, edit `$DownloadUrl` to the new version (e.g.
   `.../releases/download/v0.2.0/...`) and set `$ForceDownload = $true` for one
   run, or delete the existing binary, so the new version is fetched.
4. Tune `$ExtraArgs` (thresholds, `--backup-max-age`, per-task overrides,
   `--tls-pin`) to taste.

The script reads `dsmHost` / `dsmUsername` / `dsmPassword` via
`Ninja-Property-Get` (already uncommented in the example) and falls back to
`DSM_*` environment variables. The endpoint needs outbound HTTPS to GitHub for
the download; on locked-down networks, push the `.exe` to that fixed path
yourself and the script will use it without downloading.

## Step 4 — Schedule it

Attach the script to a **scheduled automation** (policy or per-device) on the
proxy agent, at the cadence you want health checked (e.g. every 1–4 hours). Each
run refreshes the activity log and any mapped custom fields.

## Step 5 — (Optional) Map output to custom fields

To surface the collector's results as fields on the device, create the output
fields below (same **Add → Field** flow as Step 1, but set **Script Permission =
Read/Write** since the script *writes* them), assign them to the role as in
Step 2, then set `$MapCustomFields = $true` in the script. The field names mirror
`$FieldMap` — keep the two in sync if you add or rename any.

| Custom field (machine name) | Suggested type | Source KV key | Notes |
|---|---|---|---|
| `synoStatus` | Text | `STATUS` | `OK` / `WARNING` / `CRITICAL` / `ERROR` |
| `synoModel` | Text | `NAS` | e.g. `DS723+` |
| `synoHostname` | Text | `HOSTNAME` | NAS name, e.g. `jellyflame` |
| `synoUptime` | Text | `UPTIME` | humanized, e.g. `9d 14h 40m` — Text, not a number |
| `synoDsmVersion` | Text | `DSM` | e.g. `7.2.2` |
| `synoSystemHealth` | Text | `SYSTEM_HEALTH` | worst of pool/volume/drive |
| `synoStoragePool` | Text | `STORAGE_POOL` | e.g. `Healthy` |
| `synoVolumeUsage` | Text | `VOLUME_USAGE` | includes the `%` sign (e.g. `68%`) — Text, not Integer |
| `synoAbbFailed` | Integer | `ABB_FAILED` | failed count over the monitored set |
| `synoAbbOverdue` | Integer | `ABB_OVERDUE` | overdue count over the monitored set |
| `synoLastSuccess` | Text | `LAST_SUCCESS` | a timestamp **or** `never` / `Unknown` / `N/A`, so keep it Text |
| `synoCollectedAt` | Text | `COLLECTED_AT` | RFC3339 UTC run time (see [stale-run note](#detecting-the-collector-hasnt-run-recently)) |
| `synoSummary` | Text (multi-line) | `SUMMARY` | one-line human summary |

> `synoVolumeUsage` and `synoLastSuccess` are intentionally **Text**: the
> collector emits `68%` (not a bare number) and `LAST_SUCCESS` can be a non-date
> sentinel. Integer/Date fields there would drop those values.

## Step 6 — Alert with conditions

Two complementary ways to raise alerts:

- **On the script result.** Add a **Script Result** condition: exit `1` =
  warning, `2` = critical, `3` = collector/auth/connectivity error. Simplest, and
  needs no custom fields.
- **On the mapped fields.** Add custom-field conditions such as
  `synoAbbFailed > 0`, `synoAbbOverdue > 0`, or `synoStatus = CRITICAL` when you
  want distinct tickets per problem type.

### Detecting "the collector hasn't run recently"

A crashed scheduler or an offline proxy means the script stops running — and a
check that never runs never alerts. Cover it RMM-side:

- Map `COLLECTED_AT` to `synoCollectedAt` (Step 5) and add a condition that fires
  when it is older than your schedule interval.
- If your NinjaOne instance parses RFC3339 into a **Date/Time** field, use that
  type for `synoCollectedAt` and its built-in "older than" comparison. If not,
  keep it **Text** and fall back to NinjaOne's native **agent-offline** condition
  on the proxy device as a backstop.

## Step 7 — (Optional) Render the report in a WYSIWYG field

To show the full styled report on the device page, push it into a **WYSIWYG**
custom field:

1. **Add → Field**, type **WYSIWYG**, machine name `synoReport`, **Script
   Permission = Read/Write**. Assign it to the device role (Step 2).
2. In the script, `$ReportField = 'synoReport'` (already set in the example; set
   it to `''` to disable). The script runs the collector with
   `--html-embed-file` and pushes the result with `Ninja-Property-Set-Piped`
   (piped because the HTML is larger than a command-line argument should carry).

> ⚠️ **It must be a WYSIWYG field, not an Attachment field.** Attachment fields
> are **read-only to automations** — a script cannot write one — so that route
> can't work. WYSIWYG is the writable rich-text option.

> ⚠️ **Why a separate fragment.** WYSIWYG editors **sanitize out `<style>` and
> `<script>`**, so the standalone `--html-file` page would render unstyled in a
> field. The collector's `--html-embed-file` emits an **inline-styled** fragment
> built for exactly this — it keeps its status colors inside the field. Use
> `--html-file` for a browser/share/ticket copy, `--html-embed-file` for the
> field. See [Output → Embedding](output.md#embedding-in-a-rich-text--wysiwyg-field---html-embed-file).

## Using another RMM

Any RMM that can run a binary on a schedule and read its exit code works. Point
your platform at the same `KEY=VALUE` and exit-code contract in
[Output & exit codes](output.md); the NinjaOne script is just a reference
implementation of that pattern.
