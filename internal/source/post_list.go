package source

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
	defaultMaxPostListBytes   int64 = 4 << 20
	defaultMaxPostListLine          = 8 << 10
	defaultMaxPostListEntries       = 10_000
	initialPostCapacity             = 256

	absoluteMaxPostListBytes   int64 = 64 << 20
	absoluteMaxPostListLine          = 64 << 10
	absoluteMaxPostListEntries       = 100_000
)

var (
	// ErrInvalidPostListLimits indicates unsafe or nonsensical parser limits.
	ErrInvalidPostListLimits = errors.New("invalid post-list limits")
	// ErrNilPostListReader indicates that no post-list source was provided.
	ErrNilPostListReader = errors.New("nil post-list reader")
	// ErrPostListTooLarge indicates that the source exceeded its byte budget.
	ErrPostListTooLarge = errors.New("post list exceeds byte limit")
	// ErrPostListLineTooLong indicates that one source line exceeded its limit.
	ErrPostListLineTooLong = errors.New("post-list line exceeds limit")
	// ErrPostListRead indicates that the source could not be read completely.
	ErrPostListRead = errors.New("read post list")
	// ErrTooManyPostListEntries indicates that non-blank input exceeded its count budget.
	ErrTooManyPostListEntries = errors.New("post list exceeds entry limit")
	// ErrEmptyPostList indicates that no usable posts remained after parsing.
	ErrEmptyPostList = errors.New("post list contains no posts")
)

// PostListLimits bounds source consumption and the ordered deduplication set.
// MaxEntries applies to all non-blank URL lines, including duplicates, so repeated
// input cannot bypass the memory and processing budget.
type PostListLimits struct {
	MaxBytes     int64
	MaxLineBytes int
	MaxEntries   int
}

// DefaultPostListLimits returns limits far above the 200-entry assignment source
// while remaining small enough to reject an accidental bulk download.
func DefaultPostListLimits() PostListLimits {
	return PostListLimits{
		MaxBytes:     defaultMaxPostListBytes,
		MaxLineBytes: defaultMaxPostListLine,
		MaxEntries:   defaultMaxPostListEntries,
	}
}

// Post identifies one normalized Reddit post and the first source line on which it
// appeared. It contains no untrusted query, fragment, or source URL text.
type Post struct {
	ID         string
	SourceLine int
}

// Fullname returns Reddit's stable thing fullname for the post.
func (post Post) Fullname() string {
	return "t3_" + post.ID
}

// PostList is an immutable first-seen ordering of unique posts. Returned copies may
// be modified by callers without changing the list.
type PostList struct {
	posts []Post
}

// Len returns the number of unique posts.
func (list PostList) Len() int {
	return len(list.posts)
}

// At returns a copy of the post at index and reports whether index was in range.
func (list PostList) At(index int) (Post, bool) {
	if index < 0 || index >= len(list.posts) {
		return Post{}, false
	}
	return list.posts[index], true
}

// Posts returns an independent copy in first-seen source order.
func (list PostList) Posts() []Post {
	return append([]Post(nil), list.posts...)
}

// IDs returns normalized IDs in first-seen source order.
func (list PostList) IDs() []string {
	ids := make([]string, len(list.posts))
	for i, post := range list.posts {
		ids[i] = post.ID
	}
	return ids
}

// PostListStats describes the exact source and its successful normalization. On
// failure, the byte/hash fields describe bytes consumed before the failure was known.
type PostListStats struct {
	SourceBytes int64
	Lines       int
	BlankLines  int
	URLLines    int
	Duplicates  int
	Posts       int
	SHA256      string
	// PostsSHA256 hashes the canonical ordered sequence of unique normalized posts
	// as `source-line:post-id\n`. It binds later per-post evidence to this parse
	// without retaining or logging the original URLs.
	PostsSHA256 string
}

// LineError attaches a one-based source line to a sanitized parsing failure.
type LineError struct {
	Line int
	Err  error
}

// Error returns line context without including the untrusted source line.
func (e *LineError) Error() string {
	if e == nil {
		return "parse post list"
	}
	return fmt.Sprintf("parse post-list line %d: %v", e.Line, e.Err)
}

// Unwrap preserves the underlying error classification.
func (e *LineError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// LoadPostList reads, hashes, validates, and deduplicates a line-based Reddit post
// source. Any read, limit, or validation failure returns an unusable zero PostList.
func LoadPostList(r io.Reader, limits PostListLimits) (PostList, PostListStats, error) {
	if r == nil {
		return PostList{}, PostListStats{}, ErrNilPostListReader
	}
	if err := validatePostListLimits(limits); err != nil {
		return PostList{}, PostListStats{}, err
	}

	hasher := sha256.New()
	limited := &io.LimitedReader{R: r, N: limits.MaxBytes + 1}
	scanner := bufio.NewScanner(io.TeeReader(limited, hasher))
	initialBufferSize := min(limits.MaxLineBytes+2, 4<<10)
	scanner.Buffer(make([]byte, initialBufferSize), limits.MaxLineBytes+2)
	scanner.Split(splitPostListLines(limits.MaxLineBytes))

	posts := make([]Post, 0, min(limits.MaxEntries, initialPostCapacity))
	seen := make(map[string]struct{}, min(limits.MaxEntries, initialPostCapacity))
	var stats PostListStats

	for scanner.Scan() {
		stats.Lines++
		// Scanner may read ahead past the current token. Once the sentinel byte has
		// been consumed, source size is the primary failure regardless of whether
		// the truncated token would also fail URL validation.
		if limited.N == 0 {
			stats = finalizePostListStats(limited, limits, hasher, stats, len(posts))
			return PostList{}, stats, ErrPostListTooLarge
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			stats.BlankLines++
			continue
		}
		if stats.URLLines == limits.MaxEntries {
			stats = finalizePostListStats(limited, limits, hasher, stats, len(posts))
			return PostList{}, stats, &LineError{Line: stats.Lines, Err: ErrTooManyPostListEntries}
		}
		stats.URLLines++

		id, err := ParsePostURL(line)
		if err != nil {
			stats = finalizePostListStats(limited, limits, hasher, stats, len(posts))
			return PostList{}, stats, &LineError{Line: stats.Lines, Err: err}
		}
		if _, duplicate := seen[id]; duplicate {
			stats.Duplicates++
			continue
		}

		seen[id] = struct{}{}
		posts = append(posts, Post{ID: id, SourceLine: stats.Lines})
	}

	stats = finalizePostListStats(limited, limits, hasher, stats, len(posts))
	if stats.SourceBytes > limits.MaxBytes {
		return PostList{}, stats, ErrPostListTooLarge
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, ErrPostListLineTooLong) {
			return PostList{}, stats, &LineError{Line: stats.Lines + 1, Err: err}
		}
		return PostList{}, stats, &postListReadError{cause: err}
	}
	if len(posts) == 0 {
		return PostList{}, stats, ErrEmptyPostList
	}
	stats.PostsSHA256 = HashPosts(posts)

	return PostList{posts: posts}, stats, nil
}

// HashPosts returns the canonical SHA-256 identity of a source-ordered post slice.
// Decimal source lines, an explicit colon/newline framing, and the validated
// base-36 identifier grammar make the encoding unambiguous without exposing URLs.
func HashPosts(posts []Post) string {
	hasher := sha256.New()
	for _, post := range posts {
		_, _ = fmt.Fprintf(hasher, "%d:%s\n", post.SourceLine, post.ID)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// postListReadError preserves the reader cause for errors.Is/errors.As without
// formatting an arbitrary URL, file path, or transport detail into diagnostics.
type postListReadError struct {
	cause error
}

func (e *postListReadError) Error() string { return ErrPostListRead.Error() }
func (e *postListReadError) Unwrap() error { return e.cause }
func (e *postListReadError) Is(target error) bool {
	return target == ErrPostListRead
}

func validatePostListLimits(limits PostListLimits) error {
	if limits.MaxBytes <= 0 || limits.MaxBytes > absoluteMaxPostListBytes {
		return fmt.Errorf("%w: MaxBytes must be between 1 and %d", ErrInvalidPostListLimits, absoluteMaxPostListBytes)
	}
	if limits.MaxLineBytes <= 0 || limits.MaxLineBytes > absoluteMaxPostListLine {
		return fmt.Errorf("%w: MaxLineBytes must be between 1 and %d", ErrInvalidPostListLimits, absoluteMaxPostListLine)
	}
	if limits.MaxEntries <= 0 || limits.MaxEntries > absoluteMaxPostListEntries {
		return fmt.Errorf("%w: MaxEntries must be between 1 and %d", ErrInvalidPostListLimits, absoluteMaxPostListEntries)
	}
	return nil
}

func finalizePostListStats(limited *io.LimitedReader, limits PostListLimits, hasher hash.Hash, stats PostListStats, posts int) PostListStats {
	stats.SourceBytes = limits.MaxBytes + 1 - limited.N
	stats.Posts = posts
	stats.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return stats
}

func splitPostListLines(maxBytes int) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			line := data[:newline]
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(line) > maxBytes {
				return 0, nil, ErrPostListLineTooLong
			}
			return newline + 1, line, nil
		}
		if atEOF {
			line := bytes.TrimSuffix(data, []byte{'\r'})
			if len(line) > maxBytes {
				return 0, nil, ErrPostListLineTooLong
			}
			return len(data), line, nil
		}

		// A trailing CR may still become a legal CRLF delimiter. Any other byte past
		// the content limit is a terminal line-size failure.
		if len(data) > maxBytes && !(len(data) == maxBytes+1 && data[maxBytes] == '\r') {
			return 0, nil, ErrPostListLineTooLong
		}
		return 0, nil, nil
	}
}
