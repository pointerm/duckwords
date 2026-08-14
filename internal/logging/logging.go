package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
)

const redactedValue = "[REDACTED]"

var (
	// ErrNilWriter indicates that no operational-log destination was supplied.
	ErrNilWriter = errors.New("nil log writer")
	// ErrInvalidLevel indicates an unsupported minimum log level.
	ErrInvalidLevel = errors.New("invalid log level")
	// ErrInvalidFormat indicates an unsupported log encoding.
	ErrInvalidFormat = errors.New("invalid log format")
)

// Level is the minimum severity emitted by a Sink.
type Level string

const (
	// LevelDebug enables diagnostic lifecycle details.
	LevelDebug Level = "debug"
	// LevelInfo enables normal lifecycle and summary events.
	LevelInfo Level = "info"
	// LevelWarn enables warnings and failures.
	LevelWarn Level = "warn"
	// LevelError enables failures only.
	LevelError Level = "error"
)

// Format is the operational-log encoding.
type Format string

const (
	// FormatText emits slog's structured key=value representation.
	FormatText Format = "text"
	// FormatJSON emits one JSON object per record.
	FormatJSON Format = "json"
)

// Event is a stable machine-readable lifecycle event name.
type Event string

const (
	// EventRunStarted marks validated production execution beginning.
	EventRunStarted Event = "run_started"
	// EventSourceLoaded records bounded input acquisition.
	EventSourceLoaded Event = "source_loaded"
	// EventSourceParsed records successful bounded input parsing and validation.
	EventSourceParsed Event = "source_parsed"
	// EventRetry records a scheduled transient-request retry.
	EventRetry Event = "request_retry"
	// EventHTTPAttempt records one sanitized completed Reddit HTTP attempt.
	EventHTTPAttempt Event = "http_attempt"
	// EventPostOutcome records one sanitized per-post terminal outcome.
	EventPostOutcome Event = "post_outcome"
	// EventRunSummary records reconciled processing counters and provenance.
	EventRunSummary Event = "run_summary"
	// EventOutputWritten confirms that one complete result document reached stdout.
	EventOutputWritten Event = "output_written"
	// EventRunFailed marks a fatal terminal result.
	EventRunFailed Event = "run_failed"
	// EventRunCancelled marks a timeout or user cancellation.
	EventRunCancelled Event = "run_cancelled"
)

const (
	// KeyEvent names the stable lifecycle event field.
	KeyEvent = "event"
	// KeyOperation names a bounded operation without embedding a URL or payload.
	KeyOperation = "operation"
	// KeyErrorClass names a controlled error category; raw errors must not be logged.
	KeyErrorClass = "error_class"
	// KeyPostID names a validated Reddit base-36 post identifier.
	KeyPostID = "post_id"
	// KeySourceKind names input provenance without exposing an uncontrolled location.
	KeySourceKind = "source_kind"
)

// Options controls Sink construction. The zero value selects info-level text logs.
// ReplaceAttr is primarily useful for deterministic tests; UTC conversion and
// sensitive-key redaction run after it, with sensitive original keys retained for
// the redaction decision. Log messages and attribute values must still be controlled
// metadata rather than raw errors, URLs, credentials, response bodies, or comment
// text.
type Options struct {
	Level       Level
	Format      Format
	ReplaceAttr func(groups []string, attr slog.Attr) slog.Attr
}

// Sink owns a structured logger and records the first handler/write error. slog's
// Logger methods intentionally do not return handler errors, so process composition
// must inspect Err before declaring a successful application log.
type Sink struct {
	logger   *slog.Logger
	firstErr atomic.Pointer[recordedError]
}

type recordedError struct {
	err error
}

// New constructs a concurrency-safe structured log sink writing only to w.
func New(w io.Writer, options Options) (*Sink, error) {
	if w == nil {
		return nil, ErrNilWriter
	}
	if options.Level == "" {
		options.Level = LevelInfo
	}
	if options.Format == "" {
		options.Format = FormatText
	}

	level, err := slogLevel(options.Level)
	if err != nil {
		return nil, err
	}
	if _, err := ParseFormat(string(options.Format)); err != nil {
		return nil, err
	}

	handlerOptions := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			originalKey := attr.Key
			if options.ReplaceAttr != nil {
				attr = options.ReplaceAttr(groups, attr)
			}
			if attr.Equal(slog.Attr{}) {
				return attr
			}
			if attr.Key == slog.TimeKey && attr.Value.Kind() == slog.KindTime {
				attr.Value = slog.TimeValue(attr.Value.Time().UTC())
			}
			// Primitive structured fields make the application log reviewable. Arbitrary
			// values can invoke String or JSON methods that expose an error or payload,
			// so redact KindAny as a defense-in-depth boundary.
			sensitiveGroup := false
			for _, group := range groups {
				if sensitiveKey(group) {
					sensitiveGroup = true
					break
				}
			}
			if sensitiveGroup || sensitiveKey(originalKey) || sensitiveKey(attr.Key) ||
				(attr.Value.Kind() == slog.KindAny && !safeStandardAnyAttr(attr)) {
				attr.Value = slog.StringValue(redactedValue)
				return attr
			}
			// slog renders a duration as "20s" in text but as a bare nanosecond
			// integer in JSON. Emitting the canonical string keeps one readable,
			// unit-bearing value in both encodings, so a JSON reader cannot mistake
			// 20000000000 for seconds or milliseconds.
			if attr.Value.Kind() == slog.KindDuration {
				attr.Value = slog.StringValue(attr.Value.Duration().String())
			}
			return attr
		},
	}

	var next slog.Handler
	switch options.Format {
	case FormatText:
		next = slog.NewTextHandler(w, handlerOptions)
	case FormatJSON:
		next = slog.NewJSONHandler(w, handlerOptions)
	default:
		return nil, fmt.Errorf("%w: expected text or json", ErrInvalidFormat)
	}

	sink := &Sink{}
	sink.logger = slog.New(&recordingHandler{next: next, sink: sink})
	return sink, nil
}

// Logger returns the slog logger owned by the sink.
func (sink *Sink) Logger() *slog.Logger {
	if sink == nil {
		return nil
	}
	return sink.logger
}

// Err returns the first handler/write failure, if any.
func (sink *Sink) Err() error {
	if sink == nil {
		return ErrNilWriter
	}
	recorded := sink.firstErr.Load()
	if recorded == nil {
		return nil
	}
	return recorded.err
}

func (sink *Sink) record(err error) {
	if err == nil {
		return
	}
	sink.firstErr.CompareAndSwap(nil, &recordedError{err: err})
}

// ParseLevel validates and returns a structured-log threshold.
func ParseLevel(value string) (Level, error) {
	level := Level(value)
	if _, err := slogLevel(level); err != nil {
		return "", err
	}
	return level, nil
}

func slogLevel(level Level) (slog.Level, error) {
	switch level {
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelInfo:
		return slog.LevelInfo, nil
	case LevelWarn:
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%w: expected debug, info, warn, or error", ErrInvalidLevel)
	}
}

// ParseFormat validates and returns a structured-log encoding.
func ParseFormat(value string) (Format, error) {
	format := Format(value)
	switch format {
	case FormatText, FormatJSON:
		return format, nil
	default:
		return "", fmt.Errorf("%w: expected text or json", ErrInvalidFormat)
	}
}

// EventAttr returns the canonical event attribute for a lifecycle record.
func EventAttr(event Event) slog.Attr {
	if !safeIdentifier(string(event)) {
		event = "unknown"
	}
	return slog.String(KeyEvent, string(event))
}

// ErrorClassAttr returns a bounded machine-readable error category. It deliberately
// accepts only identifier characters so callers cannot accidentally substitute a raw
// transport error or response payload.
func ErrorClassAttr(class string) slog.Attr {
	if !safeIdentifier(class) {
		class = "unknown"
	}
	return slog.String(KeyErrorClass, class)
}

func safeIdentifier(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '_', character == '-', character == '.':
		default:
			return false
		}
	}
	return true
}

func safeStandardAnyAttr(attr slog.Attr) bool {
	if attr.Key != slog.LevelKey {
		return false
	}
	_, ok := attr.Value.Any().(slog.Level)
	return ok
}

func sensitiveKey(key string) bool {
	normalized := strings.Map(func(character rune) rune {
		switch {
		case character >= 'A' && character <= 'Z':
			return character + ('a' - 'A')
		case character >= 'a' && character <= 'z':
			return character
		case character >= '0' && character <= '9':
			return character
		default:
			return -1
		}
	}, key)
	if normalized == "" {
		return false
	}

	for _, fragment := range []string{
		"authorization",
		"clientid",
		"clientsecret",
		"accesstoken",
		"refreshtoken",
		"useragent",
		"responsebody",
		"commentbody",
		"rawerror",
	} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	switch normalized {
	case "token", "secret", "password", "passwd", "cookie", "setcookie", "credential", "credentials", "error", "err", "cause", "detail", "body", "payload", "url", "uri", "path", "location":
		return true
	default:
		return false
	}
}

type recordingHandler struct {
	next slog.Handler
	sink *Sink
}

func (handler *recordingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler *recordingHandler) Handle(ctx context.Context, record slog.Record) error {
	err := handler.next.Handle(ctx, record)
	handler.sink.record(err)
	return err
}

func (handler *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{next: handler.next.WithAttrs(attrs), sink: handler.sink}
}

func (handler *recordingHandler) WithGroup(name string) slog.Handler {
	return &recordingHandler{next: handler.next.WithGroup(name), sink: handler.sink}
}
