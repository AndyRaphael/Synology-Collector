#!/usr/bin/env bash
# Cross-compile Synology Collector for the RMM-relevant platforms.
#
# Usage: ./build.sh [VERSION]
#   VERSION defaults to "dev". Example: ./build.sh 0.1.0
#
# Output lands in ./dist/. CGO is disabled so every target is a static,
# dependency-free executable. One file name is written per target:
#   synologycollector_<os>_<arch>[.exe]
# The name is stable across versions, so a releases/latest/download URL (and a
# pinned releases/download/<tag>/ URL) stays constant. The version is baked into
# the binary instead — `synologycollector --version` reports it. A
# checksums.txt (SHA-256) covering every artifact is written last.
set -euo pipefail

VERSION="${1:-dev}"
OUTDIR="dist"
LDFLAGS="-s -w -X main.version=${VERSION}"

rm -rf "${OUTDIR}"
mkdir -p "${OUTDIR}"

# platform  GOOS     GOARCH   name suffix
targets=(
  "windows amd64 windows_amd64.exe"
  "linux   amd64 linux_amd64"
  "linux   arm64 linux_arm64"
  "darwin  arm64 darwin_arm64"
)

for t in "${targets[@]}"; do
  read -r goos goarch suffix <<<"${t}"
  out="synologycollector_${suffix}"
  echo "building ${goos}/${goarch} -> ${OUTDIR}/${out}"
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "${OUTDIR}/${out}" .
done

# SHA-256 checksums for every artifact (sha256sum on Linux, shasum on macOS).
echo "writing ${OUTDIR}/checksums.txt"
(
  cd "${OUTDIR}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -- synologycollector_* >checksums.txt
  else
    shasum -a 256 -- synologycollector_* >checksums.txt
  fi
)

echo
echo "done. artifacts:"
ls -lh "${OUTDIR}"

# PowerShell equivalent for a single Windows build (no bash):
#   $env:CGO_ENABLED=0; $env:GOOS="windows"; $env:GOARCH="amd64"
#   go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o synologycollector.exe .
