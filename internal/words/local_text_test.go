package words

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

const localTextFixtureDirectory = "../../testdata/performance"

type localTextFixture struct {
	name   string
	file   string
	stats  CountStats
	counts map[string]uint64
}

var localTextFixtures = []localTextFixture{
	{
		name: "ascii",
		file: "ascii.txt",
		stats: CountStats{
			Sequences:         12,
			EligibleTokens:    10,
			DictionaryMatches: 9,
			CountedTokens:     9,
			TooShort:          2,
		},
		counts: map[string]uint64{
			"bird": 1, "duck": 3, "duckling": 1, "pond": 1, "water": 2, "wings": 1,
		},
	},
	{
		name: "markdown",
		file: "markdown.txt",
		stats: CountStats{
			Sequences:         14,
			EligibleTokens:    10,
			DictionaryMatches: 4,
			CountedTokens:     4,
			TooShort:          4,
		},
		counts: map[string]uint64{"duck": 2, "pond": 1, "water": 1},
	},
	{
		name: "unicode",
		file: "unicode.txt",
		stats: CountStats{
			Sequences:         10,
			EligibleTokens:    9,
			DictionaryMatches: 6,
			CountedTokens:     6,
			TooShort:          1,
		},
		counts: map[string]uint64{"café": 2, "вода": 1, "качка": 2, "птах": 1},
	},
}

func TestLocalTextFixtures(t *testing.T) {
	t.Parallel()

	counter := newLocalTextCounter(t)
	for _, fixture := range localTextFixtures {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			text := readLocalTextFixture(t, fixture.file)
			counts := make(map[string]uint64)
			stats, err := counter.AddText(text, counts)
			if err != nil {
				t.Fatalf("AddText() error = %v", err)
			}
			if stats != fixture.stats {
				t.Fatalf("CountStats = %+v, want %+v", stats, fixture.stats)
			}
			if !maps.Equal(counts, fixture.counts) {
				t.Fatalf("counts = %#v, want %#v", counts, fixture.counts)
			}
		})
	}
}

func newLocalTextCounter(tb testing.TB) Counter {
	tb.Helper()

	dictionaryFile, err := os.Open(filepath.Join(localTextFixtureDirectory, "dictionary.txt"))
	if err != nil {
		tb.Fatalf("open local benchmark dictionary: %v", err)
	}
	defer func() {
		if closeErr := dictionaryFile.Close(); closeErr != nil {
			tb.Errorf("close local benchmark dictionary: %v", closeErr)
		}
	}()

	dictionary, _, err := LoadDictionary(dictionaryFile, DefaultDictionaryLimits())
	if err != nil {
		tb.Fatalf("LoadDictionary() error = %v", err)
	}
	matcher, err := NewMatcher(nil)
	if err != nil {
		tb.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		tb.Fatalf("NewCounter() error = %v", err)
	}
	return counter
}

func readLocalTextFixture(tb testing.TB, name string) string {
	tb.Helper()

	payload, err := os.ReadFile(filepath.Join(localTextFixtureDirectory, name))
	if err != nil {
		tb.Fatalf("read local text fixture %q: %v", name, err)
	}
	return string(payload)
}
