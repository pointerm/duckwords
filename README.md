# DuckWords

A Go CLI for the Firefly Backend Engineer assignment. It reads a list of `r/duck`
posts, walks every comment and reply in each post, counts the words that are in the
supplied word bank, and prints the top ten as pretty JSON on stdout.

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

### 1. Verify it works — no credentials, no network

```bash
make fixture-native && cat artifacts/review/native-result.json
```

This runs the real CLI and the real production wiring against an in-memory
transport, and prints:

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

### 2. Run it for real

Reddit's [Responsible Builder Policy](https://support.reddithelp.com/hc/en-us/articles/42728983564564-Responsible-Builder-Policy)
requires Reddit's approval before you access the Data API. Registering an OAuth
application gives you credentials; it is **not** that approval, and DuckWords never
treats one as the other. Until approval exists, the tool fails closed before it
opens a socket.

Once you have approval and a registered application:

```bash
export REDDIT_API_ACCESS_APPROVED=true   # only after Reddit has approved your access
export REDDIT_CLIENT_ID='<client-id>'
export REDDIT_CLIENT_SECRET='<secret>'
export REDDIT_USER_AGENT='cli:duckwords:1.0.0 (by /u/<your-reddit-name>)'

go run ./cmd/duckwords > result.json 2> application.log
```

`REDDIT_API_ACCESS_APPROVED` must be exactly `true`; it is your acknowledgement, not
a bypass. Without it you get a diagnostic and exit `1`, and no request is made:

```text
duckwords: Reddit Data API access is not confirmed; obtain Reddit's approval,
then set REDDIT_API_ACCESS_APPROVED=true (see README)
```

With no options it processes the assignment inputs using the documented defaults.
Expect roughly 5–20 minutes for the 200-post list: requests are paced at 0.8/s to
stay well inside Reddit's limits.

```bash
# Only words starting with "duck".
go run ./cmd/duckwords --filter 'duck*'

# Machine-readable logs, more workers, JSON on stdout as always.
go run ./cmd/duckwords --workers=8 --log-format=json
```

### 3. Or via Docker

```bash
make docker-build
docker run --rm \
  -e REDDIT_API_ACCESS_APPROVED -e REDDIT_CLIENT_ID \
  -e REDDIT_CLIENT_SECRET -e REDDIT_USER_AGENT \
  duckwords:review
```

**Requirements:** Go 1.26.6 (`make toolchain-check` verifies it). Docker and `jq`
only for the container targets.

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

Credentials are environment-only and are never accepted as flags, logged, or
included in error messages.

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
                        ├─► reddit.Client.WalkComments(postID)
                        │     GET  /comments/{id}          initial tree
                        │     POST /api/morechildren       "load more comments"
                        │     GET  /comments/{id}?comment= "continue this thread"
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
rate limiter and one OAuth token source are shared by every worker.

If a post cannot be proven complete — a `more` expansion fails, a limit is hit — its
counts are discarded rather than partially merged, the post is reported as
`incomplete`, and the run exits `3`. A half-counted post is worse than a missing one.

### Packages

| Package | Responsibility |
|---|---|
| [`cmd/duckwords`](cmd/duckwords) | Thin process entrypoint: signals and one CLI call |
| [`internal/cli`](internal/cli) | Argument lifecycle, stdout/stderr contract, exit codes |
| [`internal/production`](internal/production) | Environment, HTTP client, OAuth/source/Reddit dependency wiring |
| [`internal/runlog`](internal/runlog) | Stable sanitized lifecycle and evidence records |
| [`internal/config`](internal/config) | Flag parsing and complete validation before any I/O |
| [`internal/acquire`](internal/acquire) | Bounded HTTPS/file download with provenance hashing |
| [`internal/source`](internal/source) | Post-list parsing, URL → post ID, deduplication |
| [`internal/words`](internal/words) | Dictionary, tokenizer, wildcard matcher, counter |
| [`internal/reddit`](internal/reddit) | OAuth, rate limiting, retries, comment-tree traversal |
| [`internal/app`](internal/app) | Worker pool, failure policy, per-post outcomes |
| [`internal/aggregate`](internal/aggregate) | Merge and deterministic top-N |
| [`internal/logging`](internal/logging) | Structured stderr logs with secret redaction |

---

## Word semantics

A token is counted when **all** of the following hold:

1. it is a maximal run of alphabetic characters (any other byte splits it);
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
- **Deleted posts are not failures.** The supplied list contains 20 threads that no
  longer exist. Absence is proven by the comments endpoint answering HTTP 404 or
  returning a listing with no post; those are reported as `skipped` and do not make
  the run partial. A 403, or a 404 from a `more`-expansion request, stays a failure —
  the first may be restored, and the second means the tree is incomplete rather than
  the post being gone.
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
make fuzz-smoke    # short fuzz campaigns
make docker-verify # image build + native/container output parity
make help          # every target
```

CI runs the same gates plus a secret scan and container parity on every push.

---

## Further reading

- [docs/INTENTIONAL_TRADEOFFS.md](docs/INTENTIONAL_TRADEOFFS.md) — features and
  infrastructure deliberately kept outside the assignment scope, with the reason for
  each decision.
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
