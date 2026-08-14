package words

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

const (
	maxFilterPatterns = 100
	maxFilterRunes    = 256
)

var (
	// ErrTooManyPatterns indicates that a filter set exceeds its complexity bound.
	ErrTooManyPatterns = errors.New("too many filter patterns")
	// ErrPatternTooLong indicates that one filter exceeds its code-point bound.
	ErrPatternTooLong = errors.New("filter pattern exceeds length limit")
	// ErrInvalidPattern indicates empty input or unsupported filter syntax.
	ErrInvalidPattern = errors.New("invalid filter pattern")
)

// Matcher applies a validated set of case-insensitive exact or '*' glob filters. A
// zero Matcher accepts every otherwise valid word and is safe for concurrent readers.
type Matcher struct {
	patterns [][]rune
}

// NewMatcher validates and compiles filter patterns. Repeated patterns use OR
// semantics; '*' is the only metacharacter and matches zero or more letters.
func NewMatcher(patterns []string) (Matcher, error) {
	if len(patterns) > maxFilterPatterns {
		return Matcher{}, fmt.Errorf("%w: got %d, maximum %d", ErrTooManyPatterns, len(patterns), maxFilterPatterns)
	}

	compiled := make([][]rune, 0, len(patterns))
	for index, pattern := range patterns {
		if pattern == "" {
			return Matcher{}, fmt.Errorf("%w at index %d: pattern is empty", ErrInvalidPattern, index)
		}
		if utf8.RuneCountInString(pattern) > maxFilterRunes {
			return Matcher{}, fmt.Errorf("%w at index %d", ErrPatternTooLong, index)
		}

		normalized := make([]rune, 0, utf8.RuneCountInString(pattern))
		previousStar := false
		for _, r := range pattern {
			switch {
			case r == '*':
				// Collapsing adjacent stars removes redundant backtracking states
				// without changing the accepted language.
				if !previousStar {
					normalized = append(normalized, r)
				}
				previousStar = true
			case unicode.IsLetter(r):
				normalized = append(normalized, unicode.ToLower(r))
				previousStar = false
			default:
				return Matcher{}, fmt.Errorf("%w at index %d: only letters and '*' are supported", ErrInvalidPattern, index)
			}
		}

		compiled = append(compiled, normalized)
	}

	return Matcher{patterns: compiled}, nil
}

// Match reports whether word is valid and matches at least one configured pattern.
func (m Matcher) Match(word string) bool {
	normalized, runeCount, valid := normalizeLetters(word)
	if !valid || runeCount < minimumWordRunes {
		return false
	}
	return m.matchNormalized(normalized)
}

// matchNormalized relies on the tokenizer's validated lowercase-letter invariant and
// avoids repeating normalization in the counting hot path.
func (m Matcher) matchNormalized(word string) bool {
	if len(m.patterns) == 0 {
		return true
	}

	wordRunes := []rune(word)
	for _, pattern := range m.patterns {
		if matchGlob(pattern, wordRunes) {
			return true
		}
	}
	return false
}

// matchNormalizedASCIIString applies the compiled Unicode patterns directly to an
// already-lowercase ASCII word. Keeping this path byte based avoids allocating a
// []rune for the overwhelmingly common ASCII case.
func (m Matcher) matchNormalizedASCIIString(word string) bool {
	if len(m.patterns) == 0 {
		return true
	}

	for _, pattern := range m.patterns {
		if matchGlobASCII(pattern, word) {
			return true
		}
	}
	return false
}

// matchGlobASCII is equivalent to matchGlob for a lowercase ASCII word. Pattern
// literals remain runes, so a non-ASCII literal cannot equal an ASCII byte.
func matchGlobASCII(pattern []rune, word string) bool {
	patternIndex := 0
	wordIndex := 0
	lastStar := -1
	starWordIndex := 0

	for wordIndex < len(word) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == rune(word[wordIndex]):
			patternIndex++
			wordIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			lastStar = patternIndex
			patternIndex++
			starWordIndex = wordIndex
		case lastStar >= 0:
			patternIndex = lastStar + 1
			starWordIndex++
			wordIndex = starWordIndex
		default:
			return false
		}
	}

	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// matchGlob uses the standard last-star greedy algorithm without recursion or regex.
// Pattern work is capped by validation; the counting path also caps token length.
func matchGlob(pattern, word []rune) bool {
	patternIndex := 0
	wordIndex := 0
	lastStar := -1
	starWordIndex := 0

	for wordIndex < len(word) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == word[wordIndex]:
			patternIndex++
			wordIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			lastStar = patternIndex
			patternIndex++
			starWordIndex = wordIndex
		case lastStar >= 0:
			patternIndex = lastStar + 1
			starWordIndex++
			wordIndex = starWordIndex
		default:
			return false
		}
	}

	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}
