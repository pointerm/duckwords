package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/source"
	"github.com/pointerm/duckwords/internal/words"
)

type indexedPost struct {
	index int
	post  source.Post
}

type postResult struct {
	index   int
	outcome PostOutcome
	counts  map[string]uint64
	err     error
	fatal   bool
}

// Run processes each unique post through a fixed worker pool. The calling goroutine
// is the only owner of the global count map and merges only results whose complete
// WalkComments call succeeded.
func (runner *Runner) Run(ctx context.Context, posts source.PostList) (Result, error) {
	if runner == nil || runner.walker == nil {
		return emptyResult(nil), fmt.Errorf("%w: initialized runner is required", ErrInvalidConfig)
	}
	if ctx == nil {
		return emptyResult(posts.Posts()), fmt.Errorf("%w: context is required", ErrInvalidConfig)
	}

	input := posts.Posts()
	result := emptyResult(input)
	if err := ctx.Err(); err != nil {
		markMissingCanceled(result.Outcomes, make([]bool, len(input)))
		result.Summary, _ = summarize(result.Outcomes, 0)
		return result, err
	}
	if len(input) == 0 {
		return result, ErrNoCompletedPosts
	}

	// cancelCause distinguishes application policy cancellation from the parent
	// context. Derived cancellations stop siblings but never replace their primary
	// material failure.
	runCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	group, groupCtx := errgroup.WithContext(runCtx)

	// jobs is bounded by worker count; results and materialResults each have capacity
	// one. The producer owns jobs, while the tracked closer owns both result channels
	// after every worker has joined. The runner owns exactly Workers+2 orchestration
	// goroutines here: the fixed workers, one producer, and one closer.
	jobs := make(chan indexedPost, runner.config.Workers)
	results := make(chan postResult, 1)
	materialResults := make(chan postResult, 1)
	var materialOnce sync.Once

	group.Go(func() error {
		defer close(jobs)
		for index, post := range input {
			select {
			case jobs <- indexedPost{index: index, post: post}:
			case <-groupCtx.Done():
				return nil
			}
		}
		return nil
	})

	var workers sync.WaitGroup
	workers.Add(runner.config.Workers)
	for range runner.config.Workers {
		group.Go(func() error {
			defer workers.Done()
			for {
				select {
				case <-groupCtx.Done():
					return nil
				case job, ok := <-jobs:
					if !ok {
						return nil
					}
					if groupCtx.Err() != nil {
						return nil
					}

					processed := runner.processPost(groupCtx, job)
					if !routePostResult(
						groupCtx,
						runner.config.FailureMode,
						processed,
						results,
						materialResults,
						&materialOnce,
						cancelCause,
					) {
						return nil
					}
				}
			}
		})
	}
	group.Go(func() error {
		workers.Wait()
		close(results)
		close(materialResults)
		return nil
	})

	globalCounts := make(map[string]uint64)
	received := make([]bool, len(input))
	var primary error

	for results != nil || materialResults != nil {
		var processed postResult
		var ok bool
		material := false
		select {
		case processed, ok = <-results:
			if !ok {
				results = nil
				continue
			}
		case processed, ok = <-materialResults:
			if !ok {
				materialResults = nil
				continue
			}
			material = true
		}

		result.Outcomes[processed.index] = processed.outcome
		received[processed.index] = true
		if material {
			if primary == nil {
				kind := error(nil)
				if !processed.fatal {
					kind = ErrStrictFailure
				}
				primary = newRunError(kind, processed.outcome.PostID, processed.outcome.ErrorClass, processed.err)
			}
			continue
		}

		if processed.err == nil && primary == nil {
			// A later strict/fatal failure invalidates the whole returned Result, so
			// merging an earlier complete map is internal bookkeeping rather than a
			// user-visible partial commit.
			if err := aggregate.Merge(globalCounts, processed.counts); err != nil {
				primary = &pipelineError{cause: err}
				cancelCause(primary)
			}
		}
	}

	// Wait even after policy cancellation. This is the join boundary that guarantees
	// no producer or worker can retain references to post-local state after Run.
	if err := group.Wait(); err != nil && primary == nil {
		primary = &pipelineError{cause: err}
	}

	for index := range received {
		if received[index] {
			continue
		}
		if ctx.Err() == nil && primary == nil {
			primary = &pipelineError{cause: errMissingOutcome}
		}
		result.Outcomes[index].Status = OutcomeFailed
		result.Outcomes[index].ErrorClass = reddit.ErrorCanceled
	}
	var summaryErr error
	result.Summary, summaryErr = summarize(result.Outcomes, len(globalCounts))
	if summaryErr != nil && primary == nil {
		primary = &pipelineError{cause: summaryErr}
	}

	// The caller's cancellation always wins over derived sibling cancellation and
	// strict policy, matching the lifecycle owner that initiated shutdown.
	if err := ctx.Err(); err != nil {
		result.Words = []aggregate.WordCount{}
		return result, err
	}
	if primary != nil {
		result.Words = []aggregate.WordCount{}
		return result, primary
	}

	ranked, err := aggregate.TopN(globalCounts, resultWordLimit)
	if err != nil {
		return result, &pipelineError{cause: err}
	}
	result.Words = ranked
	if result.Summary.Completed == 0 {
		return result, ErrNoCompletedPosts
	}
	if result.Summary.Partial {
		return result, ErrPartialResult
	}
	return result, nil
}

// routePostResult keeps the ordinary result queue from delaying policy
// cancellation. The first strict or globally fatal result owns the capacity-one
// material channel: it cancels siblings at detection time and then publishes its
// exact outcome without a cancellation-select that could discard the primary cause.
func routePostResult(
	ctx context.Context,
	mode FailureMode,
	processed postResult,
	results chan<- postResult,
	materialResults chan<- postResult,
	materialOnce *sync.Once,
	cancelCause context.CancelCauseFunc,
) bool {
	if processed.err != nil && (processed.fatal || mode == FailureModeStrict) {
		materialOnce.Do(func() {
			cancelCause(processed.err)
			materialResults <- processed
		})
		return false
	}
	select {
	case results <- processed:
		return true
	case <-ctx.Done():
		return false
	}
}

func (runner *Runner) processPost(ctx context.Context, job indexedPost) postResult {
	counts := make(map[string]uint64)
	var countedTokens uint64
	var visitorFailure error

	stats, err := runner.walker.WalkComments(ctx, job.post.ID, func(comment reddit.Comment) error {
		if visitorFailure != nil {
			return visitorFailure
		}
		if contextErr := ctx.Err(); contextErr != nil {
			visitorFailure = contextErr
			return contextErr
		}
		countStats, countErr := runner.counter.AddTextLimited(comment.Body, counts, runner.maxDistinctWords)
		if countStats.CountedTokens > 0 {
			increment := uint64(countStats.CountedTokens)
			if math.MaxUint64-countedTokens < increment {
				visitorFailure = words.ErrCountOverflow
				return visitorFailure
			}
			countedTokens += increment
		}
		if countErr != nil {
			visitorFailure = countErr
		}
		return countErr
	})
	if err == nil && visitorFailure != nil {
		// Preserve a callback error even if a walker violates the stop/return contract.
		// Classification below distinguishes a bounded count-state exhaustion from an
		// arbitrary fatal visitor failure; either way the mutated local map is discarded.
		err = visitorFailure
	}

	outcome := outcomeFrom(job.post, stats, countedTokens)
	if !validWalkStats(stats) {
		outcome = outcomeFrom(job.post, reddit.WalkStats{}, 0)
		outcome.Status = OutcomeFailed
		return postResult{index: job.index, outcome: outcome, err: errInvalidWalkStats, fatal: true}
	}
	if err == nil {
		outcome.Status = OutcomeCompleted
		return postResult{index: job.index, outcome: outcome, counts: counts}
	}

	// Only classify a callback failure as a visitor failure when the walker returned
	// that same cause. A walker may observe policy cancellation in the callback while
	// independently returning the material protocol error that triggered its exit.
	effectiveVisitorFailure := visitorFailure
	if effectiveVisitorFailure != nil && !errors.Is(err, effectiveVisitorFailure) {
		effectiveVisitorFailure = nil
	}
	status, fatal, class, endpoint, httpStatus := classifyPostError(err, effectiveVisitorFailure)
	outcome.Status = status
	outcome.ErrorClass = class
	outcome.Endpoint = endpoint
	outcome.HTTPStatus = httpStatus
	// Counts from an unproven tree have no result meaning. Traversal work remains
	// available for diagnostics, but token counts are deliberately reported as zero.
	outcome.CountedTokens = 0
	return postResult{index: job.index, outcome: outcome, err: err, fatal: fatal}
}

func classifyPostError(err, visitorFailure error) (OutcomeStatus, bool, reddit.ErrorClass, reddit.Endpoint, int) {
	if errors.Is(visitorFailure, context.Canceled) || errors.Is(visitorFailure, context.DeadlineExceeded) {
		return OutcomeFailed, true, reddit.ErrorCanceled, "", 0
	}
	if errors.Is(visitorFailure, words.ErrDistinctWordLimit) {
		// The comment tree may be complete, but the bounded post-local count state is
		// not. Best-effort mode may continue with other posts while strict mode promotes
		// this recoverable incomplete outcome through its normal material path.
		return OutcomeIncomplete, false, reddit.ErrorResourceLimit, "", 0
	}
	if visitorFailure != nil {
		return OutcomeFailed, true, reddit.ErrorVisitor, "", 0
	}

	var adapterErr *reddit.Error
	if errors.As(err, &adapterErr) {
		class, endpoint, statusCode, valid := sanitizedAdapterMetadata(adapterErr)
		if !valid {
			return OutcomeFailed, true, class, endpoint, statusCode
		}
		switch adapterErr.Class {
		case reddit.ErrorIncomplete, reddit.ErrorResourceLimit:
			return OutcomeIncomplete, false, class, endpoint, statusCode
		case reddit.ErrorForbidden, reddit.ErrorNotFound, reddit.ErrorRateLimited,
			reddit.ErrorServer, reddit.ErrorTransport, reddit.ErrorProtocol:
			return OutcomeFailed, false, class, endpoint, statusCode
		case reddit.ErrorAuthentication, reddit.ErrorInvalidInput, reddit.ErrorVisitor,
			reddit.ErrorCanceled:
			return OutcomeFailed, true, class, endpoint, statusCode
		default:
			return OutcomeFailed, true, "", "", 0
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return OutcomeFailed, true, reddit.ErrorCanceled, "", 0
	}
	return OutcomeFailed, true, "", "", 0
}

func sanitizedAdapterMetadata(adapterErr *reddit.Error) (reddit.ErrorClass, reddit.Endpoint, int, bool) {
	if adapterErr == nil || !knownErrorClass(adapterErr.Class) {
		return "", "", 0, false
	}
	if !knownEndpoint(adapterErr.Endpoint) {
		return adapterErr.Class, "", 0, false
	}
	if adapterErr.StatusCode != 0 && (adapterErr.StatusCode < 100 || adapterErr.StatusCode > 599) {
		return adapterErr.Class, adapterErr.Endpoint, 0, false
	}
	return adapterErr.Class, adapterErr.Endpoint, adapterErr.StatusCode, true
}

func knownErrorClass(class reddit.ErrorClass) bool {
	switch class {
	case reddit.ErrorInvalidInput, reddit.ErrorAuthentication, reddit.ErrorForbidden,
		reddit.ErrorNotFound, reddit.ErrorRateLimited, reddit.ErrorServer,
		reddit.ErrorTransport, reddit.ErrorProtocol, reddit.ErrorIncomplete,
		reddit.ErrorResourceLimit, reddit.ErrorCanceled, reddit.ErrorVisitor:
		return true
	default:
		return false
	}
}

func knownEndpoint(endpoint reddit.Endpoint) bool {
	switch endpoint {
	case reddit.EndpointOAuthToken, reddit.EndpointComments,
		reddit.EndpointContinuation, reddit.EndpointMoreChildren:
		return true
	default:
		return false
	}
}

func outcomeFrom(post source.Post, stats reddit.WalkStats, countedTokens uint64) PostOutcome {
	return PostOutcome{
		PostID:               post.ID,
		SourceLine:           post.SourceLine,
		Comments:             stats.Comments,
		BodiesVisited:        stats.BodiesVisited,
		MoreRequests:         stats.MoreRequests,
		ContinuationRequests: stats.ContinuationRequests,
		CountedTokens:        countedTokens,
	}
}

func emptyResult(posts []source.Post) Result {
	outcomes := make([]PostOutcome, len(posts))
	for index, post := range posts {
		outcomes[index] = PostOutcome{PostID: post.ID, SourceLine: post.SourceLine}
	}
	return Result{
		Words:    []aggregate.WordCount{},
		Summary:  Summary{Total: len(posts)},
		Outcomes: outcomes,
	}
}

func markMissingCanceled(outcomes []PostOutcome, received []bool) {
	for index := range outcomes {
		if !received[index] {
			outcomes[index].Status = OutcomeFailed
			outcomes[index].ErrorClass = reddit.ErrorCanceled
		}
	}
}

func summarize(outcomes []PostOutcome, distinctWords int) (Summary, error) {
	summary := Summary{Total: len(outcomes), DistinctWords: distinctWords}
	for _, outcome := range outcomes {
		switch outcome.Status {
		case OutcomeCompleted:
			summary.Completed++
			if !addSummaryValue(&summary.Comments, uint64(outcome.Comments)) ||
				!addSummaryValue(&summary.BodiesVisited, uint64(outcome.BodiesVisited)) ||
				!addSummaryValue(&summary.MoreRequests, uint64(outcome.MoreRequests)) ||
				!addSummaryValue(&summary.ContinuationRequests, uint64(outcome.ContinuationRequests)) ||
				!addSummaryValue(&summary.CountedTokens, outcome.CountedTokens) {
				return Summary{}, errSummaryOverflow
			}
		case OutcomeSkipped:
			summary.Skipped++
		case OutcomeIncomplete:
			summary.Incomplete++
		default:
			summary.Failed++
		}
	}
	if summary.Completed+summary.Skipped+summary.Failed+summary.Incomplete != summary.Total {
		return Summary{}, errMissingOutcome
	}
	summary.Partial = summary.Completed != summary.Total
	return summary, nil
}

func addSummaryValue(total *uint64, value uint64) bool {
	if math.MaxUint64-*total < value {
		return false
	}
	*total += value
	return true
}

func validWalkStats(stats reddit.WalkStats) bool {
	return stats.Things >= 0 && stats.Comments >= 0 && stats.BodiesVisited >= 0 &&
		stats.BodiesSkipped >= 0 && stats.DuplicateComments >= 0 && stats.MoreIDs >= 0 &&
		stats.UniqueMoreIDs >= 0 && stats.DuplicateMoreIDs >= 0 && stats.MoreRequests >= 0 &&
		stats.ContinuationRequests >= 0 && stats.BodyBytes >= 0 && stats.ResponseBytes >= 0
}
