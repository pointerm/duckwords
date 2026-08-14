package words

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestLoadDictionaryNormalizesAndReportsStats(t *testing.T) {
	t.Parallel()

	const source = "Duck\r\nduck\nan\nbird!\nCafé\n\nwater\n"
	dictionary, stats, err := LoadDictionary(strings.NewReader(source), DefaultDictionaryLimits())
	if err != nil {
		t.Fatalf("LoadDictionary() error = %v", err)
	}

	if got, want := dictionary.Len(), 3; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	for _, word := range []string{"duck", "DUCK", "café", "CAFÉ", "water"} {
		if !dictionary.Contains(word) {
			t.Errorf("Contains(%q) = false, want true", word)
		}
	}
	for _, word := range []string{"an", "bird!", "unknown", ""} {
		if dictionary.Contains(word) {
			t.Errorf("Contains(%q) = true, want false", word)
		}
	}

	hash := sha256.Sum256([]byte(source))
	wantHash := hex.EncodeToString(hash[:])
	if stats.SourceBytes != int64(len(source)) || stats.Lines != 7 || stats.Accepted != 3 || stats.Rejected != 3 || stats.Duplicates != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.SHA256 != wantHash {
		t.Fatalf("SHA256 = %q, want %q", stats.SHA256, wantHash)
	}
}

func TestLoadDictionaryRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	tests := map[string]DictionaryLimits{
		"zero bytes":   {MaxBytes: 0, MaxLineBytes: 10, MaxEntries: 10},
		"huge bytes":   {MaxBytes: absoluteMaxDictionaryBytes + 1, MaxLineBytes: 10, MaxEntries: 10},
		"short line":   {MaxBytes: 100, MaxLineBytes: 2, MaxEntries: 10},
		"huge line":    {MaxBytes: 100, MaxLineBytes: absoluteMaxDictionaryLine + 1, MaxEntries: 10},
		"zero entries": {MaxBytes: 100, MaxLineBytes: 10, MaxEntries: 0},
	}

	for name, limits := range tests {
		limits := limits
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, _, err := LoadDictionary(strings.NewReader("duck\n"), limits)
			if !errors.Is(err, ErrInvalidDictionaryLimits) {
				t.Fatalf("LoadDictionary() error = %v, want ErrInvalidDictionaryLimits", err)
			}
		})
	}
}

func TestLoadDictionaryRejectsNilReader(t *testing.T) {
	t.Parallel()

	_, _, err := LoadDictionary(nil, DefaultDictionaryLimits())
	if !errors.Is(err, ErrNilDictionaryReader) {
		t.Fatalf("LoadDictionary() error = %v, want ErrNilDictionaryReader", err)
	}
}

func TestLoadDictionaryEnforcesByteLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultDictionaryLimits()
	limits.MaxBytes = 4

	_, stats, err := LoadDictionary(strings.NewReader("duck\n"), limits)
	if !errors.Is(err, ErrDictionaryTooLarge) {
		t.Fatalf("LoadDictionary() error = %v, want ErrDictionaryTooLarge", err)
	}
	if stats.SourceBytes != 5 {
		t.Fatalf("SourceBytes = %d, want 5", stats.SourceBytes)
	}
}

func TestLoadDictionaryEnforcesLineLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultDictionaryLimits()
	limits.MaxLineBytes = 3

	_, _, err := LoadDictionary(strings.NewReader("duck\n"), limits)
	if !errors.Is(err, ErrDictionaryLineTooLong) {
		t.Fatalf("LoadDictionary() error = %v, want ErrDictionaryLineTooLong", err)
	}
}

func TestLoadDictionaryAcceptsExactLineLimitWithCRLF(t *testing.T) {
	t.Parallel()

	limits := DefaultDictionaryLimits()
	limits.MaxLineBytes = 4

	dictionary, _, err := LoadDictionary(strings.NewReader("duck\r\n"), limits)
	if err != nil {
		t.Fatalf("LoadDictionary() error = %v", err)
	}
	if !dictionary.Contains("duck") {
		t.Fatal("dictionary does not contain exact-limit CRLF entry")
	}
}

func TestLoadDictionaryEnforcesEntryLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultDictionaryLimits()
	limits.MaxEntries = 1

	_, _, err := LoadDictionary(strings.NewReader("duck\nbird\n"), limits)
	if !errors.Is(err, ErrTooManyDictionaryEntries) {
		t.Fatalf("LoadDictionary() error = %v, want ErrTooManyDictionaryEntries", err)
	}
}

func TestLoadDictionaryRejectsEmptyNormalizedBank(t *testing.T) {
	t.Parallel()

	_, stats, err := LoadDictionary(strings.NewReader("a\n42\nbird!\n"), DefaultDictionaryLimits())
	if !errors.Is(err, ErrEmptyDictionary) {
		t.Fatalf("LoadDictionary() error = %v, want ErrEmptyDictionary", err)
	}
	if stats.Rejected != 3 {
		t.Fatalf("Rejected = %d, want 3", stats.Rejected)
	}
}

func TestLoadDictionaryPreservesReaderError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("injected read failure")
	reader := io.MultiReader(strings.NewReader("duck\n"), failingReader{err: wantErr})

	_, _, err := LoadDictionary(reader, DefaultDictionaryLimits())
	if !errors.Is(err, ErrDictionaryRead) || !errors.Is(err, wantErr) {
		t.Fatalf("LoadDictionary() error = %v, want wrapped read errors", err)
	}
}

func TestLoadDictionaryBoundaryAndMalformedEntries(t *testing.T) {
	t.Parallel()

	const source = "  Duck  \n\ufeffgoose\n" +
		"café\n" +
		"cafe\u0301\n" +
		"éé\n" +
		"ééé\n" +
		"duck-water\n" +
		"duck42\n" +
		"bird!\n" +
		"\xffbad\n" +
		"\t \n" +
		"water"

	dictionary, stats, err := LoadDictionary(strings.NewReader(source), DefaultDictionaryLimits())
	if err != nil {
		t.Fatalf("LoadDictionary() error = %v", err)
	}
	for _, word := range []string{"duck", "café", "ééé", "water"} {
		if !dictionary.Contains(word) {
			t.Errorf("dictionary does not contain %q", word)
		}
	}
	for _, word := range []string{"goose", "cafe\u0301", "éé", "duck-water", "duck42", "bird!", "bad"} {
		if dictionary.Contains(word) {
			t.Errorf("dictionary unexpectedly contains %q", word)
		}
	}
	if got, want := stats, (DictionaryStats{
		SourceBytes: int64(len(source)),
		Lines:       12,
		Accepted:    4,
		Rejected:    8,
		SHA256:      hashString(source),
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func TestLoadDictionaryByteLimitBoundary(t *testing.T) {
	t.Parallel()

	limits := DefaultDictionaryLimits()
	limits.MaxBytes = int64(len("duck\n"))
	dictionary, stats, err := LoadDictionary(strings.NewReader("duck\n"), limits)
	if err != nil {
		t.Fatalf("LoadDictionary(exact limit) error = %v", err)
	}
	if !dictionary.Contains("duck") || stats.SourceBytes != limits.MaxBytes {
		t.Fatalf("exact-limit result = dictionary length %d, stats %+v", dictionary.Len(), stats)
	}

	dictionary, _, err = LoadDictionary(strings.NewReader("duck\n\n"), limits)
	if !errors.Is(err, ErrDictionaryTooLarge) {
		t.Fatalf("LoadDictionary(over limit) error = %v, want ErrDictionaryTooLarge", err)
	}
	if dictionary.Len() != 0 {
		t.Fatalf("failed load returned usable dictionary of length %d", dictionary.Len())
	}
}

func TestLoadDictionaryLineLimitBoundaryWithoutNewline(t *testing.T) {
	t.Parallel()

	limits := DefaultDictionaryLimits()
	limits.MaxLineBytes = len("café")
	dictionary, _, err := LoadDictionary(strings.NewReader("café"), limits)
	if err != nil {
		t.Fatalf("LoadDictionary(exact line limit) error = %v", err)
	}
	if !dictionary.Contains("café") {
		t.Fatal("exact-limit final line was not accepted")
	}

	_, _, err = LoadDictionary(strings.NewReader("cafés"), limits)
	if !errors.Is(err, ErrDictionaryLineTooLong) {
		t.Fatalf("LoadDictionary(over line limit) error = %v, want ErrDictionaryLineTooLong", err)
	}
}

func TestLoadDictionaryHashUsesExactSourceBytes(t *testing.T) {
	t.Parallel()

	sources := []string{"duck\nwater\n", " DUCK \r\nwater"}
	var hashes []string
	for _, source := range sources {
		dictionary, stats, err := LoadDictionary(strings.NewReader(source), DefaultDictionaryLimits())
		if err != nil {
			t.Fatalf("LoadDictionary() error = %v", err)
		}
		if dictionary.Len() != 2 {
			t.Fatalf("dictionary length = %d, want 2", dictionary.Len())
		}
		hashes = append(hashes, stats.SHA256)
	}
	if hashes[0] == hashes[1] {
		t.Fatalf("semantically equivalent distinct sources have identical hash %q", hashes[0])
	}
}

func FuzzDictionary(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("duck\nwater\n"),
		[]byte(" Duck\r\nduck\nan\n"),
		[]byte("café\ncafe\u0301\n"),
		{0xff, 'd', 'u', 'c', 'k', '\n'},
		bytes.Repeat([]byte{'a'}, 129),
	} {
		f.Add(seed)
	}

	limits := DictionaryLimits{MaxBytes: 4 << 10, MaxLineBytes: 128, MaxEntries: 64}
	f.Fuzz(func(t *testing.T, source []byte) {
		dictionary, stats, err := LoadDictionary(bytes.NewReader(source), limits)
		if err != nil {
			for _, expected := range []error{
				ErrDictionaryTooLarge,
				ErrDictionaryLineTooLong,
				ErrTooManyDictionaryEntries,
				ErrEmptyDictionary,
			} {
				if errors.Is(err, expected) {
					return
				}
			}
			t.Fatalf("LoadDictionary() returned uncontrolled error: %v", err)
		}

		if dictionary.Len() == 0 || dictionary.Len() != stats.Accepted {
			t.Fatalf("successful load has inconsistent dictionary/stats: len=%d stats=%+v", dictionary.Len(), stats)
		}
		if stats.Lines != stats.Accepted+stats.Rejected+stats.Duplicates {
			t.Fatalf("successful stats do not reconcile: %+v", stats)
		}
		if stats.SourceBytes != int64(len(source)) || stats.SHA256 != hashBytes(source) {
			t.Fatalf("successful exact-source metadata is inconsistent: %+v", stats)
		}

		maximumRunes := 0
		for word := range dictionary.entries {
			count := 0
			if !utf8.ValidString(word) || strings.ToLower(word) != word {
				t.Fatalf("dictionary contains invalid normalized word %q", word)
			}
			for _, r := range word {
				if !unicode.IsLetter(r) {
					t.Fatalf("dictionary word %q contains non-letter %q", word, r)
				}
				count++
			}
			if count < minimumWordRunes {
				t.Fatalf("dictionary contains short word %q", word)
			}
			maximumRunes = max(maximumRunes, count)
		}
		if dictionary.maxWordRunes != maximumRunes {
			t.Fatalf("maxWordRunes = %d, want %d", dictionary.maxWordRunes, maximumRunes)
		}
	})
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}
