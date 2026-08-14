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

Operational logs go to stderr only, so `duckwords > result.json` always yields a
clean JSON document.

---

## Quick start

### 1. Verify the runtime path — no credentials, no fixture network traffic

```bash
make fixture-native && cat artifacts/review/native-result.json
```

This runs the real CLI and the real production wiring against an in-memory
transport, and prints (a cold Go toolchain/module cache may still download build
dependencies before the fixture starts):

```json
[
  {
    "word": "duck",
    "count": 2
  },
  {
    "word": "water",
    "count": 1
  }
]
```

For the richer three-post synthetic scenario with nested replies, focal comment expansion,
continuation, deduplication, and a top-10 tie boundary:

```bash
make synthetic-demo-verify
cat examples/synthetic-demo/synthetic-output.json
```

The checked-in [synthetic demo](examples/synthetic-demo) is explicitly non-live and
is not a substitute for the assignment's 200-post application log.

### 2. Run it for real

The assignment owner confirmed that these supplied public `old.reddit.com` pages
should be fetched without the official OAuth Data API. DuckWords therefore issues
only unauthenticated `GET` requests to fixed `https://old.reddit.com` JSON URLs; it
does not need an app registration, client ID, secret, token, cookie, or approval flag.

```bash
go run ./cmd/duckwords > result.json 2> application.log
```

The binary supplies a descriptive User-Agent automatically. A reviewer may override
it with a printable 8–256 byte value, but this is optional and non-secret:

```bash
export REDDIT_USER_AGENT='duckwords/0.2.0 (+https://github.com/pointerm/duckwords)'
```

This `.json` renderer is the assignment's public-page access contract, not a claim
that anonymous access is part of Reddit's current supported OAuth Data API. Reddit
may redirect hosted/cloud IP ranges to a login page. DuckWords rejects every redirect
and non-JSON response rather than following it or silently returning incomplete data;
run the opt-in smoke from the same local network intended for the final capture.

With no options it processes the assignment inputs using the documented defaults.
At 0.8 requests/second the 200 initial listings alone take about 4 minutes 10 seconds.
Every unresolved `more` child, continuation, and retry adds another paced request, so
the actual duration depends on the live trees and is bounded by the 30-minute default
(the guarded canonical capture deliberately allows up to 2 hours).

```bash
# Only words starting with "duck".
go run ./cmd/duckwords --filter 'duck*'

# Machine-readable logs, more workers, JSON on stdout as always.
go run ./cmd/duckwords --workers=8 --log-format=json
```

Before the final 200-post run, validate the public endpoint from your own network
with one supplied permalink in a local file:

```bash
printf '%s\n' 'https://old.reddit.com/r/duck/comments/<id>/<slug>/' > /tmp/duckwords-one-post.txt
make reddit-smoke LIVE_REDDIT_SMOKE=true LIVE_POSTS_FILE=/tmp/duckwords-one-post.txt
```

The smoke is deliberately opt-in and never runs in CI. It uses one worker, 0.5
requests/second, strict completeness, and writes only ignored output under
`artifacts/review/reddit-smoke/`. A `302` login redirect or `403` is reported as an
access failure; do not work around it with a proxy or hidden endpoint override.

For the final candidate, build from a clean commit and run the guarded one-shot
capture with the same metadata values in both commands:

```bash
version=0.2.0
build_date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
make submission-build VERSION="$version" BUILD_DATE="$build_date"
make submission-capture VERSION="$version" BUILD_DATE="$build_date"
```

The capture runs the fixed assignment profile in strict mode and publishes the
five-file `artifacts/submission/` bundle only after a complete exit `0`. It refuses
a dirty tree, a changed binary, a second writer, a partial result, or replacement of
an existing bundle. Attach that directory separately; do not commit it to the SHA it
attests.

### 3. Or via Docker

```bash
make docker-build
docker run --rm \
  duckwords:review
```

Pass `-e REDDIT_USER_AGENT` only when using the optional override.

**Requirements:** Go 1.26.6 (`make toolchain-check` verifies it); `jq` for synthetic
log normalization; Docker for container parity and the secret-scan target.

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
| `--max-retries N` | 3 | Transient retries per request. |
| `--retry-budget D` | 45s | Total retry time per logical request. |
| `--failure-mode MODE` | best-effort | `best-effort` keeps going; `strict` aborts on the first failed post. |
| `--log-level LEVEL` | info | debug, info, warn, error. |
| `--log-format FORMAT` | text | text or json (NDJSON). |
| `--version`, `--help` | | |

DuckWords has no Reddit credential input. The only Reddit environment setting is the
optional non-secret `REDDIT_USER_AGENT`; logs record only whether it was overridden
and its SHA-256 digest, never the raw value.

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

### Packages

| Package | Responsibility |
|---|---|
| [`cmd/duckwords`](cmd/duckwords) | Thin process entrypoint: signals and one CLI call |
| [`internal/cli`](internal/cli) | Argument lifecycle, stdout/stderr contract, exit codes |
| [`internal/production`](internal/production) | Environment, HTTP client, source/public-Reddit dependency wiring |
| [`internal/runlog`](internal/runlog) | Stable sanitized lifecycle and evidence records |
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
- [artifacts/](artifacts) — `review/` holds local, reproducible diagnostics;
  `submission/` holds the captured run attached to the submission.

## Assignment inputs

- Post list: <https://gist.githubusercontent.com/jonathan-firefly/b7fa366ce0fce7ab977db331ed169194/raw/duck_urls_200.txt>
- Word bank: <https://raw.githubusercontent.com/dwyl/english-words/master/words.txt>

## Notes

The only runtime dependency is `golang.org/x/sync` (for `errgroup` and `semaphore`);
everything else is the standard library. Generative AI tools were used during
development, as the assignment permits; all code was reviewed and is covered by the
test suite.
