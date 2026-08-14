// Package config parses and validates DuckWords command-line configuration.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/pointerm/duckwords/internal/acquire"
	"github.com/pointerm/duckwords/internal/logging"
	"github.com/pointerm/duckwords/internal/words"
)

const (
	// DefaultPostsURL is the fixed assignment post-list origin used unless a
	// local file or another validated HTTPS URL is selected explicitly.
	DefaultPostsURL = "https://gist.githubusercontent.com/jonathan-firefly/b7fa366ce0fce7ab977db331ed169194/raw/duck_urls_200.txt"
	// DefaultDictionaryURL is the assignment word-bank origin used unless a local
	// file or another validated HTTPS URL is selected explicitly.
	DefaultDictionaryURL = "https://raw.githubusercontent.com/dwyl/english-words/master/words.txt"

	// DefaultWorkers overlaps independent network waits while keeping the number of
	// simultaneously retained comment trees conservative until live memory evidence
	// supports a higher value.
	DefaultWorkers = 4
	// MinWorkers is the smallest supported post worker pool.
	MinWorkers = 1
	// MaxWorkers caps goroutines and in-flight post traversals.
	MaxWorkers = 32

	// DefaultTimeout bounds the complete 200-post run. A conservative API pace can
	// make a correct traversal take several minutes.
	DefaultTimeout = 30 * time.Minute
	// MinTimeout rejects accidental zero or sub-second whole-run deadlines.
	MinTimeout = time.Second
	// MaxTimeout prevents a duration typo from creating an effectively immortal run.
	MaxTimeout = 2 * time.Hour

	// DefaultRateLimit spaces requests below both historical Reddit ceilings and
	// leaves runtime response headers authoritative when they require a slower pace.
	DefaultRateLimit = 0.8
	// MinRateLimit prevents impractically slow configuration mistakes.
	MinRateLimit = 0.1
	// MaxRateLimit keeps even an explicit override below Reddit's documented
	// application-wide request ceiling, with headroom for clock and scheduling skew.
	MaxRateLimit = 1.5

	// DefaultRequestTimeout bounds one HTTP attempt, including response-body reads.
	DefaultRequestTimeout = 20 * time.Second
	// MinRequestTimeout rejects deadlines too small for useful network work.
	MinRequestTimeout = time.Second
	// MaxRequestTimeout prevents a single stuck attempt from consuming the run.
	MaxRequestTimeout = 2 * time.Minute

	// DefaultMaxRetries permits a small number of transient retries without hiding a
	// persistent failure.
	DefaultMaxRetries = 3
	// MinMaxRetries allows retries to be disabled explicitly.
	MinMaxRetries = 0
	// MaxMaxRetries caps amplification of a failed request.
	MaxMaxRetries = 5

	// DefaultRetryBudget bounds cumulative HTTP-attempt, observer, and retry-backoff
	// work for one logical request. Shared limiter/gate waits remain bounded by the
	// global run timeout and do not consume this per-request budget.
	DefaultRetryBudget = 45 * time.Second
	// MinRetryBudget rejects zero and negative retry windows.
	MinRetryBudget = time.Second
	// MaxRetryBudget prevents retries from monopolizing the global run deadline.
	MaxRetryBudget = 5 * time.Minute
)

// Backward-compatible package-local names keep the existing package tests concise
// while production callers use the exported operational constants above.
const (
	defaultWorkers = DefaultWorkers
	minWorkers     = MinWorkers
	maxWorkers     = MaxWorkers
	defaultTimeout = DefaultTimeout
	minTimeout     = MinTimeout
	maxTimeout     = MaxTimeout
)

var (
	// ErrWorkersOutOfRange indicates a worker count outside the supported safe range.
	ErrWorkersOutOfRange = errors.New("workers out of range")
	// ErrInvalidFailureMode indicates an unsupported completeness policy.
	ErrInvalidFailureMode = errors.New("invalid failure mode")
	// ErrTimeoutOutOfRange indicates a global timeout outside the supported safe range.
	ErrTimeoutOutOfRange = errors.New("timeout out of range")
	// ErrConflictingMode indicates that an informational action was combined with
	// processing options whose values would otherwise be silently ignored.
	ErrConflictingMode = errors.New("conflicting command modes")
	// ErrInvalidFilter indicates that one or more filter patterns violated the words
	// package's bounded glob grammar.
	ErrInvalidFilter = errors.New("invalid filter configuration")
	// ErrConflictingSources indicates that URL and file selectors were both supplied
	// for the same input.
	ErrConflictingSources = errors.New("conflicting input sources")
	// ErrDuplicateOption indicates that a non-repeatable source selector appeared
	// more than once.
	ErrDuplicateOption = errors.New("duplicate command option")
	// ErrInvalidSource indicates an empty, unsafe, or malformed source selector.
	ErrInvalidSource = errors.New("invalid input source")
	// ErrSourceHostNotAllowed indicates a syntactically valid HTTPS URL that is not
	// on the fixed origin allowlist for that input.
	ErrSourceHostNotAllowed = errors.New("input URL host is not allowed")
	// ErrSourcePathNotSupported indicates a URL on the correct origin whose path or
	// query violates the acquisition policy. It is distinct from a host rejection so
	// the diagnostic does not blame the part of the URL that was already correct.
	ErrSourcePathNotSupported = errors.New("input URL path or query is not supported")
	// ErrRateLimitOutOfRange indicates a non-finite or unsupported request rate.
	ErrRateLimitOutOfRange = errors.New("rate limit out of range")
	// ErrRequestTimeoutOutOfRange indicates an unsupported per-attempt timeout.
	ErrRequestTimeoutOutOfRange = errors.New("request timeout out of range")
	// ErrMaxRetriesOutOfRange indicates an unsupported transient retry count.
	ErrMaxRetriesOutOfRange = errors.New("maximum retries out of range")
	// ErrRetryBudgetOutOfRange indicates an unsupported cumulative retry budget.
	ErrRetryBudgetOutOfRange = errors.New("retry budget out of range")
	// ErrInvalidLogLevel indicates an unsupported structured-log threshold.
	ErrInvalidLogLevel = errors.New("invalid log level")
	// ErrInvalidLogFormat indicates an unsupported structured-log encoding.
	ErrInvalidLogFormat = errors.New("invalid log format")
)

const usage = `DuckWords counts dictionary words across complete Reddit comment trees.

Running it with no options processes the assignment inputs with the defaults below.

Usage:
  duckwords [options]

Input (URL and file forms are mutually exclusive):
  --posts-url URL               post list; must be on gist.githubusercontent.com
  --posts-file PATH             read the post list from a local file
  --dictionary-url URL          word bank; must be on raw.githubusercontent.com
  --dictionary-file PATH        read the word bank from a local file
Selection:
  --filter PATTERN              exact or '*' wildcard filter; repeatable
Reliability:
  --workers N                   concurrent posts (default 4; range 1..32)
  --rate-limit RATE             requests per second (default 0.8; range 0.1..1.5)
  --request-timeout DURATION    one HTTP attempt (default 20s; range 1s..2m)
  --timeout DURATION            global execution timeout (default 30m; range 1s..2h)
  --max-retries N               transient retries (default 3; range 0..5)
  --retry-budget DURATION       HTTP/retry work per request (default 45s; range 1s..5m)
  --failure-mode MODE           best-effort or strict (default best-effort)
Observability:
  --log-level LEVEL             debug, info, warn, or error (default info)
  --log-format FORMAT           text or json (default text)
Information:
  --version                     print build and toolchain information
  -h, --help                    show this help

Environment (optional; values are never accepted as flags):
  REDDIT_USER_AGENT             override the built-in public-request identity
  REDDIT_BROWSER_COOKIE         own Cookie value; name=value pairs, max 16 KiB (sensitive)
  REDDIT_BROWSER_ACCEPT_LANGUAGE, REDDIT_BROWSER_SEC_CH_UA,
  REDDIT_BROWSER_SEC_CH_UA_MOBILE, REDDIT_BROWSER_SEC_CH_UA_PLATFORM
                                optional printable-ASCII browser-session headers

Examples:
  duckwords
  duckwords --filter 'duck*' --log-format=json
`

// FailureMode controls whether a per-post failure aborts the run or permits an
// explicitly partial result.
type FailureMode string

const (
	// FailureModeBestEffort preserves complete post results and reports partialness.
	FailureModeBestEffort FailureMode = "best-effort"
	// FailureModeStrict aborts the run when any required post cannot be completed.
	FailureModeStrict FailureMode = "strict"
)

// SourceKind identifies how an assignment input is acquired.
type SourceKind string

const (
	// SourceURL selects bounded HTTPS acquisition.
	SourceURL SourceKind = "url"
	// SourceFile selects a bounded local regular file.
	SourceFile SourceKind = "file"
)

// InputSource is one validated source selection. Location is never suitable for
// direct logging; the acquisition layer owns safe provenance metadata.
type InputSource struct {
	Kind     SourceKind
	Location string
}

// Config contains validated process configuration. The optional Reddit User-Agent
// override remains outside this value so it cannot be exposed through process
// arguments or generic configuration formatting.
type Config struct {
	Posts          InputSource
	Dictionary     InputSource
	Workers        int
	RateLimit      float64
	RequestTimeout time.Duration
	Timeout        time.Duration
	MaxRetries     int
	RetryBudget    time.Duration
	FailureMode    FailureMode
	Filters        []string
	LogLevel       logging.Level
	LogFormat      logging.Format
	ShowVersion    bool
	// RunRequested reports that this invocation should process data. It is false only
	// for informational modes such as --version.
	RunRequested bool
}

// Parse parses and validates command-line arguments without writing usage or errors
// implicitly. Validation is complete before an application executor can be called.
func Parse(args []string) (Config, error) {
	cfg := Config{
		Posts:          InputSource{Kind: SourceURL, Location: DefaultPostsURL},
		Dictionary:     InputSource{Kind: SourceURL, Location: DefaultDictionaryURL},
		Workers:        DefaultWorkers,
		RateLimit:      DefaultRateLimit,
		RequestTimeout: DefaultRequestTimeout,
		Timeout:        DefaultTimeout,
		MaxRetries:     DefaultMaxRetries,
		RetryBudget:    DefaultRetryBudget,
		FailureMode:    FailureModeBestEffort,
		LogLevel:       logging.LevelInfo,
		LogFormat:      logging.FormatText,
	}
	failureMode := string(cfg.FailureMode)
	logLevel := string(cfg.LogLevel)
	logFormat := string(cfg.LogFormat)
	postsURL := cfg.Posts.Location
	postsFile := ""
	dictionaryURL := cfg.Dictionary.Location
	dictionaryFile := ""
	var filters repeatedStrings

	flags := flag.NewFlagSet("duckwords", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	postsURLFlag := newSingleStringValue(&postsURL)
	postsFileFlag := newSingleStringValue(&postsFile)
	dictionaryURLFlag := newSingleStringValue(&dictionaryURL)
	dictionaryFileFlag := newSingleStringValue(&dictionaryFile)
	flags.Var(postsURLFlag, "posts-url", "HTTPS post-list URL")
	flags.Var(postsFileFlag, "posts-file", "local post-list path")
	flags.Var(dictionaryURLFlag, "dictionary-url", "HTTPS word-bank URL")
	flags.Var(dictionaryFileFlag, "dictionary-file", "local word-bank path")
	flags.Var(&filters, "filter", "exact or '*' wildcard filter; repeatable")
	flags.IntVar(&cfg.Workers, "workers", cfg.Workers, "concurrent posts")
	flags.Float64Var(&cfg.RateLimit, "rate-limit", cfg.RateLimit, "requests per second")
	flags.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "one HTTP attempt timeout")
	flags.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "global execution timeout")
	flags.IntVar(&cfg.MaxRetries, "max-retries", cfg.MaxRetries, "transient retries")
	flags.DurationVar(&cfg.RetryBudget, "retry-budget", cfg.RetryBudget, "total retry session")
	flags.StringVar(&failureMode, "failure-mode", failureMode, "best-effort or strict")
	flags.StringVar(&logLevel, "log-level", logLevel, "debug, info, warn, or error")
	flags.StringVar(&logFormat, "log-format", logFormat, "text or json")
	flags.BoolVar(&cfg.ShowVersion, "version", false, "print build and toolchain information")

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: got %d", flags.NArg())
	}
	for _, option := range []struct {
		name  string
		value *singleStringValue
	}{
		{name: "posts-url", value: postsURLFlag},
		{name: "posts-file", value: postsFileFlag},
		{name: "dictionary-url", value: dictionaryURLFlag},
		{name: "dictionary-file", value: dictionaryFileFlag},
	} {
		if option.value.count > 1 {
			return Config{}, fmt.Errorf("%w: --%s may be provided only once", ErrDuplicateOption, option.name)
		}
	}

	explicit := make(map[string]bool)
	processingOption := false
	flags.Visit(func(option *flag.Flag) {
		explicit[option.Name] = true
		if option.Name != "version" {
			processingOption = true
		}
	})
	if cfg.ShowVersion && processingOption {
		return Config{}, fmt.Errorf("%w: --version cannot be combined with processing options", ErrConflictingMode)
	}
	// Processing is the default action: an invocation with no arguments runs the
	// assignment configuration. Only the informational modes opt out.
	cfg.RunRequested = !cfg.ShowVersion

	var err error
	cfg.Posts, err = selectSource("posts", acquire.KindPosts, postsURL, postsFile, explicit["posts-url"], explicit["posts-file"])
	if err != nil {
		return Config{}, err
	}
	cfg.Dictionary, err = selectSource("dictionary", acquire.KindDictionary, dictionaryURL, dictionaryFile, explicit["dictionary-url"], explicit["dictionary-file"])
	if err != nil {
		return Config{}, err
	}

	cfg.FailureMode = FailureMode(failureMode)
	switch cfg.FailureMode {
	case FailureModeBestEffort, FailureModeStrict:
	default:
		return Config{}, fmt.Errorf(
			"%w: expected %s or %s",
			ErrInvalidFailureMode,
			FailureModeBestEffort,
			FailureModeStrict,
		)
	}
	if cfg.Workers < MinWorkers || cfg.Workers > MaxWorkers {
		return Config{}, fmt.Errorf(
			"%w: got %d, expected %d..%d",
			ErrWorkersOutOfRange,
			cfg.Workers,
			MinWorkers,
			MaxWorkers,
		)
	}
	if math.IsNaN(cfg.RateLimit) || math.IsInf(cfg.RateLimit, 0) || cfg.RateLimit < MinRateLimit || cfg.RateLimit > MaxRateLimit {
		return Config{}, fmt.Errorf(
			"%w: expected a finite value in %.1f..%.1f requests/second",
			ErrRateLimitOutOfRange,
			MinRateLimit,
			MaxRateLimit,
		)
	}
	if cfg.RequestTimeout < MinRequestTimeout || cfg.RequestTimeout > MaxRequestTimeout {
		return Config{}, fmt.Errorf(
			"%w: got %s, expected %s..%s",
			ErrRequestTimeoutOutOfRange,
			cfg.RequestTimeout,
			MinRequestTimeout,
			MaxRequestTimeout,
		)
	}
	if cfg.Timeout < MinTimeout || cfg.Timeout > MaxTimeout {
		return Config{}, fmt.Errorf(
			"%w: got %s, expected %s..%s",
			ErrTimeoutOutOfRange,
			cfg.Timeout,
			MinTimeout,
			MaxTimeout,
		)
	}
	if cfg.MaxRetries < MinMaxRetries || cfg.MaxRetries > MaxMaxRetries {
		return Config{}, fmt.Errorf(
			"%w: got %d, expected %d..%d",
			ErrMaxRetriesOutOfRange,
			cfg.MaxRetries,
			MinMaxRetries,
			MaxMaxRetries,
		)
	}
	if cfg.RetryBudget < MinRetryBudget || cfg.RetryBudget > MaxRetryBudget {
		return Config{}, fmt.Errorf(
			"%w: got %s, expected %s..%s",
			ErrRetryBudgetOutOfRange,
			cfg.RetryBudget,
			MinRetryBudget,
			MaxRetryBudget,
		)
	}

	cfg.LogLevel, err = logging.ParseLevel(logLevel)
	if err != nil {
		return Config{}, fmt.Errorf("%w: expected debug, info, warn, or error", ErrInvalidLogLevel)
	}
	cfg.LogFormat, err = logging.ParseFormat(logFormat)
	if err != nil {
		return Config{}, fmt.Errorf("%w: expected text or json", ErrInvalidLogFormat)
	}

	cfg.Filters = append([]string(nil), filters...)
	if _, err := words.NewMatcher(cfg.Filters); err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalidFilter, err)
	}

	return cfg, nil
}

// selectSource resolves one input to exactly one validated source. Remote URLs are
// checked against the same acquisition policy that will later download them, so an
// unusable origin is reported here as a configuration error with the allowed host
// rather than surfacing later as an opaque setup failure.
func selectSource(name string, kind acquire.Kind, urlValue, fileValue string, urlExplicit, fileExplicit bool) (InputSource, error) {
	if urlExplicit && fileExplicit {
		return InputSource{}, fmt.Errorf(
			"%w: --%s-url and --%s-file cannot be combined",
			ErrConflictingSources,
			name,
			name,
		)
	}
	if fileExplicit {
		if err := validateFilePath(fileValue); err != nil {
			return InputSource{}, fmt.Errorf("%w: --%s-file must be a non-empty local path", ErrInvalidSource, name)
		}
		if err := acquire.ValidateSpec(acquire.Spec{Kind: kind, File: fileValue}); err != nil {
			return InputSource{}, fmt.Errorf("%w: --%s-file must be a non-empty local path", ErrInvalidSource, name)
		}
		return InputSource{Kind: SourceFile, Location: fileValue}, nil
	}
	parsed, err := validateRemoteURL(urlValue)
	if err != nil {
		return InputSource{}, fmt.Errorf("%w: --%s-url must be an HTTPS URL without credentials or fragments", ErrInvalidSource, name)
	}
	host, ok := acquire.AllowedHost(kind)
	if !ok {
		return InputSource{}, fmt.Errorf("%w: --%s-url is not a supported input", ErrInvalidSource, name)
	}
	// Separate the host decision from the rest of the locator policy. Reporting a
	// path or query problem as a wrong host sends the reader to the one part of the
	// URL that was already correct.
	if parsed.Hostname() != host {
		return InputSource{}, fmt.Errorf(
			"%w: --%s-url must be on %s",
			ErrSourceHostNotAllowed,
			name,
			host,
		)
	}
	if err := acquire.ValidateSpec(acquire.Spec{Kind: kind, URL: urlValue}); err != nil {
		return InputSource{}, fmt.Errorf(
			"%w: --%s-url is on %s but its path or query is not supported",
			ErrSourcePathNotSupported,
			name,
			host,
		)
	}
	return InputSource{Kind: SourceURL, Location: urlValue}, nil
}

func validateFilePath(path string) error {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(path) != path ||
		strings.ContainsRune(path, '\x00') || len(path) > 4<<10 {
		return ErrInvalidSource
	}
	return nil
}

// validateRemoteURL checks the transport-independent safety rules and returns the
// parsed URL so the caller can report a host decision separately from path policy.
func validateRemoteURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw || strings.ContainsRune(raw, '\x00') {
		return nil, ErrInvalidSource
	}
	if len(raw) > 4<<10 {
		return nil, ErrInvalidSource
	}
	// A query string is legitimate for raw-gist and CDN links and is preserved. The
	// acquisition layer applies the host and path policy; this check only rejects
	// forms that are unsafe or that HTTP would silently discard.
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Port() != "" || parsed.RawPath != "" {
		return nil, ErrInvalidSource
	}
	return parsed, nil
}

// WriteUsage writes the stable command synopsis to w.
func WriteUsage(w io.Writer) error {
	_, err := io.WriteString(w, usage)
	return err
}

type repeatedStrings []string

func (values *repeatedStrings) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedStrings) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type singleStringValue struct {
	value *string
	count int
}

func newSingleStringValue(value *string) *singleStringValue {
	return &singleStringValue{value: value}
}

func (value *singleStringValue) String() string {
	if value == nil || value.value == nil {
		return ""
	}
	return *value.value
}

func (value *singleStringValue) Set(next string) error {
	value.count++
	*value.value = next
	return nil
}
