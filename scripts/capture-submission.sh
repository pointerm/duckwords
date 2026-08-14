#!/bin/bash -p

# Capture one policy-approved assignment run and ask the evidence finalizer to
# publish an atomic, reviewable bundle. Credentials remain in process memory only:
# this wrapper never prints or persists them and exposes them solely to the verified
# live DuckWords child.
if [[ "$-" != *p* ]]; then
  printf 'capture-submission: invoke through make submission-capture or execute the script directly; Bash privileged mode is required\n' >&2
  exit 2
fi
{ set +x; } 2>/dev/null
set -euo pipefail
umask 077

# Canonical launch uses this privileged-mode shebang or `bash -p` from Make. Bash
# privileged mode is required here because it refuses to import environment-defined
# functions before this script executes; ordinary in-script cleanup cannot safely
# run first when functions are allowed to shadow even `set`, `unset`, or `command`.
#
# The caller must provide Reddit credentials through the environment, but no
# preflight subprocess needs them. Retain their bytes only as unexported shell
# variables, remove them from the wrapper environment before invoking any helper,
# and restore them solely for the byte-verified live DuckWords child.
live_reddit_api_approved="${REDDIT_API_ACCESS_APPROVED-}"
live_reddit_client_id="${REDDIT_CLIENT_ID-}"
live_reddit_client_secret="${REDDIT_CLIENT_SECRET-}"
live_reddit_user_agent="${REDDIT_USER_AGENT-}"
unset REDDIT_API_ACCESS_APPROVED REDDIT_CLIENT_ID REDDIT_CLIENT_SECRET REDDIT_USER_AGENT
readonly live_reddit_api_approved live_reddit_client_id live_reddit_client_secret \
  live_reddit_user_agent

# Bash imports exported functions before it starts this script. Remove every such
# function before resolving the repository or invoking a helper so an ambient
# function named git, date, tar, or another command cannot intercept credentials or
# alter preflight behavior. Environment entries whose names are not shell
# identifiers cannot be unexported safely with Bash builtins, so reject that rare
# shape before any external command instead of allowing it to cross the live-child
# boundary later.
while IFS= read -r inherited_function_name; do
  command builtin unset -f -- "${inherited_function_name}" || {
    command builtin printf \
      'capture-submission: inherited shell functions could not be removed safely\n' >&2
    exit 2
  }
done < <(command builtin compgen -A function)
command builtin unset inherited_function_name
while IFS= read -r inherited_environment_name; do
  if [[ ! "${inherited_environment_name}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
    command builtin printf \
      'capture-submission: ambient environment contains an unsupported variable name\n' >&2
    exit 2
  fi
done < <(command builtin compgen -e)
command builtin unset inherited_environment_name

verification_dir=""
verification_prefix=""
verification_source=""
stage_dir=""
stage_prefix=""
capture_lock_dir=""
capture_lock_owner_file=""
capture_lock_owned=false
capture_lock_retain=false

cleanup_verification_source() {
  local archived_path candidate_path
  if [[ -n "${verification_source:-}" && -d "${verification_source}" && ! -L "${verification_source}" &&
    "${verification_source}/" == "${verification_dir:?}/"* && "${candidate_sha:-}" =~ ^[0-9a-f]{40}$ ]]; then
    while IFS= read -r -d '' archived_path; do
      [[ -n "${archived_path}" ]] || continue
      candidate_path="${verification_source}/${archived_path}"
      if [[ (-f "${candidate_path}" || -L "${candidate_path}") && "${candidate_path}" == "${verification_source}/"* ]]; then
        rm -f -- "${candidate_path}"
      fi
    done < <(git ls-tree -r -z --name-only "${candidate_sha}" 2>/dev/null || true)
    find "${verification_source}" -depth -type d -empty -delete 2>/dev/null || true
  fi
}

cleanup_private_state() {
  local directory file

  cleanup_verification_source
  for directory in "${verification_dir:-}" "${stage_dir:-}"; do
    [[ -n "${directory}" ]] || continue
    if [[ "${directory}" == "${verification_prefix:-}"* || "${directory}" == "${stage_prefix:-}"* ]]; then
      if [[ -d "${directory}" && ! -L "${directory}" ]]; then
        for file in duckwords duckwords-evidence candidate.tar result.json application.log; do
          if [[ -f "${directory}/${file}" && ! -L "${directory}/${file}" ]]; then
            rm -f -- "${directory:?}/${file}"
          fi
        done
        rmdir -- "${directory}" 2>/dev/null || true
      fi
    fi
  done
  if [[ "${capture_lock_owned:-false}" == "true" &&
    "${capture_lock_retain:-false}" != "true" && -n "${capture_lock_dir:-}" &&
    -d "${capture_lock_dir}" && ! -L "${capture_lock_dir}" ]]; then
    if [[ -n "${capture_lock_owner_file:-}" &&
      "${capture_lock_owner_file}" == "${capture_lock_dir}/owner" &&
      -f "${capture_lock_owner_file}" && ! -L "${capture_lock_owner_file}" ]]; then
      rm -f -- "${capture_lock_owner_file}"
    fi
    rmdir -- "${capture_lock_dir}" 2>/dev/null || true
  fi
}
trap cleanup_private_state EXIT

readonly expected_go_version="go1.26.6"
if ! repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"; then
  printf 'capture-submission: repository root could not be resolved\n' >&2
  exit 2
fi
readonly repository_root
readonly duckwords_binary="${repository_root}/bin/duckwords"
readonly evidence_binary="${repository_root}/bin/duckwords-evidence"
readonly submission_dir="${SUBMISSION_DIR:-artifacts/submission}"

fail() {
  printf 'capture-submission: %s\n' "$1" >&2
  exit 2
}

if [[ "$#" -ne 0 ]]; then
  fail "this wrapper accepts no arguments; the reviewed assignment invocation is fixed"
fi
if [[ "${live_reddit_api_approved}" != "true" || -z "${live_reddit_client_id}" ||
  -z "${live_reddit_client_secret}" || -z "${live_reddit_user_agent}" ]]; then
  fail "REDDIT_API_ACCESS_APPROVED=true and all credential environment variables are required"
fi

cd "${repository_root}"

if ! repository_from_git="$(git rev-parse --show-toplevel 2>/dev/null)"; then
  fail "repository root could not be verified"
fi
if ! repository_from_git_abs="$(cd "${repository_from_git}" && pwd -P)"; then
  fail "Git repository root could not be resolved"
fi
if [[ "${repository_from_git_abs}" != "${repository_root}" ]]; then
  fail "script must run from the DuckWords repository containing its Git metadata"
fi
if ! candidate_sha="$(git rev-parse --verify HEAD 2>/dev/null)" ||
  [[ ! "${candidate_sha}" =~ ^[0-9a-f]{40}$ ]]; then
  fail "HEAD must resolve to a full lowercase Git commit SHA"
fi
if ! worktree_status="$(git status --porcelain=v1 --untracked-files=all --ignore-submodules=none)"; then
  fail "candidate worktree status could not be inspected"
fi
if [[ -n "${worktree_status}" ]]; then
  fail "the candidate worktree and index must be clean before the final run"
fi

if [[ ! -f "${duckwords_binary}" || -L "${duckwords_binary}" || ! -x "${duckwords_binary}" ]]; then
  fail "bin/duckwords must be a regular executable release binary"
fi
if [[ ! -f "${evidence_binary}" || -L "${evidence_binary}" || ! -x "${evidence_binary}" ]]; then
  fail "bin/duckwords-evidence must be a regular executable"
fi

if ! actual_go_version="$(env -i PATH="${PATH}" TMPDIR="${TMPDIR:-/tmp}" HOME="${HOME}" TZ=UTC \
  GOFIPS140=off GOTOOLCHAIN="${expected_go_version}" go env GOVERSION 2>/dev/null)"; then
  fail "the local Go toolchain could not be inspected"
fi
if [[ "${actual_go_version}" != "${expected_go_version}" ]]; then
  fail "the final run requires exactly ${expected_go_version}"
fi

: "${DUCKWORDS_RELEASE_VERSION:?DUCKWORDS_RELEASE_VERSION is required}"
: "${DUCKWORDS_BUILD_DATE:?DUCKWORDS_BUILD_DATE is required}"
readonly release_version="${DUCKWORDS_RELEASE_VERSION}"
readonly release_build_date="${DUCKWORDS_BUILD_DATE}"
if [[ ! "${release_version}" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]]; then
  fail "DUCKWORDS_RELEASE_VERSION must use the restricted release SemVer grammar"
fi
if [[ ! "${release_build_date}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  fail "DUCKWORDS_BUILD_DATE must use UTC RFC 3339 seconds"
fi
if ! normalized_build_date="$(date -u -d "${release_build_date}" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)"; then
  if ! normalized_build_date="$(date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "${release_build_date}" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)"; then
    fail "DUCKWORDS_BUILD_DATE must be a real UTC timestamp"
  fi
fi
if [[ "${normalized_build_date}" != "${release_build_date}" ]]; then
  fail "DUCKWORDS_BUILD_DATE must be a canonical UTC timestamp"
fi

# Linker metadata alone cannot prove an ignored executable was built from the clean
# candidate tree. Export HEAD into a private source snapshot, rebuild both executed
# programs there, compare bytes, and discard it before any network/source operation.
verification_prefix="${TMPDIR:-/tmp}/duckwords-build-check."
if ! verification_dir="$(mktemp -d "${verification_prefix}XXXXXXXX")"; then
  fail "a private build-verification directory could not be created"
fi
if [[ ! -d "${verification_dir}" || -L "${verification_dir}" || "${verification_dir}" != "${verification_prefix}"* ]]; then
  fail "a private build-verification directory could not be created safely"
fi
verification_source="${verification_dir}/source"
mkdir -m 0700 "${verification_source}"
if ! git archive --format=tar --output="${verification_dir}/candidate.tar" "${candidate_sha}" ||
  ! tar -xf "${verification_dir}/candidate.tar" -C "${verification_source}"; then
  fail "the clean candidate source snapshot could not be exported"
fi
rm -f -- "${verification_dir}/candidate.tar"
verification_binary="${verification_dir}/duckwords"
if ! (cd "${verification_source}" && \
  env -i PATH="${PATH}" TMPDIR="${TMPDIR:-/tmp}" HOME="${HOME}" TZ=UTC \
  CGO_ENABLED=0 GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= GOFIPS140=off GOTOOLCHAIN="${expected_go_version}" go build \
  -mod=readonly -trimpath -buildvcs=false \
  -ldflags "-s -w -buildid= -X 'github.com/pointerm/duckwords/internal/buildinfo.version=${release_version}' -X 'github.com/pointerm/duckwords/internal/buildinfo.commit=${candidate_sha}' -X 'github.com/pointerm/duckwords/internal/buildinfo.buildDate=${release_build_date}'" \
  -o "${verification_binary}" ./cmd/duckwords >/dev/null 2>&1); then
  fail "the clean candidate release could not be rebuilt for verification"
fi
if ! cmp -s -- "${duckwords_binary}" "${verification_binary}"; then
  fail "bin/duckwords bytes do not match a clean reproducible candidate build"
fi
if ! binary_version="$(env -i TZ=UTC "${verification_binary}" --version)"; then
  fail "the verified DuckWords release --version failed"
fi
readonly expected_version_line="duckwords version=${release_version} commit=${candidate_sha} built=${release_build_date} go=${expected_go_version}"
if [[ "${binary_version}" != "${expected_version_line}" ]]; then
  fail "the verified DuckWords release reports unexpected provenance"
fi
verification_evidence_binary="${verification_dir}/duckwords-evidence"
if ! (cd "${verification_source}" && \
  env -i PATH="${PATH}" TMPDIR="${TMPDIR:-/tmp}" HOME="${HOME}" TZ=UTC \
  CGO_ENABLED=0 GOFLAGS= GOWORK=off GOENV=off GOEXPERIMENT= GOFIPS140=off GOTOOLCHAIN="${expected_go_version}" go build \
  -mod=readonly -trimpath -buildvcs=false -ldflags "-s -w -buildid=" \
  -o "${verification_evidence_binary}" ./cmd/duckwords-evidence >/dev/null 2>&1); then
  fail "the clean candidate evidence finalizer could not be rebuilt for verification"
fi
if ! cmp -s -- "${evidence_binary}" "${verification_evidence_binary}"; then
  fail "bin/duckwords-evidence bytes do not match a clean reproducible candidate build"
fi
chmod 0500 "${verification_binary}" "${verification_evidence_binary}"

# Keep the verified executables in the private 0700 directory through the live
# run, but discard the exported source snapshot before any network access. This
# closes the check/use window on ignored bin/ while minimizing retained material.
cleanup_verification_source
if [[ -e "${verification_source}" || -L "${verification_source}" ]]; then
  fail "the private candidate source snapshot could not be removed safely"
fi
verification_source=""

: "${REDDIT_POLICY_VERIFIED_AT:?REDDIT_POLICY_VERIFIED_AT is required (YYYY-MM-DD)}"
: "${REDDIT_APPROVAL_REFERENCE:?REDDIT_APPROVAL_REFERENCE is required (safe opaque identifier)}"
readonly policy_verified_at="${REDDIT_POLICY_VERIFIED_AT}"
readonly approval_reference="${REDDIT_APPROVAL_REFERENCE}"

if [[ ! "${policy_verified_at}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
  fail "REDDIT_POLICY_VERIFIED_AT must use YYYY-MM-DD"
fi
normalized_policy_date=""
if normalized_policy_date="$(date -u -d "${policy_verified_at}" '+%Y-%m-%d' 2>/dev/null)"; then
  :
elif normalized_policy_date="$(date -j -u -f '%Y-%m-%d' "${policy_verified_at}" '+%Y-%m-%d' 2>/dev/null)"; then
  :
else
  fail "REDDIT_POLICY_VERIFIED_AT must be a real calendar date"
fi
if ! utc_today="$(date -u '+%Y-%m-%d')"; then
  fail "the current UTC date could not be determined"
fi
if [[ "${normalized_policy_date}" != "${policy_verified_at}" ||
  "${policy_verified_at}" > "${utc_today}" ]]; then
  fail "REDDIT_POLICY_VERIFIED_AT must be a real, non-future UTC date"
fi
if [[ "${policy_verified_at}" != "${utc_today}" ]]; then
  fail "REDDIT_POLICY_VERIFIED_AT must match the final run UTC date"
fi
if [[ ${#approval_reference} -gt 128 ||
  ! "${approval_reference}" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]*$ ]]; then
  fail "REDDIT_APPROVAL_REFERENCE must be a 1..128 character safe opaque identifier"
fi
if ! approval_reference_lower="$(printf '%s' "${approval_reference}" | tr '[:upper:]' '[:lower:]')"; then
  fail "REDDIT_APPROVAL_REFERENCE could not be validated"
fi
case "${approval_reference_lower}" in
  *secret* | *token* | *password* | *authorization* | *bearer* | *client_id* | *client-id*)
    fail "REDDIT_APPROVAL_REFERENCE contains a prohibited sensitive term"
    ;;
esac

if [[ -z "${submission_dir}" || "${submission_dir}" == /* ||
  "${submission_dir}" == */ || "${submission_dir}" == *//* ]]; then
  fail "SUBMISSION_DIR must be a normalized repository-relative path under artifacts/"
fi
IFS='/' read -r -a submission_parts <<< "${submission_dir}"
if [[ ${#submission_parts[@]} -lt 2 || "${submission_parts[0]}" != "artifacts" ||
  "${submission_parts[1]}" == "review" ]]; then
  fail "SUBMISSION_DIR must be under artifacts/ but outside artifacts/review/"
fi
for component in "${submission_parts[@]}"; do
  if [[ -z "${component}" || "${component}" == "." || "${component}" == ".." ||
    ! "${component}" =~ ^[A-Za-z0-9._-]+$ ]]; then
    fail "SUBMISSION_DIR contains an unsafe or non-normalized path component"
  fi
done

submission_parent="${submission_dir%/*}"
current_parent="${repository_root}"
for ((index = 0; index < ${#submission_parts[@]} - 1; index++)); do
  current_parent="${current_parent}/${submission_parts[index]}"
  if [[ ! -d "${current_parent}" || -L "${current_parent}" ]]; then
    fail "every SUBMISSION_DIR parent must already be a real directory"
  fi
done
if ! submission_parent_abs="$(cd "${submission_parent}" && pwd -P)"; then
  fail "SUBMISSION_DIR parent could not be resolved"
fi
readonly submission_parent_abs
case "${submission_parent_abs}/" in
  "${repository_root}/artifacts/"*) ;;
  *) fail "SUBMISSION_DIR resolved outside the repository artifacts directory" ;;
esac
readonly submission_output_abs="${submission_parent_abs}/${submission_parts[${#submission_parts[@]} - 1]}"
if [[ -e "${submission_output_abs}" || -L "${submission_output_abs}" ]]; then
  fail "SUBMISSION_DIR must not already exist; evidence is never overwritten"
fi
capture_lock_dir="${submission_output_abs}.capture-lock"
if [[ -e "${capture_lock_dir}" || -L "${capture_lock_dir}" ]]; then
  fail "the capture lock already exists; it may be active or left by an interrupted run, so inspect it and remove it manually only after confirming no capture owns the target"
fi
if ! mkdir -m 0700 -- "${capture_lock_dir}"; then
  fail "the capture lock could not be acquired; another capture may own the requested SUBMISSION_DIR"
fi
capture_lock_owned=true
capture_lock_owner_file="${capture_lock_dir}/owner"
if ! (
  set -o noclobber
  command builtin printf \
    'format=duckwords-capture-lock-v1\npid=%s\ncandidate_sha=%s\n' \
    "$$" "${candidate_sha}" > "${capture_lock_owner_file}"
); then
  fail "capture lock owner metadata could not be recorded"
fi
if [[ ! -f "${capture_lock_owner_file}" || -L "${capture_lock_owner_file}" ]]; then
  fail "capture lock owner metadata was not created safely"
fi

# Close the preflight-to-run window after all local rebuild and path work. A file,
# index, or candidate-ref change now fails before the live child is spawned.
if [[ "$(git rev-parse --verify HEAD 2>/dev/null)" != "${candidate_sha}" ]] ||
  [[ -n "$(git status --porcelain=v1 --untracked-files=all --ignore-submodules=none)" ]]; then
  fail "the candidate changed during submission preflight"
fi

readonly temporary_parent="${TMPDIR:-/tmp}"
if [[ ! -d "${temporary_parent}" || -L "${temporary_parent}" ]]; then
  fail "the temporary directory parent must be a real directory"
fi
if ! temporary_parent_abs="$(cd "${temporary_parent}" && pwd -P)"; then
  fail "the temporary directory parent could not be resolved"
fi
if ! stage_dir="$(mktemp -d "${temporary_parent_abs%/}/duckwords-submission.XXXXXXXX")"; then
  fail "a private capture directory could not be created"
fi
readonly stage_prefix="${temporary_parent_abs%/}/duckwords-submission."
if [[ ! -d "${stage_dir}" || -L "${stage_dir}" || "${stage_dir}" != "${stage_prefix}"* ]]; then
  fail "a private capture directory could not be created safely"
fi
chmod 0700 "${stage_dir}"

child_pid=""
received_signal=""
signal_count=0

signal_child() {
  local signal_name="$1"
  if [[ -n "${child_pid}" ]] && kill -0 "${child_pid}" 2>/dev/null; then
    kill -s "${signal_name}" "${child_pid}" 2>/dev/null || true
  fi
}

forward_signal() {
  received_signal="$1"
  signal_count=$((signal_count + 1))
  signal_child "${received_signal}"
}

# A trapped signal can interrupt wait before the child has actually terminated.
# Re-enter wait until the PID is gone so cleanup can never race a still-running
# process that owns the staged stdout/stderr files.
wait_for_child() {
  local waited_pid="$1"
  local wait_status

  while true; do
    wait "${waited_pid}"
    wait_status=$?
    if ! kill -0 "${waited_pid}" 2>/dev/null; then
      return "${wait_status}"
    fi
  done
}

interrupted_exit_status() {
  return 130
}

trap 'forward_signal INT' INT
trap 'forward_signal TERM' TERM

set +e
(
  # The credential values are unexported parent-shell variables. Remove the export
  # attribute from every ambient name with Bash builtins, export only the reviewed
  # live inputs, and replace this subshell directly with the verified binary. No
  # transient process receives a credential through argv.
  while IFS= read -r environment_name; do
    command builtin export -n "${environment_name}"
  done < <(command builtin compgen -e)
  command builtin export TZ=UTC
  command builtin export REDDIT_API_ACCESS_APPROVED="${live_reddit_api_approved}"
  command builtin export REDDIT_CLIENT_ID="${live_reddit_client_id}"
  command builtin export REDDIT_CLIENT_SECRET="${live_reddit_client_secret}"
  command builtin export REDDIT_USER_AGENT="${live_reddit_user_agent}"
  exec "${verification_binary}" \
    --workers=4 \
    --rate-limit=0.8 \
    --request-timeout=20s \
    --timeout=30m \
    --max-retries=3 \
    --retry-budget=45s \
    --failure-mode=best-effort \
    --log-level=info \
    --log-format=json
) \
  > "${stage_dir}/result.json" \
  2> "${stage_dir}/application.log" &
child_pid=$!
if ((signal_count > 0)); then
  signal_child "${received_signal}"
fi
wait_for_child "${child_pid}"
run_status=$?
child_pid=""
set -e

if ((signal_count > 0)); then
  set +e
  interrupted_exit_status
  run_status=$?
  set -e
fi

if [[ "${run_status}" -ne 0 && "${run_status}" -ne 3 ]]; then
  printf 'capture-submission: assignment run failed with exit status %s; no submission artifacts were published\n' "${run_status}" >&2
  exit "${run_status}"
fi

set +e
env -i \
  TZ=UTC \
  "${verification_evidence_binary}" \
  --result "${stage_dir}/result.json" \
  --log "${stage_dir}/application.log" \
  --output-dir "${submission_output_abs}" \
  --exit-code "${run_status}" \
  --binary "${verification_binary}" \
  --policy-verified-at "${policy_verified_at}" \
  --approval-reference "${approval_reference}" &
child_pid=$!
if ((signal_count > 0)); then
  signal_child "${received_signal}"
fi
wait_for_child "${child_pid}"
finalizer_status=$?
child_pid=""
set -e

# The finalizer creates the destination only with its final atomic rename. A signal
# may interrupt this shell's wait after that rename but before the child's zero exit
# status is reaped. Treat the real, non-link destination as the publication commit
# point; this avoids falsely claiming that a valid bundle was not published.
publication_committed=false
if [[ -d "${submission_output_abs}" && ! -L "${submission_output_abs}" &&
  -f "${submission_output_abs}/result.json" && ! -L "${submission_output_abs}/result.json" &&
  -f "${submission_output_abs}/application.log" && ! -L "${submission_output_abs}/application.log" &&
  -f "${submission_output_abs}/full-application.log" && ! -L "${submission_output_abs}/full-application.log" &&
  -f "${submission_output_abs}/run-manifest.json" && ! -L "${submission_output_abs}/run-manifest.json" &&
  -f "${submission_output_abs}/RUN.md" && ! -L "${submission_output_abs}/RUN.md" ]] &&
  cmp -s -- "${stage_dir}/result.json" "${submission_output_abs}/result.json" &&
  cmp -s -- "${stage_dir}/application.log" "${submission_output_abs}/application.log"; then
  publication_committed=true
fi
if [[ "${publication_committed}" == "true" &&
  ("${finalizer_status}" -eq 0 ||
    ("${signal_count}" -gt 0 &&
      ("${finalizer_status}" -eq 130 || "${finalizer_status}" -eq 143))) ]]; then
  printf 'capture-submission: published one-run evidence at %s (application exit %s)\n' \
    "${submission_dir}" "${run_status}" >&2
  exit "${run_status}"
fi

# A verified finalizer can cross its atomic rename and still report a later error.
# Do not delete or overwrite that diagnostically valuable output. Mark the exact
# bundle whose result/log bytes match this run as unfit for submission and retain
# its capture lock so recovery always requires an explicit operator decision.
quarantine_committed_publication() {
  local failure_marker="${submission_output_abs}/CAPTURE_FAILED"

  capture_lock_retain=true
  if [[ -e "${failure_marker}" || -L "${failure_marker}" ]]; then
    return 1
  fi
  if ! (
    set -o noclobber
    command builtin printf \
      '%s\n%s\nfinalizer_exit_status=%s\n' \
      'DuckWords capture failed after the evidence destination was created.' \
      'Do not submit this directory; inspect and move or remove it manually.' \
      "${finalizer_status}" > "${failure_marker}"
  ); then
    return 1
  fi
  [[ -f "${failure_marker}" && ! -L "${failure_marker}" ]]
}

if [[ "${finalizer_status}" -ne 0 ]]; then
  if [[ "${publication_committed}" == "true" ]]; then
    if quarantine_committed_publication; then
      printf 'capture-submission: evidence finalization failed after creating %s; CAPTURE_FAILED and the capture lock were retained, so do not submit it and inspect it manually\n' \
        "${submission_dir}" >&2
    else
      printf 'capture-submission: evidence finalization failed after creating %s; it could not be marked safely, so the capture lock was retained and the destination requires manual inspection\n' \
        "${submission_dir}" >&2
    fi
    if ((signal_count > 0)); then
      set +e
      interrupted_exit_status
      interrupted_status=$?
      set -e
      exit "${interrupted_status}"
    fi
    exit 1
  fi
  if ((signal_count > 0)); then
    printf 'capture-submission: evidence finalization interrupted; no valid bundle was published\n' >&2
    set +e
    interrupted_exit_status
    interrupted_status=$?
    set -e
    exit "${interrupted_status}"
  fi
  printf 'capture-submission: evidence finalization failed; no valid bundle was published\n' >&2
  exit 1
fi

# A zero finalizer status without its atomic destination is an invalid finalizer
# contract, not a successful capture.
if [[ -e "${submission_output_abs}" || -L "${submission_output_abs}" ]]; then
  capture_lock_retain=true
  printf 'capture-submission: evidence finalization produced an invalid destination at %s; the capture lock was retained and the destination requires manual inspection\n' \
    "${submission_dir}" >&2
  exit 1
fi
printf 'capture-submission: evidence finalization produced no valid bundle\n' >&2
exit 1
