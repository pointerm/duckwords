package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/source"
	"github.com/pointerm/duckwords/internal/words"
)

const testTimeout = 5 * time.Second

type walkerFunc func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error)

type refWalkerFunc func(context.Context, reddit.PostRef, func(reddit.Comment) error) (reddit.WalkStats, error)

func (walk walkerFunc) WalkComments(
	ctx context.Context,
	post reddit.PostRef,
	visit func(reddit.Comment) error,
) (reddit.WalkStats, error) {
	return walk(ctx, post.ID, visit)
}

func (walk refWalkerFunc) WalkComments(
	ctx context.Context,
	post reddit.PostRef,
	visit func(reddit.Comment) error,
) (reddit.WalkStats, error) {
	return walk(ctx, post, visit)
}

type pointerWalker struct{}

func (*pointerWalker) WalkComments(
	context.Context,
	reddit.PostRef,
	func(reddit.Comment) error,
) (reddit.WalkStats, error) {
	return reddit.WalkStats{}, nil
}

func TestNewValidatesConfigurationAndDependencies(t *testing.T) {
	t.Parallel()

	dictionary := loadDictionary(t, "duck\nwater\n")
	matcher, err := words.NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	validWalker := walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
		return reddit.WalkStats{}, nil
	})
	var typedNil *pointerWalker

	tests := []struct {
		name       string
		config     Config
		walker     CommentWalker
		dictionary words.Dictionary
		wantError  error
	}{
		{name: "minimum workers", config: Config{Workers: MinWorkers, FailureMode: FailureModeBestEffort}, walker: validWalker, dictionary: dictionary},
		{name: "maximum workers", config: Config{Workers: MaxWorkers, FailureMode: FailureModeStrict}, walker: validWalker, dictionary: dictionary},
		{name: "zero workers", config: Config{Workers: 0, FailureMode: FailureModeBestEffort}, walker: validWalker, dictionary: dictionary, wantError: ErrInvalidConfig},
		{name: "too many workers", config: Config{Workers: MaxWorkers + 1, FailureMode: FailureModeBestEffort}, walker: validWalker, dictionary: dictionary, wantError: ErrInvalidConfig},
		{name: "unknown mode", config: Config{Workers: 1, FailureMode: "continue"}, walker: validWalker, dictionary: dictionary, wantError: ErrInvalidConfig},
		{name: "negative distinct-word limit", config: Config{Workers: 1, FailureMode: FailureModeBestEffort, MaxDistinctWordsPerPost: -1}, walker: validWalker, dictionary: dictionary, wantError: ErrInvalidConfig},
		{name: "nil walker", config: DefaultConfig(), dictionary: dictionary, wantError: ErrInvalidConfig},
		{name: "typed nil walker", config: DefaultConfig(), walker: typedNil, dictionary: dictionary, wantError: ErrInvalidConfig},
		{name: "empty dictionary", config: DefaultConfig(), walker: validWalker, wantError: ErrInvalidConfig},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner, err := New(test.config, test.walker, test.dictionary, matcher)
			if test.wantError == nil {
				if err != nil || runner == nil {
					t.Fatalf("New() runner = %#v, error = %v", runner, err)
				}
				return
			}
			if !errors.Is(err, test.wantError) || runner != nil {
				t.Fatalf("New() runner = %#v, error = %v, want %v", runner, err, test.wantError)
			}
		})
	}
}

func TestDefaultConfigUsesConservativeMemoryAndConcurrencyBounds(t *testing.T) {
	t.Parallel()

	config := DefaultConfig()
	if DefaultWorkers != 4 {
		t.Fatalf("DefaultWorkers = %d, want 4", DefaultWorkers)
	}
	if config.Workers != DefaultWorkers {
		t.Fatalf("Workers = %d, want conservative default %d", config.Workers, DefaultWorkers)
	}
	if DefaultMaxDistinctWordsPerPost != 50_000 {
		t.Fatalf("DefaultMaxDistinctWordsPerPost = %d, want 50000", DefaultMaxDistinctWordsPerPost)
	}
	if config.MaxDistinctWordsPerPost != DefaultMaxDistinctWordsPerPost {
		t.Fatalf("MaxDistinctWordsPerPost = %d, want %d", config.MaxDistinctWordsPerPost, DefaultMaxDistinctWordsPerPost)
	}
}

func TestRunPassesValidatedPostIDAndJSONPathToWalker(t *testing.T) {
	t.Parallel()

	posts, _, err := source.LoadPostList(
		strings.NewReader("https://old.reddit.com/r/Duck_Pictures/comments/AbC123/a_title/\n"),
		source.DefaultPostListLimits(),
	)
	if err != nil {
		t.Fatalf("LoadPostList() error = %v", err)
	}
	var got reddit.PostRef
	walker := refWalkerFunc(func(_ context.Context, post reddit.PostRef, _ func(reddit.Comment) error) (reddit.WalkStats, error) {
		got = post
		return reddit.WalkStats{}, nil
	})
	runner := newRunner(t, 1, FailureModeStrict, walker, nil)
	if _, err := runner.Run(context.Background(), posts); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := reddit.PostRef{ID: "abc123", JSONPath: "/r/duck_pictures/comments/abc123/a_title/.json"}
	if got != want {
		t.Fatalf("walker post ref = %#v, want %#v", got, want)
	}
}

func TestRunMergesOnlyCompletePostsAndRanksDeterministically(t *testing.T) {
	t.Parallel()

	posts := loadPosts(t, "p1", "p2")
	bodies := map[string][]string{
		"p1": {"Duck duck water", "pond"},
		"p2": {"water quack duck"},
	}
	var mu sync.Mutex
	calls := make(map[string]int)
	walker := walkerFunc(func(_ context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		mu.Lock()
		calls[postID]++
		mu.Unlock()
		for index, body := range bodies[postID] {
			if err := visit(reddit.Comment{ID: fmt.Sprintf("%s_%d", postID, index), Body: body}); err != nil {
				return reddit.WalkStats{}, err
			}
		}
		stats := reddit.WalkStats{Comments: len(bodies[postID]), BodiesVisited: len(bodies[postID])}
		if postID == "p1" {
			stats.ExpansionRequests = 1
		}
		return stats, nil
	})
	runner := newRunner(t, 2, FailureModeBestEffort, walker, nil)

	result, err := runner.Run(context.Background(), posts)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantWords := []aggregate.WordCount{
		{Word: "duck", Count: 3},
		{Word: "water", Count: 2},
		{Word: "pond", Count: 1},
		{Word: "quack", Count: 1},
	}
	if !slices.Equal(result.Words, wantWords) {
		t.Fatalf("Words = %#v, want %#v", result.Words, wantWords)
	}
	wantSummary := Summary{
		Total: 2, Completed: 2, Comments: 3, BodiesVisited: 3,
		ExpansionRequests: 1, CountedTokens: 7, DistinctWords: 4,
	}
	if result.Summary != wantSummary {
		t.Fatalf("Summary = %#v, want %#v", result.Summary, wantSummary)
	}
	if len(result.Outcomes) != 2 || result.Outcomes[0].PostID != "p1" || result.Outcomes[1].PostID != "p2" ||
		result.Outcomes[0].SourceLine != 1 || result.Outcomes[1].SourceLine != 2 ||
		result.Outcomes[0].Status != OutcomeCompleted || result.Outcomes[1].Status != OutcomeCompleted {
		t.Fatalf("Outcomes = %#v", result.Outcomes)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["p1"] != 1 || calls["p2"] != 1 {
		t.Fatalf("Walk calls = %#v, want once per post", calls)
	}
}

func TestRunAppliesFixedTopTenLimitAtApplicationBoundary(t *testing.T) {
	t.Parallel()

	const bank = "alpha\nbravo\ncharlie\ndelta\necho\nfoxtrot\ngolf\nhotel\nindia\njuliet\nkilo\nlima\n"
	const body = "lima kilo juliet india hotel golf foxtrot echo delta charlie bravo alpha"
	dictionary := loadDictionary(t, bank)
	matcher, err := words.NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	walker := walkerFunc(func(_ context.Context, _ string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		if err := visit(reddit.Comment{ID: "c1", Body: body}); err != nil {
			return reddit.WalkStats{}, err
		}
		return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
	})
	runner, err := New(Config{Workers: 1, FailureMode: FailureModeBestEffort}, walker, dictionary, matcher)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runner.Run(context.Background(), loadPosts(t, "p1"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := []aggregate.WordCount{
		{Word: "alpha", Count: 1},
		{Word: "bravo", Count: 1},
		{Word: "charlie", Count: 1},
		{Word: "delta", Count: 1},
		{Word: "echo", Count: 1},
		{Word: "foxtrot", Count: 1},
		{Word: "golf", Count: 1},
		{Word: "hotel", Count: 1},
		{Word: "india", Count: 1},
		{Word: "juliet", Count: 1},
	}
	if !slices.Equal(result.Words, want) || result.Summary.DistinctWords != 12 {
		t.Fatalf("Run() Words = %#v, distinct = %d; want top ten %#v from 12", result.Words, result.Summary.DistinctWords, want)
	}
}

func TestRunReturnsAllWordsWhenExactlyTenAreEligible(t *testing.T) {
	t.Parallel()

	const bank = "alpha\nbravo\ncharlie\ndelta\necho\nfoxtrot\ngolf\nhotel\nindia\njuliet\n"
	dictionary := loadDictionary(t, bank)
	matcher, err := words.NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	walker := walkerFunc(func(_ context.Context, _ string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		if err := visit(reddit.Comment{ID: "c1", Body: strings.ReplaceAll(bank, "\n", " ")}); err != nil {
			return reddit.WalkStats{}, err
		}
		return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
	})
	runner, err := New(Config{Workers: 1, FailureMode: FailureModeBestEffort}, walker, dictionary, matcher)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runner.Run(context.Background(), loadPosts(t, "p1"))
	if err != nil || len(result.Words) != 10 || result.Summary.DistinctWords != 10 {
		t.Fatalf("Run() Words = %#v, summary = %#v, error = %v", result.Words, result.Summary, err)
	}
}

func TestRunBestEffortDiscardsIncompletePostAsAUnit(t *testing.T) {
	t.Parallel()

	posts := loadPosts(t, "bad", "good")
	walker := walkerFunc(func(_ context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		switch postID {
		case "bad":
			if err := visit(reddit.Comment{ID: "partial", Body: "duck duck duck"}); err != nil {
				return reddit.WalkStats{}, err
			}
			return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, adapterError(reddit.ErrorIncomplete, reddit.EndpointCommentExpansion, postID, 0)
		case "good":
			if err := visit(reddit.Comment{ID: "complete", Body: "water water"}); err != nil {
				return reddit.WalkStats{}, err
			}
			return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
		default:
			return reddit.WalkStats{}, errors.New("unexpected post")
		}
	})
	runner := newRunner(t, 2, FailureModeBestEffort, walker, nil)

	result, err := runner.Run(context.Background(), posts)
	if err != ErrPartialResult {
		t.Fatalf("Run() error = %v, want exact ErrPartialResult", err)
	}
	if !slices.Equal(result.Words, []aggregate.WordCount{{Word: "water", Count: 2}}) {
		t.Fatalf("Words = %#v; incomplete post leaked counts", result.Words)
	}
	wantSummary := Summary{
		Total: 2, Completed: 1, Incomplete: 1, Comments: 1,
		BodiesVisited: 1, CountedTokens: 2, DistinctWords: 1, Partial: true,
	}
	if result.Summary != wantSummary {
		t.Fatalf("Summary = %#v, want %#v", result.Summary, wantSummary)
	}
	if result.Outcomes[0].Status != OutcomeIncomplete || result.Outcomes[0].ErrorClass != reddit.ErrorIncomplete ||
		result.Outcomes[0].Endpoint != reddit.EndpointCommentExpansion || result.Outcomes[0].CountedTokens != 0 {
		t.Fatalf("incomplete outcome = %#v", result.Outcomes[0])
	}
}

func TestRunBestEffortFailureMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		class      reddit.ErrorClass
		wantStatus OutcomeStatus
		wantFatal  bool
	}{
		{name: "resource limit", class: reddit.ErrorResourceLimit, wantStatus: OutcomeIncomplete},
		{name: "protocol", class: reddit.ErrorProtocol, wantStatus: OutcomeFailed},
		{name: "access", class: reddit.ErrorAccess, wantStatus: OutcomeFailed},
		{name: "rate limited", class: reddit.ErrorRateLimited, wantStatus: OutcomeFailed},
		{name: "server", class: reddit.ErrorServer, wantStatus: OutcomeFailed},
		{name: "transport", class: reddit.ErrorTransport, wantStatus: OutcomeFailed},
		{name: "invalid input", class: reddit.ErrorInvalidInput, wantStatus: OutcomeFailed, wantFatal: true},
		{name: "visitor", class: reddit.ErrorVisitor, wantStatus: OutcomeFailed, wantFatal: true},
		{name: "canceled", class: reddit.ErrorCanceled, wantStatus: OutcomeFailed, wantFatal: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			posts := loadPosts(t, "bad", "good")
			var badCalls atomic.Int32
			walker := walkerFunc(func(_ context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
				if postID == "bad" {
					badCalls.Add(1)
					return reddit.WalkStats{}, adapterError(test.class, reddit.EndpointComments, postID, 503)
				}
				if err := visit(reddit.Comment{ID: "good", Body: "duck"}); err != nil {
					return reddit.WalkStats{}, err
				}
				return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
			})
			runner := newRunner(t, 1, FailureModeBestEffort, walker, nil)

			result, err := runner.Run(context.Background(), posts)
			if test.wantFatal {
				if err == nil || err == ErrPartialResult || len(result.Words) != 0 {
					t.Fatalf("Run() result = %#v, error = %v; want fatal without words", result, err)
				}
				if badCalls.Load() != 1 {
					t.Fatalf("failed post calls = %d, want exactly one without application retry", badCalls.Load())
				}
				var typed *reddit.Error
				if !errors.As(err, &typed) || typed.Class != test.class {
					t.Fatalf("Run() error = %v, want Reddit class %q", err, test.class)
				}
				return
			}
			if err != ErrPartialResult {
				t.Fatalf("Run() error = %v, want ErrPartialResult", err)
			}
			if badCalls.Load() != 1 {
				t.Fatalf("failed post calls = %d, want exactly one without application retry", badCalls.Load())
			}
			if result.Outcomes[0].Status != test.wantStatus || result.Outcomes[0].ErrorClass != test.class {
				t.Fatalf("outcome = %#v", result.Outcomes[0])
			}
			if !slices.Equal(result.Words, []aggregate.WordCount{{Word: "duck", Count: 1}}) {
				t.Fatalf("Words = %#v", result.Words)
			}
		})
	}
}

func TestNotFoundIsSkippedOnlyForInitialHTTP404Or410(t *testing.T) {
	t.Parallel()

	for _, statusCode := range []int{404, 410} {
		status, fatal, class, endpoint, gotStatus := classifyPostError(
			adapterError(reddit.ErrorNotFound, reddit.EndpointComments, "post1", statusCode), nil,
		)
		if status != OutcomeSkipped || fatal || class != reddit.ErrorNotFound || endpoint != reddit.EndpointComments || gotStatus != statusCode {
			t.Fatalf("status %d classification = %q fatal=%t class=%q endpoint=%q HTTP=%d", statusCode, status, fatal, class, endpoint, gotStatus)
		}
	}

	for _, test := range []struct {
		name       string
		endpoint   reddit.Endpoint
		statusCode int
	}{
		{name: "statusless initial", endpoint: reddit.EndpointComments},
		{name: "expansion 404", endpoint: reddit.EndpointCommentExpansion, statusCode: 404},
		{name: "continuation 410", endpoint: reddit.EndpointContinuation, statusCode: 410},
	} {
		status, fatal, _, _, _ := classifyPostError(adapterError(reddit.ErrorNotFound, test.endpoint, "post1", test.statusCode), nil)
		if status != OutcomeFailed || fatal {
			t.Errorf("%s classification = %q fatal=%t, want recoverable failed", test.name, status, fatal)
		}
	}
}

func TestRunTreatsDistinctWordLimitAsRecoverableIncompletePost(t *testing.T) {
	t.Parallel()

	posts := loadPosts(t, "wide", "good")
	walker := walkerFunc(func(_ context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		body := "duck"
		if postID == "wide" {
			body = "duck water pond"
		}
		if err := visit(reddit.Comment{ID: postID, Body: body}); err != nil {
			return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, err
		}
		return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
	})
	dictionary := loadDictionary(t, "duck\nwater\npond\n")
	matcher, err := words.NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner, err := New(Config{
		Workers:                 1,
		FailureMode:             FailureModeBestEffort,
		MaxDistinctWordsPerPost: 2,
	}, walker, dictionary, matcher)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runner.Run(context.Background(), posts)
	if err != ErrPartialResult {
		t.Fatalf("Run() error = %v, want exact ErrPartialResult", err)
	}
	if !slices.Equal(result.Words, []aggregate.WordCount{{Word: "duck", Count: 1}}) {
		t.Fatalf("Words = %#v; limited post counts leaked", result.Words)
	}
	if result.Summary.Completed != 1 || result.Summary.Incomplete != 1 || !result.Summary.Partial {
		t.Fatalf("Summary = %#v", result.Summary)
	}
	limited := result.Outcomes[0]
	if limited.Status != OutcomeIncomplete || limited.ErrorClass != reddit.ErrorResourceLimit ||
		limited.Endpoint != "" || limited.CountedTokens != 0 {
		t.Fatalf("limited outcome = %#v", limited)
	}
}

func TestRunDistinctWordLimitIsMaterialInStrictMode(t *testing.T) {
	t.Parallel()

	walker := walkerFunc(func(_ context.Context, _ string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		err := visit(reddit.Comment{ID: "c1", Body: "duck water"})
		return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, err
	})
	dictionary := loadDictionary(t, "duck\nwater\n")
	matcher, err := words.NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	runner, err := New(Config{
		Workers:                 1,
		FailureMode:             FailureModeStrict,
		MaxDistinctWordsPerPost: 1,
	}, walker, dictionary, matcher)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	result, err := runner.Run(context.Background(), loadPosts(t, "post1"))
	if !errors.Is(err, ErrStrictFailure) || !errors.Is(err, words.ErrDistinctWordLimit) || len(result.Words) != 0 {
		t.Fatalf("Run() result = %#v, error = %v", result, err)
	}
	if result.Outcomes[0].Status != OutcomeIncomplete || result.Outcomes[0].ErrorClass != reddit.ErrorResourceLimit {
		t.Fatalf("outcome = %#v", result.Outcomes[0])
	}
}

func TestRunEmptyCompletedPostAndNoCompletedPosts(t *testing.T) {
	t.Parallel()

	t.Run("empty complete tree is meaningful", func(t *testing.T) {
		t.Parallel()
		runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
			return reddit.WalkStats{}, nil
		}), nil)
		result, err := runner.Run(context.Background(), loadPosts(t, "empty"))
		if err != nil || result.Summary.Completed != 1 || result.Summary.Partial || result.Words == nil || len(result.Words) != 0 {
			t.Fatalf("Run() result = %#v, error = %v", result, err)
		}
		encoded, marshalErr := json.Marshal(result.Words)
		if marshalErr != nil || string(encoded) != "[]" {
			t.Fatalf("empty Words JSON = %s, error = %v", encoded, marshalErr)
		}
	})

	t.Run("all failed", func(t *testing.T) {
		t.Parallel()
		runner := newRunner(t, 2, FailureModeBestEffort, walkerFunc(func(_ context.Context, postID string, _ func(reddit.Comment) error) (reddit.WalkStats, error) {
			return reddit.WalkStats{}, adapterError(reddit.ErrorTransport, reddit.EndpointComments, postID, 0)
		}), nil)
		result, err := runner.Run(context.Background(), loadPosts(t, "p1", "p2"))
		if err != ErrNoCompletedPosts || len(result.Words) != 0 || result.Summary.Failed != 2 || !result.Summary.Partial {
			t.Fatalf("Run() result = %#v, error = %v", result, err)
		}
	})

	t.Run("zero post list", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
			calls.Add(1)
			return reddit.WalkStats{}, nil
		}), nil)
		result, err := runner.Run(context.Background(), source.PostList{})
		if err != ErrNoCompletedPosts || calls.Load() != 0 || result.Words == nil {
			t.Fatalf("Run() result = %#v, error = %v, calls = %d", result, err, calls.Load())
		}
	})
}

func TestRunValidatesLifecycleAndCancellationBeforeInputState(t *testing.T) {
	t.Parallel()

	var nilRunner *Runner
	result, err := nilRunner.Run(context.Background(), source.PostList{})
	if !errors.Is(err, ErrInvalidConfig) || result.Words == nil {
		t.Fatalf("nil Runner.Run() result = %#v, error = %v", result, err)
	}

	runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
		t.Fatal("walker was called")
		return reddit.WalkStats{}, nil
	}), nil)
	//lint:ignore SA1012 This assertion deliberately exercises the public nil-context contract.
	result, err = runner.Run(nil, source.PostList{})
	if !errors.Is(err, ErrInvalidConfig) || result.Words == nil {
		t.Fatalf("Run(nil) result = %#v, error = %v", result, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = runner.Run(ctx, source.PostList{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoCompletedPosts) || result.Words == nil {
		t.Fatalf("Run(canceled, empty) result = %#v, error = %v", result, err)
	}
}

func TestRunStrictCancelsAndJoinsSiblingsWithoutMaskingPrimary(t *testing.T) {
	t.Parallel()

	failureStarted := make(chan struct{})
	blockedStarted := make(chan struct{})
	releaseFailure := make(chan struct{})
	blockedReturned := make(chan struct{})
	var laterCalls atomic.Int32
	primary := adapterError(reddit.ErrorTransport, reddit.EndpointComments, "fail", 0)
	walker := walkerFunc(func(ctx context.Context, postID string, _ func(reddit.Comment) error) (reddit.WalkStats, error) {
		switch postID {
		case "fail":
			close(failureStarted)
			select {
			case <-releaseFailure:
				return reddit.WalkStats{}, primary
			case <-ctx.Done():
				return reddit.WalkStats{}, ctx.Err()
			}
		case "block":
			close(blockedStarted)
			<-ctx.Done()
			close(blockedReturned)
			return reddit.WalkStats{}, ctx.Err()
		default:
			laterCalls.Add(1)
			return reddit.WalkStats{}, nil
		}
	})
	runner := newRunner(t, 2, FailureModeStrict, walker, nil)

	done := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = runner.Run(context.Background(), loadPosts(t, "fail", "block", "later"))
	}()
	awaitSignal(t, failureStarted, "primary start")
	awaitSignal(t, blockedStarted, "blocked sibling start")
	close(releaseFailure)
	awaitSignal(t, done, "strict Run return")
	err := runErr
	if !errors.Is(err, ErrStrictFailure) || !errors.Is(err, primary) || errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want strict primary transport failure", err)
	}
	if len(result.Words) != 0 {
		t.Fatalf("Words = %#v, want suppressed strict result", result.Words)
	}
	awaitSignal(t, blockedReturned, "blocked sibling join")
	if laterCalls.Load() != 0 {
		t.Fatalf("later post calls = %d, want 0 after strict cancellation", laterCalls.Load())
	}
}

func TestRunStrictSuppressesPreviouslyCompletedCountsAndNeverRetries(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := make(map[string]int)
	failure := adapterError(reddit.ErrorTransport, reddit.EndpointComments, "fail", 0)
	walker := walkerFunc(func(_ context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		mu.Lock()
		calls[postID]++
		mu.Unlock()
		switch postID {
		case "complete":
			if err := visit(reddit.Comment{ID: "c1", Body: "duck duck"}); err != nil {
				return reddit.WalkStats{}, err
			}
			return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
		case "fail":
			return reddit.WalkStats{}, failure
		default:
			return reddit.WalkStats{}, nil
		}
	})
	runner := newRunner(t, 1, FailureModeStrict, walker, nil)

	result, err := runner.Run(context.Background(), loadPosts(t, "complete", "fail", "later"))
	if !errors.Is(err, ErrStrictFailure) || !errors.Is(err, failure) || len(result.Words) != 0 {
		t.Fatalf("Run() result = %#v, error = %v; want strict failure without prior counts", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["complete"] != 1 || calls["fail"] != 1 || calls["later"] != 0 {
		t.Fatalf("WalkComments calls = %#v; want complete/fail once and no later work", calls)
	}
}

func TestRoutePostResultCancelsThroughFullNormalQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		mode  FailureMode
		fatal bool
	}{
		{name: "strict post failure", mode: FailureModeStrict},
		{name: "best effort global fatal", mode: FailureModeBestEffort, fatal: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancelCause := context.WithCancelCause(context.Background())
			defer cancelCause(nil)
			results := make(chan postResult, 1)
			results <- postResult{index: 99}
			materialResults := make(chan postResult, 1)
			var materialOnce sync.Once
			cause := adapterError(reddit.ErrorTransport, reddit.EndpointComments, "p1", 0)
			processed := postResult{index: 1, err: cause, fatal: test.fatal}

			done := make(chan bool, 1)
			go func() {
				done <- routePostResult(
					ctx,
					test.mode,
					processed,
					results,
					materialResults,
					&materialOnce,
					cancelCause,
				)
			}()

			select {
			case continued := <-done:
				if continued {
					t.Fatal("routePostResult() continued after a material failure")
				}
			case <-time.After(testTimeout):
				// Release a buggy implementation that waited on the deliberately full
				// ordinary queue before failing the test, avoiding a leaked goroutine.
				<-results
				<-done
				t.Fatal("material failure was blocked by the ordinary result queue")
			}
			if !errors.Is(context.Cause(ctx), cause) {
				t.Fatalf("context cause = %v, want exact material cause", context.Cause(ctx))
			}
			select {
			case got := <-materialResults:
				if got.index != processed.index || !errors.Is(got.err, cause) {
					t.Fatalf("material result = %#v, want exact processed result", got)
				}
			default:
				t.Fatal("material result was not preserved out of band")
			}
		})
	}
}

func TestRoutePostResultUnblocksOrdinaryResultOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan postResult, 1)
	results <- postResult{index: 99}
	materialResults := make(chan postResult, 1)
	var materialOnce sync.Once
	done := make(chan bool, 1)
	go func() {
		done <- routePostResult(
			ctx,
			FailureModeBestEffort,
			postResult{index: 1},
			results,
			materialResults,
			&materialOnce,
			func(error) {},
		)
	}()
	cancel()
	if continued := awaitValue(t, done, "ordinary result cancellation"); continued {
		t.Fatal("routePostResult() continued after cancellation")
	}
}

func TestRunCallerCancellationWinsAndJoinsWorker(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	returned := make(chan struct{})
	walker := walkerFunc(func(walkCtx context.Context, _ string, _ func(reddit.Comment) error) (reddit.WalkStats, error) {
		close(started)
		<-walkCtx.Done()
		close(returned)
		return reddit.WalkStats{}, walkCtx.Err()
	})
	runner := newRunner(t, 1, FailureModeBestEffort, walker, nil)

	done := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = runner.Run(ctx, loadPosts(t, "p1", "p2"))
	}()
	awaitSignal(t, started, "worker start")
	cancel()
	awaitSignal(t, done, "Run return")
	awaitSignal(t, returned, "worker return")
	if !errors.Is(runErr, context.Canceled) || len(result.Words) != 0 {
		t.Fatalf("Run() result = %#v, error = %v", result, runErr)
	}
}

func TestRunPreCanceledContextDoesNotStartWalker(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	runner := newRunner(t, 2, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
		calls.Add(1)
		return reddit.WalkStats{}, nil
	}), nil)

	result, err := runner.Run(ctx, loadPosts(t, "p1", "p2"))
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 || result.Summary.Failed != 2 {
		t.Fatalf("Run() result = %#v, error = %v, calls = %d", result, err, calls.Load())
	}
}

func TestRunTreatsIgnoredVisitorCancellationAsFatal(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	visitorErrors := make(chan error, 1)
	runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(_ context.Context, _ string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		cancel()
		visitorErrors <- visit(reddit.Comment{ID: "c1", Body: "duck"})
		// Deliberately violate CommentWalker's contract to prove that Runner still
		// promotes an ignored visitor error and discards the local map.
		return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
	}), nil)

	result, err := runner.Run(ctx, loadPosts(t, "p1"))
	visitorErr := awaitValue(t, visitorErrors, "visitor cancellation")
	if !errors.Is(visitorErr, context.Canceled) || !errors.Is(err, context.Canceled) || len(result.Words) != 0 {
		t.Fatalf("Run() result = %#v, error = %v, visitor error = %v", result, err, visitorErr)
	}
}

func TestRunWorkerBoundAndExactlyOnce(t *testing.T) {
	t.Parallel()

	const workers = 3
	const postCount = 24
	release := make(chan struct{})
	started := make(chan struct{}, postCount)
	var active atomic.Int32
	var peak atomic.Int32
	var mu sync.Mutex
	calls := make(map[string]int, postCount)
	walker := walkerFunc(func(ctx context.Context, postID string, _ func(reddit.Comment) error) (reddit.WalkStats, error) {
		current := active.Add(1)
		defer active.Add(-1)
		updatePeak(&peak, current)
		mu.Lock()
		calls[postID]++
		mu.Unlock()
		started <- struct{}{}
		select {
		case <-release:
			return reddit.WalkStats{}, nil
		case <-ctx.Done():
			return reddit.WalkStats{}, ctx.Err()
		}
	})
	runner := newRunner(t, workers, FailureModeBestEffort, walker, nil)
	posts := generatedPosts(t, postCount, false)

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = runner.Run(context.Background(), posts)
	}()
	for range workers {
		awaitSignal(t, started, "initial worker start")
	}
	close(release)
	awaitSignal(t, done, "bounded run")
	if runErr != nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if got := peak.Load(); got > workers || got < 1 {
		t.Fatalf("peak active walkers = %d, want 1..%d", got, workers)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != postCount {
		t.Fatalf("called posts = %d, want %d", len(calls), postCount)
	}
	for postID, count := range calls {
		if count != 1 {
			t.Fatalf("post %q calls = %d, want 1", postID, count)
		}
	}
}

func TestRunOneWorkerAndManyWorkersAreByteIdentical(t *testing.T) {
	t.Parallel()

	posts := generatedPosts(t, 20, false)
	walker := deterministicWalker()
	var baseline []byte
	for _, workers := range []int{1, 2, 8} {
		runner := newRunner(t, workers, FailureModeBestEffort, walker, []string{"duck*", "water", "pond"})
		result, err := runner.Run(context.Background(), posts)
		if err != nil {
			t.Fatalf("Run(workers=%d) error = %v", workers, err)
		}
		encoded, marshalErr := json.MarshalIndent(result.Words, "", "  ")
		if marshalErr != nil {
			t.Fatalf("MarshalIndent() error = %v", marshalErr)
		}
		if baseline == nil {
			baseline = encoded
		} else if !slices.Equal(encoded, baseline) {
			t.Fatalf("workers=%d JSON differs:\n%s\nwant:\n%s", workers, encoded, baseline)
		}
		for index, outcome := range result.Outcomes {
			wantPost, _ := posts.At(index)
			if outcome.PostID != wantPost.ID {
				t.Fatalf("workers=%d outcome[%d] = %q, want %q", workers, index, outcome.PostID, wantPost.ID)
			}
		}
	}
}

func TestRunCompletionOrderDoesNotReorderOutcomes(t *testing.T) {
	t.Parallel()

	posts := loadPosts(t, "p1", "p2", "p3", "s3", "s2", "s1")
	primaryStarted := make(chan string, 3)
	sentinelStarted := make(chan string, 3)
	releases := map[string]chan struct{}{
		"p1": make(chan struct{}),
		"p2": make(chan struct{}),
		"p3": make(chan struct{}),
	}
	releaseSentinels := make(chan struct{})
	walker := walkerFunc(func(ctx context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		if strings.HasPrefix(postID, "p") {
			primaryStarted <- postID
			select {
			case <-releases[postID]:
			case <-ctx.Done():
				return reddit.WalkStats{}, ctx.Err()
			}
		} else {
			sentinelStarted <- postID
			select {
			case <-releaseSentinels:
			case <-ctx.Done():
				return reddit.WalkStats{}, ctx.Err()
			}
		}
		if err := visit(reddit.Comment{ID: postID, Body: "duck"}); err != nil {
			return reddit.WalkStats{}, err
		}
		return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
	})
	runner := newRunner(t, 3, FailureModeBestEffort, walker, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = runner.Run(ctx, posts)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Error("Run did not join after completion-order test cleanup")
		}
	})
	seen := make(map[string]bool)
	for range 3 {
		seen[awaitValue(t, primaryStarted, "primary post start")] = true
	}
	if len(seen) != 3 {
		t.Fatalf("started posts = %#v", seen)
	}
	// A sentinel can start only after the same worker has published its preceding
	// primary result and requested another job. Keeping each sentinel blocked proves
	// the primary results were routed in p3, p2, p1 order.
	close(releases["p3"])
	if got := awaitValue(t, sentinelStarted, "p3 result publication"); got != "s3" {
		t.Fatalf("first sentinel = %q, want s3", got)
	}
	close(releases["p2"])
	if got := awaitValue(t, sentinelStarted, "p2 result publication"); got != "s2" {
		t.Fatalf("second sentinel = %q, want s2", got)
	}
	close(releases["p1"])
	if got := awaitValue(t, sentinelStarted, "p1 result publication"); got != "s1" {
		t.Fatalf("third sentinel = %q, want s1", got)
	}
	close(releaseSentinels)
	awaitSignal(t, done, "ordered outcome run")
	if runErr != nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	gotIDs := make([]string, len(result.Outcomes))
	for index, outcome := range result.Outcomes {
		gotIDs[index] = outcome.PostID
	}
	if !slices.Equal(gotIDs, []string{"p1", "p2", "p3", "s3", "s2", "s1"}) {
		t.Fatalf("outcome order = %q", gotIDs)
	}
}

func TestRunInputPermutationDoesNotChangeRankedWords(t *testing.T) {
	t.Parallel()

	runner := newRunner(t, 3, FailureModeBestEffort, deterministicWalker(), nil)
	left, err := runner.Run(context.Background(), generatedPosts(t, 12, false))
	if err != nil {
		t.Fatalf("left Run() error = %v", err)
	}
	right, err := runner.Run(context.Background(), generatedPosts(t, 12, true))
	if err != nil {
		t.Fatalf("right Run() error = %v", err)
	}
	if !slices.Equal(left.Words, right.Words) {
		t.Fatalf("permuted Words differ:\nleft %#v\nright %#v", left.Words, right.Words)
	}
}

func TestRunUnknownFailureIsSanitizedAndUnwraps(t *testing.T) {
	t.Parallel()

	const secret = "planted-secret-and-comment-content"
	cause := errors.New(secret)
	runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
		return reddit.WalkStats{}, cause
	}), nil)

	result, err := runner.Run(context.Background(), loadPosts(t, "safeid"))
	if err == nil || err == ErrPartialResult || !errors.Is(err, cause) {
		t.Fatalf("Run() result = %#v, error = %v", result, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Run() error leaked dependency text: %q", err)
	}
	if len(result.Words) != 0 {
		t.Fatalf("Words = %#v, want suppressed", result.Words)
	}
}

func TestRunRejectsForgedAdapterMetadataWithoutLeakingIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		class     reddit.ErrorClass
		point     reddit.Endpoint
		status    int
		canary    string
		wantClass reddit.ErrorClass
		wantPoint reddit.Endpoint
	}{
		{name: "unknown class", class: "class-canary\ncontent", point: reddit.EndpointComments, status: 503, canary: "class-canary"},
		{name: "unknown endpoint", class: reddit.ErrorTransport, point: "endpoint-canary\ncontent", status: 503, canary: "endpoint-canary", wantClass: reddit.ErrorTransport},
		{name: "invalid status", class: reddit.ErrorTransport, point: reddit.EndpointComments, status: 1000, canary: "1000", wantClass: reddit.ErrorTransport, wantPoint: reddit.EndpointComments},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			forged := &reddit.Error{Class: test.class, Endpoint: test.point, StatusCode: test.status}
			runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
				return reddit.WalkStats{}, forged
			}), nil)

			result, err := runner.Run(context.Background(), loadPosts(t, "safeid"))
			if err == nil || err == ErrPartialResult || !errors.Is(err, forged) || len(result.Words) != 0 {
				t.Fatalf("Run() result = %#v, error = %v; want fatal sanitized failure", result, err)
			}
			encoded, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.Fatalf("json.Marshal() error = %v", marshalErr)
			}
			if strings.Contains(err.Error(), test.canary) || strings.Contains(string(encoded), test.canary) {
				t.Fatalf("forged metadata leaked canary %q: error=%q result=%s", test.canary, err, encoded)
			}
			if result.Outcomes[0].ErrorClass != test.wantClass || result.Outcomes[0].Endpoint != test.wantPoint || result.Outcomes[0].HTTPStatus != 0 {
				t.Fatalf("forged endpoint/status survived sanitization: %#v", result.Outcomes[0])
			}
		})
	}
}

func TestRunRejectsInvalidWalkerStatistics(t *testing.T) {
	t.Parallel()

	runner := newRunner(t, 1, FailureModeBestEffort, walkerFunc(func(context.Context, string, func(reddit.Comment) error) (reddit.WalkStats, error) {
		return reddit.WalkStats{Comments: -1}, nil
	}), nil)
	result, err := runner.Run(context.Background(), loadPosts(t, "p1"))
	if !errors.Is(err, errInvalidWalkStats) || len(result.Words) != 0 {
		t.Fatalf("Run() result = %#v, error = %v", result, err)
	}
}

func TestSummarizeRejectsOverflow(t *testing.T) {
	t.Parallel()

	_, err := summarize([]PostOutcome{
		{Status: OutcomeCompleted, CountedTokens: ^uint64(0)},
		{Status: OutcomeCompleted, CountedTokens: 1},
	}, 1)
	if !errors.Is(err, errSummaryOverflow) {
		t.Fatalf("summarize() error = %v, want errSummaryOverflow", err)
	}
}

func BenchmarkRun(b *testing.B) {
	posts := generatedPosts(b, 200, false)
	walker := deterministicWalker()
	for _, workers := range []int{1, DefaultWorkers, 8} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			runner := newRunner(b, workers, FailureModeBestEffort, walker, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, err := runner.Run(context.Background(), posts)
				if err != nil || result.Summary.Completed != 200 {
					b.Fatalf("Run() completed = %d, error = %v", result.Summary.Completed, err)
				}
			}
		})
	}
}

func deterministicWalker() CommentWalker {
	return walkerFunc(func(ctx context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
		for index, body := range []string{"duck water pond", "duckling duck water"} {
			if err := ctx.Err(); err != nil {
				return reddit.WalkStats{}, err
			}
			if err := visit(reddit.Comment{ID: fmt.Sprintf("%s_%d", postID, index), Body: body}); err != nil {
				return reddit.WalkStats{}, err
			}
		}
		return reddit.WalkStats{Comments: 2, BodiesVisited: 2}, nil
	})
}

func newRunner(
	tb testing.TB,
	workers int,
	mode FailureMode,
	walker CommentWalker,
	patterns []string,
) *Runner {
	tb.Helper()
	dictionary := loadDictionary(tb, "duck\nduckling\nwater\npond\nquack\n")
	matcher, err := words.NewMatcher(patterns)
	if err != nil {
		tb.Fatalf("NewMatcher() error = %v", err)
	}
	runner, err := New(Config{Workers: workers, FailureMode: mode}, walker, dictionary, matcher)
	if err != nil {
		tb.Fatalf("New() error = %v", err)
	}
	return runner
}

func loadDictionary(tb testing.TB, contents string) words.Dictionary {
	tb.Helper()
	dictionary, _, err := words.LoadDictionary(strings.NewReader(contents), words.DefaultDictionaryLimits())
	if err != nil {
		tb.Fatalf("LoadDictionary() error = %v", err)
	}
	return dictionary
}

func loadPosts(tb testing.TB, ids ...string) source.PostList {
	tb.Helper()
	var input strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&input, "https://www.reddit.com/comments/%s\n", id)
	}
	posts, _, err := source.LoadPostList(strings.NewReader(input.String()), source.DefaultPostListLimits())
	if err != nil {
		tb.Fatalf("LoadPostList() error = %v", err)
	}
	return posts
}

func generatedPosts(tb testing.TB, count int, reverse bool) source.PostList {
	tb.Helper()
	ids := make([]string, count)
	for index := range count {
		ids[index] = "p" + strconv.FormatInt(int64(index+1), 36)
	}
	if reverse {
		slices.Reverse(ids)
	}
	return loadPosts(tb, ids...)
}

func adapterError(class reddit.ErrorClass, endpoint reddit.Endpoint, postID string, status int) *reddit.Error {
	return &reddit.Error{Class: class, Endpoint: endpoint, PostID: postID, StatusCode: status}
}

func updatePeak(peak *atomic.Int32, current int32) {
	for {
		observed := peak.Load()
		if current <= observed || peak.CompareAndSwap(observed, current) {
			return
		}
	}
}

func awaitSignal(tb testing.TB, signal <-chan struct{}, label string) {
	tb.Helper()
	select {
	case <-signal:
	case <-time.After(testTimeout):
		tb.Fatalf("timed out waiting for %s", label)
	}
}

func awaitValue[T any](tb testing.TB, values <-chan T, label string) T {
	tb.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(testTimeout):
		tb.Fatalf("timed out waiting for %s", label)
		var zero T
		return zero
	}
}

// TestRunSkipsAbsentPostsWithoutPartialResult covers the assignment post list, which
// deliberately contains deleted threads. A post that provably does not exist cannot
// be counted under any policy, so it is reconciled as skipped rather than reported
// as a failure that makes an otherwise complete run partial.
func TestRunSkipsAbsentPostsWithoutPartialResult(t *testing.T) {
	t.Parallel()

	for _, mode := range []FailureMode{FailureModeBestEffort, FailureModeStrict} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			posts := loadPosts(t, "gone", "good")
			walker := walkerFunc(func(_ context.Context, postID string, visit func(reddit.Comment) error) (reddit.WalkStats, error) {
				if postID == "gone" {
					return reddit.WalkStats{}, adapterError(reddit.ErrorNotFound, reddit.EndpointComments, postID, 404)
				}
				if err := visit(reddit.Comment{ID: "c1", Body: "duck duck water"}); err != nil {
					return reddit.WalkStats{}, err
				}
				return reddit.WalkStats{Comments: 1, BodiesVisited: 1}, nil
			})

			result, err := runnerRun(t, mode, walker, posts)
			if err != nil {
				t.Fatalf("Run() error = %v, want nil; an absent post must not fail the run", err)
			}
			if result.Summary.Partial {
				t.Fatalf("Summary.Partial = true, want false; summary = %+v", result.Summary)
			}
			if result.Summary.Skipped != 1 || result.Summary.Completed != 1 || result.Summary.Failed != 0 {
				t.Fatalf("summary = %+v, want one skipped and one completed", result.Summary)
			}
			if result.Outcomes[0].Status != OutcomeSkipped || result.Outcomes[0].ErrorClass != reddit.ErrorNotFound {
				t.Fatalf("absent-post outcome = %#v, want skipped/not_found", result.Outcomes[0])
			}
			if result.Outcomes[0].CountedTokens != 0 {
				t.Fatalf("skipped post contributed %d tokens", result.Outcomes[0].CountedTokens)
			}
			// The surviving post still produces the complete ranked result.
			want := []aggregate.WordCount{{Word: "duck", Count: 2}, {Word: "water", Count: 1}}
			if !slices.Equal(result.Words, want) {
				t.Fatalf("Words = %#v, want %#v", result.Words, want)
			}
		})
	}
}

func runnerRun(tb testing.TB, mode FailureMode, walker CommentWalker, posts source.PostList) (Result, error) {
	tb.Helper()
	return newRunner(tb, 2, mode, walker, nil).Run(context.Background(), posts)
}
