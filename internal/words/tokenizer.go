package words

import (
	"errors"
	"fmt"
	"unicode"
)

const defaultMaxTokenRunes = defaultMaxDictionaryLine

var (
	// ErrInvalidTokenLimit indicates a limit too small to emit a valid word.
	ErrInvalidTokenLimit = errors.New("invalid token length limit")
	// ErrNilTokenVisitor indicates that token output has no consumer.
	ErrNilTokenVisitor = errors.New("nil token visitor")
)

// TokenStats describes alphabetic sequences observed during one scan.
type TokenStats struct {
	Sequences int
	Emitted   int
	TooShort  int
	TooLong   int
}

// Tokenizer extracts bounded, normalized Unicode-letter sequences. It contains no
// mutable state and is safe for concurrent use.
type Tokenizer struct {
	maxTokenRunes int
}

// NewTokenizer returns a tokenizer with an explicit maximum candidate size.
func NewTokenizer(maxTokenRunes int) (Tokenizer, error) {
	if maxTokenRunes < minimumWordRunes || maxTokenRunes > absoluteMaxDictionaryLine {
		return Tokenizer{}, fmt.Errorf(
			"%w: maximum must be between %d and %d",
			ErrInvalidTokenLimit,
			minimumWordRunes,
			absoluteMaxDictionaryLine,
		)
	}
	return Tokenizer{maxTokenRunes: maxTokenRunes}, nil
}

// DefaultTokenizer returns a tokenizer aligned with the default dictionary line
// bound. A longer token cannot be present in a dictionary loaded under that bound.
func DefaultTokenizer() Tokenizer {
	return Tokenizer{maxTokenRunes: defaultMaxTokenRunes}
}

// Scan visits each valid-length maximal Unicode-letter sequence in text. The visitor
// receives lowercase tokens and may stop the scan by returning an error.
func (t Tokenizer) Scan(text string, visit func(string) error) (TokenStats, error) {
	if t.maxTokenRunes < minimumWordRunes || t.maxTokenRunes > absoluteMaxDictionaryLine {
		return TokenStats{}, ErrInvalidTokenLimit
	}
	if visit == nil {
		return TokenStats{}, ErrNilTokenVisitor
	}

	buffer := make([]rune, 0, min(t.maxTokenRunes, 64))
	runeCount := 0
	tooLong := false
	var stats TokenStats

	flush := func() error {
		if runeCount == 0 {
			return nil
		}

		stats.Sequences++
		switch {
		case tooLong:
			stats.TooLong++
		case runeCount < minimumWordRunes:
			stats.TooShort++
		default:
			stats.Emitted++
			if err := visit(string(buffer)); err != nil {
				return fmt.Errorf("visit token: %w", err)
			}
		}

		buffer = buffer[:0]
		runeCount = 0
		tooLong = false
		return nil
	}

	for _, r := range text {
		if unicode.IsLetter(r) {
			runeCount++
			if runeCount <= t.maxTokenRunes {
				buffer = append(buffer, unicode.ToLower(r))
			} else {
				tooLong = true
			}
			continue
		}

		if err := flush(); err != nil {
			return stats, err
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}

	return stats, nil
}
