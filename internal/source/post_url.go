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

// ParsePostURL validates a supported Reddit permalink and returns its normalized
// lowercase base-36 post ID. Query parameters and fragments are deliberately ignored:
// they cannot change the stable resource identity and are never forwarded by this
// package.
func ParsePostURL(raw string) (string, error) {
	if raw == "" || !utf8.ValidString(raw) || containsUnsafeURLRune(raw) {
		return "", &PostURLError{Reason: PostURLMalformed}
	}

	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return "", &PostURLError{Reason: PostURLMalformed}
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", &PostURLError{Reason: PostURLUnsupportedScheme}
	}
	// Comparing Host with Hostname also rejects an explicitly empty port, which
	// URL.Port cannot distinguish from no port at all.
	if parsed.User != nil || parsed.Host != parsed.Hostname() || parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
		return "", &PostURLError{Reason: PostURLUnsafe}
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return "", &PostURLError{Reason: PostURLUnsupportedHost}
	}

	segments, validPath := splitPath(parsed.Path)
	if !validPath {
		return "", &PostURLError{Reason: PostURLUnsupportedPath}
	}

	var candidate string
	switch host {
	case "redd.it":
		if len(segments) != 1 {
			return "", &PostURLError{Reason: PostURLUnsupportedPath}
		}
		candidate = segments[0]
	default:
		if !isLongRedditHost(host) {
			return "", &PostURLError{Reason: PostURLUnsupportedHost}
		}

		switch {
		case len(segments) >= 4 && segments[0] == "r" && segments[2] == "comments":
			if !validSubreddit(segments[1]) {
				return "", &PostURLError{Reason: PostURLUnsupportedPath}
			}
			candidate = segments[3]
		case len(segments) >= 2 && segments[0] == "comments":
			candidate = segments[1]
		default:
			return "", &PostURLError{Reason: PostURLUnsupportedPath}
		}
	}

	// Reddit's JSON listing form may suffix the post-ID segment with .json. The
	// suffix is syntax, not part of the stable base-36 identity.
	candidate = strings.TrimSuffix(candidate, ".json")
	if !validPostID(candidate) {
		return "", &PostURLError{Reason: PostURLInvalidID}
	}
	return strings.ToLower(candidate), nil
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
