package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/words"
)

const (
	// MinWorkers is the smallest supported post worker pool.
	MinWorkers = 1
	// MaxWorkers prevents configuration mistakes from creating excessive Reddit
	// concurrency. Request rate is governed separately by the transport layer.
	MaxWorkers = 32
	// DefaultWorkers overlaps independent post requests without aggressively loading
	// Reddit or a small local machine.
	DefaultWorkers = 4
	// DefaultMaxDistinctWordsPerPost bounds each worker-local count map while leaving
	// ample headroom for a natural-language comment tree.
	DefaultMaxDistinctWordsPerPost = 50_000

	resultWordLimit = 10
)

var (
	// ErrInvalidConfig identifies an invalid runner dependency or option.
	ErrInvalidConfig = errors.New("invalid application configuration")
	// ErrStrictFailure identifies a post-level failure that strict policy promoted to
	// a run failure. Its wrapped cause remains available through errors.Is/As.
	ErrStrictFailure = errors.New("strict processing failure")
	// ErrPartialResult reports that a usable best-effort result excludes at least one
	// post. The Result returned alongside this error is safe to encode.
	ErrPartialResult = errors.New("partial application result")
	// ErrNoCompletedPosts reports that a non-empty run produced no complete post whose
	// counts could be trusted.
	ErrNoCompletedPosts = errors.New("no posts completed")

	errMissingOutcome   = errors.New("worker pipeline produced no post outcome")
	errSummaryOverflow  = errors.New("application summary overflow")
	errInvalidWalkStats = errors.New("comment walker returned invalid statistics")
)

// FailureMode controls whether an isolated post failure aborts the run.
type FailureMode string

const (
	// FailureModeBestEffort preserves complete posts and explicitly reports a partial
	// result when another post failed or was incomplete.
	FailureModeBestEffort FailureMode = "best-effort"
	// FailureModeStrict cancels sibling work after the first material post failure.
	FailureModeStrict FailureMode = "strict"
)

// Config defines bounded application concurrency, count state, and failure policy.
type Config struct {
	Workers     int
	FailureMode FailureMode
	// MaxDistinctWordsPerPost bounds one worker-local count map. Zero selects the
	// conservative default; a value above the dictionary size is harmlessly clamped
	// because no other key can be counted.
	MaxDistinctWordsPerPost int
}

// DefaultConfig returns the conservative application defaults.
func DefaultConfig() Config {
	return Config{
		Workers:                 DefaultWorkers,
		FailureMode:             FailureModeBestEffort,
		MaxDistinctWordsPerPost: DefaultMaxDistinctWordsPerPost,
	}
}

// CommentWalker is the application boundary for one complete Reddit comment tree.
// Implementations must support concurrent WalkComments calls up to Config.Workers,
// stop visiting after the callback returns an error, and return a non-nil error
// whenever completeness was not proven. A single call's callback is synchronous and
// must never be invoked concurrently or after WalkComments returns; this lets each
// worker retain exclusive ownership of its post-local count map.
type CommentWalker interface {
	WalkComments(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error)
}

// OutcomeStatus is the terminal processing state of one unique input post.
type OutcomeStatus string

const (
	// OutcomeCompleted means the complete post-local count was merged.
	OutcomeCompleted OutcomeStatus = "completed"
	// OutcomeSkipped is reserved for a future adapter signal that reliably proves an
	// expected terminal state. HTTP 403/404 alone are not such a signal.
	OutcomeSkipped OutcomeStatus = "skipped"
	// OutcomeFailed means the post ended in an API, transport, protocol, or fatal
	// processing failure.
	OutcomeFailed OutcomeStatus = "failed"
	// OutcomeIncomplete means post-local progress was discarded because either the
	// complete reachable tree or its bounded count state could not be proven complete.
	OutcomeIncomplete OutcomeStatus = "incomplete"
)

// PostOutcome is sanitized, source-ordered metadata for one post. It intentionally
// excludes URLs, comment text, authors, and arbitrary dependency error strings.
type PostOutcome struct {
	PostID               string            `json:"post_id"`
	SourceLine           int               `json:"source_line"`
	Status               OutcomeStatus     `json:"status"`
	ErrorClass           reddit.ErrorClass `json:"error_class,omitempty"`
	Endpoint             reddit.Endpoint   `json:"endpoint,omitempty"`
	HTTPStatus           int               `json:"http_status,omitempty"`
	Comments             int               `json:"comments"`
	BodiesVisited        int               `json:"bodies_visited"`
	MoreRequests         int               `json:"more_requests"`
	ContinuationRequests int               `json:"continuation_requests"`
	CountedTokens        uint64            `json:"counted_tokens"`
}

// Summary reconciles every unique input post. Comment and token totals include only
// completed posts because incomplete post-local work is deliberately discarded.
type Summary struct {
	Total                int    `json:"total"`
	Completed            int    `json:"completed"`
	Skipped              int    `json:"skipped"`
	Failed               int    `json:"failed"`
	Incomplete           int    `json:"incomplete"`
	Comments             uint64 `json:"comments"`
	BodiesVisited        uint64 `json:"bodies_visited"`
	MoreRequests         uint64 `json:"more_requests"`
	ContinuationRequests uint64 `json:"continuation_requests"`
	CountedTokens        uint64 `json:"counted_tokens"`
	DistinctWords        int    `json:"distinct_words"`
	Partial              bool   `json:"partial"`
}

// Result contains deterministic ranks plus diagnostic metadata. Words is always a
// non-nil slice. Only nil errors and ErrPartialResult make Words suitable for CLI
// output; strict, cancellation, and fatal errors must suppress JSON.
type Result struct {
	Words    []aggregate.WordCount `json:"words"`
	Summary  Summary               `json:"summary"`
	Outcomes []PostOutcome         `json:"outcomes"`
}

// Runner is an immutable application use case and is safe for sequential Run calls.
// Concurrent Run calls are safe when the injected walker is also safe for them.
type Runner struct {
	config           Config
	walker           CommentWalker
	counter          words.Counter
	maxDistinctWords int
}

// New validates all application dependencies before any worker can start.
func New(config Config, walker CommentWalker, dictionary words.Dictionary, matcher words.Matcher) (*Runner, error) {
	if config.Workers < MinWorkers || config.Workers > MaxWorkers {
		return nil, fmt.Errorf("%w: workers must be between %d and %d", ErrInvalidConfig, MinWorkers, MaxWorkers)
	}
	if config.FailureMode != FailureModeBestEffort && config.FailureMode != FailureModeStrict {
		return nil, fmt.Errorf("%w: unsupported failure mode %q", ErrInvalidConfig, config.FailureMode)
	}
	if nilCommentWalker(walker) {
		return nil, fmt.Errorf("%w: comment walker is required", ErrInvalidConfig)
	}
	if config.MaxDistinctWordsPerPost < 0 {
		return nil, fmt.Errorf("%w: maximum distinct words per post must be positive, or zero for the default", ErrInvalidConfig)
	}

	counter, err := words.NewCounter(dictionary, matcher)
	if err != nil {
		return nil, fmt.Errorf("%w: initialize word counter: %w", ErrInvalidConfig, err)
	}

	maxDistinctWords := config.MaxDistinctWordsPerPost
	if maxDistinctWords == 0 {
		maxDistinctWords = DefaultMaxDistinctWordsPerPost
	}
	maxDistinctWords = min(maxDistinctWords, dictionary.Len())

	return &Runner{
		config:           config,
		walker:           walker,
		counter:          counter,
		maxDistinctWords: maxDistinctWords,
	}, nil
}

type runError struct {
	kind   error
	postID string
	class  reddit.ErrorClass
	cause  error
}

func (e *runError) Error() string {
	if e == nil {
		return "application run failure"
	}
	label := "fatal processing failure"
	if errors.Is(e.kind, ErrStrictFailure) {
		label = ErrStrictFailure.Error()
	}
	if e.class != "" {
		return fmt.Sprintf("%s for post %q (class %s)", label, e.postID, e.class)
	}
	return fmt.Sprintf("%s for post %q", label, e.postID)
}

// Unwrap exposes policy and dependency classifications without formatting a
// potentially sensitive dependency error.
func (e *runError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errorsToUnwrap := make([]error, 0, 2)
	if e.kind != nil {
		errorsToUnwrap = append(errorsToUnwrap, e.kind)
	}
	if e.cause != nil {
		errorsToUnwrap = append(errorsToUnwrap, e.cause)
	}
	return errorsToUnwrap
}

func newRunError(kind error, postID string, class reddit.ErrorClass, cause error) error {
	return &runError{kind: kind, postID: postID, class: class, cause: cause}
}

func nilCommentWalker(walker CommentWalker) bool {
	if walker == nil {
		return true
	}
	value := reflect.ValueOf(walker)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type pipelineError struct {
	cause error
}

func (e *pipelineError) Error() string { return "application result aggregation failed" }
func (e *pipelineError) Unwrap() error { return e.cause }
