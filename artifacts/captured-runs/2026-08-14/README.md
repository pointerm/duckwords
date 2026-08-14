# Reviewed partial live captures — 2026-08-14

These captures demonstrate DuckWords' real stdout and sanitized execution log against
the assignment's 200-post input and production word bank. They were produced through
the explicitly optional browser-session fallback because cookie-free Reddit JSON
access was blocked from the test network. No cookie, browser header value, raw
response, username, or free-form error is stored here.

| Capture | Effective filter | Result and SHA-256 | Application log and SHA-256 |
|---|---|---|---|
| Unfiltered | none | [result.json](unfiltered/result.json)<br>`62f6a4bd96b995be7c1f575439a7cc0358faaf456eaee4b80b46043fecc064a9` | [application.log](unfiltered/application.log)<br>`881c872218ec4a657501e2eeaaffb433a3cf878062fce9aff20c64ca3333984f` |
| Filtered | `duck*` | [result.json](filter-duck/result.json)<br>`1cefe76fa3e943a1e2bba663e4f03ce32191a8c7b5e59dcae78d2c705fd437c1` | [application.log](filter-duck/application.log)<br>`e63673ea84f5d97dd0d8203ba53a46ef910c2bf94f1902a69ad59f1c4bee520d` |

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

The committed application logs show source loading, every sanitized HTTP-attempt
record, all 200 source-ordered post outcomes, the final summary, and the result hash.
They intentionally contain no request headers, cookie values, raw URLs, response
bodies, or free-form transport errors. The JSON shape, ordering, log/result hashes,
and secret scan were checked before version control.
