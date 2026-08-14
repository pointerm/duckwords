# DuckWords

A Go CLI for the Firefly Backend Engineer assignment. It reads a list of `r/duck`
posts, walks every comment and reply in each post, counts the words that are in the
supplied word bank, and prints the top ten as pretty JSON on stdout.

Illustrative output shape (not a live assignment result):

```json
[
  {
    "word": "duck",
    "count": 1462
  },
  {
    "word": "ducks",
    "count": 981
  }
]
```

Operational logs go to stderr only, so redirecting stdout yields a clean JSON
document.

---

## Quick start

You need Go and network access; `go.mod` declares the Go 1.26.6 toolchain and the
first invocation may download it plus the single module dependency. No Reddit API
key and no separate build command are required. Clone the repository, then run from
its root. The Make helpers select the exact Go 1.26.6 toolchain.

```bash
git clone https://github.com/pointerm/duckwords.git
cd duckwords
```

### Run the assignment

```bash
go run ./cmd/duckwords > result.json 2> application.log
```

`result.json` may remain empty until processing finishes. After a successful or
partial run it contains only the top-ten JSON result. `application.log` is updated
as HTTP attempts finish, so progress is visible from another terminal:

```bash
tail -f application.log
```

The observed live runs completed within the default 30-minute timeout. On a slower
network, add `--timeout=2h`. To count only matching words:

```bash
go run ./cmd/duckwords --timeout=2h --filter 'duck*' \
  > result.json 2> application.log
```

This replaces the two local output files from the previous run.

Exit `0` means every available post was processed completely. Exit `3` means the
JSON is intentionally partial; the log identifies skipped, failed, or incomplete
posts. `go run` maps every non-zero program status to shell status `1` and appends an
`exit status N` line to stderr. Use the artifact helper below when the exact
application status and a clean machine-readable log matter; it reports that status
in its final message.

### Optional browser-session fallback

The default request is cookie-free. If Reddit redirects it to login or returns an
HTML denial, a reviewer may retry with their **own temporary browser session**. Copy
only the value after the browser request's `Cookie:` header, without the `Cookie:`
prefix:

```bash
export REDDIT_BROWSER_COOKIE='name=value; another_name=value'

# Optional: copy matching values from the same browser request, or omit these lines.
export REDDIT_USER_AGENT='Mozilla/5.0 (...)'
export REDDIT_BROWSER_ACCEPT_LANGUAGE='en-US,en;q=0.9'
export REDDIT_BROWSER_SEC_CH_UA='"Chromium";v="140", "Not=A?Brand";v="24"'
export REDDIT_BROWSER_SEC_CH_UA_MOBILE='?0'
export REDDIT_BROWSER_SEC_CH_UA_PLATFORM='"macOS"'

go run ./cmd/duckwords --timeout=2h \
  > result.json 2> application.log

unset REDDIT_BROWSER_COOKIE REDDIT_USER_AGENT REDDIT_BROWSER_ACCEPT_LANGUAGE \
  REDDIT_BROWSER_SEC_CH_UA REDDIT_BROWSER_SEC_CH_UA_MOBILE \
  REDDIT_BROWSER_SEC_CH_UA_PLATFORM
```

Only `REDDIT_BROWSER_COOKIE` is required; the other five environment variables are
optional. An empty or malformed value is rejected. This direct run needs no separate
build command and writes `result.json` plus `application.log` in the repository root.

The cookie is sensitive and the `export` command may remain in shell history. Use a
temporary session you own, never commit or share it, unset it immediately after the
run, and sign out or revoke it afterwards. DuckWords sends it only to the fixed
`old.reddit.com` origin, never logs it, never follows redirects, keeps no cookie jar,
and does not persist response cookies. This is authenticated browser access, not
OAuth and not proof that anonymous Reddit JSON access works.

### Create the three review files

For a cookie-free run, the same helper needs no separate build step:

```bash
make assignment-run ARGS='--timeout=2h'
```

For a browser-session capture, replace the `go run` command in the previous section
with this helper command, then run the documented `unset`:

```bash
ASSIGNMENT_OUTPUT_DIR='artifacts/run/browser-session' \
  make assignment-run ARGS='--timeout=2h'
```

The default helper creates `artifacts/run/result.json`, `application.log`, and
`full-application.log`. The full log is the application log followed by a fixed
marker and the exact JSON output. The helper refuses to overwrite an existing run;
choose another directory when repeating it:

```bash
ASSIGNMENT_OUTPUT_DIR='artifacts/run/retry-2' \
  make assignment-run ARGS='--timeout=2h'
```

Submit a run only when the helper reports application exit `0`. Exit `3` is a valid
but explicitly partial JSON result; exit `1` is a failed run. GNU Make itself may
return status `2` when the application reports any non-zero status; the helper's
final `(exit N)` message is the application status.

### Reviewed output examples

Reviewers who cannot access Reddit can inspect an
[unfiltered result](artifacts/captured-runs/2026-08-14/unfiltered/result.json) and a
[`duck*`-filtered result](artifacts/captured-runs/2026-08-14/filter-duck/result.json)
without running the CLI. Both are explicitly **partial authenticated browser-session
snapshots**: 179 posts completed, 20 unavailable posts were skipped, and `18gfvqs`
remained incomplete. They demonstrate the JSON shape and filter behavior, not a
canonical complete submission. Their sanitized execution records are available as
the [unfiltered application log](artifacts/captured-runs/2026-08-14/unfiltered/application.log)
and [`duck*` application log](artifacts/captured-runs/2026-08-14/filter-duck/application.log).
See the
[capture notes](artifacts/captured-runs/2026-08-14/README.md).

### Reddit access note

The assignment implementation uses cookie-free `GET` requests to the fixed
`https://old.reddit.com/.../.json` renderer by default. On 2026-08-14 that path
returned login redirects or HTML denials from the candidate's environment; an
existing browser session returned JSON. This is why the optional fallback is
documented above.

Reddit's current [Data API Wiki](https://support.reddithelp.com/hc/en-us/articles/16160319875092-Reddit-Data-API-Wiki)
states that traffic without OAuth or login credentials will be blocked. Reddit has
also [announced the shutdown of unauthenticated `.json` endpoints](https://www.reddit.com/r/modnews/comments/1tq9vxo/protecting_communities_from_scrapers_and_platform/).
DuckWords rejects redirects, non-`200` responses, and non-JSON bodies rather than
silently accepting incomplete data. Browser-session output must always be identified
as authenticated. A 200-post result is complete only when the application exits `0`.

After each physical Reddit HTTP attempt completes, the log immediately receives an
`event=http_attempt` record with only the operation, post ID, one-based attempt,
success/failure, status, and duration. These progress records use worker completion
order. Authoritative `post_outcome` records remain source-ordered and are emitted at
the end of processing.

The binary supplies a descriptive User-Agent automatically. A reviewer may override
it with a printable 8–256 byte value, but this is optional and non-secret:

```bash
export REDDIT_USER_AGENT='duckwords/0.2.0 (+https://github.com/pointerm/duckwords)'
```

### Offline verification

To verify the full application path without contacting Reddit or the remote input
sources:

```bash
make fixture-native
cat artifacts/review/native-result.json
```

The richer checked-in scenario covers nested replies, comment expansion,
continuations, deduplication, and a top-10 tie boundary:

```bash
make synthetic-demo-verify
cat examples/synthetic-demo/synthetic-output.json
```

These are implementation checks, not live assignment results. `jq` is required only
for synthetic-log normalization. Docker is optional; `make docker-verify` runs the
container hardening and native/container parity checks.

---

## Options

| Option | Default | Notes |
|---|---|---|
| `--filter PATTERN` | none | Exact word or `*` wildcard. Repeatable, OR semantics. |
| `--posts-url URL` | assignment gist | Must be on `gist.githubusercontent.com`. |
| `--posts-file PATH` | — | Local post list instead of the URL. |
| `--dictionary-url URL` | dwyl/english-words | Must be on `raw.githubusercontent.com`. |
| `--dictionary-file PATH` | — | Local word bank instead of the URL. |
| `--workers N` | 4 | Concurrent posts, 1–32. |
| `--rate-limit RATE` | 0.8 | Requests/second, 0.1–1.5. Reddit's own headers can lower it further. |
| `--request-timeout D` | 20s | One HTTP attempt. |
| `--timeout D` | 30m | Whole run. |
| `--max-retries N` | 3 | Transient HTTP retries and bounded no-progress continuation replays per logical request. |
| `--retry-budget D` | 45s | HTTP-attempt and retry-backoff time per logical request; shared rate-limit queueing is bounded by `--timeout`. |
| `--failure-mode MODE` | best-effort | `best-effort` keeps going; `strict` aborts on the first failed post. |
| `--log-level LEVEL` | info | debug, info, warn, error. |
| `--log-format FORMAT` | text | text or json (NDJSON). |
| `--version`, `--help` | | |

The default and canonical modes have no Reddit credential input. The optional
`REDDIT_USER_AGENT` override is non-secret; logs record only whether it was overridden
and its SHA-256 digest, never the raw value. Local browser-session fallback is enabled
only by `REDDIT_BROWSER_COOKIE`; `REDDIT_BROWSER_ACCEPT_LANGUAGE`,
`REDDIT_BROWSER_SEC_CH_UA`, `REDDIT_BROWSER_SEC_CH_UA_MOBILE`, and
`REDDIT_BROWSER_SEC_CH_UA_PLATFORM` optionally supply matching browser headers.
These values are accepted only through the environment, never as flags.

| Exit code | Meaning | JSON on stdout |
|---:|---|---|
| `0` | Every post that exists was counted | yes |
| `1` | Fatal failure, timeout, or strict-mode abort | normally not, but a failure detected after the document was written can leave it on stdout |
| `2` | Invalid command line | no |
| `3` | Partial: at least one post failed or could not be proven complete | yes |
| `130` | Interrupted (`SIGINT`/`SIGTERM`) | no |

---

## How it works

```
post list URL ─┐
               ├─► acquire ──► source.LoadPostList ──► 200 unique post IDs
word bank URL ─┘          └──► words.LoadDictionary ─► normalized word set
                                                            │
                        ┌───────────────────────────────────┘
                        ▼
              app.Runner: N workers, one post each
                        │
                        ├─► reddit.Client.WalkComments(post reference)
                        │     GET /.../comments/{id}/.../.json         initial tree
                        │     GET same .json?comment={child}&context=0 expand `more`
                        │     GET same .json?comment={parent}&context=0 continue depth
                        │       retry: /comments/{post}/_/{parent}/.json on proven zero progress
                        │        └─ streams each comment body exactly once
                        │
                        └─► words.Counter → post-local map[string]uint64
                                                            │
                        aggregate.Merge (single owner)  ◄───┘
                        aggregate.TopN(10)  ──►  pretty JSON on stdout
```

Each worker owns a private count map for its post and hands it to the aggregating
goroutine only after the whole comment tree was proven complete. Nothing shared is
mutated during traversal, so there is no lock on the hot path. One process-wide
rate limiter, retry policy, HTTP pool, and traversal budgets are shared by every worker.

If a post cannot be proven complete, its counts are discarded rather than partially
merged. HTTP/transport/protocol expansion errors are reported as `failed`; explicit
completeness or resource-limit exhaustion is `incomplete`. Best-effort mode keeps
other complete posts and exits `3`, while strict mode cancels and exits `1`. A
half-counted post is worse than a missing one.

A continuation can return HTTP 200 while containing no new comments or unresolved
child IDs. DuckWords treats that as a semantic failure, replays it within the same
`--max-retries` count and `--retry-budget`, and uses Reddit's equivalent path-form
focal URL on the replay to avoid a stale query-form cache entry. The replay is allowed
only before the response has added any new graph state or invoked the visitor; it
therefore cannot double-count a body. A persistently empty/duplicate response remains
`incomplete` rather than being silently accepted as complete. Its retry is visible as
`event=request_retry error_class=incomplete operation=continuation`.

#### Reviewer note: observed `18gfvqs` continuation

During the 2026-08-14 assignment-data run, post `18gfvqs` returned an initial
depth-continuation marker. The initial focal request and all three configured replays
then completed with HTTP 200, but none added a new comment, child ID, or continuation
to the graph. The sanitized outcome reported 80 observed comment records and 51
visited bodies as `incomplete`; those post-local counts were discarded and were not
merged into the final ranking.

This status does **not** mean that an HTTP request or the retry mechanism failed. It
means DuckWords could not prove the all-comments completeness required by this
implementation. A marker-free empty focal view can be normal when descendants were
deleted, redacted, or are not visible to the current session, while a stale or repeated
marker can represent a still-unresolved branch. Reddit's focal-comments response does
not provide a general completeness guarantee that lets the captured run distinguish
those cases safely ([API contract](https://www.reddit.com/dev/api/#GET_comments_{article});
[historical builder behavior](https://github.com/reddit-archive/reddit/blob/master/r2/r2/models/builder.py#L1369-L1397)).
DuckWords therefore fails closed instead of silently treating a potentially partial
tree as complete. The post ID is not special-cased; the same rule applies to any
non-progressing continuation, and a later run may differ because Reddit data is
mutable.

### Packages

| Package | Responsibility |
|---|---|
| [`cmd/duckwords`](cmd/duckwords) | Thin process entrypoint: signals and one CLI call |
| [`internal/cli`](internal/cli) | Argument lifecycle, stdout/stderr contract, exit codes |
| [`internal/production`](internal/production) | Environment, HTTP client, source/public-Reddit dependency wiring |
| [`internal/runlog`](internal/runlog) | Stable sanitized lifecycle and application-log records |
| [`internal/config`](internal/config) | Flag parsing and complete validation before any I/O |
| [`internal/acquire`](internal/acquire) | Bounded HTTPS/file download with provenance hashing |
| [`internal/source`](internal/source) | Post-list parsing, URL → normalized post ID and public JSON path, deduplication |
| [`internal/words`](internal/words) | Dictionary, tokenizer, wildcard matcher, counter |
| [`internal/reddit`](internal/reddit) | Public JSON requests, rate limiting, retries, comment-tree traversal |
| [`internal/app`](internal/app) | Worker pool, failure policy, per-post outcomes |
| [`internal/aggregate`](internal/aggregate) | Merge and deterministic top-N |
| [`internal/logging`](internal/logging) | Structured stderr logs with secret redaction |

---

## Word semantics

A token is counted when **all** of the following hold:

1. it is a maximal run of alphabetic characters (any non-letter code point splits it);
2. it is at least 3 characters long;
3. it is in the word bank, compared case-insensitively;
4. it matches at least one `--filter` pattern, when filters are supplied.

Ties in the top ten are broken by word ascending, so the output is deterministic for
a given input.

### Decisions worth knowing

- **Comment bodies only.** Post titles and self-text are not counted — the
  assignment asks for comment text.
- **Deleted and removed comments are skipped**, so `deleted` and `removed` never
  appear in the results.
- **Deleted posts are not failures.** Some supplied threads may no longer exist.
  Absence is proven only when the initial comments endpoint answers HTTP 404 or 410;
  those are reported as `skipped` and do not make the run partial. A 403, an empty
  HTTP-200 listing, or a 404/410 from a focal expansion stays a failure — access may
  be restored, and an expansion failure means the tree is incomplete rather than the
  post being gone.
- **Markdown is not rendered and URLs are not stripped.** A link like
  `[a duck](https://example.com/pond)` contributes `duck`, `example`, and `pond`.
  Counting the raw comment text as written is simple and predictable; stripping
  markup would need a full CommonMark parser to be correct.
- **`raw_json=1`** is set on every Reddit request so bodies arrive unescaped.
  Without it, every `&amp;` in a comment would be counted as the word `amp`.
- **Comment trees are fully expanded**, including `more` placeholders and Reddit's
  depth-truncated "continue this thread" branches.

---

## Development

```bash
make verify        # format, vet, tests, shuffled tests, race, build — the main gate
make test          # tests only
make race          # race detector
make lint          # pinned Staticcheck
make vuln          # govulncheck
make bench         # benchmarks with allocation counts
make bench-text    # readable local text fixtures and focused counter benchmark
make fuzz-smoke    # short fuzz campaigns
make docker-verify # image build + native/container output parity
make help          # every target
```

CI runs the same gates plus a secret scan and container parity on pushes to `main`
and on pull requests.

---

## Further reading

- [docs/INTENTIONAL_TRADEOFFS.md](docs/INTENTIONAL_TRADEOFFS.md) — features and
  infrastructure deliberately kept outside the assignment scope, with the reason for
  each decision.
- [docs/BENCHMARKS.md](docs/BENCHMARKS.md) — what the word-counting path costs, why it is not
  the bottleneck, and how it extrapolates to larger corpora.
- [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) — the single dependency
  (`golang.org/x/sync`) and its license.
- [artifacts/](artifacts) — `review/` holds local diagnostics, `run/` holds the
  current local assignment output, and `captured-runs/` contains reviewed snapshots.

## Assignment inputs

- Post list: <https://gist.githubusercontent.com/jonathan-firefly/b7fa366ce0fce7ab977db331ed169194/raw/duck_urls_200.txt>
- Word bank: <https://raw.githubusercontent.com/dwyl/english-words/master/words.txt>

## Notes

The only runtime dependency is `golang.org/x/sync` (for `errgroup` and `semaphore`);
everything else is the standard library. Generative AI tools were used during
development, as the assignment permits; all code was reviewed and is covered by the
test suite.
