package reddit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTraversalBudgetConfiguration(t *testing.T) {
	t.Parallel()

	defaults := DefaultTraversalBudgetConfig()
	if defaults.MaxInFlightResponseBytes != 32<<20 || defaults.MaxRetainedThings != 500_000 {
		t.Fatalf("DefaultTraversalBudgetConfig() = %#v", defaults)
	}
	tests := []TraversalBudgetConfig{
		{},
		{MaxInFlightResponseBytes: -1, MaxRetainedThings: 1},
		{MaxInFlightResponseBytes: absoluteMaxInFlightResponseBytes + 1, MaxRetainedThings: 1},
		{MaxInFlightResponseBytes: 1, MaxRetainedThings: -1},
		{MaxInFlightResponseBytes: 1, MaxRetainedThings: absoluteMaxRetainedThings + 1},
	}
	for _, config := range tests {
		if budget, err := NewTraversalBudget(config); budget != nil || !errors.Is(err, ErrTraversalBudgetConfig) {
			t.Errorf("NewTraversalBudget(%#v) = %#v, %v", config, budget, err)
		}
	}
}

func TestTraversalBudgetResponseWaitIsCancelableAndReusable(t *testing.T) {
	t.Parallel()

	budget, err := NewTraversalBudget(TraversalBudgetConfig{MaxInFlightResponseBytes: 10, MaxRetainedThings: 1})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	releaseEight, err := budget.acquireResponse(context.Background(), 8)
	if err != nil {
		t.Fatalf("acquireResponse(8) error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := budget.acquireResponse(canceled, 4); release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire returned release=%t, error=%v", release != nil, err)
	}
	if release, err := budget.acquireResponse(context.Background(), 11); release != nil || !errors.Is(err, errInFlightResponseBudget) {
		t.Fatalf("oversized acquire returned release=%t, error=%v", release != nil, err)
	}

	releaseEight()
	releaseAll, err := budget.acquireResponse(context.Background(), 10)
	if err != nil {
		t.Fatalf("acquire after cancellation error = %v", err)
	}
	// Releases are idempotent so layered cleanup cannot accidentally over-credit the
	// weighted semaphore during an error return.
	releaseAll()
	releaseAll()
}

func TestTraversalBudgetResponseContentionWaitsForRelease(t *testing.T) {
	t.Parallel()

	budget, err := NewTraversalBudget(TraversalBudgetConfig{MaxInFlightResponseBytes: 10, MaxRetainedThings: 1})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	releaseFirst, err := budget.acquireResponse(context.Background(), 8)
	if err != nil {
		t.Fatalf("first acquire error = %v", err)
	}
	if budget.responseBytes.TryAcquire(4) {
		budget.responseBytes.Release(4)
		t.Fatal("four-byte reservation unexpectedly fit while eight of ten bytes were held")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attempting := make(chan struct{})
	acquired := make(chan func(), 1)
	errorsSeen := make(chan error, 1)
	go func() {
		close(attempting)
		release, acquireErr := budget.acquireResponse(waitCtx, 4)
		if acquireErr != nil {
			errorsSeen <- acquireErr
			return
		}
		acquired <- release
	}()
	<-attempting
	releaseFirst()
	select {
	case acquireErr := <-errorsSeen:
		t.Fatalf("contended acquire error = %v", acquireErr)
	case release := <-acquired:
		release()
	}
}

func TestAcceptedResponseWaitsForAdmissionBeforeBodyReadAndTransfersLease(t *testing.T) {
	t.Parallel()

	budget, err := NewTraversalBudget(TraversalBudgetConfig{MaxInFlightResponseBytes: 10, MaxRetainedThings: 1})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	releaseBlocker, err := budget.acquireResponse(context.Background(), 10)
	if err != nil {
		t.Fatalf("blocking acquire error = %v", err)
	}
	defer releaseBlocker()

	body := newTrackingResponseBody([]byte(`{}`), nil, nil)
	client := &http.Client{
		Timeout: time.Second,
		Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": {"application/json"}},
				Body:          body,
				ContentLength: 2,
			}, nil
		}),
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	admissionEntered := make(chan struct{})
	type attemptOutcome struct {
		result policyAttemptResult
		err    error
	}
	finished := make(chan attemptOutcome, 1)
	go func() {
		result, attemptErr := executePayloadAttempt(
			context.Background(), client, request, EndpointComments, "post1", 10,
			func(ctx context.Context, weight int64) (func(), error) {
				close(admissionEntered)
				return budget.acquireResponse(ctx, weight)
			},
		)
		finished <- attemptOutcome{result: result, err: attemptErr}
	}()

	select {
	case <-admissionEntered:
	case outcome := <-finished:
		t.Fatalf("attempt returned before admission: error = %v", outcome.err)
	case <-time.After(2 * time.Second):
		t.Fatal("attempt did not reach response admission")
	}
	if reads := body.reads.Load(); reads != 0 {
		t.Fatalf("body reads before admission = %d, want 0", reads)
	}
	releaseBlocker()
	outcome := <-finished
	if outcome.err != nil || string(outcome.result.payload) != `{}` || outcome.result.releasePayload == nil {
		t.Fatalf("attempt result = %q, lease=%v, error=%v", outcome.result.payload, outcome.result.releasePayload != nil, outcome.err)
	}
	if budget.responseBytes.TryAcquire(1) {
		budget.responseBytes.Release(1)
		t.Fatal("response lease was released before decoder ownership ended")
	}
	outcome.result.releaseResponse()
	if !budget.responseBytes.TryAcquire(10) {
		t.Fatal("response capacity was not fully released")
	}
	budget.responseBytes.Release(10)
}

func TestRequestPolicyReleasesTransferredPayloadOnCallerCancellation(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Second,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	var releases int
	_, err := policy.newRetrySession().doAfterPayload(
		ctx,
		EndpointComments,
		"post1",
		nil,
		nil,
		func(context.Context) (policyAttemptResult, error) {
			clock.jump(2 * time.Second)
			cancel()
			return policyAttemptResult{
				payload:   []byte(`{}`),
				attempted: true,
				releasePayload: func() {
					releases++
				},
			}, nil
		},
	)
	assertErrorClass(t, err, ErrorCanceled)
	if !errors.Is(err, context.Canceled) || errors.Is(err, errRetryBudget) {
		t.Fatalf("policy error = %v, want parent cancellation to outrank simultaneous budget expiry", err)
	}
	if releases != 1 {
		t.Fatalf("payload releases = %d, want 1", releases)
	}
}

func TestRequestPolicyInitialHTTPBudgetExpiryReleasesAcceptedPayload(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Second,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        time.Millisecond,
	})
	budget, err := NewTraversalBudget(TraversalBudgetConfig{MaxInFlightResponseBytes: 10, MaxRetainedThings: 1})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	requests := 0
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			deadline, ok := request.Context().Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
				t.Errorf("initial request deadline present = %t, remaining = %s", ok, time.Until(deadline))
			}
			// Advancing the injected policy clock makes post-attempt exhaustion
			// deterministic while the real child deadline verifies HTTP propagation.
			clock.jump(2 * time.Second)
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": {"application/json"}},
				Body:          io.NopCloser(strings.NewReader(`{}`)),
				ContentLength: 2,
			}, nil
		}),
	}

	payload, err := policy.newRetrySession().doAfterPayload(
		context.Background(),
		EndpointComments,
		"post1",
		nil,
		nil,
		func(attemptCtx context.Context) (policyAttemptResult, error) {
			request, requestErr := http.NewRequestWithContext(attemptCtx, http.MethodGet, "https://example.invalid/comments/post1", nil)
			if requestErr != nil {
				return policyAttemptResult{}, requestErr
			}
			return executePayloadAttempt(
				attemptCtx,
				httpClient,
				request,
				EndpointComments,
				"post1",
				10,
				budget.acquireResponse,
			)
		},
	)
	assertErrorClass(t, err, ErrorTransport)
	if len(payload.data) != 0 || payload.release != nil || requests != 1 ||
		!errors.Is(err, errRetryBudget) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("payload = %#v, requests = %d, error = %v; want released internal budget failure", payload, requests, err)
	}
	if !budget.responseBytes.TryAcquire(10) {
		t.Fatal("accepted response lease leaked after initial HTTP attempt exhausted the session budget")
	}
	budget.responseBytes.Release(10)
}

func TestTraversalBudgetRetainedLeaseReleasesOnWalkFailure(t *testing.T) {
	t.Parallel()

	budget, err := NewTraversalBudget(TraversalBudgetConfig{
		MaxInFlightResponseBytes: 1 << 20,
		MaxRetainedThings:        1,
	})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	limits := DefaultThingLimits()
	initial := testInitial(
		t,
		"post1",
		testComment("c1", "body", "t3_post1", ""),
		testComment("c2", "body", "t3_post1", ""),
	)
	_, err = walkDecodedCompleteWithBudget(
		context.Background(), "post1", responsePayload{data: initial}, limits, budget,
		adaptExpansionFetcher(unexpectedFetch(t)), nil, func(Comment) error { return nil },
	)
	assertErrorClass(t, err, ErrorResourceLimit)
	if !errors.Is(err, errRetainedThingBudget) {
		t.Fatalf("walk error = %v, want retained-state budget", err)
	}

	// The failed walk's defer must return its first reservation. Reusing the same
	// process-wide budget for a smaller complete tree proves there is no leaked unit.
	one := testInitial(t, "post1", testComment("c1", "body", "t3_post1", ""))
	stats, err := walkDecodedCompleteWithBudget(
		context.Background(), "post1", responsePayload{data: one}, limits, budget,
		adaptExpansionFetcher(unexpectedFetch(t)), nil, func(Comment) error { return nil },
	)
	if err != nil || stats.Comments != 1 {
		t.Fatalf("second walk stats = %#v, error = %v", stats, err)
	}
}

func TestTraversalBudgetCancellationClassifiesAtWalkBoundary(t *testing.T) {
	t.Parallel()

	budget, err := NewTraversalBudget(TraversalBudgetConfig{MaxInFlightResponseBytes: 1, MaxRetainedThings: 1})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = walkDecodedCompleteWithBudget(
		ctx, "post1", responsePayload{data: testInitial(t, "post1")}, DefaultThingLimits(), budget,
		adaptExpansionFetcher(unexpectedFetch(t)), nil, func(Comment) error { return nil },
	)
	assertErrorClass(t, err, ErrorCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("walk error = %v, want context.Canceled", err)
	}
}

func TestWalkReleasesLeasedInitialResponseBeforeValidationReturn(t *testing.T) {
	t.Parallel()

	budget, err := NewTraversalBudget(TraversalBudgetConfig{MaxInFlightResponseBytes: 10, MaxRetainedThings: 1})
	if err != nil {
		t.Fatalf("NewTraversalBudget() error = %v", err)
	}
	release, err := budget.acquireResponse(context.Background(), 10)
	if err != nil {
		t.Fatalf("acquireResponse() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = walkDecodedCompleteWithBudget(
		ctx,
		"post1",
		responsePayload{data: []byte(`{}`), release: release},
		DefaultThingLimits(),
		budget,
		adaptExpansionFetcher(unexpectedFetch(t)),
		nil,
		func(Comment) error { return nil },
	)
	assertErrorClass(t, err, ErrorCanceled)
	if !budget.responseBytes.TryAcquire(10) {
		t.Fatal("initial response lease leaked on validation return")
	}
	budget.responseBytes.Release(10)
}
