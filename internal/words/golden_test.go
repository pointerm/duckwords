package words_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/words"
)

const phaseOneFixtureDirectory = "../../testdata/phase1"

var referenceWordPattern = regexp.MustCompile(`\p{L}+`)

func TestPhaseOneGoldenPipeline(t *testing.T) {
	t.Parallel()

	dictionarySource := readFixture(t, "dictionary.txt")
	corpus := readCorpusFixture(t)
	dictionary := loadProductionDictionary(t, dictionarySource)
	counts := countProductionCorpus(t, dictionary, nil, corpus)

	wantCounts := countReferenceCorpus(t, dictionarySource, nil, corpus)
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("production counts = %#v, reference counts = %#v", counts, wantCounts)
	}

	ranked, err := aggregate.TopN(counts, 10)
	if err != nil {
		t.Fatalf("TopN() error = %v", err)
	}
	encoded, err := json.MarshalIndent(ranked, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	encoded = append(encoded, '\n')

	wantJSON := readFixture(t, "expected.json")
	if !bytes.Equal(encoded, wantJSON) {
		t.Fatalf("golden JSON mismatch\ngot:\n%s\nwant:\n%s", encoded, wantJSON)
	}
}

func TestPhaseOneRawSourceSemantics(t *testing.T) {
	t.Parallel()

	dictionary := loadProductionDictionary(t, readFixture(t, "dictionary.txt"))
	corpus := []string{"&amp; [Duckling](https://example.test/duck)"}

	// Phase 1 intentionally scans raw source. It neither decodes HTML entities nor
	// renders Markdown, so both the literal entity name and URL path are candidates.
	want := map[string]uint64{"amp": 1, "duck": 1, "duckling": 1}
	got := countProductionCorpus(t, dictionary, nil, corpus)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw-source counts = %#v, want %#v", got, want)
	}
}

func TestPhaseOneFilterGoldenAndSubset(t *testing.T) {
	t.Parallel()

	dictionarySource := readFixture(t, "dictionary.txt")
	dictionary := loadProductionDictionary(t, dictionarySource)
	corpus := readCorpusFixture(t)
	allCounts := countProductionCorpus(t, dictionary, nil, corpus)

	patterns := []string{"DUCK*", "wings"}
	got := countProductionCorpus(t, dictionary, patterns, corpus)
	want := countReferenceCorpus(t, dictionarySource, patterns, corpus)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered counts = %#v, reference counts = %#v", got, want)
	}
	if !reflect.DeepEqual(got, map[string]uint64{"duck": 7, "duckling": 1, "wings": 1}) {
		t.Fatalf("filtered counts = %#v, want duck wildcard plus exact wings", got)
	}
	for word, count := range got {
		if count > allCounts[word] {
			t.Fatalf("filter increased %q from %d to %d", word, allCounts[word], count)
		}
	}
}

func TestPhaseOnePartitionMergeAndPermutation(t *testing.T) {
	t.Parallel()

	dictionary := loadProductionDictionary(t, readFixture(t, "dictionary.txt"))
	corpus := readCorpusFixture(t)
	baselineCounts := countProductionCorpus(t, dictionary, nil, []string{strings.Join(corpus, "\n")})
	baselineJSON := rankJSON(t, baselineCounts)

	partitioned := make(map[string]uint64)
	for _, body := range corpus {
		local := countProductionCorpus(t, dictionary, nil, []string{body})
		if err := aggregate.Merge(partitioned, local); err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
	}
	if !reflect.DeepEqual(partitioned, baselineCounts) {
		t.Fatalf("partitioned counts = %#v, combined counts = %#v", partitioned, baselineCounts)
	}

	for index, permuted := range permutations(corpus) {
		gotCounts := countProductionCorpus(t, dictionary, nil, permuted)
		if !reflect.DeepEqual(gotCounts, baselineCounts) {
			t.Fatalf("permutation %d counts = %#v, baseline = %#v", index, gotCounts, baselineCounts)
		}
		if gotJSON := rankJSON(t, gotCounts); !bytes.Equal(gotJSON, baselineJSON) {
			t.Fatalf("permutation %d JSON differs\ngot:\n%s\nwant:\n%s", index, gotJSON, baselineJSON)
		}
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(phaseOneFixtureDirectory, name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return content
}

func readCorpusFixture(t *testing.T) []string {
	t.Helper()

	var corpus []string
	if err := json.Unmarshal(readFixture(t, "corpus.json"), &corpus); err != nil {
		t.Fatalf("decode corpus fixture: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatal("corpus fixture must not be empty")
	}
	return corpus
}

func loadProductionDictionary(t *testing.T, source []byte) words.Dictionary {
	t.Helper()

	dictionary, _, err := words.LoadDictionary(bytes.NewReader(source), words.DefaultDictionaryLimits())
	if err != nil {
		t.Fatalf("LoadDictionary() error = %v", err)
	}
	return dictionary
}

func countProductionCorpus(t *testing.T, dictionary words.Dictionary, patterns, corpus []string) map[string]uint64 {
	t.Helper()

	matcher, err := words.NewMatcher(patterns)
	if err != nil {
		t.Fatalf("NewMatcher(%q) error = %v", patterns, err)
	}
	counter, err := words.NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := make(map[string]uint64)
	for _, body := range corpus {
		if _, err := counter.AddText(body, counts); err != nil {
			t.Fatalf("AddText() error = %v", err)
		}
	}
	return counts
}

// countReferenceCorpus deliberately does not call the production tokenizer,
// dictionary membership, matcher, counter, or merger. This keeps the fixture useful
// for detecting a consistent bug shared by the production pipeline components.
func countReferenceCorpus(t *testing.T, dictionarySource []byte, patterns, corpus []string) map[string]uint64 {
	t.Helper()

	dictionary := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(dictionarySource))
	for scanner.Scan() {
		candidate := strings.TrimSpace(scanner.Text())
		if utf8.RuneCountInString(candidate) >= 3 && onlyLetters(candidate) {
			dictionary[strings.ToLower(candidate)] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan reference dictionary: %v", err)
	}

	counts := make(map[string]uint64)
	for _, body := range corpus {
		for _, candidate := range referenceWordPattern.FindAllString(body, -1) {
			word := strings.ToLower(candidate)
			if utf8.RuneCountInString(word) < 3 {
				continue
			}
			if _, found := dictionary[word]; !found {
				continue
			}
			if !referenceMatches(patterns, word) {
				continue
			}
			counts[word]++
		}
	}
	return counts
}

func onlyLetters(value string) bool {
	for _, r := range value {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return value != ""
}

func referenceMatches(patterns []string, word string) bool {
	if len(patterns) == 0 {
		return true
	}
	wordRunes := []rune(word)
	for _, pattern := range patterns {
		if referenceGlob([]rune(strings.ToLower(pattern)), wordRunes) {
			return true
		}
	}
	return false
}

func referenceGlob(pattern, word []rune) bool {
	table := make([][]bool, len(pattern)+1)
	for i := range table {
		table[i] = make([]bool, len(word)+1)
	}
	table[0][0] = true
	for patternIndex := 1; patternIndex <= len(pattern); patternIndex++ {
		if pattern[patternIndex-1] == '*' {
			table[patternIndex][0] = table[patternIndex-1][0]
		}
		for wordIndex := 1; wordIndex <= len(word); wordIndex++ {
			switch pattern[patternIndex-1] {
			case '*':
				table[patternIndex][wordIndex] = table[patternIndex-1][wordIndex] || table[patternIndex][wordIndex-1]
			default:
				table[patternIndex][wordIndex] = table[patternIndex-1][wordIndex-1] && pattern[patternIndex-1] == word[wordIndex-1]
			}
		}
	}
	return table[len(pattern)][len(word)]
}

func rankJSON(t *testing.T, counts map[string]uint64) []byte {
	t.Helper()

	ranked, err := aggregate.TopN(counts, 10)
	if err != nil {
		t.Fatalf("TopN() error = %v", err)
	}
	encoded, err := json.MarshalIndent(ranked, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	return append(encoded, '\n')
}

func permutations(values []string) [][]string {
	working := append([]string(nil), values...)
	result := make([][]string, 0)

	var visit func(int)
	visit = func(index int) {
		if index == len(working) {
			result = append(result, append([]string(nil), working...))
			return
		}
		for candidate := index; candidate < len(working); candidate++ {
			working[index], working[candidate] = working[candidate], working[index]
			visit(index + 1)
			working[index], working[candidate] = working[candidate], working[index]
		}
	}
	visit(0)
	return result
}
