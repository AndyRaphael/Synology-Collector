<#
.SYNOPSIS
    NinjaOne wrapper for Synology Collector.

.DESCRIPTION
    Ensures synologycollector.exe is present on the agent (downloading it from
    GitHub Releases on first run), runs it against a Synology NAS, writes the full
    output to the NinjaOne activity log for troubleshooting, optionally maps the
    KEY=VALUE lines to NinjaOne custom fields, and exits with the collector's own
    exit code so NinjaOne conditions can trigger on it:

        0 = healthy    1 = warning    2 = critical    3 = collector/auth/connectivity error

.NOTES
    Credentials come from NinjaOne custom fields (dsmHost, dsmUsername, dsmPassword)
    or, as a fallback, DSM_* environment variables. Never hard-code a password here.
    The DSM account should be a dedicated service account (administrators group,
    2FA disabled). See docs/ninjaone.md and docs/synology-setup.md for setup.

    Set $MapCustomFields = $true and adjust $FieldMap once you have created the
    matching output custom fields. Ninja-Property-Get/Set only work in an
    agent-run script context.
#>

# --- Configuration -----------------------------------------------------------

# The collector binary is installed to a FIXED path, not alongside this script:
# NinjaOne runs the script from its own temporary directory, so $PSScriptRoot
# would not point anywhere you control. On first run (or when $ForceDownload is
# set) the binary is downloaded from GitHub Releases to $Exe.
#
# TO UPDATE THE COLLECTOR: change the version in $DownloadUrl below (e.g.
# v0.1.0 -> v0.2.0), then either delete the existing binary or set
# $ForceDownload = $true for one run so the new version is fetched.
$InstallDir    = 'C:\ProgramData\SynologyCollector'
$Exe           = Join-Path $InstallDir 'synologycollector.exe'
$DownloadUrl   = 'https://github.com/AndyRaphael/Synology-Collector/releases/download/v0.1.0/synologycollector_windows_amd64.exe'
$ForceDownload = $false

# Credentials. Read from NinjaOne custom fields, falling back to environment
# variables if a field is empty. Never hard-code a password in this script.
$DsmHost     = $env:DSM_HOST
$DsmUsername = $env:DSM_USERNAME
$DsmPassword = $env:DSM_PASSWORD

if (-not $DsmHost)     { $DsmHost     = Ninja-Property-Get dsmHost }
if (-not $DsmUsername) { $DsmUsername = Ninja-Property-Get dsmUsername }
if (-not $DsmPassword) { $DsmPassword = Ninja-Property-Get dsmPassword }

# Extra collector arguments (thresholds, per-task overrides, TLS pin, etc.).
$ExtraArgs = @(
    '--vol-warn', '80',
    '--vol-crit', '90',
    '--backup-max-age', '24h'
    # '--tls-pin', '<sha256-fingerprint>'
)

# Map KV output to NinjaOne custom fields? Requires the output fields to exist
# first, each with Script Permission = Read/Write.
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

# --- Validate credentials ----------------------------------------------------

if (-not $DsmHost -or -not $DsmUsername -or -not $DsmPassword) {
    Write-Output "STATUS=ERROR"
    Write-Output "ERROR=missing DSM_HOST, DSM_USERNAME, or DSM_PASSWORD"
    exit 3
}

# --- Ensure the collector binary is present ----------------------------------

if ($ForceDownload -and (Test-Path $Exe)) {
    Remove-Item $Exe -Force -ErrorAction SilentlyContinue
}

if (-not (Test-Path $Exe)) {
    try {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        # TLS 1.2 is required for GitHub on Windows PowerShell 5.1.
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $DownloadUrl -OutFile $Exe -UseBasicParsing
    } catch {
        Write-Output "STATUS=ERROR"
        Write-Output "ERROR=failed to download collector from $DownloadUrl : $_"
        exit 3
    }
}

if (-not (Test-Path $Exe)) {
    Write-Output "STATUS=ERROR"
    Write-Output "ERROR=collector binary not found at $Exe"
    exit 3
}

# --- Run ---------------------------------------------------------------------

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
