<#
.SYNOPSIS
    NinjaOne wrapper for Synology Collector.

.DESCRIPTION
    Runs synologycollector.exe against a Synology NAS, writes the full output to the
    NinjaOne activity log for troubleshooting, optionally maps the KEY=VALUE lines to
    NinjaOne custom fields, and exits with the collector's own exit code so NinjaOne
    conditions can trigger on it:

        0 = healthy    1 = warning    2 = critical    3 = collector/auth/connectivity error

.NOTES
    Provide credentials via NinjaOne-injected environment variables or custom fields.
    Never hard-code a password in this script. The DSM account should be a dedicated
    service account (administrators group, 2FA disabled) — see README.md.

    Set $MapCustomFields = $true and adjust $FieldMap once you have created matching
    custom fields in NinjaOne. Ninja-Property-Set is only available on agent-run
    script policies.
#>

# --- Configuration -----------------------------------------------------------

# Path to the collector binary (deploy alongside this script or to a fixed path).
$Exe = Join-Path $PSScriptRoot 'synologycollector.exe'

# Credentials. Prefer NinjaOne environment variables or secure custom fields.
$DsmHost     = $env:DSM_HOST
$DsmUsername = $env:DSM_USERNAME
$DsmPassword = $env:DSM_PASSWORD

# Optional: also allow reading from NinjaOne custom fields if env vars are empty.
# if (-not $DsmHost)     { $DsmHost     = Ninja-Property-Get dsmHost }
# if (-not $DsmUsername) { $DsmUsername = Ninja-Property-Get dsmUsername }
# if (-not $DsmPassword) { $DsmPassword = Ninja-Property-Get dsmPassword }

# Extra collector arguments (thresholds, per-task overrides, TLS pin, etc.).
$ExtraArgs = @(
    '--vol-warn', '80',
    '--vol-crit', '90',
    '--backup-max-age', '24h'
    # '--tls-pin', '<sha256-fingerprint>'
)

# Map KV output to NinjaOne custom fields? Requires the fields to exist first.
$MapCustomFields = $false
$FieldMap = @{
    'STATUS'         = 'synoStatus'
    'NAS'            = 'synoModel'
    'DSM'            = 'synoDsmVersion'
    'SYSTEM_HEALTH'  = 'synoSystemHealth'
    'STORAGE_POOL'   = 'synoStoragePool'
    'VOLUME_USAGE'   = 'synoVolumeUsage'
    'ABB_FAILED'     = 'synoAbbFailed'
    'ABB_OVERDUE'    = 'synoAbbOverdue'
    'LAST_SUCCESS'   = 'synoLastSuccess'
    'COLLECTED_AT'   = 'synoCollectedAt'
    'SUMMARY'        = 'synoSummary'
}

# --- Run ---------------------------------------------------------------------

if (-not (Test-Path $Exe)) {
    Write-Output "STATUS=ERROR"
    Write-Output "ERROR=collector binary not found at $Exe"
    exit 3
}
if (-not $DsmHost -or -not $DsmUsername -or -not $DsmPassword) {
    Write-Output "STATUS=ERROR"
    Write-Output "ERROR=missing DSM_HOST, DSM_USERNAME, or DSM_PASSWORD"
    exit 3
}

# Pass the password through the environment, NOT the command line, so it never
# appears in a process listing or RMM command-line log. The collector reads
# DSM_PASSWORD automatically.
$env:DSM_PASSWORD = $DsmPassword

$collectorArgs = @(
    '--host', $DsmHost,
    '--username', $DsmUsername,
    '--format', 'both'
) + $ExtraArgs

try {
    $output = & $Exe @collectorArgs 2>&1
    $code = $LASTEXITCODE
}
finally {
    # Scrub the secret from this session's environment.
    Remove-Item Env:\DSM_PASSWORD -ErrorAction SilentlyContinue
}

# Relay the full report to the NinjaOne activity log.
$output | ForEach-Object { Write-Output $_ }

# --- Optional: map KV lines to custom fields ---------------------------------

if ($MapCustomFields) {
    foreach ($line in $output) {
        if ($line -match '^(?<k>[A-Z_]+)=(?<v>.*)$') {
            $key = $Matches['k']
            $val = $Matches['v']
            if ($FieldMap.ContainsKey($key)) {
                try {
                    Ninja-Property-Set $FieldMap[$key] $val
                } catch {
                    Write-Output "WARN=could not set custom field $($FieldMap[$key]): $_"
                }
            }
        }
        # Stop mapping once the KV block ends (JSON section begins after '---').
        if ($line -eq '---') { break }
    }
}

exit $code
