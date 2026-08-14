package words

import (
	"strings"
	"testing"
)

var (
	benchmarkDictionary Dictionary
	benchmarkInteger    int
	benchmarkMatch      bool
)

func BenchmarkLoadDictionary(b *testing.B) {
	const entries = 50_000

	var source strings.Builder
	source.Grow(entries * 7)
	for index := range entries {
		source.WriteString(benchmarkWord(index))
		source.WriteByte('\n')
	}
	input := source.String()
	limits := DefaultDictionaryLimits()

	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for range b.N {
		dictionary, _, err := LoadDictionary(strings.NewReader(input), limits)
		if err != nil {
			b.Fatalf("LoadDictionary() error = %v", err)
		}
		benchmarkDictionary = dictionary
	}
}

func BenchmarkTokenizer(b *testing.B) {
	text := strings.Repeat("Duck-water café42 bird's wings! ", 4_096)
	tokenizer := DefaultTokenizer()

	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for range b.N {
		emitted := 0
		_, err := tokenizer.Scan(text, func(string) error {
			emitted++
			return nil
		})
		if err != nil {
			b.Fatalf("Scan() error = %v", err)
		}
		benchmarkInteger = emitted
	}
}

func BenchmarkMatcher(b *testing.B) {
	tests := []struct {
		name    string
		pattern string
		word    string
	}{
		{name: "exact", pattern: "duckling", word: "duckling"},
		{name: "wildcard", pattern: "d*u*c*k*", word: "duckling"},
	}
	for _, test := range tests {
		matcher, err := NewMatcher([]string{test.pattern})
		if err != nil {
			b.Fatalf("NewMatcher() error = %v", err)
		}
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkMatch = matcher.Match(test.word)
			}
		})
	}
}

func BenchmarkCounter(b *testing.B) {
	dictionary, _, err := LoadDictionary(
		strings.NewReader("bird\nduck\nduckling\npond\nwater\nwings\n"),
		DefaultDictionaryLimits(),
	)
	if err != nil {
		b.Fatalf("LoadDictionary() error = %v", err)
	}
	matcher, err := NewMatcher([]string{"duck*", "water", "bird", "wings"})
	if err != nil {
		b.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		b.Fatalf("NewCounter() error = %v", err)
	}
	text := strings.Repeat("Duck duckling water bird wings unknown42. ", 1_024)

	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for range b.N {
		counts := make(map[string]uint64)
		if _, err := counter.AddText(text, counts); err != nil {
			b.Fatalf("AddText() error = %v", err)
		}
		benchmarkInteger = len(counts)
	}
}

func BenchmarkCounterASCIIHotPath(b *testing.B) {
	dictionary, _, err := LoadDictionary(
		strings.NewReader("bird\nduck\nduckling\npond\nwater\nwings\n"),
		DefaultDictionaryLimits(),
	)
	if err != nil {
		b.Fatalf("LoadDictionary() error = %v", err)
	}
	matcher, err := NewMatcher([]string{"duck*"})
	if err != nil {
		b.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		b.Fatalf("NewCounter() error = %v", err)
	}

	benchmarks := []struct {
		name     string
		text     string
		existing bool
		clear    bool
	}{
		{name: "dictionary_misses", text: strings.Repeat("unknown ", 1_024)},
		{name: "lowercase_repeated_hits", text: strings.Repeat("duck ", 1_024), existing: true},
		{name: "mixed_case_repeated_hits", text: strings.Repeat("DUCK Duck dUcK ", 342), existing: true},
		{name: "first_distinct_preallocated", text: "DUCK", clear: true},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			counts := make(map[string]uint64, 1)
			if benchmark.existing {
				counts["duck"] = 1
			} else if benchmark.clear {
				counts["warmup"] = 1
				clear(counts)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(benchmark.text)))
			b.ResetTimer()
			for range b.N {
				if benchmark.clear {
					clear(counts)
				}
				if _, countErr := counter.AddText(benchmark.text, counts); countErr != nil {
					b.Fatalf("AddText() error = %v", countErr)
				}
			}
		})
	}
}

func benchmarkWord(value int) string {
	var word [6]byte
	for index := range word {
		word[index] = byte('a' + value%26)
		value /= 26
	}
	return string(word[:])
}
