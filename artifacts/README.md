# Artifacts

Most generated files under this directory are intentionally ignored by Git.

- `review/` contains deterministic fixture, Docker-parity, and one-post smoke output.
- `run/` is created by `make assignment-run` and contains `result.json`,
  `application.log`, and `full-application.log` from one invocation.
- `captured-runs/` contains deliberately committed, manually reviewed result-only
  snapshots. Each capture documents its provenance and completeness limitations;
  these examples are not canonical submission evidence.

`full-application.log` is the application log followed by a fixed marker and the
exact JSON result, matching the assignment's attachment requirement. Check the
process exit status before submitting it: exit `0` is complete, exit `3` is partial,
and exit `1` is a failed run.

Do not commit arbitrary generated run output, browser cookies, raw Reddit responses,
or other session material. A file belongs under `captured-runs/` only after manual
review and a blocking secret scan. Review every file again before attaching anything
to the submission.
