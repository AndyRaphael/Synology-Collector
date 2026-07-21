# Installation & building

Synology Collector is a single self-contained binary with no runtime
dependencies. Download a prebuilt binary from a GitHub Release or build from
source.

## Download a prebuilt binary

Prebuilt binaries are attached to each [GitHub Release](https://github.com/AndyRaphael/Synology-Collector/releases).
(They are **not** committed to the repo — `dist/` is a local build directory and
is gitignored.) Every release ships two names per platform plus a
`checksums.txt`:

| Platform | Stable name (always latest) | Versioned name |
|----------|-----------------------------|----------------|
| Windows x86-64 | `synologycollector_windows_amd64.exe` | `synologycollector_<ver>_windows_amd64.exe` |
| Linux x86-64 | `synologycollector_linux_amd64` | `synologycollector_<ver>_linux_amd64` |
| Linux ARM64 | `synologycollector_linux_arm64` | `synologycollector_<ver>_linux_arm64` |
| macOS Apple silicon | `synologycollector_darwin_arm64` | `synologycollector_<ver>_darwin_arm64` |

The **versioned** name is self-describing — good for archiving and support
tickets. The **stable** name is byte-identical and never changes, so a
`releases/latest/download/` URL stays constant across versions (see below).

The binary is statically linked (`CGO_ENABLED=0`) and needs no installer,
service, or agent — your RMM invokes it directly. The version is baked in at
build time, so `synologycollector --version` and the `COLLECTOR_VERSION` output
field report it regardless of which filename you downloaded.

## Automating the "latest" download

Two approaches, neither of which depends on knowing the version number.

**Stable permalink (simplest).** A fixed URL that always redirects to the newest
release's asset of that name — ideal for an RMM deployment script:

```powershell
# Windows / PowerShell
$repo = "AndyRaphael/Synology-Collector"
Invoke-WebRequest "https://github.com/$repo/releases/latest/download/synologycollector_windows_amd64.exe" `
  -OutFile synologycollector.exe
```

```bash
# Linux / macOS
repo=AndyRaphael/Synology-Collector
curl -fsSL -o synologycollector \
  "https://github.com/$repo/releases/latest/download/synologycollector_linux_amd64"
chmod +x synologycollector
```

> The `latest/download` URL only resolves if the release is marked as the repo's
> **Latest** release — not a draft or pre-release. If it 404s, mark the release as
> Latest (`gh release edit <tag> --latest`, or the checkbox in the Releases UI) or
> pin to a version: `.../releases/download/v0.1.0/synologycollector_windows_amd64.exe`.
> The [release workflow](#releasing-a-new-version) marks non-pre-release tags as
> Latest automatically.

**Releases API (discovers the version and checksum).** Use when you want to log
which version you fetched or verify it against `checksums.txt`:

```powershell
# Windows / PowerShell
$repo  = "AndyRaphael/Synology-Collector"
$rel   = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
$asset = $rel.assets | Where-Object name -like "*windows_amd64.exe" | Select-Object -First 1
Invoke-WebRequest $asset.browser_download_url -OutFile synologycollector.exe
$rel.tag_name   # the version you just fetched
```

```bash
# Linux / macOS (needs jq)
repo=AndyRaphael/Synology-Collector
url=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" \
  | jq -r '.assets[] | select(.name|endswith("linux_amd64")) | .browser_download_url')
curl -fsSL -o synologycollector "$url"
```

The unauthenticated API is rate-limited to 60 requests/hour **per source IP**;
across separate customer sites (each its own egress IP) that is ample, but the
permalink avoids the limit entirely.

## Build from source

Building requires the Go toolchain (see [Toolchain](#toolchain) below).

Cross-compile every platform into `./dist/` (versioned names, stable aliases,
and `checksums.txt` — exactly what a release ships):

```bash
./build.sh 0.1.0
```

Single-target build (e.g. on Windows PowerShell):

```powershell
$env:CGO_ENABLED=0; $env:GOOS="windows"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o synologycollector.exe .
```

The `-X main.version=...` ldflag stamps the version reported by `--version`
and in the `COLLECTOR_VERSION` output field.

Releases are built automatically by GitHub Actions on version tags — see
[Releasing a new version](#releasing-a-new-version).

## Toolchain

`go.mod` pins the compiler:

```
go 1.25
toolchain go1.26.5
```

The `go` command auto-downloads the pinned toolchain, so builds are
reproducible regardless of the locally installed Go version. To update, bump
the `toolchain` line to the latest patch from <https://go.dev/dl> and rebuild.

## Releasing a new version

CI (`.github/workflows/release.yml`) builds and publishes **only on version
tags** — a normal push to a branch never produces a release. To cut one:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow runs the tests, derives the version from the tag (`v0.1.0` →
`0.1.0`), runs `build.sh`, and creates a GitHub Release with all binaries and
`checksums.txt` attached. A hyphenated tag (`v0.2.0-rc1`) is published as a
**pre-release**; any other tag is marked as the repo's **Latest** release, so the
`releases/latest/download/` permalinks resolve to it. Use a pre-1.0 tag like
`v0.1.0` while the tool is still stabilizing.
