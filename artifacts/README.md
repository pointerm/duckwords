# Evidence artifacts

This directory separates reproducible local review output from the canonical
assignment evidence:

- `review/` is ignored scratch evidence from deterministic tests, Docker checks,
  and other non-submission diagnostics. Raw stdout/stderr from a failed or
  interrupted live capture is never copied here; the capture helper securely
  discards its private staging directory.
- `submission/` is created exactly once by the capture helper after a complete
  (`0`) public-JSON live run. It contains
  `result.json`, `application.log`, `full-application.log`,
  `run-manifest.json`, and `RUN.md` from that one invocation.

The capture helper refuses to replace an existing `submission/` directory. Review
the generated bundle for secrets and reconcile it against the submitted full commit
before attaching the directory separately to the submission. `submission/` is
ignored intentionally and must never be committed to the candidate branch: adding
the evidence would create a different commit from the SHA that the evidence proves.
Preserve the directory byte for byte and identify it by the full candidate SHA in its
manifest and in the submission portal or cover note. No live bundle is present until
the final candidate metadata and documented capture prerequisites exist.

A hard termination may leave `submission.capture-lock/` with a safe PID and candidate
SHA. The wrapper deliberately refuses to guess that it is stale; inspect running
processes and the candidate before removing it manually. If the evidence finalizer
fails after creating the destination, the wrapper writes `CAPTURE_FAILED`, retains
the lock, and exits nonzero. That directory is quarantined diagnostic output, not a
submission bundle.

Canonical wrapper invocations are serialized before live I/O. Because Go exposes no
portable atomic rename-with-no-replace operation, an unrelated same-user process that
creates an empty destination directory in the final publication window is outside
this guarantee; use a controlled candidate workspace for the one final capture.
