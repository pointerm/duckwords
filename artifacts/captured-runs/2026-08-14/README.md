# Reviewed partial live result snapshots — 2026-08-14

These result-only snapshots demonstrate DuckWords' real stdout format against the
assignment's 200-post input and production word bank. They were produced through the
explicitly optional browser-session fallback because cookie-free Reddit JSON access
was blocked from the test network. No cookie, browser header, raw response, username,
or free-form error is stored here.

| Capture | Effective filter | Result | SHA-256 |
|---|---|---|---|
| Unfiltered | none | [result.json](unfiltered/result.json) | `62f6a4bd96b995be7c1f575439a7cc0358faaf456eaee4b80b46043fecc064a9` |
| Filtered | `duck*` | [result.json](filter-duck/result.json) | `1cefe76fa3e943a1e2bba663e4f03ce32191a8c7b5e59dcae78d2c705fd437c1` |

Both invocations ended with exit code `3` and the same source-ordered outcome totals:

- 200 input posts;
- 179 completed;
- 20 skipped after HTTP 404;
- 0 failed;
- 1 incomplete (`18gfvqs`).

Counts from the incomplete post were discarded transactionally before aggregation.
Consequently, both JSON files contain only counts from the 179 proven-complete posts.
The filtered snapshot predates the bounded semantic continuation-retry enhancement;
the unfiltered snapshot demonstrates all three configured replays. Their matching
`duck` and `ducks` counts provide a useful filter consistency check, but neither file
is claimed to have been produced from the final commit or to be a complete canonical
submission result. See the main README's reviewer note for the `18gfvqs` explanation.

The full sanitized application logs remain local under ignored `artifacts/review/`.
Only these compact JSON examples were selected for version control after their hashes,
JSON shape, ordering, and secret scan were checked.
