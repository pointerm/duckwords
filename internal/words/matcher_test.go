package words

import (
	"errors"
	"strings"
	"testing"
)

func TestMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		patterns []string
		word     string
		want     bool
	}{
		{name: "no filters", word: "duck", want: true},
		{name: "no filters reject short", word: "an", want: false},
		{name: "no filters reject punctuation", word: "duck!", want: false},
		{name: "exact", patterns: []string{"duck"}, word: "DUCK", want: true},
		{name: "exact anchored", patterns: []string{"duck"}, word: "duckling", want: false},
		{name: "prefix", patterns: []string{"DUCK*"}, word: "duckling", want: true},
		{name: "suffix", patterns: []string{"*ling"}, word: "duckling", want: true},
		{name: "middle", patterns: []string{"d*ck"}, word: "duck", want: true},
		{name: "star matches zero", patterns: []string{"d*ck"}, word: "dck", want: true},
		{name: "star all", patterns: []string{"*"}, word: "goose", want: true},
		{name: "adjacent stars", patterns: []string{"du**ck"}, word: "duck", want: true},
		{name: "OR semantics", patterns: []string{"goose", "duck*"}, word: "duckling", want: true},
		{name: "unicode case", patterns: []string{"CAFÉ"}, word: "café", want: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			matcher, err := NewMatcher(test.patterns)
			if err != nil {
				t.Fatalf("NewMatcher() error = %v", err)
			}
			if got := matcher.Match(test.word); got != test.want {
				t.Fatalf("Match(%q) = %t, want %t", test.word, got, test.want)
			}
		})
	}
}

func TestNewMatcherRejectsInvalidPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		patterns []string
		wantErr  error
	}{
		{name: "empty", patterns: []string{""}, wantErr: ErrInvalidPattern},
		{name: "question mark", patterns: []string{"duck?"}, wantErr: ErrInvalidPattern},
		{name: "space", patterns: []string{"duck *"}, wantErr: ErrInvalidPattern},
		{name: "digit", patterns: []string{"duck2"}, wantErr: ErrInvalidPattern},
		{name: "invalid utf8", patterns: []string{string([]byte{'d', 0xff})}, wantErr: ErrInvalidPattern},
		{name: "too long", patterns: []string{strings.Repeat("d", maxFilterRunes+1)}, wantErr: ErrPatternTooLong},
	}

	tooMany := make([]string, maxFilterPatterns+1)
	for index := range tooMany {
		tooMany[index] = "duck"
	}
	tests = append(tests, struct {
		name     string
		patterns []string
		wantErr  error
	}{name: "too many", patterns: tooMany, wantErr: ErrTooManyPatterns})

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewMatcher(test.patterns)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("NewMatcher() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestNewMatcherAcceptsConfiguredBoundaries(t *testing.T) {
	t.Parallel()

	patterns := make([]string, maxFilterPatterns)
	for index := range patterns {
		patterns[index] = strings.Repeat("a", maxFilterRunes)
	}
	matcher, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("NewMatcher(boundary patterns) error = %v", err)
	}
	if len(matcher.patterns) != maxFilterPatterns {
		t.Fatalf("compiled patterns = %d, want %d", len(matcher.patterns), maxFilterPatterns)
	}

	collapsed, err := NewMatcher([]string{"***"})
	if err != nil {
		t.Fatalf("NewMatcher(adjacent stars) error = %v", err)
	}
	if got := string(collapsed.patterns[0]); got != "*" {
		t.Fatalf("compiled adjacent stars = %q, want *", got)
	}
}

func TestMatchGlobAgainstReference(t *testing.T) {
	t.Parallel()

	patterns := generateRuneStrings([]rune{'a', 'b', '*'}, 5)
	words := generateRuneStrings([]rune{'a', 'b'}, 5)
	for _, pattern := range patterns {
		for _, word := range words {
			got := matchGlob(pattern, word)
			want := referenceGlob(pattern, word)
			if got != want {
				t.Fatalf("matchGlob(%q, %q) = %t, want %t", string(pattern), string(word), got, want)
			}
			if gotASCII := matchGlobASCII(pattern, string(word)); gotASCII != want {
				t.Fatalf("matchGlobASCII(%q, %q) = %t, want %t", string(pattern), string(word), gotASCII, want)
			}
		}
	}
}

func FuzzMatcher(f *testing.F) {
	seeds := [][3]string{
		{"duck", "goose", "DUCK"},
		{"d*ck", "*ling", "duck"},
		{"***", "CAFÉ", "café"},
		{"", "duck", "duck"},
		{string([]byte{'d', 0xff}), "*", "duck"},
	}
	for _, seed := range seeds {
		f.Add(seed[0], seed[1], seed[2])
	}

	f.Fuzz(func(t *testing.T, first, second, word string) {
		// Keep the independent DP oracle itself bounded. Production counting
		// applies a tighter dictionary-derived token limit.
		if len(word) > 4<<10 {
			return
		}
		matcher, err := NewMatcher([]string{first, second})
		if err != nil {
			if !errors.Is(err, ErrInvalidPattern) && !errors.Is(err, ErrPatternTooLong) {
				t.Fatalf("NewMatcher() returned uncontrolled error: %v", err)
			}
			return
		}

		normalized, runeCount, valid := normalizeLetters(word)
		want := false
		if valid && runeCount >= minimumWordRunes {
			wordRunes := []rune(normalized)
			for _, pattern := range matcher.patterns {
				want = want || referenceGlob(pattern, wordRunes)
			}
		}
		if got := matcher.Match(word); got != want {
			t.Fatalf("Match(%q) with patterns %q = %t, want %t", word, []string{first, second}, got, want)
		}
	})
}

func referenceGlob(pattern, word []rune) bool {
	rows := len(pattern) + 1
	columns := len(word) + 1
	dp := make([]bool, rows*columns)
	dp[0] = true
	for patternIndex := 1; patternIndex < rows; patternIndex++ {
		if pattern[patternIndex-1] == '*' {
			dp[patternIndex*columns] = dp[(patternIndex-1)*columns]
		}
	}

	for patternIndex := 1; patternIndex < rows; patternIndex++ {
		for wordIndex := 1; wordIndex < columns; wordIndex++ {
			cell := patternIndex*columns + wordIndex
			switch pattern[patternIndex-1] {
			case '*':
				dp[cell] = dp[(patternIndex-1)*columns+wordIndex] || dp[patternIndex*columns+wordIndex-1]
			default:
				dp[cell] = pattern[patternIndex-1] == word[wordIndex-1] && dp[(patternIndex-1)*columns+wordIndex-1]
			}
		}
	}
	return dp[len(dp)-1]
}

func generateRuneStrings(alphabet []rune, maximumLength int) [][]rune {
	result := [][]rune{{}}
	frontier := [][]rune{{}}
	for length := 1; length <= maximumLength; length++ {
		next := make([][]rune, 0, len(frontier)*len(alphabet))
		for _, prefix := range frontier {
			for _, r := range alphabet {
				value := append(append([]rune(nil), prefix...), r)
				result = append(result, value)
				next = append(next, value)
			}
		}
		frontier = next
	}
	return result
}
