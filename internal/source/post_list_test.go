package source

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestLoadPostListNormalizesDeduplicatesAndReportsProvenance(t *testing.T) {
	t.Parallel()

	const input = "  https://old.reddit.com/r/duck/comments/ABC123/first  \r\n" +
		"\r\n" +
		"https://redd.it/def456\n" +
		"https://www.reddit.com/comments/abc123/duplicate\n" +
		"https://reddit.com/r/duck/comments/789xyz/final"

	list, stats, err := LoadPostList(strings.NewReader(input), DefaultPostListLimits())
	if err != nil {
		t.Fatalf("LoadPostList() error = %v", err)
	}
	wantPosts := []Post{
		{ID: "abc123", JSONPath: "/r/duck/comments/abc123/first/.json", SourceLine: 1},
		{ID: "def456", JSONPath: "/comments/def456/.json", SourceLine: 3},
		{ID: "789xyz", JSONPath: "/r/duck/comments/789xyz/final/.json", SourceLine: 5},
	}
	if got := list.Posts(); !reflect.DeepEqual(got, wantPosts) {
		t.Fatalf("Posts() = %#v, want %#v", got, wantPosts)
	}
	hash := sha256.Sum256([]byte(input))
	wantStats := PostListStats{
		SourceBytes: int64(len(input)),
		Lines:       5,
		BlankLines:  1,
		URLLines:    4,
		Duplicates:  1,
		Posts:       3,
		SHA256:      hex.EncodeToString(hash[:]),
		PostsSHA256: HashPosts(wantPosts),
	}
	if !reflect.DeepEqual(stats, wantStats) {
		t.Fatalf("stats = %+v, want %+v", stats, wantStats)
	}
	if got, want := list.IDs(), []string{"abc123", "def456", "789xyz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	post, ok := list.At(1)
	if !ok || post != wantPosts[1] || post.Fullname() != "t3_def456" {
		t.Fatalf("At(1) = %+v, %v", post, ok)
	}
	if _, ok := list.At(-1); ok {
		t.Fatal("At(-1) unexpectedly succeeded")
	}
	if _, ok := list.At(list.Len()); ok {
		t.Fatal("At(Len()) unexpectedly succeeded")
	}
}

func TestPostListReturnedSlicesDoNotMutateList(t *testing.T) {
	t.Parallel()

	list, _, err := LoadPostList(strings.NewReader("https://redd.it/abc123\n"), DefaultPostListLimits())
	if err != nil {
		t.Fatalf("LoadPostList() error = %v", err)
	}
	posts := list.Posts()
	posts[0].ID = "changed"
	ids := list.IDs()
	ids[0] = "changed"
	post, _ := list.At(0)
	if post.ID != "abc123" {
		t.Fatalf("caller mutated immutable list: %+v", post)
	}
}

func TestLoadPostListRejectsInvalidLimitsAndNilReader(t *testing.T) {
	t.Parallel()

	_, _, err := LoadPostList(nil, DefaultPostListLimits())
	if !errors.Is(err, ErrNilPostListReader) {
		t.Fatalf("nil reader error = %v, want ErrNilPostListReader", err)
	}

	tests := map[string]PostListLimits{
		"zero bytes":   {MaxBytes: 0, MaxLineBytes: 10, MaxEntries: 10},
		"huge bytes":   {MaxBytes: absoluteMaxPostListBytes + 1, MaxLineBytes: 10, MaxEntries: 10},
		"zero line":    {MaxBytes: 100, MaxLineBytes: 0, MaxEntries: 10},
		"huge line":    {MaxBytes: 100, MaxLineBytes: absoluteMaxPostListLine + 1, MaxEntries: 10},
		"zero entries": {MaxBytes: 100, MaxLineBytes: 10, MaxEntries: 0},
		"huge entries": {MaxBytes: 100, MaxLineBytes: 10, MaxEntries: absoluteMaxPostListEntries + 1},
	}
	for name, limits := range tests {
		limits := limits
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := LoadPostList(strings.NewReader("https://redd.it/abc123\n"), limits)
			if !errors.Is(err, ErrInvalidPostListLimits) {
				t.Fatalf("LoadPostList() error = %v, want ErrInvalidPostListLimits", err)
			}
		})
	}
}

func TestLoadPostListEnforcesByteBoundary(t *testing.T) {
	t.Parallel()

	const input = "https://redd.it/abc123\n"
	limits := DefaultPostListLimits()
	limits.MaxBytes = int64(len(input))
	list, stats, err := LoadPostList(strings.NewReader(input), limits)
	if err != nil {
		t.Fatalf("exact boundary error = %v", err)
	}
	if list.Len() != 1 || stats.SourceBytes != limits.MaxBytes {
		t.Fatalf("exact boundary result = len %d, stats %+v", list.Len(), stats)
	}

	list, stats, err = LoadPostList(strings.NewReader(input+"\n"), limits)
	if !errors.Is(err, ErrPostListTooLarge) {
		t.Fatalf("over boundary error = %v, want ErrPostListTooLarge", err)
	}
	if list.Len() != 0 || stats.SourceBytes != limits.MaxBytes+1 {
		t.Fatalf("failed result = len %d, stats %+v", list.Len(), stats)
	}
}

func TestLoadPostListReportsByteLimitBeforeTruncatedURL(t *testing.T) {
	t.Parallel()

	const input = "https://redd.it/abc123"
	limits := DefaultPostListLimits()
	limits.MaxBytes = int64(len(input) - 1)
	limits.MaxLineBytes = len(input) + 10

	list, stats, err := LoadPostList(strings.NewReader(input), limits)
	if !errors.Is(err, ErrPostListTooLarge) {
		t.Fatalf("LoadPostList() error = %v, want ErrPostListTooLarge", err)
	}
	if errors.Is(err, ErrInvalidPostURL) {
		t.Fatalf("LoadPostList() misclassified truncated source as an invalid URL: %v", err)
	}
	if list.Len() != 0 || stats.SourceBytes != limits.MaxBytes+1 {
		t.Fatalf("failed result = len %d, stats %+v", list.Len(), stats)
	}
}

func TestLoadPostListEnforcesLineBoundaryWithCRLF(t *testing.T) {
	t.Parallel()

	const line = "https://redd.it/abc123"
	limits := DefaultPostListLimits()
	limits.MaxLineBytes = len(line)
	list, _, err := LoadPostList(strings.NewReader(line+"\r\n"), limits)
	if err != nil || list.Len() != 1 {
		t.Fatalf("exact CRLF boundary = len %d, error %v", list.Len(), err)
	}

	list, _, err = LoadPostList(strings.NewReader(line+"x\n"), limits)
	if !errors.Is(err, ErrPostListLineTooLong) || list.Len() != 0 {
		t.Fatalf("over line boundary = len %d, error %v", list.Len(), err)
	}
	var lineErr *LineError
	if !errors.As(err, &lineErr) || lineErr.Line != 1 {
		t.Fatalf("line error = %#v, want line 1", lineErr)
	}
}

func TestLoadPostListEntryLimitCountsDuplicates(t *testing.T) {
	t.Parallel()

	const line = "https://redd.it/abc123\n"
	limits := DefaultPostListLimits()
	limits.MaxEntries = 1
	list, stats, err := LoadPostList(strings.NewReader(line+line), limits)
	if !errors.Is(err, ErrTooManyPostListEntries) {
		t.Fatalf("LoadPostList() error = %v, want ErrTooManyPostListEntries", err)
	}
	if list.Len() != 0 || stats.URLLines != 1 || stats.Posts != 1 {
		t.Fatalf("failed result = len %d, stats %+v", list.Len(), stats)
	}
	var lineErr *LineError
	if !errors.As(err, &lineErr) || lineErr.Line != 2 {
		t.Fatalf("line error = %#v, want line 2", lineErr)
	}
}

func TestLoadPostListRejectsInvalidLineAtomicallyAndSanitizesError(t *testing.T) {
	t.Parallel()

	const secret = "planted-secret"
	input := "https://redd.it/abc123\nhttps://user:" + secret + "@evil.test/comments/def456\n"
	list, stats, err := LoadPostList(strings.NewReader(input), DefaultPostListLimits())
	if !errors.Is(err, ErrInvalidPostURL) {
		t.Fatalf("LoadPostList() error = %v, want ErrInvalidPostURL", err)
	}
	if list.Len() != 0 || stats.Posts != 1 {
		t.Fatalf("failed result = len %d, stats %+v", list.Len(), stats)
	}
	var lineErr *LineError
	if !errors.As(err, &lineErr) || lineErr.Line != 2 {
		t.Fatalf("line error = %#v, want line 2", lineErr)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "evil.test") {
		t.Fatalf("error leaked source content: %q", err)
	}
}

func TestLoadPostListDoesNotTreatCommentsAsInputSyntax(t *testing.T) {
	t.Parallel()

	_, _, err := LoadPostList(strings.NewReader("# https://redd.it/abc123\n"), DefaultPostListLimits())
	if !errors.Is(err, ErrInvalidPostURL) {
		t.Fatalf("comment line error = %v, want ErrInvalidPostURL", err)
	}
}

func TestLoadPostListRejectsEmptySource(t *testing.T) {
	t.Parallel()

	list, stats, err := LoadPostList(strings.NewReader(" \r\n\t\n"), DefaultPostListLimits())
	if !errors.Is(err, ErrEmptyPostList) || list.Len() != 0 {
		t.Fatalf("LoadPostList() = len %d, error %v", list.Len(), err)
	}
	if stats.Lines != 2 || stats.BlankLines != 2 || stats.URLLines != 0 || stats.Posts != 0 {
		t.Fatalf("empty stats = %+v", stats)
	}
}

func TestLoadPostListPreservesReaderFailureAndReturnsNoPosts(t *testing.T) {
	t.Parallel()

	const planted = "injected-secret-bearing-read-failure"
	wantErr := errors.New(planted)
	reader := io.MultiReader(strings.NewReader("https://redd.it/abc123\n"), postListFailingReader{err: wantErr})
	list, stats, err := LoadPostList(reader, DefaultPostListLimits())
	if !errors.Is(err, ErrPostListRead) || !errors.Is(err, wantErr) {
		t.Fatalf("LoadPostList() error = %v, want wrapped read errors", err)
	}
	if list.Len() != 0 || stats.Posts != 1 {
		t.Fatalf("failed result = len %d, stats %+v", list.Len(), stats)
	}
	if strings.Contains(err.Error(), planted) {
		t.Fatalf("LoadPostList() leaked reader detail: %q", err)
	}
}

func TestLineErrorNilSafety(t *testing.T) {
	t.Parallel()

	var lineErr *LineError
	if got := lineErr.Error(); got != "parse post list" {
		t.Fatalf("nil LineError.Error() = %q", got)
	}
	if lineErr.Unwrap() != nil {
		t.Fatal("nil LineError.Unwrap() returned non-nil")
	}
}

func FuzzLoadPostList(f *testing.F) {
	f.Add([]byte("https://redd.it/abc123\n"))
	f.Add([]byte("\r\nhttps://old.reddit.com/r/duck/comments/ABC123/title\r\n"))
	f.Add([]byte("https://evil.test/\n"))
	f.Add([]byte{0xff, '\n'})

	limits := PostListLimits{MaxBytes: 4 << 10, MaxLineBytes: 256, MaxEntries: 32}
	f.Fuzz(func(t *testing.T, input []byte) {
		list, stats, err := LoadPostList(strings.NewReader(string(input)), limits)
		if stats.SourceBytes < 0 || stats.SourceBytes > limits.MaxBytes+1 {
			t.Fatalf("SourceBytes = %d outside loader bound", stats.SourceBytes)
		}
		if err != nil {
			if list.Len() != 0 {
				t.Fatalf("failed load returned %d posts", list.Len())
			}
			return
		}
		if list.Len() == 0 || list.Len() > limits.MaxEntries || list.Len() != stats.Posts {
			t.Fatalf("successful result mismatch: len %d, stats %+v", list.Len(), stats)
		}
		seen := make(map[string]struct{}, list.Len())
		for _, post := range list.Posts() {
			if !validPostID(post.ID) || post.ID != strings.ToLower(post.ID) || post.SourceLine <= 0 {
				t.Fatalf("invalid accepted post: %+v", post)
			}
			if _, duplicate := seen[post.ID]; duplicate {
				t.Fatalf("duplicate accepted post ID %q", post.ID)
			}
			seen[post.ID] = struct{}{}
		}
	})
}

type postListFailingReader struct {
	err error
}

func (reader postListFailingReader) Read([]byte) (int, error) {
	return 0, reader.err
}
