# Local run output

Generated files under this directory are intentionally ignored by Git.

- `review/` contains deterministic fixture, Docker-parity, and one-post smoke output.
- `run/` is created by `make assignment-run` and contains `result.json`,
  `application.log`, and `full-application.log` from one invocation.

`full-application.log` is the application log followed by a fixed marker and the
exact JSON result, matching the assignment's attachment requirement. Check the
process exit status before submitting it: exit `0` is complete, exit `3` is partial,
and exit `1` is a failed run.

Do not commit generated run output, browser cookies, raw Reddit responses, or other
session material. Review every file for sensitive values before attaching it to the
submission.
