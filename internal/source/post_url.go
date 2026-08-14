package source

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxPostIDLength = 16

var (
	// ErrInvalidPostURL classifies malformed, unsafe, or unsupported post URLs.
	ErrInvalidPostURL = errors.New("invalid Reddit post URL")
)

// PostURLReason identifies why a post URL was rejected without retaining the
// untrusted URL in an error or log message.
type PostURLReason uint8

const (
	// PostURLMalformed means the value is not a valid absolute URL.
	PostURLMalformed PostURLReason = iota + 1
	// PostURLUnsafe means the URL contains credentials, a port, controls, or an
	// ambiguously escaped path.
	PostURLUnsafe
	// PostURLUnsupportedScheme means the URL is not HTTPS.
	PostURLUnsupportedScheme
	// PostURLUnsupportedHost means the host is not an allowlisted Reddit host.
	PostURLUnsupportedHost
	// PostURLUnsupportedPath means the path is not a supported Reddit permalink.
	PostURLUnsupportedPath
	// PostURLInvalidID means the permalink does not contain a valid base-36 post ID.
	PostURLInvalidID
)

// PostURLError reports a sanitized URL validation category. It intentionally does
// not retain the input because URLs can contain tracking data or accidental secrets.
type PostURLError struct {
	Reason PostURLReason
}

// Error returns a stable, content-free validation message.
func (e *PostURLError) Error() string {
	if e == nil {
		return ErrInvalidPostURL.Error()
	}
	return fmt.Sprintf("%s: %s", ErrInvalidPostURL, e.Reason)
}

// Unwrap supports errors.Is(err, ErrInvalidPostURL).
func (e *PostURLError) Unwrap() error {
	return ErrInvalidPostURL
}

// String returns a stable name suitable for diagnostics and tests.
func (reason PostURLReason) String() string {
	switch reason {
	case PostURLMalformed:
		return "malformed URL"
	case PostURLUnsafe:
		return "unsafe URL components"
	case PostURLUnsupportedScheme:
		return "unsupported scheme"
	case PostURLUnsupportedHost:
		return "unsupported host"
	case PostURLUnsupportedPath:
		return "unsupported path"
	case PostURLInvalidID:
		return "invalid post ID"
	default:
		return "unknown validation failure"
	}
}

// ParsedPostURL is the normalized, non-secret identity and public JSON path of a
// supported Reddit post permalink. JSONPath is always a relative, ASCII-only path
// ending in "/.json" and is safe to resolve against the pinned old.reddit.com
// origin.
type ParsedPostURL struct {
	ID       string
	JSONPath string
}

// ParsePostURL validates a supported Reddit permalink and returns its normalized
// lowercase base-36 post ID and safe public JSON path. Query parameters and
// fragments are rejected rather than silently discarded, so an operator can prove
// that the requested resource is exactly the one present in the source file.
func ParsePostURL(raw string) (ParsedPostURL, error) {
	if raw == "" || !utf8.ValidString(raw) || containsUnsafeURLRune(raw) {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLMalformed}
	}

	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLMalformed}
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedScheme}
	}
	// Comparing Host with Hostname also rejects an explicitly empty port, which
	// URL.Port cannot distinguish from no port at all.
	if parsed.User != nil || parsed.Host != parsed.Hostname() || parsed.RawPath != "" ||
		strings.Contains(parsed.EscapedPath(), "%") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsafe}
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedHost}
	}

	segments, validPath := splitPath(parsed.Path)
	if !validPath {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedPath}
	}

	var candidate string
	var jsonSegments []string
	switch host {
	case "redd.it":
		if len(segments) != 1 {
			return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedPath}
		}
		candidate = segments[0]
		jsonSegments = []string{"comments", candidate}
	default:
		if !isLongRedditHost(host) {
			return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedHost}
		}

		switch {
		case len(segments) >= 4 && segments[0] == "r" && segments[2] == "comments":
			if !validSubreddit(segments[1]) {
				return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedPath}
			}
			candidate = segments[3]
			jsonSegments = append([]string(nil), segments...)
		case len(segments) >= 2 && segments[0] == "comments":
			candidate = segments[1]
			jsonSegments = append([]string(nil), segments...)
		default:
			return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedPath}
		}
	}

	if !validPostID(candidate) {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLInvalidID}
	}
	if !normalizeJSONSegments(&jsonSegments) {
		return ParsedPostURL{}, &PostURLError{Reason: PostURLUnsupportedPath}
	}
	id := strings.ToLower(candidate)
	// Normalize the identity-bearing segment as well as subreddit spelling. Slugs
	// are already restricted to Reddit's stable ASCII permalink alphabet.
	if len(jsonSegments) >= 4 && jsonSegments[0] == "r" {
		jsonSegments[1] = strings.ToLower(jsonSegments[1])
		jsonSegments[3] = id
	} else {
		jsonSegments[1] = id
	}
	return ParsedPostURL{ID: id, JSONPath: "/" + strings.Join(jsonSegments, "/") + "/.json"}, nil
}

// normalizeJSONSegments accepts only a post permalink, never a focal-comment URL.
// A trailing .json segment is normalized away so callers cannot produce .json.json.
func normalizeJSONSegments(segments *[]string) bool {
	values := *segments
	if len(values) > 0 && values[len(values)-1] == ".json" {
		values = values[:len(values)-1]
	}
	prefix := 2
	if len(values) >= 4 && values[0] == "r" {
		prefix = 4
	}
	// The slug is optional, but a second suffix segment would identify a comment,
	// not the post itself.
	if len(values) < prefix || len(values) > prefix+1 {
		return false
	}
	for index, segment := range values {
		if segment == "" || !validPathSegment(segment) {
			return false
		}
		if index == prefix && segment == ".json" {
			return false
		}
	}
	*segments = values
	return true
}

func validPathSegment(value string) bool {
	for _, c := range []byte(value) {
		if !isASCIIAlphaNumeric(c) && c != '_' && c != '-' {
			return false
		}
	}
	return value != ""
}

func isLongRedditHost(host string) bool {
	switch host {
	case "reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com", "np.reddit.com", "m.reddit.com":
		return true
	default:
		return false
	}
}

func containsUnsafeURLRune(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func splitPath(path string) ([]string, bool) {
	if path == "" || path[0] != '/' {
		return nil, false
	}
	trimmed := strings.TrimPrefix(path, "/")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return nil, false
	}
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, false
		}
	}
	return segments, true
}

func validSubreddit(value string) bool {
	if len(value) < 2 || len(value) > 21 {
		return false
	}
	for _, c := range []byte(value) {
		if !isASCIIAlphaNumeric(c) && c != '_' {
			return false
		}
	}
	return true
}

func validPostID(value string) bool {
	if len(value) == 0 || len(value) > maxPostIDLength {
		return false
	}
	for _, c := range []byte(value) {
		if !isASCIIAlphaNumeric(c) {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
