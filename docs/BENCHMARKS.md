# Local text measurement

A one-time measurement of DuckWords' word-counting path on synthetic local fixtures.
No Reddit request, credential, or assignment dataset is involved.

## Does this path matter?

**No — and that is the useful conclusion.** A DuckWords run is bound by Reddit request
pacing, not by counting words. At the default 0.8 requests/second the 200-post list
needs at least 250 seconds of network time before a single `more` expansion, while the
same run counts a few megabytes of comment text in tens of milliseconds.

Extrapolating the measurement below, **the comment corpus would have to reach roughly
37 GB before the text path merely equalled that 250-second network floor.** The
assignment corpus is smaller by four orders of magnitude.

This file exists to make that ratio explicit and to record what the counting path
actually costs, so a future change to it can be compared against a known number. It is
not a claim that the counting path was worth optimizing for this workload; the
allocation-free ASCII path exists because it is also the simpler contract to reason
about, not because a profile demanded it.

## Reproduce

```bash
make bench-text BENCH_COUNT=10
```

The target first runs `TestLocalTextFixtures`, then benchmarks the same inputs with
allocation reporting. The broader package suite remains available through `make bench`.

Nothing verifies these numbers automatically. `bench-text` is deliberately outside
`make verify` and CI, where shared-runner variance would make a threshold either noisy
or meaningless. Treat the table as a measurement to repeat by hand, not a gate.

## Correctness fixtures

The dictionary and texts live under [`testdata/performance`](../testdata/performance).
Their expected token statistics and exact count maps are asserted in
[`internal/words/local_text_test.go`](../internal/words/local_text_test.go).

| Fixture | Source bytes | Sequences | Eligible | Dictionary matches / counted | Expected counts |
|---|---:|---:|---:|---:|---|
| ASCII mixed case | 66 | 12 | 10 | 9 | `duck=3`, `duckling=1`, `pond=1`, `water=2`, `bird=1`, `wings=1` |
| Markdown and URL | 72 | 14 | 10 | 4 | `duck=2`, `pond=1`, `water=1` |
| Unicode | 83 | 10 | 9 | 6 | `café=2`, `вода=1`, `качка=2`, `птах=1` |

These cases intentionally cover ASCII lowercasing, apostrophes, hyphens, Markdown,
URL components, digits, Cyrillic letters, accented Latin letters, and a decomposed
combining-mark spelling.

## Measurement

Measured on 2026-08-14 with Go `go1.26.6`, `GOFIPS140=off`, `darwin/arm64`, Apple M4
Max with low-power mode disabled, ten repetitions, approximately 128 KiB of text per
operation prepared before timing.

Median of ten runs, with the observed spread relative to that median:

| Fixture | Input bytes/op | Counted tokens/op | Median ns/op | Spread | Throughput | ns/token | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| ASCII mixed case | 131,010 | 17,865 | 617,300 | −1.0% / +5.2% | 212.2 MB/s | 34.6 | 0 | 0 |
| Markdown and URL | 131,040 | 7,280 | 433,469 | −0.6% / +6.4% | 302.3 MB/s | 59.5 | 0 | 0 |
| Unicode | 131,057 | 9,474 | 1,674,278 | −2.3% / +7.1% | 78.3 MB/s | 176.7 | 189,480 | 14,211 |

Both ASCII fixtures are allocation-free. Markdown is faster per byte than plain ASCII
because most of its letter runs are URL and markup fragments that miss the dictionary,
so they cost one lookup instead of a lookup plus a counter update.

## What the numbers mean

**The ASCII path is at the cost of its hash lookups.** Each eligible token costs about
31 ns, which covers scanning roughly seven bytes, one dictionary lookup, one counter
read-modify-write, and a filter check. Two Go map operations on string keys account for
essentially all of that, so scanning and case normalization are close to free. Getting
materially faster would mean replacing the map — a perfect hash or an FST — which is
not justified by a workload measured in milliseconds.

**The Unicode path allocates once per token, and that is not the price of Unicode.**
The tokenizer materializes every candidate with `string(buffer)` in
[`tokenizer.go`](../internal/words/tokenizer.go), producing one allocation per emitted
token — about one per 9.2 input bytes, and 1.45× the input size in garbage. The ASCII
path avoids this by keying the map lookup off reusable scratch and storing only the
dictionary-owned canonical string. The same technique applies here: accumulate
lowercased runes into a `[]byte` scratch with `utf8.AppendRune`, then look up
`entries[string(scratch)]`, which the compiler performs without allocating. The
semantics would be unchanged.

It has not been done because the assignment corpus is English: bodies containing only
ASCII take the fast path, and the change would optimize a path that this workload
barely uses. It is an accepted, unoptimized branch, not a constraint.

## Scaling to real volumes

Two properties govern the extrapolation:

1. Throughput is linear in input size — the counting loop is a single pass with no
   super-linear structure.
2. **The ASCII/Unicode choice is made per comment body, not per token.** One emoji or
   accented character anywhere in a comment sends that entire body through the slower
   path. On Reddit that is common, so a realistic corpus is a blend.

Blending the two measured throughputs by the share of bytes in non-ASCII bodies:

| Non-ASCII bodies | Blended throughput | 5 MB | 50 MB | 500 MB |
|---:|---:|---:|---:|---:|
| 0% | 212.2 MB/s | 0.024 s | 0.24 s | 2.4 s |
| 10% | 181.2 MB/s | 0.028 s | 0.28 s | 2.8 s |
| 25% | 148.6 MB/s | 0.034 s | 0.34 s | 3.4 s |
| 50% | 114.4 MB/s | 0.044 s | 0.44 s | 4.4 s |

Against the 250-second network floor for 200 posts, a 50 MB corpus at a 25% non-ASCII
mix spends **0.135% of the run counting words**. The corpus for this assignment is far
smaller; the real figure is a rounding error, and this holds even if the estimate is
wrong by an order of magnitude in either direction.

Memory does not scale with corpus size. Each worker's post-local map is bounded by
`MaxDistinctWordsPerPost` (50,000) and by the dictionary size, whichever is smaller, so
a larger corpus increases counter values rather than the number of keys. Unicode
allocations do scale: a 50 MB corpus at a 25% mix produces roughly 1.4 million
short-lived allocations, which is GC pressure rather than retained memory.

## Limits

- Synthetic fixtures on one machine. Compare changes on the same machine and toolchain;
  these are not cross-machine numbers.
- The benchmark reuses a preallocated post-local count map and excludes fixture I/O,
  dictionary construction, HTTP, OAuth, Reddit latency, retries, and aggregation.
  Aggregation is bounded by distinct words rather than corpus size and is negligible at
  any of the volumes above.
- The 250-second network floor assumes one request per post at the default rate. A real
  run issues more: every `more` placeholder batch and every depth-truncated branch adds
  a request, which widens the gap rather than narrowing it.
- The non-ASCII share of a real r/duck corpus has not been measured, because no live run
  has been made. The table brackets the plausible range instead of asserting a value.
