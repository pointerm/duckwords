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

### 2. Attempt the live assignment run

The assignment owner directed the submission to use cookie-free `GET`
requests to the fixed `https://old.reddit.com/.../.json` renderer rather than the
OAuth Data API. DuckWords retains that narrow assignment-specific default; this is
not a claim that anonymous JSON access is currently supported or available from
every network.

On 2026-08-14 the candidate's cookie-free CLI received HTTP `302` redirects to
`/login` or HTTP `403` HTML responses for the supplied URLs. An existing browser
profile returned `200 application/json` only while sending Reddit session state, so
that did not demonstrate anonymous access. The default path does not send browser
cookies, login credentials, or tokens. An explicitly enabled,
local-only browser-session fallback is documented below for a reviewer who wants to
try their own temporary session; its output must be identified as authenticated.

Reddit's current [Data API Wiki](https://support.reddithelp.com/hc/en-us/articles/16160319875092-Reddit-Data-API-Wiki)
states that traffic without OAuth or login credentials will be blocked. Reddit has
also [announced the shutdown of unauthenticated `.json` endpoints](https://www.reddit.com/r/modnews/comments/1tq9vxo/protecting_communities_from_scrapers_and_platform/).
The assignment owner was informed of the observed behavior and asked the candidate
to choose and explain the implementation approach independently.

DuckWords rejects redirects, non-`200` responses, and non-JSON bodies. A 200-post
result is complete only when the process exits `0`; offline and synthetic output must
never be presented as a live assignment result.

```bash
go run ./cmd/duckwords > result.json 2> application.log

# In another terminal, follow sanitized request progress as it is written.
tail -f application.log
```

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

#### Optional local browser-session fallback

If cookie-free access is blocked, a reviewer may optionally retry the ordinary local
CLI with their **own temporary Reddit browser session**. This fallback is disabled by
default and is enabled only by a valid, non-empty `REDDIT_BROWSER_COOKIE`; an empty
value is rejected. It is an authenticated browser-session request, not OAuth, not
Reddit's public Data API, and not proof that anonymous `.json` access works. Label
any output from this mode accordingly.

Avoid placing the cookie in a command, shell history, `.env` file, repository,
application log, issue, or message. Build before introducing the cookie so the Go
toolchain never inherits it. Then, in a Bash shell, read it interactively, export it
only for this run, and clear it immediately afterwards:

```bash
make build

read -rsp 'Paste only your own temporary Reddit Cookie header value: ' REDDIT_BROWSER_COOKIE
printf '\n'
export REDDIT_BROWSER_COOKIE

# Optional: copy matching non-secret values from the same browser request.
export REDDIT_USER_AGENT='<your browser User-Agent header>'
export REDDIT_BROWSER_ACCEPT_LANGUAGE='<your browser Accept-Language header>'
export REDDIT_BROWSER_SEC_CH_UA='<your browser Sec-CH-UA header>'
export REDDIT_BROWSER_SEC_CH_UA_MOBILE='?0' # only ?0 or ?1
export REDDIT_BROWSER_SEC_CH_UA_PLATFORM='<your browser Sec-CH-UA-Platform header>'

scripts/run-assignment.sh

unset REDDIT_BROWSER_COOKIE REDDIT_USER_AGENT REDDIT_BROWSER_ACCEPT_LANGUAGE
unset REDDIT_BROWSER_SEC_CH_UA REDDIT_BROWSER_SEC_CH_UA_MOBILE
unset REDDIT_BROWSER_SEC_CH_UA_PLATFORM
```

Environment variables are not a secret store: the cookie remains in this shell until
it is unset and may be visible to same-user process inspection, crash diagnostics, or
other tooling while DuckWords runs. Use only a session you are authorized to use,
never reuse or share somebody else's cookie, and sign out/revoke the session when the
test is finished. Browser sessions may expire at any time. DuckWords sends the
configured cookie only to its fixed Reddit request origin, never logs it, keeps no
cookie jar, and deliberately ignores response `Set-Cookie` values, so it neither
refreshes nor persists the session.

This `.json` renderer is the assignment's public-page access contract, not a claim
that anonymous access is part of Reddit's current supported OAuth Data API. Reddit
may return a login redirect or HTML denial based on current access policy, session
state, IP range, or network. DuckWords rejects every redirect and non-JSON response
rather than following it or silently returning incomplete data; run the opt-in smoke
from the same local network intended for the final capture.

With no options it attempts to process the assignment inputs using the documented
defaults. At 0.8 requests/second the 200 initial listings alone take about 4 minutes
10 seconds after access succeeds. Every unresolved `more` child, continuation, and
retry adds another paced request, so the actual duration depends on the live trees and
is bounded by the 30-minute default unless a larger validated timeout is selected
explicitly.

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
`artifacts/review/reddit-smoke/`. It strips every `REDDIT_BROWSER_*` value so a stale
session cannot make this cookie-free check look successful. If it returns `302` or
`403`, the cookie-free path is unavailable from that environment. The optional
personal-session fallback above may be used only as an authenticated run; never
present it as cookie-free and do not use a proxy or hidden endpoint override.

To create the three files requested for review, run the lightweight assignment
helper. It invokes the main CLI once, forces JSON operational logs, and refuses to
overwrite existing output files:

```bash
make assignment-run ARGS='--failure-mode=strict --timeout=2h'
```

This writes `artifacts/run/result.json`, `application.log`, and
`full-application.log`. The full log contains the application log followed by a
fixed marker and the exact JSON output, as requested by the assignment. Submit these
files only after checking that the command exited `0`; exit `3` is an explicitly
partial result. The directory is local and ignored by Git.

### 3. Or via Docker

```bash
make docker-build
docker run --rm \
  duckwords:review
```

Pass `-e REDDIT_USER_AGENT` only when using the optional non-secret override. The
direct-environment browser-session fallback is intentionally documented for local CLI
use only; do not place a session cookie in an image, Compose file, or Docker command.

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
