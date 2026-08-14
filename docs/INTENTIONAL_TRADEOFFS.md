# Intentional trade-offs and non-goals

DuckWords is a bounded, one-shot CLI built for the assignment, not a general-purpose
Reddit ingestion platform. The items below were considered and deliberately left out;
their absence is a scope decision rather than an overlooked implementation task.

## 1. Persistent caching

DuckWords does not cache source documents, Reddit responses, comment trees, or final
counts across runs. The assignment expects a fresh aggregation, and
a persistent cache would require expiry rules, invalidation, storage permissions,
schema migration, and careful handling of deleted Reddit content. Within one process,
HTTP connections are reused where safe, but nothing is written as a reusable data cache.

## 2. Prometheus metrics

There is no Prometheus endpoint or metrics server. DuckWords exits after one bounded
job, so keeping an HTTP listener alive solely for scraping would complicate the
process lifecycle and container security model. Structured stderr events and the
terminal summary already expose post outcomes, retries, throttling, duration, and
work counters for this use case. A long-running service deployment would be the right
place to add Prometheus instrumentation.

## 3. Third-party CLI frameworks

The CLI uses Go's standard `flag` package rather than Cobra, urfave/cli, Viper, or a
similar framework. The command has one level and a small, fixed option set, so a
framework would add dependency and configuration precedence without materially
improving usability. Parsing, duplicate-option checks, help text, and sanitized
diagnostics remain explicit and testable.

## 4. Third-party HTTP client libraries

HTTP is implemented with Go's standard `net/http` stack instead of Resty or another
wrapper. This is intentional: the project needs precise control over transports,
timeouts, redirects, body limits, retries, rate limiting, connection reuse, fixed
destinations, and response closing. The native client provides those primitives
without hiding request ownership or adding another runtime dependency.

## 5. Cross-process rate-limit coordination

The adaptive limiter, retry counters, and Reddit rate-limit state are shared by all
workers **inside one process only**. Two DuckWords processes do not coordinate, so
together they may send requests faster than either process observes locally. The
supported operational model is one active DuckWords process. Supporting
horizontal execution would require an external lease and distributed rate limiter
(for example Redis or a database), plus shared retry-state semantics.

## 6. Checkpointing and resume

A stopped run starts again from the input list; it does not resume from a checkpoint.
Persisting partial post maps would weaken the current transactional rule that counts
are merged only after a complete comment tree is proven. A production-scale crawler
could add a versioned checkpoint store, but that is unnecessary for 200 bounded posts.

## 7. General-purpose Reddit client behavior

The implementation is intentionally narrow: one fixed public `old.reddit.com` JSON
origin, the assignment's comment pages, and allowlisted input hosts. It does not
support OAuth, browser cookies, user-login flows, arbitrary Reddit
endpoints, arbitrary remote input origins, posting, moderation actions, or streaming.
Local input files remain available for deterministic and offline use.

The cookie-free public-JSON choice follows the assignment owner's explicit direction
and keeps the run reproducible and independent of a personal browser session. Current
Reddit access policy may return `302` or `403` for that request profile, making a live
capture impossible from some networks. DuckWords reports that limitation instead of
replaying browser cookies or presenting synthetic output as live evidence.

## 8. Rich-text interpretation

Comment bodies are tokenized as the raw text returned by Reddit. Markdown rendering,
HTML interpretation, URL stripping, and natural-language segmentation were not added
because they would change the assignment's word semantics and require substantially
more parsing policy. The chosen behavior is deterministic and documented in the main
README.
