#!/usr/bin/env bash

# Run DuckWords once and assemble the full application log requested by the
# assignment. This is intentionally a small capture helper, not an attestation or
# artifact-publication system.
set -u
set -o pipefail
umask 077

readonly repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly binary="${repository_root}/bin/duckwords"
readonly output_dir="${ASSIGNMENT_OUTPUT_DIR:-${repository_root}/artifacts/run}"

if [[ ! -x "${binary}" || -L "${binary}" ]]; then
  printf '%s\n' 'run-assignment: bin/duckwords must be a regular executable; run make build first' >&2
  exit 2
fi
if [[ -e "${output_dir}" && (! -d "${output_dir}" || -L "${output_dir}") ]]; then
  printf '%s\n' 'run-assignment: ASSIGNMENT_OUTPUT_DIR must be a real directory' >&2
  exit 2
fi
mkdir -p -- "${output_dir}"

readonly result_file="${output_dir}/result.json"
readonly application_log="${output_dir}/application.log"
readonly full_log="${output_dir}/full-application.log"
for file in "${result_file}" "${application_log}" "${full_log}"; do
  if [[ -e "${file}" || -L "${file}" ]]; then
    printf 'run-assignment: refusing to overwrite %s\n' "${file}" >&2
    exit 2
  fi
done

"${binary}" --log-format=json "$@" >"${result_file}" 2>"${application_log}"
readonly run_status=$?

if ! {
  cat -- "${application_log}"
  printf '%s\n' '--- DUCKWORDS JSON OUTPUT ---'
  cat -- "${result_file}"
} >"${full_log}"; then
  printf '%s\n' 'run-assignment: failed to assemble full-application.log' >&2
  exit 1
fi

printf 'run-assignment: wrote result and logs to %s (exit %s)\n' \
  "${output_dir}" "${run_status}" >&2
exit "${run_status}"
