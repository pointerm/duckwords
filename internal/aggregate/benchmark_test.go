package aggregate

import "testing"

var (
	benchmarkDestination map[string]uint64
	benchmarkRanking     []WordCount
)

func BenchmarkMerge(b *testing.B) {
	const entries = 20_000
	source := make(map[string]uint64, entries)
	for index := range entries {
		source[benchmarkAggregateWord(index)] = uint64(index%100 + 1)
	}

	b.ReportAllocs()
	b.ReportMetric(entries, "entries/op")
	b.ResetTimer()
	for range b.N {
		destination := make(map[string]uint64, entries)
		if err := Merge(destination, source); err != nil {
			b.Fatalf("Merge() error = %v", err)
		}
		benchmarkDestination = destination
	}
}

func BenchmarkTopN(b *testing.B) {
	const entries = 50_000
	counts := make(map[string]uint64, entries)
	for index := range entries {
		counts[benchmarkAggregateWord(index)] = uint64(index%1_000 + 1)
	}

	b.ReportAllocs()
	b.ReportMetric(entries, "entries/op")
	b.ResetTimer()
	for range b.N {
		var err error
		benchmarkRanking, err = TopN(counts, 10)
		if err != nil {
			b.Fatalf("TopN() error = %v", err)
		}
	}
}

func benchmarkAggregateWord(value int) string {
	var word [8]byte
	for index := range word {
		word[index] = byte('a' + value%26)
		value /= 26
	}
	return string(word[:])
}
