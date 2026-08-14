#!/usr/bin/env bash

# Exercise the live-capture wrapper without Go compilation, network access, Reddit
# credentials, or assignment data. Each case gets a clean minimal Git repository;
# the fake Go tool deterministically rebuilds executable fixtures from tracked HEAD.
set -euo pipefail
umask 077

readonly expected_go_version="go1.26.6"
readonly fixture_approval="true"
readonly fixture_client_id="fixture-value-two"
readonly fixture_client_secret="fixture-value-three"
readonly fixture_user_agent="fixture-value-four"
readonly fixture_ambient_value="fixture-unrelated-value-five"
readonly fixture_version="1.2.3"
readonly fixture_command_path="${PATH}"
readonly fixture_home="${HOME}"
if [[ "${fixture_command_path}" == *$'\n'* || "${fixture_home}" == *$'\n'* ]]; then
  printf 'capture-submission test: PATH and HOME must not contain newline characters\n' >&2
  exit 1
fi
if [[ ! -x /bin/sh || ! -x /bin/mkdir || ! -x /bin/cp || ! -x /bin/cat ]]; then
  printf 'capture-submission test: required absolute POSIX tool path is unavailable\n' >&2
  exit 1
fi

# The harness never consumes caller credentials. Only the visibly synthetic values
# exported inside run_wrapper reach the process fixture.
unset REDDIT_API_ACCESS_APPROVED REDDIT_CLIENT_ID REDDIT_CLIENT_SECRET REDDIT_USER_AGENT \
  CAPTURE_TEST_AMBIENT_VALUE

for required_command in awk bash cmp cp date env find git grep mkdir mktemp rm sed; do
  if ! command -v "${required_command}" >/dev/null 2>&1; then
    printf 'capture-submission test: required command is unavailable: %s\n' \
      "${required_command}" >&2
    exit 1
  fi
done

if ! source_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"; then
  printf 'capture-submission test: source root could not be resolved\n' >&2
  exit 1
fi
readonly source_root
readonly source_wrapper="${source_root}/scripts/capture-submission.sh"
if [[ ! -f "${source_wrapper}" || -L "${source_wrapper}" ]]; then
  printf 'capture-submission test: wrapper under test must be a regular file\n' >&2
  exit 1
fi

readonly test_prefix="${TMPDIR:-/tmp}/duckwords-capture-test."
if ! test_root="$(mktemp -d "${test_prefix}XXXXXXXX")"; then
  printf 'capture-submission test: private test root could not be created\n' >&2
  exit 1
fi
readonly test_root
if [[ ! -d "${test_root}" || -L "${test_root}" || "${test_root}" != "${test_prefix}"* ]]; then
  printf 'capture-submission test: private test root was not created safely\n' >&2
  exit 1
fi

cleanup() {
  if [[ -z "${test_root:-}" || "${test_root}" != "${test_prefix}"* ||
    ! -d "${test_root}" || -L "${test_root}" ]]; then
    return
  fi
  # Refuse recursive cleanup if an unexpected link was planted inside the private
  # tree. The harness itself never creates links, so this turns corruption into a
  # visible, recoverable temporary directory instead of following an unsafe shape.
  if find "${test_root}" -type l -print -quit | grep -q .; then
    printf 'capture-submission test: refusing to clean a test tree containing a symbolic link: %s\n' \
      "${test_root}" >&2
    return
  fi
  rm -rf -- "${test_root}"
}
trap cleanup EXIT

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

assert_equal() {
  local expected="$1"
  local actual="$2"
  local context="$3"
  if [[ "${actual}" != "${expected}" ]]; then
    fail "${context}: got [${actual}], want [${expected}]"
  fi
}

assert_path_absent() {
  local path="$1"
  local context="$2"
  if [[ -e "${path}" || -L "${path}" ]]; then
    fail "${context}: unexpected path ${path}"
  fi
}

assert_regular_file() {
  local path="$1"
  local context="$2"
  if [[ ! -f "${path}" || -L "${path}" ]]; then
    fail "${context}: expected regular file ${path}"
  fi
}

assert_capture_lock_absent() {
  assert_path_absent "${case_repo}/artifacts/submission.capture-lock" \
    "${case_name}: wrapper-owned capture lock cleanup"
}

assert_contains() {
  local path="$1"
  local expected="$2"
  local context="$3"
  if ! grep -F -- "${expected}" "${path}" >/dev/null 2>&1; then
    fail "${context}: ${path} does not contain the expected text"
  fi
}

assert_event_count() {
  local expected="$1"
  local event="$2"
  local context="$3"
  local actual
  actual="$(awk -v expected_event="${event}" '$0 == expected_event { count++ } END { print count + 0 }' "${case_events}")"
  assert_equal "${expected}" "${actual}" "${context}"
}

assert_no_fixture_values() {
  local path="$1"
  local value
  for value in "${fixture_client_id}" "${fixture_client_secret}" "${fixture_user_agent}" \
    "${fixture_ambient_value}"; do
    if grep -F -- "${value}" "${path}" >/dev/null 2>&1; then
      fail "fixture environment value leaked into ${path}"
    fi
  done
}

assert_private_cleanup() {
  local remaining
  remaining="$(find "${case_tmp}" -mindepth 1 -print -quit)"
  if [[ -n "${remaining}" ]]; then
    fail "${case_name}: private build/capture state was not removed"
  fi
}

readonly mock_bin="${test_root}/mock-bin"
mkdir -m 0700 "${mock_bin}"

cat > "${mock_bin}/go" <<'MOCK_GO'
#!/bin/bash
set -euo pipefail

events_path=""
case "${TMPDIR:-}" in
  */tmp) events_path="${TMPDIR%/tmp}/state/events.log" ;;
  *) exit 90 ;;
esac

record() {
  printf '%s\n' "$1" >> "${events_path}"
}

untrusted_environment_present() {
  [[ -n "${REDDIT_API_ACCESS_APPROVED+x}" || -n "${REDDIT_CLIENT_ID+x}" ||
    -n "${REDDIT_CLIENT_SECRET+x}" || -n "${REDDIT_USER_AGENT+x}" ||
    -n "${CAPTURE_TEST_AMBIENT_VALUE+x}" || -n "${REDDIT_POLICY_VERIFIED_AT+x}" ||
    -n "${REDDIT_APPROVAL_REFERENCE+x}" || -n "${DUCKWORDS_RELEASE_VERSION+x}" ||
    -n "${DUCKWORDS_BUILD_DATE+x}" || -n "${SUBMISSION_DIR+x}" ]]
}

assert_reference_build_environment() {
  [[ "${TZ:-}" == "UTC" && "${CGO_ENABLED:-}" == "0" && "${GOFLAGS+x}" == "x" &&
    -z "${GOFLAGS}" && "${GOWORK:-}" == "off" && "${GOENV:-}" == "off" &&
    "${GOEXPERIMENT+x}" == "x" && -z "${GOEXPERIMENT}" &&
    "${GOTOOLCHAIN:-}" == "go1.26.6" && -n "${PATH:-}" && -n "${TMPDIR:-}" &&
    -n "${HOME:-}" ]] || return 1
  [[ "${GOFIPS140+x}" == "x" && "${GOFIPS140}" == "off" ]] || return 1
  untrusted_environment_present && return 1
  return 0
}

if [[ "$#" -eq 2 && "$1" == "env" && "$2" == "GOVERSION" ]]; then
  [[ "${GOTOOLCHAIN:-}" == "go1.26.6" ]] || exit 91
  if [[ -n "${REDDIT_CLIENT_ID+x}" || -n "${REDDIT_CLIENT_SECRET+x}" ||
    -n "${REDDIT_USER_AGENT+x}" || -n "${CAPTURE_TEST_AMBIENT_VALUE+x}" ]]; then
    record "go-env:environment-present"
  else
    record "go-env:environment-scrubbed"
  fi
  record "go-env:go1.26.6"
  printf '%s\n' 'go1.26.6'
  exit 0
fi

if [[ "$#" -lt 1 || "$1" != "build" ]]; then
  printf 'mock go: unsupported invocation\n' >&2
  exit 92
fi
shift

output=""
ldflags=""
package=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      [[ "$#" -ge 2 ]] || exit 92
      output="$2"
      shift 2
      ;;
    -ldflags)
      [[ "$#" -ge 2 ]] || exit 92
      ldflags="$2"
      shift 2
      ;;
    *)
      package="$1"
      shift
      ;;
  esac
done

[[ -n "${output}" ]] || exit 92
if ! assert_reference_build_environment; then
  record "build:${package}:environment-leaked"
  exit 93
fi
mkdir -p -- "$(dirname "${output}")"

IFS= read -r expected_status < test-fixtures/expectations || exit 95
IFS= read -r expected_policy < <(sed -n '2p' test-fixtures/expectations) || exit 95
IFS= read -r expected_approval < <(sed -n '3p' test-fixtures/expectations) || exit 95
IFS= read -r expected_finalizer_mode < <(sed -n '4p' test-fixtures/expectations) || exit 95

case "${package}" in
  ./cmd/duckwords)
    if [[ ! "${ldflags}" =~ \.version=([^\'[:space:]]+) ]]; then
      exit 94
    fi
    embedded_version="${BASH_REMATCH[1]}"
    if [[ ! "${ldflags}" =~ \.commit=([0-9a-f]{40}) ]]; then
      exit 94
    fi
    embedded_commit="${BASH_REMATCH[1]}"
    if [[ ! "${ldflags}" =~ \.buildDate=([^\'[:space:]]+) ]]; then
      exit 94
    fi
    embedded_build_date="${BASH_REMATCH[1]}"
    while IFS= read -r fixture_line || [[ -n "${fixture_line}" ]]; do
      fixture_line="${fixture_line//@EVENTS@/${events_path}}"
      fixture_line="${fixture_line//@VERSION@/${embedded_version}}"
      fixture_line="${fixture_line//@COMMIT@/${embedded_commit}}"
      fixture_line="${fixture_line//@BUILD_DATE@/${embedded_build_date}}"
      fixture_line="${fixture_line//@APP_STATUS@/${expected_status}}"
      printf '%s\n' "${fixture_line}"
    done < test-fixtures/duckwords-template > "${output}"
    chmod 0755 "${output}"
    record "build:duckwords:environment-scrubbed"
    ;;
  ./cmd/duckwords-evidence)
    while IFS= read -r fixture_line || [[ -n "${fixture_line}" ]]; do
      fixture_line="${fixture_line//@EVENTS@/${events_path}}"
      fixture_line="${fixture_line//@APP_STATUS@/${expected_status}}"
      fixture_line="${fixture_line//@POLICY_DATE@/${expected_policy}}"
      fixture_line="${fixture_line//@APPROVAL_REFERENCE@/${expected_approval}}"
      fixture_line="${fixture_line//@FINALIZER_MODE@/${expected_finalizer_mode}}"
      printf '%s\n' "${fixture_line}"
    done < test-fixtures/evidence-template > "${output}"
    chmod 0755 "${output}"
    record "build:duckwords-evidence:environment-scrubbed"
    ;;
  *)
    printf 'mock go: unsupported build package %s\n' "${package}" >&2
    exit 92
    ;;
esac
MOCK_GO
chmod 0755 "${mock_bin}/go"

case_name=""
case_repo=""
case_state=""
case_tmp=""
case_events=""
case_sha=""
case_app_status=""
case_finalizer_mode=""
case_policy_date=""
case_approval_reference=""
case_build_date=""

setup_case() {
  case_name="$1"
  case_app_status="$2"
  local existing_target="${3:-false}"
  case_finalizer_mode="${4:-normal}"
  local case_root="${test_root}/cases/${case_name}"
  case_repo="${case_root}/repo"
  case_state="${case_root}/state"
  case_tmp="${case_root}/tmp"
  case_events="${case_state}/events.log"

  mkdir -p "${case_repo}/scripts" "${case_repo}/bin" "${case_repo}/artifacts" \
    "${case_repo}/cmd/duckwords" "${case_repo}/cmd/duckwords-evidence" \
    "${case_repo}/test-fixtures" "${case_state}" "${case_tmp}"
  cp -- "${source_wrapper}" "${case_repo}/scripts/capture-submission.sh"
  chmod 0755 "${case_repo}/scripts/capture-submission.sh"

cat > "${case_repo}/.gitignore" <<'EOF_GITIGNORE'
/bin/
/artifacts/**/*.capture-lock/
EOF_GITIGNORE
  cat > "${case_repo}/go.mod" <<'EOF_GO_MOD'
module github.com/pointerm/duckwords

go 1.25.0

toolchain go1.26.6
EOF_GO_MOD
  case_policy_date="$(date -u '+%Y-%m-%d')"
  case_build_date="${case_policy_date}T00:00:00Z"
  case_approval_reference="approval-fixture-${case_policy_date}"
  printf '%s\n%s\n%s\n%s\n' "${case_app_status}" "${case_policy_date}" \
    "${case_approval_reference}" "${case_finalizer_mode}" > "${case_repo}/test-fixtures/expectations"

  cat > "${case_repo}/test-fixtures/duckwords-template" <<'DUCKWORDS_FIXTURE'
#!/bin/sh
set -eu

events_path="@EVENTS@"
record() {
  printf '%s\n' "$1" >> "${events_path}"
}

if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  if [ "${TZ:-}" != "UTC" ] || [ -n "${REDDIT_API_ACCESS_APPROVED+x}" ] ||
    [ -n "${REDDIT_CLIENT_ID+x}" ] || [ -n "${REDDIT_CLIENT_SECRET+x}" ] ||
    [ -n "${REDDIT_USER_AGENT+x}" ] || [ -n "${CAPTURE_TEST_AMBIENT_VALUE+x}" ] ||
    [ -n "${REDDIT_POLICY_VERIFIED_AT+x}" ] || [ -n "${REDDIT_APPROVAL_REFERENCE+x}" ] ||
    [ -n "${DUCKWORDS_RELEASE_VERSION+x}" ] || [ -n "${DUCKWORDS_BUILD_DATE+x}" ] ||
    [ -n "${SUBMISSION_DIR+x}" ]; then
    record "version:environment-leaked"
    exit 81
  fi
  record "version:environment-scrubbed"
  printf '%s\n' 'duckwords version=@VERSION@ commit=@COMMIT@ built=@BUILD_DATE@ go=go1.26.6'
  exit 0
fi

if [ "$#" -ne 9 ] ||
  [ "$1" != "--workers=4" ] ||
  [ "$2" != "--rate-limit=0.8" ] ||
  [ "$3" != "--request-timeout=20s" ] ||
  [ "$4" != "--timeout=30m" ] ||
  [ "$5" != "--max-retries=3" ] ||
  [ "$6" != "--retry-budget=45s" ] ||
  [ "$7" != "--failure-mode=best-effort" ] ||
  [ "$8" != "--log-level=info" ] ||
  [ "$9" != "--log-format=json" ]; then
  record "live:arguments-invalid"
  exit 82
fi
record "live:arguments-verified"

if command -v capture_test_exported_function >/dev/null 2>&1; then
  record "live:exported-function-leaked"
  exit 86
fi

if [ "${REDDIT_API_ACCESS_APPROVED:-}" != "true" ] ||
  [ "${REDDIT_CLIENT_ID:-}" != "fixture-value-two" ] ||
  [ "${REDDIT_CLIENT_SECRET:-}" != "fixture-value-three" ] ||
  [ "${REDDIT_USER_AGENT:-}" != "fixture-value-four" ]; then
  record "live:credentials-missing"
  exit 83
fi
record "live:credentials-present"
if [ "${TZ:-}" != "UTC" ] || [ -n "${CAPTURE_TEST_AMBIENT_VALUE+x}" ] ||
  [ -n "${REDDIT_POLICY_VERIFIED_AT+x}" ] || [ -n "${REDDIT_APPROVAL_REFERENCE+x}" ] ||
  [ -n "${DUCKWORDS_RELEASE_VERSION+x}" ] || [ -n "${DUCKWORDS_BUILD_DATE+x}" ] ||
  [ -n "${SUBMISSION_DIR+x}" ]; then
  record "live:environment-leaked"
  exit 85
fi
record "live:environment-scrubbed"

printf '[]\n'
printf '%s\n' '{"event":"fixture_run"}' >&2
case "@APP_STATUS@" in
  0) exit 0 ;;
  1) exit 1 ;;
  3) exit 3 ;;
  *) exit 84 ;;
esac
DUCKWORDS_FIXTURE

  cat > "${case_repo}/test-fixtures/evidence-template" <<'EVIDENCE_FIXTURE'
#!/bin/sh
set -eu

events_path="@EVENTS@"
record() {
  printf '%s\n' "$1" >> "${events_path}"
}

if [ "${TZ:-}" != "UTC" ] || [ -n "${REDDIT_API_ACCESS_APPROVED+x}" ] ||
  [ -n "${REDDIT_CLIENT_ID+x}" ] || [ -n "${REDDIT_CLIENT_SECRET+x}" ] ||
  [ -n "${REDDIT_USER_AGENT+x}" ] || [ -n "${CAPTURE_TEST_AMBIENT_VALUE+x}" ] ||
  [ -n "${REDDIT_POLICY_VERIFIED_AT+x}" ] || [ -n "${REDDIT_APPROVAL_REFERENCE+x}" ] ||
  [ -n "${DUCKWORDS_RELEASE_VERSION+x}" ] || [ -n "${DUCKWORDS_BUILD_DATE+x}" ] ||
  [ -n "${SUBMISSION_DIR+x}" ]; then
  record "finalizer:environment-leaked"
  exit 71
fi
record "finalizer:environment-scrubbed"

result=""
log=""
output_dir=""
exit_code=""
binary=""
policy_verified_at=""
approval_reference=""
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] || exit 72
  case "$1" in
    --result) result="$2" ;;
    --log) log="$2" ;;
    --output-dir) output_dir="$2" ;;
    --exit-code) exit_code="$2" ;;
    --binary) binary="$2" ;;
    --policy-verified-at) policy_verified_at="$2" ;;
    --approval-reference) approval_reference="$2" ;;
    *) exit 72 ;;
  esac
  shift 2
done

[ -f "${result}" ] && [ ! -L "${result}" ] || exit 73
[ -f "${log}" ] && [ ! -L "${log}" ] || exit 73
[ -x "${binary}" ] && [ ! -L "${binary}" ] || exit 73
[ ! -e "${output_dir}" ] && [ ! -L "${output_dir}" ] || exit 73
[ "${exit_code}" = "@APP_STATUS@" ] || exit 73
[ "${policy_verified_at}" = "@POLICY_DATE@" ] || exit 73
[ "${approval_reference}" = "@APPROVAL_REFERENCE@" ] || exit 73

/bin/mkdir "${output_dir}"
/bin/cp "${result}" "${output_dir}/result.json"
/bin/cp "${log}" "${output_dir}/application.log"
{
  /bin/cat "${log}"
  printf '%s\n' '--- result.json ---'
  /bin/cat "${result}"
} > "${output_dir}/full-application.log"
printf '%s\n' '{"schema_version":1,"fixture":true}' > "${output_dir}/run-manifest.json"
printf '%s\n' '# Offline capture fixture' > "${output_dir}/RUN.md"
record "finalizer:exit=${exit_code}"
if [ "@FINALIZER_MODE@" = "signal-after-publish" ]; then
  record "finalizer:signal-after-publish"
  kill -INT "${PPID}"
  /bin/sleep 1
elif [ "@FINALIZER_MODE@" = "fail-after-publish" ]; then
  record "finalizer:fail-after-publish"
  exit 1
fi
EVIDENCE_FIXTURE
  chmod 0755 "${case_repo}/test-fixtures/duckwords-template" \
    "${case_repo}/test-fixtures/evidence-template"

  if [[ "${existing_target}" == "true" ]]; then
    mkdir "${case_repo}/artifacts/submission"
    printf '%s\n' 'preserve-existing-evidence' > "${case_repo}/artifacts/submission/sentinel"
  else
    printf '%s\n' 'capture test artifact parent' > "${case_repo}/artifacts/README.md"
  fi

  (
    cd "${case_repo}"
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git init -q
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git config user.name 'DuckWords Capture Test'
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git config user.email 'capture-test.invalid@example.invalid'
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git add -A
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
      GIT_AUTHOR_NAME='DuckWords Capture Test' \
      GIT_AUTHOR_EMAIL='capture-test.invalid@example.invalid' \
      GIT_COMMITTER_NAME='DuckWords Capture Test' \
      GIT_COMMITTER_EMAIL='capture-test.invalid@example.invalid' \
      GIT_AUTHOR_DATE='2000-01-01T00:00:00Z' GIT_COMMITTER_DATE='2000-01-01T00:00:00Z' \
      git commit -q -m 'capture wrapper fixture'
  )
  case_sha="$(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -C "${case_repo}" rev-parse HEAD)"
  if [[ ! "${case_sha}" =~ ^[0-9a-f]{40}$ ]]; then
    fail "${case_name}: fixture Git repository did not use a 40-character SHA"
  fi

  : > "${case_events}"
  (
    cd "${case_repo}"
    env -u REDDIT_API_ACCESS_APPROVED -u REDDIT_CLIENT_ID -u REDDIT_CLIENT_SECRET \
      -u REDDIT_USER_AGENT -u CAPTURE_TEST_AMBIENT_VALUE -u REDDIT_POLICY_VERIFIED_AT \
      -u REDDIT_APPROVAL_REFERENCE -u DUCKWORDS_RELEASE_VERSION -u DUCKWORDS_BUILD_DATE \
      -u SUBMISSION_DIR \
      PATH="${mock_bin}:${fixture_command_path}" TMPDIR="${case_tmp}" HOME="${fixture_home}" TZ=UTC \
      CGO_ENABLED=0 GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= GOFIPS140=off \
      GOTOOLCHAIN="${expected_go_version}" \
      "${mock_bin}/go" build -mod=readonly -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X 'github.com/pointerm/duckwords/internal/buildinfo.version=${fixture_version}' -X 'github.com/pointerm/duckwords/internal/buildinfo.commit=${case_sha}' -X 'github.com/pointerm/duckwords/internal/buildinfo.buildDate=${case_build_date}'" \
      -o "${case_repo}/bin/duckwords" ./cmd/duckwords
    env -u REDDIT_API_ACCESS_APPROVED -u REDDIT_CLIENT_ID -u REDDIT_CLIENT_SECRET \
      -u REDDIT_USER_AGENT -u CAPTURE_TEST_AMBIENT_VALUE -u REDDIT_POLICY_VERIFIED_AT \
      -u REDDIT_APPROVAL_REFERENCE -u DUCKWORDS_RELEASE_VERSION -u DUCKWORDS_BUILD_DATE \
      -u SUBMISSION_DIR \
      PATH="${mock_bin}:${fixture_command_path}" TMPDIR="${case_tmp}" HOME="${fixture_home}" TZ=UTC \
      CGO_ENABLED=0 GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= GOFIPS140=off \
      GOTOOLCHAIN="${expected_go_version}" \
      "${mock_bin}/go" build -mod=readonly -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid=" -o "${case_repo}/bin/duckwords-evidence" \
      ./cmd/duckwords-evidence
  )
  : > "${case_events}"
  if [[ -n "$(GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -C "${case_repo}" status --porcelain=v1 --untracked-files=all)" ]]; then
    fail "${case_name}: fixture repository is not clean after ignored binary creation"
  fi
}

run_wrapper() {
  local expected_status="$1"
  local xtrace="${2:-false}"
  local exported_functions="${3:-false}"
  local invalid_environment_name="${4:-false}"
  local unprivileged_bash="${5:-false}"
  local actual_status
  : > "${case_state}/wrapper.stdout"
  : > "${case_state}/wrapper.stderr"

  set +e
  (
    cd "${case_repo}"
    export PATH="${mock_bin}:${fixture_command_path}"
    export HOME="${fixture_home}"
    export TMPDIR="${case_tmp}"
    export SUBMISSION_DIR="artifacts/submission"
    export GIT_CONFIG_GLOBAL=/dev/null
    export GIT_CONFIG_NOSYSTEM=1
    export DUCKWORDS_RELEASE_VERSION="${fixture_version}"
    export DUCKWORDS_BUILD_DATE="${case_build_date}"
    export REDDIT_POLICY_VERIFIED_AT="${case_policy_date}"
    export REDDIT_APPROVAL_REFERENCE="${case_approval_reference}"
    export REDDIT_API_ACCESS_APPROVED="${fixture_approval}"
    export REDDIT_CLIENT_ID="${fixture_client_id}"
    export REDDIT_CLIENT_SECRET="${fixture_client_secret}"
    export REDDIT_USER_AGENT="${fixture_user_agent}"
    export CAPTURE_TEST_AMBIENT_VALUE="${fixture_ambient_value}"
    if [[ "${exported_functions}" == "true" ]]; then
      set() { return 91; }
      unset() { return 92; }
      readonly() { return 93; }
      command() { return 94; }
      builtin() { return 95; }
      git() { return 97; }
      capture_test_exported_function() { return 0; }
      export -f set unset readonly command builtin git capture_test_exported_function
    fi
    if [[ "${invalid_environment_name}" == "true" ]]; then
      env 'CAPTURE.TEST.INVALID=fixture-invalid-name' /bin/bash -p scripts/capture-submission.sh
    elif [[ "${unprivileged_bash}" == "true" ]]; then
      /bin/bash scripts/capture-submission.sh
    elif [[ "${xtrace}" == "true" ]]; then
      /bin/bash -p -x scripts/capture-submission.sh
    else
      /bin/bash -p scripts/capture-submission.sh
    fi
  ) > "${case_state}/wrapper.stdout" 2> "${case_state}/wrapper.stderr"
  actual_status=$?
  set -e

  assert_equal "${expected_status}" "${actual_status}" "${case_name}: wrapper exit status"
  assert_no_fixture_values "${case_state}/wrapper.stdout"
  assert_no_fixture_values "${case_state}/wrapper.stderr"
  assert_private_cleanup
}

assert_verified_preflight() {
  assert_event_count 1 "go-env:${expected_go_version}" "${case_name}: exact toolchain preflight"
  assert_event_count 1 "go-env:environment-scrubbed" "${case_name}: toolchain probe boundary"
  assert_event_count 1 "build:duckwords:environment-scrubbed" "${case_name}: clean CLI rebuild"
  assert_event_count 1 "version:environment-scrubbed" "${case_name}: verified version environment"
  assert_event_count 1 "build:duckwords-evidence:environment-scrubbed" "${case_name}: clean finalizer rebuild"
}

assert_bundle_payload() {
  local exit_code="$1"
  local output_dir="${case_repo}/artifacts/submission"
  local name
  for name in result.json application.log full-application.log run-manifest.json RUN.md; do
    assert_regular_file "${output_dir}/${name}" "${case_name}: published bundle"
    assert_no_fixture_values "${output_dir}/${name}"
  done
  printf '[]\n' > "${case_state}/expected-result.json"
  if ! cmp -s -- "${case_state}/expected-result.json" "${output_dir}/result.json"; then
    fail "${case_name}: published result differs from captured stdout"
  fi
  assert_event_count 1 "live:arguments-verified" "${case_name}: fixed invocation"
  assert_event_count 1 "live:credentials-present" "${case_name}: live child environment"
  assert_event_count 1 "live:environment-scrubbed" "${case_name}: live child boundary"
  assert_event_count 1 "finalizer:environment-scrubbed" "${case_name}: finalizer environment"
  assert_event_count 1 "finalizer:exit=${exit_code}" "${case_name}: finalizer application status"
}

assert_published_bundle() {
  local exit_code="$1"
  local output_dir="${case_repo}/artifacts/submission"
  local artifact_count

  assert_bundle_payload "${exit_code}"
  artifact_count="$(find "${output_dir}" -mindepth 1 -maxdepth 1 -type f -print | awk 'END { print NR + 0 }')"
  assert_equal 5 "${artifact_count}" "${case_name}: canonical artifact count"
  assert_capture_lock_absent
}

assert_quarantined_bundle() {
  local exit_code="$1"
  local output_dir="${case_repo}/artifacts/submission"
  local lock_dir="${case_repo}/artifacts/submission.capture-lock"
  local artifact_count

  assert_bundle_payload "${exit_code}"
  assert_regular_file "${output_dir}/CAPTURE_FAILED" \
    "${case_name}: failed-publication quarantine marker"
  assert_contains "${output_dir}/CAPTURE_FAILED" \
    'Do not submit this directory' "${case_name}: quarantine instruction"
  assert_contains "${output_dir}/CAPTURE_FAILED" \
    'finalizer_exit_status=1' "${case_name}: quarantine finalizer status"
  assert_no_fixture_values "${output_dir}/CAPTURE_FAILED"
  artifact_count="$(find "${output_dir}" -mindepth 1 -maxdepth 1 -type f -print | awk 'END { print NR + 0 }')"
  assert_equal 6 "${artifact_count}" "${case_name}: quarantined artifact count"
  assert_regular_file "${lock_dir}/owner" "${case_name}: retained capture lock owner"
  assert_contains "${lock_dir}/owner" 'format=duckwords-capture-lock-v1' \
    "${case_name}: retained lock format"
  assert_contains "${lock_dir}/owner" "candidate_sha=${case_sha}" \
    "${case_name}: retained lock candidate"
}

# Bash privileged mode consistently refuses to import exported functions, but
# versions differ in what happens to their encoded BASH_FUNC_* environment
# entries. Bash 3.2 leaves those invalid shell-variable names visible so the
# wrapper rejects them; Bash 5 removes them before the wrapper starts. Both
# outcomes preserve the same security boundary, so detect the interpreter's
# behavior and assert the corresponding fail-closed or successful path below.
privileged_bash_preserves_exported_function_environment() {
  (
    capture_test_privileged_probe() { return 0; }
    export -f capture_test_privileged_probe
    /bin/bash -p -c '
      while IFS= read -r environment_name; do
        if [[ "${environment_name}" == "BASH_FUNC_capture_test_privileged_probe%%" ]]; then
          exit 0
        fi
      done < <(compgen -e)
      exit 1
    '
  )
}

test_success() {
  setup_case success 0
  run_wrapper 0
  assert_verified_preflight
  assert_published_bundle 0
  assert_equal "" "$(< "${case_state}/wrapper.stdout")" "success: wrapper stdout"
  assert_contains "${case_state}/wrapper.stderr" \
    'published one-run evidence at artifacts/submission (application exit 0)' \
    'success publication diagnostic'
  printf '%s\n' 'ok - complete exit 0 publishes one verified bundle'
}

test_xtrace_secret_boundary() {
  setup_case xtrace-secret-boundary 0
  run_wrapper 0 true
  assert_verified_preflight
  assert_published_bundle 0
  assert_contains "${case_state}/wrapper.stderr" \
    'published one-run evidence at artifacts/submission (application exit 0)' \
    'xtrace-safe publication diagnostic'
  printf '%s\n' 'ok - inherited xtrace is disabled before credential capture'
}

test_exported_function_boundary() {
  setup_case exported-function-boundary 0
  if privileged_bash_preserves_exported_function_environment; then
    run_wrapper 2 false true
    assert_path_absent "${case_repo}/artifacts/submission" \
      'exported function publication'
    assert_capture_lock_absent
    assert_event_count 0 "go-env:${expected_go_version}" \
      'exported function toolchain suppression'
    assert_event_count 0 "live:arguments-verified" \
      'exported function live suppression'
    assert_contains "${case_state}/wrapper.stderr" \
      'ambient environment contains an unsupported variable name' \
      'exported function diagnostic'
  else
    run_wrapper 0 false true
    assert_verified_preflight
    assert_published_bundle 0
    assert_contains "${case_state}/wrapper.stderr" \
      'published one-run evidence at artifacts/submission (application exit 0)' \
      'exported function safe-publication diagnostic'
  fi
  assert_event_count 0 "live:exported-function-leaked" \
    'exported function live-child boundary'
  printf '%s\n' 'ok - privileged Bash strips or rejects inherited exported functions'
}

test_invalid_environment_name() {
  setup_case invalid-environment-name 0
  run_wrapper 2 false false true
  assert_path_absent "${case_repo}/artifacts/submission" \
    'invalid environment name publication'
  assert_capture_lock_absent
  assert_event_count 0 "go-env:${expected_go_version}" \
    'invalid environment name toolchain suppression'
  assert_event_count 0 "live:arguments-verified" \
    'invalid environment name live suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'ambient environment contains an unsupported variable name' \
    'invalid environment name diagnostic'
  printf '%s\n' 'ok - ambient names that Bash cannot scrub are rejected before helpers'
}

test_unprivileged_bash_rejected() {
  setup_case unprivileged-bash 0
  run_wrapper 2 false false false true
  assert_path_absent "${case_repo}/artifacts/submission" \
    'unprivileged Bash publication'
  assert_capture_lock_absent
  assert_event_count 0 "go-env:${expected_go_version}" \
    'unprivileged Bash toolchain suppression'
  assert_event_count 0 "live:arguments-verified" \
    'unprivileged Bash live suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'Bash privileged mode is required' \
    'unprivileged Bash diagnostic'
  printf '%s\n' 'ok - raw non-privileged Bash invocation is rejected before helpers'
}

test_partial() {
  setup_case partial 3
  run_wrapper 3
  assert_verified_preflight
  assert_published_bundle 3
  assert_contains "${case_state}/wrapper.stderr" \
    'published one-run evidence at artifacts/submission (application exit 3)' \
    'partial publication diagnostic'
  printf '%s\n' 'ok - partial exit 3 publishes one explicitly partial bundle'
}

test_signal_after_publish() {
  setup_case signal-after-publish 0 false signal-after-publish
  run_wrapper 0
  assert_verified_preflight
  assert_published_bundle 0
  assert_event_count 1 "finalizer:signal-after-publish" \
    'signal-after-publish commit point'
  assert_contains "${case_state}/wrapper.stderr" \
    'published one-run evidence at artifacts/submission (application exit 0)' \
    'signal-after-publish publication diagnostic'
  if grep -F -- 'no valid bundle was published' "${case_state}/wrapper.stderr" >/dev/null 2>&1; then
    fail 'signal-after-publish falsely reported that committed evidence was absent'
  fi
  printf '%s\n' 'ok - atomic publication remains authoritative across a late signal'
}

test_failure_after_publish_is_not_masked() {
  setup_case failure-after-publish 0 false fail-after-publish
  run_wrapper 1
  assert_verified_preflight
  assert_quarantined_bundle 0
  assert_event_count 1 "finalizer:fail-after-publish" \
    'post-rename finalizer failure'
  assert_contains "${case_state}/wrapper.stderr" \
    'CAPTURE_FAILED and the capture lock were retained' \
    'post-rename finalizer failure diagnostic'
  if grep -F -- 'published one-run evidence' "${case_state}/wrapper.stderr" >/dev/null 2>&1; then
    fail 'post-rename finalizer failure was masked as success'
  fi
  printf '%s\n' 'ok - post-rename finalizer failure is visibly quarantined'
}

test_fatal() {
  setup_case fatal 1
  run_wrapper 1
  assert_verified_preflight
  assert_path_absent "${case_repo}/artifacts/submission" 'fatal run publication'
  assert_event_count 1 "live:arguments-verified" 'fatal fixed invocation'
  assert_event_count 1 "live:credentials-present" 'fatal live child environment'
  assert_event_count 1 "live:environment-scrubbed" 'fatal live child boundary'
  assert_event_count 0 "finalizer:environment-scrubbed" 'fatal finalizer suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'assignment run failed with exit status 1; no submission artifacts were published' \
    'fatal failure diagnostic'
  assert_capture_lock_absent
  printf '%s\n' 'ok - fatal exit publishes nothing and removes private capture files'
}

test_existing_target() {
  setup_case existing-target 0 true
  run_wrapper 2
  assert_verified_preflight
  assert_regular_file "${case_repo}/artifacts/submission/sentinel" 'existing evidence preservation'
  assert_contains "${case_repo}/artifacts/submission/sentinel" \
    'preserve-existing-evidence' 'existing evidence content'
  assert_event_count 0 "live:arguments-verified" 'existing target live suppression'
  assert_event_count 0 "finalizer:environment-scrubbed" 'existing target finalizer suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'SUBMISSION_DIR must not already exist; evidence is never overwritten' \
    'existing target diagnostic'
  printf '%s\n' 'ok - existing canonical evidence is rejected without overwrite'
}

test_concurrent_capture_lock() {
  setup_case concurrent-capture-lock 0
  mkdir -m 0700 "${case_repo}/artifacts/submission.capture-lock"
  printf '%s\n' 'concurrent-owner-sentinel' > \
    "${case_repo}/artifacts/submission.capture-lock/sentinel"
  run_wrapper 2
  assert_path_absent "${case_repo}/artifacts/submission" 'concurrent capture publication'
  assert_contains "${case_repo}/artifacts/submission.capture-lock/sentinel" \
    'concurrent-owner-sentinel' 'concurrent capture lock ownership'
  assert_event_count 0 "live:arguments-verified" 'concurrent capture live suppression'
  assert_event_count 0 "finalizer:environment-scrubbed" 'concurrent capture finalizer suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'the capture lock already exists; it may be active or left by an interrupted run' \
    'concurrent capture diagnostic'
  printf '%s\n' 'ok - a concurrent capture is rejected before live execution'
}

test_stale_capture_lock() {
  setup_case stale-capture-lock 0
  mkdir -m 0700 "${case_repo}/artifacts/submission.capture-lock"
  printf '%s\n%s\n%s\n' \
    'format=duckwords-capture-lock-v1' 'pid=999999' "candidate_sha=${case_sha}" > \
    "${case_repo}/artifacts/submission.capture-lock/owner"
  run_wrapper 2
  assert_path_absent "${case_repo}/artifacts/submission" 'stale capture publication'
  assert_regular_file "${case_repo}/artifacts/submission.capture-lock/owner" \
    'stale capture lock preservation'
  assert_contains "${case_repo}/artifacts/submission.capture-lock/owner" 'pid=999999' \
    'stale capture owner preservation'
  assert_event_count 0 "live:arguments-verified" 'stale capture live suppression'
  assert_event_count 0 "finalizer:environment-scrubbed" 'stale capture finalizer suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'remove it manually only after confirming no capture owns the target' \
    'stale capture manual-recovery diagnostic'
  printf '%s\n' 'ok - a hard-kill lock is preserved for explicit manual recovery'
}

test_mismatched_cli() {
  setup_case mismatched-cli 0
  printf '%s\n' '# deterministic mismatch' >> "${case_repo}/bin/duckwords"
  run_wrapper 2
  assert_path_absent "${case_repo}/artifacts/submission" 'mismatched CLI publication'
  assert_event_count 1 "build:duckwords:environment-scrubbed" 'mismatched CLI clean rebuild'
  assert_event_count 0 "version:environment-scrubbed" 'mismatched CLI never executed'
  assert_event_count 0 "build:duckwords-evidence:environment-scrubbed" 'mismatched CLI early rejection'
  assert_event_count 0 "live:arguments-verified" 'mismatched CLI live suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'bin/duckwords bytes do not match a clean reproducible candidate build' \
    'mismatched CLI diagnostic'
  printf '%s\n' 'ok - CLI bytes that differ from clean HEAD are rejected'
}

test_mismatched_finalizer() {
  setup_case mismatched-finalizer 0
  printf '%s\n' '# deterministic mismatch' >> "${case_repo}/bin/duckwords-evidence"
  run_wrapper 2
  assert_path_absent "${case_repo}/artifacts/submission" 'mismatched finalizer publication'
  assert_event_count 1 "build:duckwords:environment-scrubbed" 'mismatched finalizer clean CLI rebuild'
  assert_event_count 1 "version:environment-scrubbed" 'mismatched finalizer verified version'
  assert_event_count 1 "build:duckwords-evidence:environment-scrubbed" \
    'mismatched finalizer clean rebuild'
  assert_event_count 0 "live:arguments-verified" 'mismatched finalizer live suppression'
  assert_contains "${case_state}/wrapper.stderr" \
    'bin/duckwords-evidence bytes do not match a clean reproducible candidate build' \
    'mismatched finalizer diagnostic'
  printf '%s\n' 'ok - finalizer bytes that differ from clean HEAD are rejected'
}

test_success
test_xtrace_secret_boundary
test_exported_function_boundary
test_invalid_environment_name
test_unprivileged_bash_rejected
test_partial
test_signal_after_publish
test_failure_after_publish_is_not_masked
test_fatal
test_existing_target
test_concurrent_capture_lock
test_stale_capture_lock
test_mismatched_cli
test_mismatched_finalizer
printf '%s\n' 'capture-submission offline behavioral harness passed'
