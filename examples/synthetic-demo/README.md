# NON-LIVE SYNTHETIC DEMO — NOT ASSIGNMENT EVIDENCE

This directory contains checked-in output from a deterministic, fully offline test
fixture. It is deliberately separate from `artifacts/submission/` and must not be
presented as the full application log or result requested by the assignment.

The fixture makes no network request, downloads no Reddit user data, does not use
real credentials, and does not prove current public Reddit JSON compatibility. It
exists to make the implemented process-level behavior reviewable before a successful
full assignment capture.

## Checked-in results

- `synthetic-output.json` is the exact pretty-printed stdout produced by the CLI.
- `synthetic-log.normalized.ndjson` is the structured stderr lifecycle log with only
  nondeterministic `time`, `duration`, `throttle_wait`, `throttle_waits`, `goos`, and
  `goarch` fields removed. Concurrent `http_attempt` progress records are sorted by
  post and operation, and object keys are sorted, so the sample is reproducible
  across supported platforms without changing the raw completion-order log.

The raw stdout and unmodified timestamped log are regenerated under the ignored
`artifacts/review/synthetic-demo/` directory.

## Synthetic dataset

The test-only inputs live in `cmd/duckwords/testdata/synthetic/` and are embedded
only in the compiled Go test binary. The production `duckwords` binary contains no
fixture transport or endpoint override.

The compact dataset intentionally crosses the important seams:

- 3 distinct posts processed with 4 workers;
- 11 unique comments and 8 usable comment bodies;
- nested replies, a live descendant under a deleted parent, and removed bodies;
- two focal comment expansions from one placeholder and one depth-continuation request;
- duplicate comment IDs that must not be counted twice;
- mixed case, punctuation, a hyphen, an apostrophe, two-character tokens, digits,
  and a word absent from the dictionary;
- 15 eligible dictionary words, with a deterministic count tie at the top-10 cutoff.

The completed run reports 30 counted tokens, 15 distinct eligible words, 6 public
JSON HTTP attempts, and no retries or partial outcomes.

## Reproduce

From a clean repository with Go 1.26.6 and `jq` installed:

```sh
make synthetic-demo-verify
```

The target compiles the existing test-only process fixture, executes the real CLI
and production composition through an injected in-memory HTTP transport, normalizes
the raw log, and byte-compares both generated documents with the files here.

## Scope

This demonstrates source parsing, public JSON request construction, tree traversal,
bounded parallel processing, dictionary membership, tokenization, aggregation,
deterministic ranking, JSON stdout, structured logging, and authorization-header absence
on this successful path. It does **not** demonstrate the company-provided 200-post dataset,
the live dwyl word bank, real Reddit rate-limit behavior, or completeness of the
final assignment result.

Canonical evidence is created only by a complete live run. It is published separately
as the five-file ignored `artifacts/submission/` bundle.
