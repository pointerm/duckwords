package reddit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	moreChildrenBatchSize = 100
	maxPostIDBytes        = 16
	maxCommentIDBytes     = 32

	defaultMaxThings               = 500_000
	defaultMaxComments             = 250_000
	defaultMaxMoreIDs              = 250_000
	defaultMaxMoreRequests         = 5_000
	defaultMaxContinuationRequests = 5_000
	defaultMaxBodyBytes            = 1 << 20
	defaultMaxTotalBodyBytes       = 256 << 20
	defaultMaxTotalResponseBytes   = 256 << 20

	absoluteMaxThings               = 2_000_000
	absoluteMaxComments             = 1_000_000
	absoluteMaxMoreIDs              = 1_000_000
	absoluteMaxMoreRequests         = 20_000
	absoluteMaxContinuationRequests = 20_000
	absoluteMaxBodyBytes            = 16 << 20
	absoluteMaxTotalBodyBytes       = 1 << 30
	absoluteMaxTotalResponseBytes   = 1 << 30

	// encoding/json itself rejects deeper inputs, but an explicit lower bound keeps
	// adversarial wire nesting away from the decoder's implementation ceiling while
	// retaining far more depth than Reddit normally returns before a continuation.
	maxJSONNestingDepth = 8_192
	// Protocol arrays are budgeted separately. This allowance is only for small
	// auxiliary metadata arrays which DuckWords never consumes.
	maxAuxiliaryArrayElements = 4_096
	// Reddit protocol objects contain far fewer fields. Independent per-object and
	// per-response caps prevent one wide map, or many metadata maps outside recognized
	// thing arrays, from turning a bounded wire payload into unbounded decoder state.
	maxObjectFields            = 256
	maxObjectFieldsPerResponse = 262_144
	// Preallocation is an optimization, not permission to allocate in proportion to
	// untrusted collections before their nodes pass the retained-state budget.
	maxTraversalPreallocation = 4_096
)

var (
	errInvalidThingLimits       = errors.New("invalid reddit thing limits")
	errInvalidPostID            = errors.New("invalid reddit post ID")
	errNilMoreFetcher           = errors.New("nil morechildren fetcher")
	errNilCommentVisitor        = errors.New("nil comment visitor")
	errMalformedResponse        = errors.New("malformed reddit response")
	errPostNotFound             = errors.New("reddit post does not exist")
	errIncompleteTree           = errors.New("incomplete reddit comment tree")
	errThingLimit               = errors.New("reddit thing limit exceeded")
	errCommentLimit             = errors.New("reddit comment limit exceeded")
	errMoreIDLimit              = errors.New("reddit more-child ID limit exceeded")
	errMoreRequestLimit         = errors.New("reddit morechildren request limit exceeded")
	errContinuationRequestLimit = errors.New("reddit continuation request limit exceeded")
	errCommentBodyTooLarge      = errors.New("reddit comment body limit exceeded")
	errCommentBodiesTooLarge    = errors.New("reddit cumulative comment-body limit exceeded")
	errResponsesTooLarge        = errors.New("reddit cumulative response limit exceeded")
)

// ThingLimits bounds all work and state retained while traversing one post.
// MaxThings counts decoded post, comment, and continuation things; MaxComments and
// MaxMoreIDs count occurrences before deduplication so duplicates cannot bypass a
// resource budget. MaxBodyBytes applies to each decoded body independently;
// MaxTotalBodyBytes applies to every decoded body occurrence, including duplicates.
// MaxTotalResponseBytes covers the initial response and every successfully fetched
// expansion payload; failed HTTP attempts are already bounded by the client layer.
type ThingLimits struct {
	MaxThings               int
	MaxComments             int
	MaxMoreIDs              int
	MaxMoreRequests         int
	MaxContinuationRequests int
	MaxBodyBytes            int
	MaxTotalBodyBytes       int64
	MaxTotalResponseBytes   int64
}

// DefaultThingLimits returns limits well above a normal Reddit comment tree while
// still preventing one post from consuming unbounded memory or API requests.
func DefaultThingLimits() ThingLimits {
	return ThingLimits{
		MaxThings:               defaultMaxThings,
		MaxComments:             defaultMaxComments,
		MaxMoreIDs:              defaultMaxMoreIDs,
		MaxMoreRequests:         defaultMaxMoreRequests,
		MaxContinuationRequests: defaultMaxContinuationRequests,
		MaxBodyBytes:            defaultMaxBodyBytes,
		MaxTotalBodyBytes:       defaultMaxTotalBodyBytes,
		MaxTotalResponseBytes:   defaultMaxTotalResponseBytes,
	}
}

// Comment is the minimal user-content record passed to a streaming visitor.
// Fullname is validated to be exactly "t1_" followed by ID.
type Comment struct {
	ID       string
	Fullname string
	Body     string
}

// WalkStats records bounded traversal work without retaining comment content.
// Comments is the number of unique comment IDs, while DuplicateComments records
// additional t1 occurrences. MoreIDs counts all child-ID occurrences and
// UniqueMoreIDs counts the first occurrence of each unresolved ID.
type WalkStats struct {
	Things               int
	Comments             int
	BodiesVisited        int
	BodiesSkipped        int
	DuplicateComments    int
	MoreIDs              int
	UniqueMoreIDs        int
	DuplicateMoreIDs     int
	MoreRequests         int
	ContinuationRequests int
	BodyBytes            int64
	ResponseBytes        int64
}

type moreFetcher func(context.Context, []string) ([]byte, error)
type continuationFetcher func(context.Context, string) ([]byte, error)
type admittedMoreFetcher func(context.Context, []string) (responsePayload, error)
type admittedContinuationFetcher func(context.Context, string) (responsePayload, error)
type commentVisitor func(Comment) error

type visitError struct {
	err error
}

func (e *visitError) Error() string { return "visit Reddit comment" }
func (e *visitError) Unwrap() error { return e.err }

type queuedThing struct {
	value          any
	expectedParent string
	endpoint       Endpoint
}

type moreGroup struct {
	parent string
	ids    []string
}

type moreBatch struct {
	parent string
	ids    []string
}

type continuationCheckpoint struct {
	comments      int
	uniqueMoreIDs int
	continuations int
}

type jsonObject map[string]any

// walkDecoded traverses one already size-bounded comments response and streams each
// usable body once. HTTP policy remains outside this pure decoder.
func walkDecoded(
	ctx context.Context,
	postID string,
	initial []byte,
	fetch moreFetcher,
	visit commentVisitor,
) (WalkStats, error) {
	return walkDecodedWithLimits(ctx, postID, initial, DefaultThingLimits(), fetch, visit)
}

func walkDecodedWithLimits(
	ctx context.Context,
	postID string,
	initial []byte,
	limits ThingLimits,
	fetch moreFetcher,
	visit commentVisitor,
) (WalkStats, error) {
	return walkDecodedCompleteWithLimits(ctx, postID, initial, limits, fetch, nil, visit)
}

// walkDecodedCompleteWithLimits is the complete pure traversal core. A nil
// continuation fetcher preserves the fail-closed Phase 2 behavior for Reddit's
// t1__/"_" depth-truncation sentinel; a non-nil hook fetches the focal parent
// listing without coupling tree validation to HTTP construction.
func walkDecodedCompleteWithLimits(
	ctx context.Context,
	postID string,
	initial []byte,
	limits ThingLimits,
	fetch moreFetcher,
	fetchContinuation continuationFetcher,
	visit commentVisitor,
) (WalkStats, error) {
	budget, err := NewTraversalBudget(DefaultTraversalBudgetConfig())
	if err != nil {
		return WalkStats{}, newError(ErrorInvalidInput, EndpointComments, postID, 0, err)
	}
	initialPayload := responsePayload{data: initial}
	initial = nil
	return walkDecodedCompleteWithBudget(
		ctx,
		postID,
		initialPayload.take(),
		limits,
		budget,
		adaptMoreFetcher(fetch),
		adaptContinuationFetcher(fetchContinuation),
		visit,
	)
}

func adaptMoreFetcher(fetch moreFetcher) admittedMoreFetcher {
	if fetch == nil {
		return nil
	}
	return func(ctx context.Context, ids []string) (responsePayload, error) {
		payload, err := fetch(ctx, ids)
		owned := responsePayload{data: payload}
		payload = nil
		return owned.take(), err
	}
}

func adaptContinuationFetcher(fetch continuationFetcher) admittedContinuationFetcher {
	if fetch == nil {
		return nil
	}
	return func(ctx context.Context, parentID string) (responsePayload, error) {
		payload, err := fetch(ctx, parentID)
		owned := responsePayload{data: payload}
		payload = nil
		return owned.take(), err
	}
}

func walkDecodedCompleteWithBudget(
	ctx context.Context,
	postID string,
	initial responsePayload,
	limits ThingLimits,
	budget *TraversalBudget,
	fetch admittedMoreFetcher,
	fetchContinuation admittedContinuationFetcher,
	visit commentVisitor,
) (WalkStats, error) {
	var stats WalkStats
	activeResponse := initial.take()
	defer activeResponse.releaseNow()
	if ctx == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, postID, 0, errMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return stats, newError(ErrorCanceled, EndpointComments, postID, 0, err)
	}
	if !validPostID(postID) {
		return stats, newError(ErrorInvalidInput, EndpointComments, "", 0, errInvalidPostID)
	}
	if err := validateThingLimits(limits); err != nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, postID, 0, err)
	}
	if fetch == nil {
		return stats, newError(ErrorInvalidInput, EndpointMoreChildren, postID, 0, errNilMoreFetcher)
	}
	if visit == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, postID, 0, errNilCommentVisitor)
	}
	if budget == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, postID, 0, ErrTraversalBudgetConfig)
	}
	retained := retainedLease{budget: budget}
	defer retained.release()

	if err := accountResponseBytes(&stats, limits, len(activeResponse.data)); err != nil {
		return stats, resourceError(EndpointComments, postID, err)
	}
	if err := ensureResponseAdmission(ctx, budget, &activeResponse); err != nil {
		return stats, classifyTreeError(EndpointComments, postID, err)
	}
	if err := preflightResponse(ctx, activeResponse.data, limits, stats); err != nil {
		return stats, classifyTreeError(EndpointComments, postID, err)
	}

	roots, err := decodeInitialContext(ctx, activeResponse.data)
	if err != nil {
		return stats, classifyTreeError(EndpointComments, postID, err)
	}

	postChildren, err := decodeListing(roots[0])
	if err != nil {
		return stats, protocolError(EndpointComments, postID, err)
	}
	// A deleted or never-existing post returns a well-formed listing with no post
	// thing. That is an expected terminal absence, not a protocol violation, so it
	// is classified exactly like an HTTP 404 and callers can skip it explicitly.
	if len(postChildren) == 0 {
		return stats, newError(ErrorNotFound, EndpointComments, postID, 0, errPostNotFound)
	}
	if len(postChildren) != 1 {
		return stats, protocolError(EndpointComments, postID, errMalformedResponse)
	}
	if err := accountThings(&stats, limits, 1); err != nil {
		return stats, resourceError(EndpointComments, postID, err)
	}
	if err := validatePostThing(postChildren[0], postID); err != nil {
		return stats, protocolError(EndpointComments, postID, err)
	}

	commentChildren, err := decodeListing(roots[1])
	if err != nil {
		return stats, protocolError(EndpointComments, postID, err)
	}
	initialCommentCapacity := min(len(commentChildren), limits.MaxComments, maxTraversalPreallocation)
	stack := make([]queuedThing, 0, min(len(commentChildren), limits.MaxThings, maxTraversalPreallocation))
	if err := enqueueThings(&stack, commentChildren, "t3_"+postID, EndpointComments, &stats, limits); err != nil {
		return stats, resourceError(EndpointComments, postID, err)
	}
	// The stack now owns the only required child references. Clearing the root/listing
	// views lets processed subtrees become collectible before the response-byte permit
	// is released and another decoder is admitted.
	roots = nil
	postChildren = nil
	commentChildren = nil
	activeResponse.data = nil

	commentParents := make(map[string]string, initialCommentCapacity)
	commentOrigins := make(map[string]Endpoint, initialCommentCapacity)
	commentOrder := make([]string, 0, initialCommentCapacity)
	moreParents := make(map[string]string)
	pendingMore := make([]moreGroup, 0)
	pendingContinuations := make([]string, 0)
	seenContinuations := make(map[string]struct{})
	var requested map[string]string
	var activeContinuation *continuationCheckpoint

	for len(stack) > 0 || len(pendingMore) > 0 || len(pendingContinuations) > 0 || requested != nil || activeContinuation != nil {
		if err := ctx.Err(); err != nil {
			return stats, newError(ErrorCanceled, EndpointComments, postID, 0, err)
		}
		if len(stack) == 0 && activeResponse.release != nil {
			activeResponse.releaseNow()
		}

		if len(stack) > 0 {
			index := len(stack) - 1
			item := stack[index]
			stack[index] = queuedThing{}
			stack = stack[:index]

			kind, data, err := decodeThing(item.value)
			if err != nil {
				return stats, protocolError(item.endpoint, postID, err)
			}
			switch kind {
			case "t1":
				err = processComment(ctx, data, item.expectedParent, item.endpoint, postID, limits, visit, commentParents, commentOrigins, &commentOrder, moreParents, requested, &stack, &stats, &retained)
			case "more":
				err = processMore(ctx, data, item.expectedParent, postID, limits, fetchContinuation != nil, commentParents, moreParents, seenContinuations, &pendingMore, &pendingContinuations, &stats, &retained)
			default:
				err = errMalformedResponse
			}
			if err != nil {
				return stats, classifyTreeError(item.endpoint, postID, err)
			}
			continue
		}
		if activeContinuation != nil {
			if stats.Comments == activeContinuation.comments &&
				stats.UniqueMoreIDs == activeContinuation.uniqueMoreIDs &&
				len(seenContinuations) == activeContinuation.continuations {
				return stats, protocolError(EndpointContinuation, postID, errIncompleteTree)
			}
			activeContinuation = nil
		}

		if requested != nil {
			if len(requested) != 0 {
				return stats, protocolError(EndpointMoreChildren, postID, errIncompleteTree)
			}
			requested = nil
			continue
		}

		if len(pendingMore) > 0 {
			batch := takeMoreBatch(&pendingMore, commentParents)
			if len(batch.ids) == 0 {
				continue
			}
			if stats.MoreRequests == limits.MaxMoreRequests {
				return stats, resourceError(EndpointMoreChildren, postID, errMoreRequestLimit)
			}
			stats.MoreRequests++
			requested = make(map[string]string, len(batch.ids))
			for _, id := range batch.ids {
				requested[id] = batch.parent
			}

			payload, fetchErr := fetch(ctx, batch.ids)
			if err := classifyFetchError(ctx, EndpointMoreChildren, postID, fetchErr); err != nil {
				payload.releaseNow()
				return stats, err
			}
			activeResponse = payload.take()
			if err := accountResponseBytes(&stats, limits, len(activeResponse.data)); err != nil {
				return stats, resourceError(EndpointMoreChildren, postID, err)
			}
			if err := ensureResponseAdmission(ctx, budget, &activeResponse); err != nil {
				return stats, classifyTreeError(EndpointMoreChildren, postID, err)
			}
			if err := preflightResponse(ctx, activeResponse.data, limits, stats); err != nil {
				return stats, classifyTreeError(EndpointMoreChildren, postID, err)
			}

			things, err := decodeMoreResponseContext(ctx, activeResponse.data)
			if err != nil {
				return stats, classifyTreeError(EndpointMoreChildren, postID, err)
			}
			if err := enqueueThings(&stack, things, "", EndpointMoreChildren, &stats, limits); err != nil {
				return stats, resourceError(EndpointMoreChildren, postID, err)
			}
			things = nil
			activeResponse.data = nil
			continue
		}
		if len(pendingContinuations) == 0 {
			continue
		}

		parentID := pendingContinuations[0]
		pendingContinuations[0] = ""
		pendingContinuations = pendingContinuations[1:]
		if _, present := commentParents[parentID]; !present {
			return stats, protocolError(EndpointContinuation, postID, errIncompleteTree)
		}
		if stats.ContinuationRequests == limits.MaxContinuationRequests {
			return stats, resourceError(EndpointContinuation, postID, errContinuationRequestLimit)
		}
		stats.ContinuationRequests++
		payload, fetchErr := fetchContinuation(ctx, parentID)
		if err := classifyFetchError(ctx, EndpointContinuation, postID, fetchErr); err != nil {
			payload.releaseNow()
			return stats, err
		}
		activeResponse = payload.take()
		if err := accountResponseBytes(&stats, limits, len(activeResponse.data)); err != nil {
			return stats, resourceError(EndpointContinuation, postID, err)
		}
		if err := ensureResponseAdmission(ctx, budget, &activeResponse); err != nil {
			return stats, classifyTreeError(EndpointContinuation, postID, err)
		}
		if err := preflightResponse(ctx, activeResponse.data, limits, stats); err != nil {
			return stats, classifyTreeError(EndpointContinuation, postID, err)
		}
		focal, err := decodeContinuationResponseContext(ctx, activeResponse.data, postID, parentID)
		if err != nil {
			return stats, classifyTreeError(EndpointContinuation, postID, err)
		}
		// A focal response contains a freshly decoded t3 post thing in addition
		// to the focal t1 returned below. Count both occurrences so repeated
		// continuations cannot bypass the per-post work budget.
		if err := accountThings(&stats, limits, 1); err != nil {
			return stats, resourceError(EndpointContinuation, postID, err)
		}
		if err := enqueueThings(&stack, []any{focal}, "", EndpointContinuation, &stats, limits); err != nil {
			return stats, resourceError(EndpointContinuation, postID, err)
		}
		focal = nil
		activeResponse.data = nil
		activeContinuation = &continuationCheckpoint{
			comments:      stats.Comments,
			uniqueMoreIDs: stats.UniqueMoreIDs,
			continuations: len(seenContinuations),
		}
	}
	activeResponse.releaseNow()

	if endpoint, err := validateCommentGraph(ctx, commentParents, commentOrigins, commentOrder, postID); err != nil {
		return stats, classifyTreeError(endpoint, postID, err)
	}
	return stats, nil
}

// ensureResponseAdmission keeps the pure decoder helpers usable without an HTTP
// client: production responses arrive with a configured-capacity lease, while an
// already bounded in-memory test payload acquires only its actual byte length.
func ensureResponseAdmission(ctx context.Context, budget *TraversalBudget, payload *responsePayload) error {
	if payload == nil {
		return ErrTraversalBudgetConfig
	}
	if payload.release != nil {
		return nil
	}
	release, err := budget.acquireResponse(ctx, int64(len(payload.data)))
	if err != nil {
		return err
	}
	payload.release = release
	return nil
}

func validateThingLimits(limits ThingLimits) error {
	if limits.MaxThings <= 0 || limits.MaxThings > absoluteMaxThings {
		return fmt.Errorf("%w: MaxThings must be between 1 and %d", errInvalidThingLimits, absoluteMaxThings)
	}
	if limits.MaxComments <= 0 || limits.MaxComments > absoluteMaxComments || limits.MaxComments > limits.MaxThings {
		return fmt.Errorf("%w: MaxComments must be between 1 and MaxThings", errInvalidThingLimits)
	}
	if limits.MaxMoreIDs <= 0 || limits.MaxMoreIDs > absoluteMaxMoreIDs {
		return fmt.Errorf("%w: MaxMoreIDs must be between 1 and %d", errInvalidThingLimits, absoluteMaxMoreIDs)
	}
	if limits.MaxMoreRequests <= 0 || limits.MaxMoreRequests > absoluteMaxMoreRequests {
		return fmt.Errorf("%w: MaxMoreRequests must be between 1 and %d", errInvalidThingLimits, absoluteMaxMoreRequests)
	}
	if limits.MaxContinuationRequests <= 0 || limits.MaxContinuationRequests > absoluteMaxContinuationRequests {
		return fmt.Errorf("%w: MaxContinuationRequests must be between 1 and %d", errInvalidThingLimits, absoluteMaxContinuationRequests)
	}
	if limits.MaxBodyBytes <= 0 || limits.MaxBodyBytes > absoluteMaxBodyBytes {
		return fmt.Errorf("%w: MaxBodyBytes must be between 1 and %d", errInvalidThingLimits, absoluteMaxBodyBytes)
	}
	if limits.MaxTotalBodyBytes <= 0 || limits.MaxTotalBodyBytes > absoluteMaxTotalBodyBytes ||
		limits.MaxTotalBodyBytes < int64(limits.MaxBodyBytes) {
		return fmt.Errorf("%w: MaxTotalBodyBytes must be between MaxBodyBytes and %d", errInvalidThingLimits, absoluteMaxTotalBodyBytes)
	}
	if limits.MaxTotalResponseBytes <= 0 || limits.MaxTotalResponseBytes > absoluteMaxTotalResponseBytes {
		return fmt.Errorf("%w: MaxTotalResponseBytes must be between 1 and %d", errInvalidThingLimits, absoluteMaxTotalResponseBytes)
	}
	return nil
}

// preflightResponse streams protocol structure before materializing maps and
// slices. The HTTP byte cap bounds raw input; this second pass ties allocation
// cardinality to the remaining per-post work budgets and rejects oversized nested
// metadata before the generic decoder can preallocate it.
func preflightResponse(ctx context.Context, payload []byte, limits ThingLimits, stats WalkStats) error {
	remainingThings := limits.MaxThings - stats.Things
	remainingComments := limits.MaxComments - stats.Comments - stats.DuplicateComments
	remainingMoreIDs := limits.MaxMoreIDs - stats.MoreIDs
	remainingBodyBytes := limits.MaxTotalBodyBytes - stats.BodyBytes
	if remainingThings < 0 {
		return errThingLimit
	}
	if remainingComments < 0 {
		return errCommentLimit
	}
	if remainingMoreIDs < 0 {
		return errMoreIDLimit
	}
	if remainingBodyBytes < 0 {
		return errCommentBodiesTooLarge
	}
	return preflightJSON(ctx, payload, remainingThings, remainingComments, remainingMoreIDs, limits.MaxBodyBytes, remainingBodyBytes)
}

func preflightJSON(ctx context.Context, payload []byte, maxThings, maxComments, maxMoreIDs, maxBodyBytes int, maxTotalBodyBytes int64) error {
	return scanJSONPreflight(ctx, payload, preflightLimits{
		maxThings:         maxThings,
		maxComments:       maxComments,
		maxMoreIDs:        maxMoreIDs,
		maxBodyBytes:      maxBodyBytes,
		maxTotalBodyBytes: maxTotalBodyBytes,
	})
}

func decodeInitialContext(ctx context.Context, payload []byte) ([]any, error) {
	value, err := decodeOneContext(ctx, payload)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errMalformedResponse
	}
	roots, ok := value.([]any)
	if !ok || len(roots) != 2 {
		return nil, errMalformedResponse
	}
	return roots, nil
}

func decodeListing(value any) ([]any, error) {
	kind, data, err := decodeThing(value)
	if err != nil || kind != "Listing" {
		return nil, errMalformedResponse
	}
	for _, field := range [...]string{"after", "before"} {
		if err := validateListingCursor(data, field); err != nil {
			return nil, err
		}
	}
	children, err := decodeRequiredArray(data, "children")
	if err != nil {
		return nil, errMalformedResponse
	}
	return children, nil
}

func validatePostThing(value any, postID string) error {
	kind, data, err := decodeThing(value)
	if err != nil || kind != "t3" {
		return errMalformedResponse
	}
	id, err := decodeRequiredString(data["id"])
	if err != nil || id != postID || !validPostID(id) {
		return errMalformedResponse
	}
	name, err := decodeRequiredString(data["name"])
	if err != nil || name != "t3_"+postID {
		return errMalformedResponse
	}
	return nil
}

func processComment(
	ctx context.Context,
	data jsonObject,
	expectedParent string,
	endpoint Endpoint,
	postID string,
	limits ThingLimits,
	visit commentVisitor,
	commentParents map[string]string,
	commentOrigins map[string]Endpoint,
	commentOrder *[]string,
	moreParents map[string]string,
	requested map[string]string,
	stack *[]queuedThing,
	stats *WalkStats,
	retained *retainedLease,
) error {
	id, err := decodeRequiredString(data["id"])
	if err != nil || !validCommentID(id) {
		return errMalformedResponse
	}
	name, err := decodeRequiredString(data["name"])
	if err != nil || name != "t1_"+id {
		return errMalformedResponse
	}
	if err := validateRequiredLinkID(data["link_id"], postID); err != nil {
		return err
	}
	parent, err := decodeRequiredParentID(data["parent_id"], expectedParent, postID)
	if err != nil {
		return err
	}
	if canonicalParent, duplicate := commentParents[id]; duplicate && canonicalParent != parent {
		return errMalformedResponse
	}
	if moreParent, expected := moreParents[id]; expected && moreParent != parent {
		return errMalformedResponse
	}
	if requestedParent, expected := requested[id]; expected && requestedParent != parent {
		return errMalformedResponse
	}

	repliesValue, repliesPresent := data["replies"]
	if !repliesPresent {
		return errMalformedResponse
	}
	replies, err := decodeReplies(repliesValue)
	if err != nil {
		return err
	}
	body, present, err := decodeOptionalString(data["body"])
	if err != nil {
		return errMalformedResponse
	}
	if present && len(body) > limits.MaxBodyBytes {
		return errCommentBodyTooLarge
	}
	if present {
		bodyBytes := int64(len(body))
		if stats.BodyBytes > limits.MaxTotalBodyBytes-bodyBytes {
			return errCommentBodiesTooLarge
		}
		stats.BodyBytes += bodyBytes
	}
	if requested != nil {
		delete(requested, id)
	}

	if stats.Comments+stats.DuplicateComments == limits.MaxComments {
		return errCommentLimit
	}
	if _, duplicate := commentParents[id]; duplicate {
		stats.DuplicateComments++
	} else {
		if err := retained.reserve(); err != nil {
			return err
		}
		commentParents[id] = parent
		commentOrigins[id] = endpoint
		*commentOrder = append(*commentOrder, id)
		stats.Comments++

		trimmed := strings.TrimSpace(body)
		if !present || trimmed == "" || trimmed == "[deleted]" || trimmed == "[removed]" {
			stats.BodiesSkipped++
		} else {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(Comment{ID: id, Fullname: name, Body: body}); err != nil {
				return &visitError{err: err}
			}
			stats.BodiesVisited++
		}
	}

	return enqueueThings(stack, replies, name, endpoint, stats, limits)
}

func processMore(
	ctx context.Context,
	data jsonObject,
	expectedParent string,
	postID string,
	limits ThingLimits,
	allowContinuation bool,
	commentParents map[string]string,
	moreParents map[string]string,
	seenContinuations map[string]struct{},
	pendingMore *[]moreGroup,
	pendingContinuations *[]string,
	stats *WalkStats,
	retained *retainedLease,
) error {
	parent, err := decodeRequiredParentID(data["parent_id"], expectedParent, postID)
	if err != nil {
		return err
	}
	count, err := decodeRequiredNonNegativeInt(data["count"])
	if err != nil {
		return errMalformedResponse
	}
	children, err := decodeRequiredArray(data, "children")
	if err != nil {
		return errMalformedResponse
	}
	if count > 0 && len(children) == 0 {
		return errIncompleteTree
	}
	if count == 0 && len(children) == 0 {
		// Reddit uses the exact t1__ / "_" placeholder for a depth-truncated
		// "continue this thread" branch. It cannot be expanded through
		// /api/morechildren; callers without the focal-comment hook fail explicitly.
		id, idErr := decodeRequiredString(data["id"])
		name, nameErr := decodeRequiredString(data["name"])
		if idErr == nil && nameErr == nil && id == "_" && name == "t1__" && strings.HasPrefix(parent, "t1_") {
			parentID := strings.TrimPrefix(parent, "t1_")
			if _, duplicate := seenContinuations[parentID]; duplicate {
				return errIncompleteTree
			}
			if !allowContinuation {
				return errIncompleteTree
			}
			if err := retained.reserve(); err != nil {
				return err
			}
			seenContinuations[parentID] = struct{}{}
			*pendingContinuations = append(*pendingContinuations, parentID)
			return nil
		}
		return errMalformedResponse
	}
	if count == 0 || count < len(children) {
		return errMalformedResponse
	}
	moreID, err := decodeRequiredString(data["id"])
	if err != nil || !validCommentID(moreID) {
		return errMalformedResponse
	}
	moreName, err := decodeRequiredString(data["name"])
	if err != nil || moreName != "t1_"+moreID {
		return errMalformedResponse
	}

	group := moreGroup{parent: parent, ids: make([]string, 0, min(len(children), maxTraversalPreallocation))}
	for index, rawID := range children {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		id, err := decodeRequiredString(rawID)
		if err != nil || !validCommentID(id) {
			return errMalformedResponse
		}
		if index == 0 && id != moreID {
			return errMalformedResponse
		}
		if stats.MoreIDs == limits.MaxMoreIDs {
			return errMoreIDLimit
		}
		stats.MoreIDs++
		if knownParent, known := moreParents[id]; known {
			if knownParent != parent {
				return errMalformedResponse
			}
			stats.DuplicateMoreIDs++
			continue
		}
		if err := retained.reserve(); err != nil {
			return err
		}
		moreParents[id] = parent
		if commentParent, resolved := commentParents[id]; resolved {
			if commentParent != parent {
				return errMalformedResponse
			}
			stats.DuplicateMoreIDs++
			continue
		}
		group.ids = append(group.ids, id)
		stats.UniqueMoreIDs++
	}
	if len(group.ids) > 0 {
		*pendingMore = append(*pendingMore, group)
	}
	return nil
}

func decodeReplies(value any) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		if text != "" {
			return nil, errMalformedResponse
		}
		return nil, nil
	}
	return decodeListing(value)
}

func decodeMoreResponseContext(ctx context.Context, payload []byte) ([]any, error) {
	value, err := decodeOneContext(ctx, payload)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errMalformedResponse
	}
	envelope, ok := asJSONObject(value)
	if !ok {
		return nil, errMalformedResponse
	}
	response, err := decodeRequiredObject(envelope, "json")
	if err != nil {
		return nil, errMalformedResponse
	}
	apiErrors, err := decodeRequiredArray(response, "errors")
	if err != nil {
		return nil, errMalformedResponse
	}
	if len(apiErrors) != 0 {
		return nil, errIncompleteTree
	}
	data, err := decodeRequiredObject(response, "data")
	if err != nil {
		return nil, errMalformedResponse
	}
	things, err := decodeRequiredArray(data, "things")
	if err != nil {
		return nil, errMalformedResponse
	}
	return things, nil
}

func decodeThing(value any) (string, jsonObject, error) {
	thing, ok := asJSONObject(value)
	if !ok {
		return "", nil, errMalformedResponse
	}
	kind, err := decodeRequiredString(thing["kind"])
	if err != nil {
		return "", nil, errMalformedResponse
	}
	data, err := decodeRequiredObject(thing, "data")
	if err != nil {
		return "", nil, errMalformedResponse
	}
	return kind, data, nil
}

// decodeOneContext builds a single generic wire tree. Nested replies are then queued as
// references into that tree instead of being repeatedly unmarshaled and copied at
// every ancestor, keeping decode memory linear in the size of the bounded response.
func decodeOneContext(ctx context.Context, payload []byte) (any, error) {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return nil, errMalformedResponse
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: bytes.NewReader(payload)})
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errMalformedResponse
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errMalformedResponse
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return value, nil
}

type contextReader struct {
	ctx    context.Context
	reader *bytes.Reader
}

func (r *contextReader) Read(destination []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(destination)
}

func asJSONObject(value any) (jsonObject, bool) {
	object, ok := value.(map[string]any)
	return jsonObject(object), ok
}

func decodeRequiredObject(object jsonObject, field string) (jsonObject, error) {
	value, present := object[field]
	if !present || value == nil {
		return nil, errMalformedResponse
	}
	decoded, ok := asJSONObject(value)
	if !ok {
		return nil, errMalformedResponse
	}
	return decoded, nil
}

func decodeRequiredArray(object jsonObject, field string) ([]any, error) {
	value, present := object[field]
	if !present || value == nil {
		return nil, errMalformedResponse
	}
	decoded, ok := value.([]any)
	if !ok {
		return nil, errMalformedResponse
	}
	return decoded, nil
}

func validateListingCursor(data jsonObject, field string) error {
	value, present := data[field]
	if !present || value == nil {
		return nil
	}
	cursor, ok := value.(string)
	if !ok {
		return errMalformedResponse
	}
	if cursor != "" {
		return errIncompleteTree
	}
	return nil
}

func decodeRequiredString(value any) (string, error) {
	decoded, ok := value.(string)
	if !ok || decoded == "" {
		return "", errMalformedResponse
	}
	return decoded, nil
}

func decodeOptionalString(value any) (string, bool, error) {
	if value == nil {
		return "", false, nil
	}
	decoded, ok := value.(string)
	if !ok {
		return "", false, errMalformedResponse
	}
	return decoded, true, nil
}

func decodeRequiredNonNegativeInt(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errMalformedResponse
	}
	decoded, err := number.Int64()
	if err != nil || decoded < 0 || int64(int(decoded)) != decoded {
		return 0, errMalformedResponse
	}
	return int(decoded), nil
}

func validateRequiredLinkID(raw any, postID string) error {
	value, err := decodeRequiredString(raw)
	if err != nil || value != "t3_"+postID {
		return errMalformedResponse
	}
	return nil
}

func decodeRequiredParentID(raw any, expectedParent, postID string) (string, error) {
	value, err := decodeRequiredString(raw)
	if err != nil {
		return "", errMalformedResponse
	}
	if expectedParent != "" && value != expectedParent {
		return "", errMalformedResponse
	}
	if value == "t3_"+postID {
		return value, nil
	}
	if !strings.HasPrefix(value, "t1_") || !validCommentID(strings.TrimPrefix(value, "t1_")) {
		return "", errMalformedResponse
	}
	return value, nil
}

func validPostID(value string) bool {
	return validBase36ID(value, maxPostIDBytes)
}

func validCommentID(value string) bool {
	return validBase36ID(value, maxCommentIDBytes)
}

func validBase36ID(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			if value[i] < 'a' || value[i] > 'z' {
				return false
			}
		}
	}
	return true
}

func enqueueThings(stack *[]queuedThing, things []any, expectedParent string, endpoint Endpoint, stats *WalkStats, limits ThingLimits) error {
	if err := accountThings(stats, limits, len(things)); err != nil {
		return err
	}
	// Reverse insertion preserves Reddit's source order while using a bounded LIFO
	// stack, so reply depth cannot consume the Go call stack.
	for index := len(things) - 1; index >= 0; index-- {
		*stack = append(*stack, queuedThing{value: things[index], expectedParent: expectedParent, endpoint: endpoint})
	}
	return nil
}

func accountThings(stats *WalkStats, limits ThingLimits, additional int) error {
	if additional < 0 || stats.Things > limits.MaxThings-additional {
		return errThingLimit
	}
	stats.Things += additional
	return nil
}

func accountResponseBytes(stats *WalkStats, limits ThingLimits, additional int) error {
	if additional < 0 || int64(additional) > limits.MaxTotalResponseBytes-stats.ResponseBytes {
		return errResponsesTooLarge
	}
	stats.ResponseBytes += int64(additional)
	return nil
}

// takeMoreBatch consumes IDs from exactly one Reddit MoreComments stub. The API
// accepts a list of IDs, but does not guarantee complete results when unrelated
// placeholder depths are mixed; retaining the stub boundary avoids that ambiguity.
func takeMoreBatch(pending *[]moreGroup, commentParents map[string]string) moreBatch {
	for len(*pending) > 0 {
		group := &(*pending)[0]
		batch := moreBatch{parent: group.parent, ids: make([]string, 0, min(moreChildrenBatchSize, len(group.ids)))}
		consumed := 0
		for consumed < len(group.ids) && len(batch.ids) < moreChildrenBatchSize {
			id := group.ids[consumed]
			group.ids[consumed] = ""
			consumed++
			if _, resolved := commentParents[id]; !resolved {
				batch.ids = append(batch.ids, id)
			}
		}
		group.ids = group.ids[consumed:]
		if len(group.ids) == 0 {
			(*pending)[0] = moreGroup{}
			*pending = (*pending)[1:]
		}
		if len(batch.ids) > 0 {
			return batch
		}
	}
	return moreBatch{}
}

func classifyFetchError(ctx context.Context, endpoint Endpoint, postID string, err error) error {
	if err == nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return newError(ErrorCanceled, endpoint, postID, 0, ctxErr)
		}
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return newError(ErrorCanceled, endpoint, postID, 0, ctxErr)
	}
	var adapterErr *Error
	if errors.As(err, &adapterErr) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newError(ErrorCanceled, endpoint, postID, 0, err)
	}
	return newError(ErrorTransport, endpoint, postID, 0, err)
}

// decodeContinuationResponseContext validates the focal-comment response shape used
// for a depth-truncated t1__/"_" branch. Requiring at least one reply makes an empty,
// mutable, or otherwise no-progress response explicit rather than silently complete.
func decodeContinuationResponseContext(ctx context.Context, payload []byte, postID, parentID string) (any, error) {
	roots, err := decodeInitialContext(ctx, payload)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errMalformedResponse
	}
	postChildren, err := decodeListing(roots[0])
	if err != nil {
		return nil, err
	}
	if len(postChildren) != 1 || validatePostThing(postChildren[0], postID) != nil {
		return nil, errMalformedResponse
	}
	comments, err := decodeListing(roots[1])
	if err != nil {
		return nil, err
	}
	if len(comments) != 1 {
		return nil, errMalformedResponse
	}
	kind, data, err := decodeThing(comments[0])
	if err != nil || kind != "t1" {
		return nil, errMalformedResponse
	}
	id, err := decodeRequiredString(data["id"])
	if err != nil || id != parentID {
		return nil, errMalformedResponse
	}
	repliesValue, present := data["replies"]
	if !present {
		return nil, errMalformedResponse
	}
	replies, err := decodeReplies(repliesValue)
	if err != nil || len(replies) == 0 {
		return nil, errIncompleteTree
	}
	return comments[0], nil
}

// validateCommentGraph proves that every t1 parent is present in the accepted post
// tree and that the canonical parent relation is acyclic. It runs after all flat
// morechildren responses have been reconciled so response order is irrelevant.
func validateCommentGraph(ctx context.Context, parents map[string]string, origins map[string]Endpoint, order []string, postID string) (Endpoint, error) {
	root := "t3_" + postID
	checked := 0
	for _, id := range order {
		parent := parents[id]
		if checked%256 == 0 {
			if err := ctx.Err(); err != nil {
				return origins[id], err
			}
		}
		checked++
		if parent == root {
			continue
		}
		parentID := strings.TrimPrefix(parent, "t1_")
		if _, present := parents[parentID]; !present {
			return origins[id], errMalformedResponse
		}
	}

	state := make(map[string]uint8, len(parents))
	path := make([]string, 0, 8)
	for _, start := range order {
		if checked%256 == 0 {
			if err := ctx.Err(); err != nil {
				return origins[start], err
			}
		}
		checked++
		if state[start] == 2 {
			continue
		}
		path = path[:0]
		current := start
		for {
			if checked%256 == 0 {
				if err := ctx.Err(); err != nil {
					return origins[current], err
				}
			}
			checked++
			switch state[current] {
			case 1:
				return origins[current], errMalformedResponse
			case 2:
				for _, id := range path {
					state[id] = 2
				}
				current = ""
			}
			if current == "" {
				break
			}
			state[current] = 1
			path = append(path, current)
			parent := parents[current]
			if parent == root {
				for _, id := range path {
					state[id] = 2
				}
				break
			}
			current = strings.TrimPrefix(parent, "t1_")
		}
	}
	return EndpointComments, nil
}

func classifyTreeError(endpoint Endpoint, postID string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newError(ErrorCanceled, endpoint, postID, 0, err)
	}
	if errors.Is(err, errThingLimit) || errors.Is(err, errCommentLimit) || errors.Is(err, errMoreIDLimit) ||
		errors.Is(err, errMoreRequestLimit) || errors.Is(err, errContinuationRequestLimit) ||
		errors.Is(err, errCommentBodyTooLarge) || errors.Is(err, errCommentBodiesTooLarge) || errors.Is(err, errResponsesTooLarge) ||
		errors.Is(err, errInFlightResponseBudget) || errors.Is(err, errRetainedThingBudget) {
		return resourceError(endpoint, postID, err)
	}
	var visitorErr *visitError
	if errors.As(err, &visitorErr) {
		return newError(ErrorVisitor, endpoint, postID, 0, err)
	}
	return protocolError(endpoint, postID, err)
}

func protocolError(endpoint Endpoint, postID string, cause error) error {
	class := ErrorProtocol
	if errors.Is(cause, errIncompleteTree) {
		class = ErrorIncomplete
	}
	return newError(class, endpoint, postID, 0, &treeProtocolError{cause: cause})
}

func resourceError(endpoint Endpoint, postID string, cause error) error {
	return newError(ErrorResourceLimit, endpoint, postID, 0, cause)
}

// treeProtocolError preserves sentinel classification without ever formatting
// response-derived details at an application or logging boundary.
type treeProtocolError struct {
	cause error
}

func (e *treeProtocolError) Error() string { return errMalformedResponse.Error() }
func (e *treeProtocolError) Unwrap() error { return e.cause }
