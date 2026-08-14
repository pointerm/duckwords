package words

import (
	"errors"
	"reflect"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestTokenizerScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		want      []string
		sequences int
		tooShort  int
	}{
		{name: "empty", text: "", want: nil},
		{name: "case punctuation and digits", text: "Duck duck's duck-like duck42", want: []string{"duck", "duck", "duck", "like", "duck"}, sequences: 6, tooShort: 1},
		{name: "unicode and combining mark", text: "Café CAFÉ cafe\u0301", want: []string{"café", "café", "cafe"}, sequences: 3},
		{name: "non-ASCII adjacency remains one token", text: "éduck", want: []string{"éduck"}, sequences: 1},
		{name: "short words", text: "a an the", want: []string{"the"}, sequences: 3, tooShort: 2},
		{name: "underscore emoji and NUL separators", text: "d_uck duck👩‍💻water\x00bird", want: []string{"uck", "duck", "water", "bird"}, sequences: 5, tooShort: 1},
		{name: "invalid utf8 delimiter", text: string([]byte{'d', 'u', 'c', 'k', 0xff, 'g', 'o', 'o', 's', 'e'}), want: []string{"duck", "goose"}, sequences: 2},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			stats, err := DefaultTokenizer().Scan(test.text, func(token string) error {
				got = append(got, token)
				return nil
			})
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("tokens = %#v, want %#v", got, test.want)
			}
			if stats.Sequences != test.sequences || stats.Emitted != len(test.want) || stats.TooShort != test.tooShort {
				t.Fatalf("unexpected stats: %+v", stats)
			}
		})
	}
}

func TestTokenizerSkipsOverlongSequence(t *testing.T) {
	t.Parallel()

	tokenizer, err := NewTokenizer(3)
	if err != nil {
		t.Fatalf("NewTokenizer() error = %v", err)
	}

	var got []string
	stats, err := tokenizer.Scan("duck bird cat", func(token string) error {
		got = append(got, token)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cat"}) {
		t.Fatalf("tokens = %#v, want [cat]", got)
	}
	if stats.TooLong != 2 || stats.Emitted != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestTokenizerValidationAndVisitorError(t *testing.T) {
	t.Parallel()

	if _, err := NewTokenizer(2); !errors.Is(err, ErrInvalidTokenLimit) {
		t.Fatalf("NewTokenizer() error = %v, want ErrInvalidTokenLimit", err)
	}
	if _, err := (Tokenizer{}).Scan("duck", func(string) error { return nil }); !errors.Is(err, ErrInvalidTokenLimit) {
		t.Fatalf("Scan() error = %v, want ErrInvalidTokenLimit", err)
	}
	if _, err := DefaultTokenizer().Scan("duck", nil); !errors.Is(err, ErrNilTokenVisitor) {
		t.Fatalf("Scan() error = %v, want ErrNilTokenVisitor", err)
	}

	wantErr := errors.New("stop")
	stats, err := DefaultTokenizer().Scan("duck goose", func(string) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("Scan() error = %v, want wrapped visitor error", err)
	}
	if stats.Sequences != 1 || stats.Emitted != 1 {
		t.Fatalf("visitor-error stats = %+v, want one delivered sequence", stats)
	}
}

func FuzzTokenizer(f *testing.F) {
	for _, seed := range []string{"", "Duck duck!", "Café", "cafe\u0301", string([]byte{0xff, 'a', 'b', 'c'})} {
		f.Add(seed)
	}

	tokenizer := DefaultTokenizer()
	f.Fuzz(func(t *testing.T, text string) {
		stats, err := tokenizer.Scan(text, func(token string) error {
			count := 0
			for _, r := range token {
				if !unicode.IsLetter(r) {
					t.Fatalf("token %q contains non-letter %q", token, r)
				}
				if unicode.ToLower(r) != r {
					t.Fatalf("token %q is not lowercase", token)
				}
				count++
			}
			if count < minimumWordRunes || count > defaultMaxTokenRunes {
				t.Fatalf("token %q has invalid rune count %d", token, count)
			}
			if !utf8.ValidString(token) {
				t.Fatalf("token %q is not valid UTF-8", token)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Scan() error = %v", err)
		}
		if stats.Emitted+stats.TooShort+stats.TooLong != stats.Sequences {
			t.Fatalf("stats do not reconcile: %+v", stats)
		}
	})
}
