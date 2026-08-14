package words

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

const (
	defaultMaxDictionaryBytes   int64 = 16 << 20
	defaultMaxDictionaryLine          = 4 << 10
	defaultMaxDictionaryEntries       = 1_000_000
	initialDictionaryCapacity         = 4 << 10

	absoluteMaxDictionaryBytes   int64 = 1 << 30
	absoluteMaxDictionaryLine          = 1 << 20
	absoluteMaxDictionaryEntries       = 10_000_000
)

var (
	// ErrInvalidDictionaryLimits indicates unsafe or nonsensical loader limits.
	ErrInvalidDictionaryLimits = errors.New("invalid dictionary limits")
	// ErrNilDictionaryReader indicates that no dictionary source was provided.
	ErrNilDictionaryReader = errors.New("nil dictionary reader")
	// ErrDictionaryTooLarge indicates that the source exceeded its byte budget.
	ErrDictionaryTooLarge = errors.New("dictionary exceeds byte limit")
	// ErrDictionaryLineTooLong indicates that one source line exceeded its limit.
	ErrDictionaryLineTooLong = errors.New("dictionary line exceeds limit")
	// ErrDictionaryRead indicates that the source could not be read completely.
	ErrDictionaryRead = errors.New("read dictionary")
	// ErrTooManyDictionaryEntries indicates that the unique-entry budget was exceeded.
	ErrTooManyDictionaryEntries = errors.New("dictionary exceeds entry limit")
	// ErrEmptyDictionary indicates that no valid words remained after normalization.
	ErrEmptyDictionary = errors.New("dictionary contains no valid words")
)

// DictionaryLimits bounds memory and input consumed while loading a word bank.
type DictionaryLimits struct {
	MaxBytes     int64
	MaxLineBytes int
	MaxEntries   int
}

// DefaultDictionaryLimits returns conservative limits that comfortably contain the
// supplied word bank while rejecting accidental or adversarial unbounded input.
func DefaultDictionaryLimits() DictionaryLimits {
	return DictionaryLimits{
		MaxBytes:     defaultMaxDictionaryBytes,
		MaxLineBytes: defaultMaxDictionaryLine,
		MaxEntries:   defaultMaxDictionaryEntries,
	}
}

// DictionaryStats describes how an exact dictionary source was normalized.
type DictionaryStats struct {
	SourceBytes int64
	Lines       int
	Accepted    int
	Rejected    int
	Duplicates  int
	SHA256      string
}

// Dictionary is an immutable normalized word-membership set. Once loaded, it is safe
// for concurrent readers.
type Dictionary struct {
	entries      map[string]string
	maxWordRunes int
}

// Len returns the number of unique valid entries in the dictionary.
func (d Dictionary) Len() int {
	return len(d.entries)
}

// Contains reports whether value is a valid case-insensitive dictionary word.
func (d Dictionary) Contains(value string) bool {
	normalized, runeCount, valid := normalizeLetters(value)
	if !valid || runeCount < minimumWordRunes {
		return false
	}

	return d.containsNormalized(normalized)
}

func (d Dictionary) containsNormalized(word string) bool {
	_, found := d.entries[word]
	return found
}

// canonicalNormalized returns the dictionary-owned normalized key. Counting uses
// that stable string so it never retains a substring of an input comment and does
// not need to copy the same word on every occurrence.
func (d Dictionary) canonicalNormalized(word string) (string, bool) {
	canonical, found := d.entries[word]
	return canonical, found
}

// LoadDictionary reads, hashes, normalizes, validates, and deduplicates a line-based
// word bank without retaining the source bytes.
func LoadDictionary(r io.Reader, limits DictionaryLimits) (Dictionary, DictionaryStats, error) {
	if r == nil {
		return Dictionary{}, DictionaryStats{}, ErrNilDictionaryReader
	}
	if err := validateDictionaryLimits(limits); err != nil {
		return Dictionary{}, DictionaryStats{}, err
	}

	hasher := sha256.New()
	limited := &io.LimitedReader{R: r, N: limits.MaxBytes + 1}
	scanner := bufio.NewScanner(io.TeeReader(limited, hasher))
	initialBufferSize := min(limits.MaxLineBytes+2, 4<<10)
	scanner.Buffer(make([]byte, initialBufferSize), limits.MaxLineBytes+2)
	scanner.Split(splitDictionaryLines(limits.MaxLineBytes))

	// Start modestly so malformed or tiny sources do not trigger a supplied-bank-sized
	// allocation. The map grows naturally for the real dictionary.
	entries := make(map[string]string, min(limits.MaxEntries, initialDictionaryCapacity))
	var stats DictionaryStats
	maxWordRunes := 0

	for scanner.Scan() {
		stats.Lines++
		candidate := strings.TrimSpace(scanner.Text())
		normalized, runeCount, valid := normalizeLetters(candidate)
		if !valid || runeCount < minimumWordRunes {
			stats.Rejected++
			continue
		}
		if _, duplicate := entries[normalized]; duplicate {
			stats.Duplicates++
			continue
		}
		if len(entries) == limits.MaxEntries {
			return Dictionary{}, dictionaryStats(limited, limits, hasher, stats), ErrTooManyDictionaryEntries
		}

		entries[normalized] = normalized
		stats.Accepted++
		if runeCount > maxWordRunes {
			maxWordRunes = runeCount
		}
	}

	stats = dictionaryStats(limited, limits, hasher, stats)
	if stats.SourceBytes > limits.MaxBytes {
		return Dictionary{}, stats, ErrDictionaryTooLarge
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, ErrDictionaryLineTooLong) {
			return Dictionary{}, stats, err
		}
		return Dictionary{}, stats, fmt.Errorf("%w: %w", ErrDictionaryRead, err)
	}
	if len(entries) == 0 {
		return Dictionary{}, stats, ErrEmptyDictionary
	}

	return Dictionary{entries: entries, maxWordRunes: maxWordRunes}, stats, nil
}

func validateDictionaryLimits(limits DictionaryLimits) error {
	if limits.MaxBytes <= 0 || limits.MaxBytes > absoluteMaxDictionaryBytes {
		return fmt.Errorf("%w: MaxBytes must be between 1 and %d", ErrInvalidDictionaryLimits, absoluteMaxDictionaryBytes)
	}
	if limits.MaxLineBytes < minimumWordRunes || limits.MaxLineBytes > absoluteMaxDictionaryLine {
		return fmt.Errorf("%w: MaxLineBytes must be between %d and %d", ErrInvalidDictionaryLimits, minimumWordRunes, absoluteMaxDictionaryLine)
	}
	if limits.MaxEntries <= 0 || limits.MaxEntries > absoluteMaxDictionaryEntries {
		return fmt.Errorf("%w: MaxEntries must be between 1 and %d", ErrInvalidDictionaryLimits, absoluteMaxDictionaryEntries)
	}
	return nil
}

func dictionaryStats(limited *io.LimitedReader, limits DictionaryLimits, hasher hash.Hash, stats DictionaryStats) DictionaryStats {
	stats.SourceBytes = limits.MaxBytes + 1 - limited.N
	stats.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return stats
}

func splitDictionaryLines(maxBytes int) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			line := data[:newline]
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(line) > maxBytes {
				return 0, nil, ErrDictionaryLineTooLong
			}
			return newline + 1, line, nil
		}
		if atEOF {
			line := bytes.TrimSuffix(data, []byte{'\r'})
			if len(line) > maxBytes {
				return 0, nil, ErrDictionaryLineTooLong
			}
			return len(data), line, nil
		}

		// Permit one additional trailing CR while waiting for the LF in a CRLF
		// sequence; any other growth beyond the content limit is terminal.
		if len(data) > maxBytes && !(len(data) == maxBytes+1 && data[maxBytes] == '\r') {
			return 0, nil, ErrDictionaryLineTooLong
		}
		return 0, nil, nil
	}
}
