package words

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrNilCounts indicates that a caller did not provide its owned result map.
	ErrNilCounts = errors.New("nil count map")
	// ErrCountOverflow indicates that another occurrence cannot be represented.
	ErrCountOverflow = errors.New("word count overflow")
	// ErrInvalidDistinctWordLimit indicates that a bounded count was configured
	// without a positive maximum.
	ErrInvalidDistinctWordLimit = errors.New("invalid distinct-word limit")
	// ErrDistinctWordLimit indicates that retaining another unique word would exceed
	// the caller's per-result memory bound.
	ErrDistinctWordLimit = errors.New("distinct-word limit exceeded")
)

// CountStats describes how candidate tokens were filtered while processing text.
type CountStats struct {
	Sequences         int
	EligibleTokens    int
	DictionaryMatches int
	CountedTokens     int
	TooShort          int
	TooLong           int
}

// Counter applies one immutable dictionary and matcher to tokenized text. A Counter
// is safe for concurrent use when each caller owns a separate counts map.
type Counter struct {
	dictionary Dictionary
	matcher    Matcher
	tokenizer  Tokenizer
}

// NewCounter constructs a counter whose token buffer cannot grow beyond the longest
// accepted dictionary word. Longer alphabetic sequences cannot match the dictionary
// and are skipped without being retained in memory.
func NewCounter(dictionary Dictionary, matcher Matcher) (Counter, error) {
	if dictionary.Len() == 0 {
		return Counter{}, ErrEmptyDictionary
	}
	tokenizer, err := NewTokenizer(dictionary.maxWordRunes)
	if err != nil {
		return Counter{}, fmt.Errorf("configure tokenizer: %w", err)
	}
	return Counter{
		dictionary: dictionary,
		matcher:    matcher,
		tokenizer:  tokenizer,
	}, nil
}

// AddText adds every valid occurrence in text to the caller-owned counts map. If an
// overflow is reported, earlier tokens from this text may already be present; callers
// must discard that post-local map rather than merge a partial result.
func (c Counter) AddText(text string, counts map[string]uint64) (CountStats, error) {
	return c.addText(text, counts, 0)
}

// AddTextLimited is AddText with an explicit upper bound on the number of distinct
// keys retained in counts. maxDistinct must be positive, and a map already above the
// limit is rejected without mutation. Occurrences of keys already present exactly at
// the limit are still counted; the first new key beyond it returns ErrDistinctWordLimit
// without inserting that key. As with overflow, earlier tokens from this text may
// already be present and callers must discard a post-local map after an error.
func (c Counter) AddTextLimited(
	text string,
	counts map[string]uint64,
	maxDistinct int,
) (CountStats, error) {
	if maxDistinct <= 0 {
		return CountStats{}, fmt.Errorf("%w: maximum must be positive", ErrInvalidDistinctWordLimit)
	}
	if len(counts) > maxDistinct {
		return CountStats{}, fmt.Errorf("%w: maximum %d", ErrDistinctWordLimit, maxDistinct)
	}
	return c.addText(text, counts, maxDistinct)
}

func (c Counter) addText(
	text string,
	counts map[string]uint64,
	maxDistinct int,
) (CountStats, error) {
	if counts == nil {
		return CountStats{}, ErrNilCounts
	}
	if c.dictionary.Len() == 0 {
		return CountStats{}, ErrEmptyDictionary
	}
	if isASCII(text) {
		return c.addASCIIText(text, counts, maxDistinct)
	}
	return c.addTextUnicode(text, counts, maxDistinct)
}

func (c Counter) addTextUnicode(
	text string,
	counts map[string]uint64,
	maxDistinct int,
) (CountStats, error) {
	var stats CountStats
	tokenStats, err := c.tokenizer.Scan(text, func(token string) error {
		canonical, found := c.dictionary.canonicalNormalized(token)
		if !found {
			return nil
		}
		stats.DictionaryMatches++
		if !c.matcher.matchNormalized(canonical) {
			return nil
		}

		current, exists := counts[canonical]
		if current == math.MaxUint64 {
			return fmt.Errorf("%w for %q", ErrCountOverflow, canonical)
		}
		if !exists && maxDistinct > 0 && len(counts) >= maxDistinct {
			return fmt.Errorf("%w: maximum %d", ErrDistinctWordLimit, maxDistinct)
		}

		counts[canonical] = current + 1
		stats.CountedTokens++
		return nil
	})
	stats.Sequences = tokenStats.Sequences
	stats.EligibleTokens = tokenStats.Emitted
	stats.TooShort = tokenStats.TooShort
	stats.TooLong = tokenStats.TooLong
	if err != nil {
		return stats, err
	}

	return stats, nil
}

func (c Counter) addASCIIText(
	text string,
	counts map[string]uint64,
	maxDistinct int,
) (CountStats, error) {
	var stats CountStats
	sequenceStart := -1
	sequenceLength := 0
	hasUpper := false
	var stackScratch [64]byte
	var heapScratch []byte

	flush := func(end int) error {
		if sequenceStart < 0 {
			return nil
		}

		stats.Sequences++
		switch {
		case sequenceLength > c.tokenizer.maxTokenRunes:
			stats.TooLong++
		case sequenceLength < minimumWordRunes:
			stats.TooShort++
		default:
			stats.EligibleTokens++
			word := text[sequenceStart:end]
			if hasUpper {
				var normalized []byte
				if len(word) <= len(stackScratch) {
					normalized = stackScratch[:len(word)]
				} else {
					if cap(heapScratch) < len(word) {
						heapScratch = make([]byte, len(word))
					}
					normalized = heapScratch[:len(word)]
				}
				lowerASCII(normalized, word)
				return c.countASCIIBytes(normalized, counts, maxDistinct, &stats)
			}
			return c.countASCIIString(word, counts, maxDistinct, &stats)
		}
		return nil
	}

	for index := 0; index <= len(text); index++ {
		if index < len(text) && isASCIILetter(text[index]) {
			if sequenceStart < 0 {
				sequenceStart = index
			}
			sequenceLength++
			hasUpper = hasUpper || isASCIIUpper(text[index])
			continue
		}

		if err := flush(index); err != nil {
			return stats, fmt.Errorf("visit token: %w", err)
		}
		sequenceStart = -1
		sequenceLength = 0
		hasUpper = false
	}

	return stats, nil
}

func (c Counter) countASCIIString(
	word string,
	counts map[string]uint64,
	maxDistinct int,
	stats *CountStats,
) error {
	canonical, found := c.dictionary.canonicalNormalized(word)
	if !found {
		return nil
	}
	stats.DictionaryMatches++
	if !c.matcher.matchNormalizedASCIIString(canonical) {
		return nil
	}

	current, exists := counts[canonical]
	if current == math.MaxUint64 {
		return fmt.Errorf("%w for %q", ErrCountOverflow, canonical)
	}
	if !exists {
		if maxDistinct > 0 && len(counts) >= maxDistinct {
			return fmt.Errorf("%w: maximum %d", ErrDistinctWordLimit, maxDistinct)
		}
	}

	counts[canonical] = current + 1
	stats.CountedTokens++
	return nil
}

func (c Counter) countASCIIBytes(
	word []byte,
	counts map[string]uint64,
	maxDistinct int,
	stats *CountStats,
) error {
	// Conversion in a string-keyed map lookup is allocation-free. The returned value
	// is the stable, dictionary-owned key used for matching and counting.
	canonical, found := c.dictionary.entries[string(word)]
	if !found {
		return nil
	}
	stats.DictionaryMatches++
	if !c.matcher.matchNormalizedASCIIString(canonical) {
		return nil
	}

	current, exists := counts[canonical]
	if current == math.MaxUint64 {
		return fmt.Errorf("%w for %q", ErrCountOverflow, canonical)
	}
	if !exists && maxDistinct > 0 && len(counts) >= maxDistinct {
		return fmt.Errorf("%w: maximum %d", ErrDistinctWordLimit, maxDistinct)
	}

	counts[canonical] = current + 1
	stats.CountedTokens++
	return nil
}

func isASCII(text string) bool {
	for index := range len(text) {
		if text[index] >= 0x80 {
			return false
		}
	}
	return true
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isASCIIUpper(value byte) bool {
	return value >= 'A' && value <= 'Z'
}

func lowerASCII(destination []byte, source string) {
	for index := range len(source) {
		value := source[index]
		if isASCIIUpper(value) {
			value += 'a' - 'A'
		}
		destination[index] = value
	}
}
