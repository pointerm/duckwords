package words

import (
	"strings"
	"unicode"
)

const minimumWordRunes = 3

// normalizeLetters lowercases value and reports its Unicode code-point length. It
// rejects non-letter runes so every component shares one validity contract.
func normalizeLetters(value string) (normalized string, runeCount int, valid bool) {
	if value == "" {
		return "", 0, false
	}

	for _, r := range value {
		if !unicode.IsLetter(r) {
			return "", 0, false
		}
		runeCount++
	}

	return strings.ToLower(value), runeCount, true
}
