# Local text tests and benchmarks

This file records a small, readable performance baseline for DuckWords' text path.
The inputs are synthetic local fixtures; no Reddit request, credential, or assignment
dataset is used.

## Reproduce

```bash
make bench-text BENCH_COUNT=5
```

The target first runs `TestLocalTextFixtures`, then benchmarks the same inputs with
allocation reporting. The broader package benchmark suite remains available through
`make bench`.

## Correctness fixtures

The dictionary and texts live under [`testdata/performance`](testdata/performance).
Their expected token statistics and exact count maps are asserted in
[`internal/words/local_text_test.go`](internal/words/local_text_test.go).

| Fixture | Source bytes | Sequences | Eligible | Dictionary matches / counted | Expected counts |
|---|---:|---:|---:|---:|---|
| ASCII mixed case | 66 | 12 | 10 | 9 | `duck=3`, `duckling=1`, `pond=1`, `water=2`, `bird=1`, `wings=1` |
| Markdown and URL | 72 | 14 | 10 | 4 | `duck=2`, `pond=1`, `water=1` |
| Unicode | 83 | 10 | 9 | 6 | `café=2`, `вода=1`, `качка=2`, `птах=1` |

These cases intentionally cover ASCII lowercasing, apostrophes, hyphens, Markdown,
URL components, digits, Cyrillic letters, accented Latin letters, and a decomposed
combining-mark spelling.

## Recorded baseline

Recorded on 2026-08-14 with:

- Go `go1.26.6`, `GOFIPS140=off`;
- `darwin/arm64`;
- Apple M4 Max;
- five benchmark repetitions;
- approximately 128 KiB of text per operation, prepared before timing.

The table reports the median `ns/op` from five runs. Throughput is the value from the
same median run.

| Fixture | Input bytes/op | Counted tokens/op | Median ns/op | Throughput | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| ASCII mixed case | 131,010 | 17,865 | 937,749 | 139.71 MB/s | 0 | 0 |
| Markdown and URL | 131,040 | 7,280 | 657,175 | 199.40 MB/s | 0 | 0 |
| Unicode | 131,057 | 9,474 | 2,492,968 | 52.57 MB/s | 189,481 | 14,211 |

## Interpretation and limits

- ASCII fixtures use the optimized counter path and perform no timed allocations.
- Unicode uses the general `unicode.IsLetter` tokenizer and allocates normalized
  token strings. The result is slower but preserves the documented Unicode semantics.
- The benchmark reuses a preallocated post-local count map and excludes fixture I/O,
  dictionary construction, HTTP, OAuth, Reddit latency, retries, and aggregation.
- These numbers are a local regression baseline, not a cross-machine performance
  promise. Compare changes on the same machine and toolchain.
