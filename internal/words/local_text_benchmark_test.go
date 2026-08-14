package words

import (
	"maps"
	"strings"
	"testing"
)

const localTextBenchmarkTargetBytes = 128 << 10

var benchmarkLocalTextCounts map[string]uint64

func BenchmarkCounterLocalTextFixtures(b *testing.B) {
	counter := newLocalTextCounter(b)

	for _, fixture := range localTextFixtures {
		fixture := fixture
		b.Run(fixture.name, func(b *testing.B) {
			baseText := readLocalTextFixture(b, fixture.file)
			repetitions := max(1, localTextBenchmarkTargetBytes/len(baseText))
			text := strings.Repeat(baseText, repetitions)
			wantCounts := make(map[string]uint64, len(fixture.counts))
			for word, count := range fixture.counts {
				wantCounts[word] = count * uint64(repetitions)
			}

			counts := make(map[string]uint64, len(wantCounts))
			stats, err := counter.AddText(text, counts)
			if err != nil {
				b.Fatalf("AddText() validation error = %v", err)
			}
			if stats.CountedTokens != fixture.stats.CountedTokens*repetitions || !maps.Equal(counts, wantCounts) {
				b.Fatalf("scaled fixture produced stats=%+v counts=%#v", stats, counts)
			}

			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			b.ResetTimer()
			for range b.N {
				clear(counts)
				if _, err := counter.AddText(text, counts); err != nil {
					b.Fatalf("AddText() error = %v", err)
				}
			}
			b.ReportMetric(float64(stats.CountedTokens), "tokens/op")
			benchmarkLocalTextCounts = counts
		})
	}
}
