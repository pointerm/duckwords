#!/usr/bin/env bash

# Build reproducible, checksummed release archives without publishing them. This
# script intentionally accepts metadata only through validated environment values,
# so neither a tag name nor manual workflow input can inject linker options.
set -euo pipefail
umask 022

readonly expected_go_version="go1.26.6"
readonly module="github.com/pointerm/duckwords"
readonly buildinfo_package="${module}/internal/buildinfo"
readonly repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

: "${VERSION:?VERSION is required}"
: "${COMMIT:?COMMIT is required}"
: "${BUILD_DATE:?BUILD_DATE is required}"
: "${SOURCE_DATE_EPOCH:?SOURCE_DATE_EPOCH is required}"

if [[ ! "${VERSION}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]]; then
  echo "VERSION must use the restricted release grammar: SemVer core, optional prerelease, no leading v/build metadata" >&2
  exit 2
fi
if [[ ! "${COMMIT}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "COMMIT must be a full lowercase Git commit SHA" >&2
  exit 2
fi
if [[ ! "${BUILD_DATE}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  echo "BUILD_DATE must be a UTC RFC 3339 timestamp" >&2
  exit 2
fi
if [[ ! "${SOURCE_DATE_EPOCH}" =~ ^[0-9]+$ ]]; then
  echo "SOURCE_DATE_EPOCH must be an integer Unix timestamp" >&2
  exit 2
fi
if [[ "$(GOFIPS140=off go env GOVERSION)" != "${expected_go_version}" ]]; then
  echo "release packaging requires ${expected_go_version}" >&2
  exit 2
fi

readonly output_dir="${OUTPUT_DIR:-dist}"
if [[ -e "${output_dir}" ]]; then
  if [[ ! -d "${output_dir}" || -L "${output_dir}" ]]; then
    echo "OUTPUT_DIR must be a real directory, not a file or symbolic link" >&2
    exit 2
  fi
  if find "${output_dir}" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    echo "OUTPUT_DIR must be empty to prevent stale release contents" >&2
    exit 2
  fi
else
  mkdir -p -- "${output_dir}"
fi
output_abs="$(cd "${output_dir}" && pwd -P)"
if [[ -z "${output_abs}" ]]; then
  echo "failed to resolve OUTPUT_DIR" >&2
  exit 2
fi
readonly output_abs

readonly temp_parent="${TMPDIR:-/tmp}"
stage_root="$(mktemp -d "${temp_parent%/}/duckwords-release.XXXXXX")"
readonly stage_root
cleanup() {
  if [[ -n "${stage_root}" && -d "${stage_root}" &&
    "${stage_root}" == "${temp_parent%/}/duckwords-release."* ]]; then
    rm -rf -- "${stage_root}"
  fi
}
trap cleanup EXIT

readonly ldflags="-s -w -buildid= -X ${buildinfo_package}.version=${VERSION} -X ${buildinfo_package}.commit=${COMMIT} -X ${buildinfo_package}.buildDate=${BUILD_DATE}"
readonly targets=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)
archives=()

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  binary="duckwords"
  if [[ "${goos}" == "windows" ]]; then
    binary="duckwords.exe"
  fi

  package="duckwords_${VERSION}_${goos}_${goarch}"
  package_dir="${stage_root}/${package}"
  mkdir -- "${package_dir}"

  CGO_ENABLED=0 GOFIPS140=off GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= \
    GOOS="${goos}" GOARCH="${goarch}" \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags "${ldflags}" \
    -o "${package_dir}/${binary}" ./cmd/duckwords
  cp -- THIRD_PARTY_NOTICES.md "${package_dir}/THIRD_PARTY_NOTICES.md"
  chmod 0755 "${package_dir}/${binary}"
  chmod 0644 "${package_dir}/THIRD_PARTY_NOTICES.md"
  touch --date="@${SOURCE_DATE_EPOCH}" \
    "${package_dir}" \
    "${package_dir}/${binary}" \
    "${package_dir}/THIRD_PARTY_NOTICES.md"

  archive="${package}.tar.gz"
  tar --sort=name --format=ustar --owner=0 --group=0 --numeric-owner \
    --mtime="@${SOURCE_DATE_EPOCH}" -C "${stage_root}" -cf - "${package}" |
    gzip -n -9 > "${output_abs}/${archive}"
  archives+=("${archive}")
done

expected_version="duckwords version=${VERSION} commit=${COMMIT} built=${BUILD_DATE} go=${expected_go_version}"
actual_version="$("${stage_root}/duckwords_${VERSION}_linux_amd64/duckwords" --version)"
if [[ "${actual_version}" != "${expected_version}" ]]; then
  echo "packaged binary reported unexpected build metadata" >&2
  exit 1
fi

(
  cd "${output_abs}"
  sha256sum "${archives[@]}" > SHA256SUMS
)

printf 'packaged DuckWords %s (%s) into %s\n' "${VERSION}" "${COMMIT}" "${output_abs}"
