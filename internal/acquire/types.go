package acquire

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	// MaximumDocumentBytes is the hard allocation ceiling enforced by acquisition.
	// Parser-specific limits should normally be smaller and are passed as MaxBytes.
	MaximumDocumentBytes int64 = 64 << 20

	maxSourceLocatorBytes = 4 << 10
	maxSourceQueryBytes   = 2 << 10
	maxUserAgentBytes     = 256
	maxRedirects          = 3
)

var (
	// ErrInvalidSpec identifies a missing, contradictory, or unsupported source.
	ErrInvalidSpec = errors.New("invalid input source")
	// ErrInvalidConfig identifies an unsafe acquisition limit or HTTP configuration.
	ErrInvalidConfig = errors.New("invalid input acquisition configuration")
	// ErrURLPolicy identifies a remote URL outside the fixed assignment-input policy.
	ErrURLPolicy = errors.New("remote input URL rejected")
	// ErrRedirect identifies a redirect that exceeded the hop limit or left policy.
	ErrRedirect = errors.New("remote input redirect rejected")
	// ErrOpen identifies a local file that could not be inspected or opened.
	ErrOpen = errors.New("open local input")
	// ErrNotRegular identifies a local source that is not one stable regular file.
	ErrNotRegular = errors.New("local input is not a regular file")
	// ErrSourceChanged identifies a local file that changed while it was being loaded.
	ErrSourceChanged = errors.New("local input changed while loading")
	// ErrHTTPStatus identifies a remote response other than exact 200 OK.
	ErrHTTPStatus = errors.New("unexpected remote input status")
	// ErrContentType identifies a response that is not a supported plain-text document.
	ErrContentType = errors.New("unsupported remote input content type")
	// ErrContentEncoding identifies an encoded response that cannot be hashed as exact source bytes.
	ErrContentEncoding = errors.New("unsupported remote input content encoding")
	// ErrTooLarge identifies a source that exceeded its explicit byte budget.
	ErrTooLarge = errors.New("input exceeds byte limit")
	// ErrRead identifies a source that could not be read completely.
	ErrRead = errors.New("read input")
	// ErrClose identifies a successfully read source whose handle could not be closed.
	ErrClose = errors.New("close input")
	// ErrCanceled identifies acquisition stopped by its context.
	ErrCanceled = errors.New("input acquisition canceled")
	// ErrTransport identifies a remote request that failed before a valid response.
	ErrTransport = errors.New("remote input transport failure")
)

// Kind identifies the format that will consume an acquired document. It selects a
// fixed remote-host allowlist but does not parse the document itself.
type Kind uint8

const (
	// KindUnknown is the zero value and is rejected by Validate.
	KindUnknown Kind = iota
	// KindPosts identifies the supplied Reddit post-list document.
	KindPosts
	// KindDictionary identifies the supplied word-bank document.
	KindDictionary
)

// String returns a stable, non-sensitive name suitable for logs.
func (kind Kind) String() string {
	switch kind {
	case KindPosts:
		return "posts"
	case KindDictionary:
		return "dictionary"
	default:
		return "unknown"
	}
}

// Mode identifies whether bytes came from an allowlisted HTTPS origin or a local
// regular file.
type Mode uint8

const (
	// ModeUnknown is the zero value used before one valid source is selected.
	ModeUnknown Mode = iota
	// ModeHTTPS identifies an allowlisted HTTPS source.
	ModeHTTPS
	// ModeFile identifies a local regular-file source.
	ModeFile
)

// String returns a stable, non-sensitive name suitable for logs.
func (mode Mode) String() string {
	switch mode {
	case ModeHTTPS:
		return "https"
	case ModeFile:
		return "file"
	default:
		return "unknown"
	}
}

// Spec selects exactly one input source. URL and File are mutually exclusive. File
// paths are never retained in returned provenance or formatted errors.
type Spec struct {
	Kind Kind
	URL  string
	File string
}

// Config contains dependencies and the allocation limit for one acquisition. The
// HTTP fields are required only for an HTTPS source; local files never instantiate
// or consult a network client.
type Config struct {
	HTTPClient *http.Client
	UserAgent  string
	MaxBytes   int64
	// Retry optionally overrides the bounded HTTPS retry policy. Nil selects
	// DefaultRetryConfig. Local-file acquisition ignores this field entirely.
	// Callers must not mutate a non-nil value while Load or Validate is using it.
	Retry *RetryConfig
}

// RetryReason is a sanitized classification for one source replay. It never
// contains a URL, filesystem path, response body, or underlying error text.
type RetryReason string

const (
	// RetryReasonTransport identifies a failure before a usable HTTP response.
	RetryReasonTransport RetryReason = "transport"
	// RetryReasonHTTPStatus identifies one of the explicitly retryable statuses.
	RetryReasonHTTPStatus RetryReason = "http_status"
	// RetryReasonRead identifies a transient failure while reading a successful body.
	RetryReasonRead RetryReason = "read"
	// RetryReasonClose identifies a transient failure closing an otherwise valid body.
	RetryReasonClose RetryReason = "close"
)

// RetryEvent describes a source replay using only bounded, non-sensitive metadata.
// Attempt is the one-based HTTP attempt scheduled to run after Delay.
type RetryEvent struct {
	Kind       Kind
	Reason     RetryReason
	StatusCode int
	Attempt    int
	Delay      time.Duration
}

// RetryObserver receives sanitized source retry events synchronously. Calls must be
// concurrency-safe, return quickly, and must not call back into the same acquisition.
type RetryObserver func(RetryEvent)

// RetryConfig bounds HTTPS source attempts. MaxRetries may be zero to disable
// replays; MaxElapsed must be positive and is the total budget shared by the initial
// request, backoff, and every replay. The caller context and HTTP client timeout may
// end it sooner.
type RetryConfig struct {
	MaxRetries int
	MaxElapsed time.Duration
	Observer   RetryObserver
}

// Provenance is safe to emit in structured logs. Origin is either an allowlisted
// hostname or the literal "local-file"; it never contains a URL path or file path.
type Provenance struct {
	Kind   Kind
	Mode   Mode
	Origin string
	Bytes  int64
	SHA256 string
}

// Document is an immutable, all-or-nothing acquisition result. Its source bytes are
// private so a parser and a provenance hash cannot be invalidated by caller mutation.
type Document struct {
	data       []byte
	provenance Provenance
}

// Len returns the number of exact acquired source bytes.
func (document Document) Len() int {
	return len(document.data)
}

// Reader returns an independent reader over the immutable source bytes without
// copying the potentially multi-megabyte assignment dictionary.
func (document Document) Reader() io.Reader {
	return bytes.NewReader(document.data)
}

// Bytes returns an independent source copy for callers that require a byte slice.
func (document Document) Bytes() []byte {
	return append([]byte(nil), document.data...)
}

// Provenance returns the document's safe source metadata and exact-byte digest.
func (document Document) Provenance() Provenance {
	return document.provenance
}

// LoadError classifies acquisition failures without retaining raw URLs, file paths,
// response headers, response bodies, or transport error strings.
type LoadError struct {
	Kind       Kind
	Mode       Mode
	StatusCode int
	code       error
	cause      error
}

// Error returns a stable, sanitized diagnostic.
func (e *LoadError) Error() string {
	if e == nil {
		return "load input"
	}
	prefix := "load " + e.Kind.String() + " " + e.Mode.String() + " source"
	if e.code == nil {
		return prefix
	}
	if e.StatusCode != 0 {
		return prefix + ": " + e.code.Error() + " (status " + statusText(e.StatusCode) + ")"
	}
	return prefix + ": " + e.code.Error()
}

// Unwrap preserves only context cancellation/deadline causes, which are safe and
// useful to process lifecycle code. Raw filesystem and network errors are omitted
// because their strings can contain sensitive local paths or requested URLs.
func (e *LoadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is supports stable sentinel classification in addition to any safe context cause.
func (e *LoadError) Is(target error) bool {
	return e != nil && e.code != nil && target == e.code
}

func statusText(status int) string {
	// Status codes are bounded protocol metadata, unlike http.StatusText strings or
	// arbitrary response status lines. Integer formatting avoids retaining either.
	const digits = "0123456789"
	if status <= 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for status > 0 {
		index--
		buffer[index] = digits[status%10]
		status /= 10
	}
	return string(buffer[index:])
}
