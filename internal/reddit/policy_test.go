package reddit

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRequestPolicyDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	policy, err := NewRequestPolicy(RequestPolicyConfig{})
	if err != nil {
		t.Fatalf("NewRequestPolicy(zero) error = %v", err)
	}
	if policy.baseInterval != time.Second*5/4 || policy.maxRetries != defaultMaxRetries ||
		policy.maxRetryElapsed != defaultMaxRetryElapsed {
		t.Fatalf("zero config selected unexpected defaults: %#v", policy)
	}

	valid := DefaultRequestPolicyConfig()
	tests := []struct {
		name   string
		mutate func(*RequestPolicyConfig)
	}{
		{name: "NaN rate", mutate: func(config *RequestPolicyConfig) { config.RequestsPerSecond = math.NaN() }},
		{name: "rate below minimum", mutate: func(config *RequestPolicyConfig) { config.RequestsPerSecond = minRequestsPerSecond / 2 }},
		{name: "rate above maximum", mutate: func(config *RequestPolicyConfig) { config.RequestsPerSecond = maxRequestsPerSecond + 1 }},
		{name: "negative retries", mutate: func(config *RequestPolicyConfig) { config.MaxRetries = -1 }},
		{name: "excess retries", mutate: func(config *RequestPolicyConfig) { config.MaxRetries = maxConfiguredRetries + 1 }},
		{name: "zero elapsed budget", mutate: func(config *RequestPolicyConfig) { config.MaxRetryElapsed = 0 }},
		{name: "excess elapsed budget", mutate: func(config *RequestPolicyConfig) { config.MaxRetryElapsed = maxRetryElapsed + time.Nanosecond }},
		{name: "zero initial backoff", mutate: func(config *RequestPolicyConfig) { config.InitialBackoff = 0 }},
		{name: "backoff order", mutate: func(config *RequestPolicyConfig) { config.MaxBackoff = config.InitialBackoff - 1 }},
		{name: "excess backoff", mutate: func(config *RequestPolicyConfig) { config.MaxBackoff = maxBackoff + time.Nanosecond }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := NewRequestPolicy(config); !errors.Is(err, ErrRequestPolicyConfig) {
				t.Fatalf("NewRequestPolicy() error = %v, want ErrRequestPolicyConfig", err)
			}
		})
	}
	for _, rate := range []float64{minRequestsPerSecond, maxRequestsPerSecond} {
		config := valid
		config.RequestsPerSecond = rate
		if _, err := NewRequestPolicy(config); err != nil {
			t.Errorf("NewRequestPolicy(rate %v) error = %v", rate, err)
		}
	}

	invalidRate := valid
	invalidRate.RequestsPerSecond = maxRequestsPerSecond + 1
	_, err = NewRequestPolicy(invalidRate)
	wantRateError := "invalid reddit request policy configuration: requests per second must be between 0.1 and 1.5"
	if err == nil || err.Error() != wantRateError {
		t.Errorf("NewRequestPolicy(invalid rate) error = %q, want %q", err, wantRateError)
	}

	if _, err := newRequestPolicy(valid, nil, waitWithTimer, defaultRetryJitter); !errors.Is(err, ErrRequestPolicyConfig) {
		t.Fatalf("newRequestPolicy(nil clock) error = %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	capDelay := 10 * time.Second
	tests := []struct {
		name      string
		value     string
		want      time.Duration
		wantValid bool
	}{
		{name: "delta seconds", value: "7", want: 7 * time.Second, wantValid: true},
		{name: "delta whitespace", value: " 3 ", want: 3 * time.Second, wantValid: true},
		{name: "delta capped", value: "999", want: capDelay, wantValid: true},
		{name: "overflowing decimal capped", value: strings.Repeat("9", 64), want: capDelay, wantValid: true},
		{name: "HTTP date", value: now.Add(4 * time.Second).Format(http.TimeFormat), want: 4 * time.Second, wantValid: true},
		{name: "future date capped", value: now.Add(time.Hour).Format(http.TimeFormat), want: capDelay, wantValid: true},
		{name: "zero", value: "0"},
		{name: "past date", value: now.Add(-time.Second).Format(http.TimeFormat)},
		{name: "negative", value: "-1"},
		{name: "fraction", value: "1.5"},
		{name: "malformed", value: "later"},
		{name: "oversized header", value: strings.Repeat("1", maxPolicyHeaderBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := parseRetryAfter(test.value, now, capDelay)
			if got != test.want || valid != test.wantValid {
				t.Fatalf("parseRetryAfter(%q) = %s, %t; want %s, %t", test.value, got, valid, test.want, test.wantValid)
			}
		})
	}
}

func TestRetryEligibilityMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection reset", err: newError(ErrorTransport, EndpointComments, "post1", 0, &net.OpError{Op: "read", Err: syscall.ECONNRESET}), want: true},
		{name: "connection closed before response", err: newError(ErrorTransport, EndpointComments, "post1", 0, io.EOF), want: true},
		{name: "attempt timeout", err: newError(ErrorTransport, EndpointComments, "post1", 0, context.DeadlineExceeded), want: true},
		{name: "temporary DNS", err: newError(ErrorTransport, EndpointComments, "post1", 0, &net.DNSError{IsTemporary: true}), want: true},
		{name: "opaque transport", err: newError(ErrorTransport, EndpointComments, "post1", 0, errors.New("network"))},
		{name: "DNS not found", err: newError(ErrorTransport, EndpointComments, "post1", 0, &net.DNSError{IsNotFound: true})},
		{name: "certificate verification", err: newError(ErrorTransport, EndpointComments, "post1", 0, &tls.CertificateVerificationError{Err: errors.New("unknown authority")})},
		{name: "proxy configuration", err: newError(ErrorTransport, EndpointComments, "post1", 0, &net.OpError{Op: "proxyconnect", Err: syscall.ECONNREFUSED})},
		{name: "request timeout", err: newError(ErrorTransport, EndpointComments, "post1", http.StatusRequestTimeout, errUnexpectedHTTPStatus), want: true},
		{name: "rate limited", err: newError(ErrorRateLimited, EndpointComments, "post1", http.StatusTooManyRequests, errUnexpectedHTTPStatus), want: true},
		{name: "internal server", err: newError(ErrorServer, EndpointComments, "post1", http.StatusInternalServerError, errUnexpectedHTTPStatus), want: true},
		{name: "bad gateway", err: newError(ErrorServer, EndpointComments, "post1", http.StatusBadGateway, errUnexpectedHTTPStatus), want: true},
		{name: "unavailable", err: newError(ErrorServer, EndpointComments, "post1", http.StatusServiceUnavailable, errUnexpectedHTTPStatus), want: true},
		{name: "gateway timeout", err: newError(ErrorServer, EndpointComments, "post1", http.StatusGatewayTimeout, errUnexpectedHTTPStatus), want: true},
		{name: "not implemented", err: newError(ErrorServer, EndpointComments, "post1", http.StatusNotImplemented, errUnexpectedHTTPStatus)},
		{name: "authentication", err: newError(ErrorAuthentication, EndpointComments, "post1", http.StatusUnauthorized, errUnexpectedHTTPStatus)},
		{name: "forbidden", err: newError(ErrorForbidden, EndpointComments, "post1", http.StatusForbidden, errUnexpectedHTTPStatus)},
		{name: "not found", err: newError(ErrorNotFound, EndpointComments, "post1", http.StatusNotFound, errUnexpectedHTTPStatus)},
		{name: "invalid", err: newError(ErrorInvalidInput, EndpointComments, "post1", http.StatusBadRequest, errUnexpectedHTTPStatus)},
		{name: "protocol", err: newError(ErrorProtocol, EndpointComments, "post1", http.StatusOK, errMalformedJSON)},
		{name: "resource", err: newError(ErrorResourceLimit, EndpointComments, "post1", http.StatusOK, errResponseTooLarge)},
		{name: "canceled", err: newError(ErrorCanceled, EndpointComments, "post1", 0, context.Canceled)},
		{name: "visitor", err: newError(ErrorVisitor, EndpointComments, "post1", 0, errors.New("visitor"))},
		{name: "opaque", err: errors.New("opaque")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableHTTPError(test.err); got != test.want {
				t.Fatalf("retryableHTTPError() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRequestPolicyRetriesWithRetryAfterAndSanitizedObserver(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	var events []RetryEvent
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        2,
		MaxRetryElapsed:   10 * time.Second,
		InitialBackoff:    100 * time.Millisecond,
		MaxBackoff:        5 * time.Second,
		Observer:          func(event RetryEvent) { events = append(events, event) },
	})
	attempts := 0
	payload, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		attempts++
		if attempts == 1 {
			return policyAttemptResult{
				attempted: true,
				headers:   responsePolicyHeaders{retryAfter: "2"},
			}, newError(ErrorRateLimited, EndpointComments, "post1", http.StatusTooManyRequests, errors.New("secret-bearing raw error"))
		}
		return policyAttemptResult{attempted: true, payload: []byte("accepted")}, nil
	})
	if err != nil || string(payload) != "accepted" || attempts != 2 {
		t.Fatalf("policy.do() = %q, %v after %d attempts", payload, err, attempts)
	}
	if got := clock.delays(); !slices.Equal(got, []time.Duration{2 * time.Second}) {
		t.Fatalf("wait delays = %v, want [2s]", got)
	}
	wantEvent := RetryEvent{
		Endpoint: EndpointComments, PostID: "post1", Class: ErrorRateLimited,
		StatusCode: http.StatusTooManyRequests, Attempt: 2, Delay: 2 * time.Second,
	}
	if len(events) != 1 || events[0] != wantEvent {
		t.Fatalf("retry events = %#v, want %#v", events, wantEvent)
	}
	snapshot := policy.Snapshot()
	if snapshot.HTTPAttempts != 2 || snapshot.Retries != 1 || snapshot.ThrottleWaitCount != 0 {
		t.Fatalf("snapshot = %#v, want two attempts and one retry", snapshot)
	}
	if formatted := fmt.Sprintf("%#v", events); strings.Contains(formatted, "secret-bearing") {
		t.Fatalf("observer event leaked underlying error: %s", formatted)
	}
}

func TestRequestPolicyDoesNotShortenRetryAfterToBackoffCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		retryAfter   string
		wantAttempts int
		wantDelays   []time.Duration
	}{
		{
			name:         "server delay beyond backoff is honored",
			retryAfter:   "7",
			wantAttempts: 2,
			wantDelays:   []time.Duration{7 * time.Second},
		},
		{
			name:         "server delay beyond retry budget stops",
			retryAfter:   "60",
			wantAttempts: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newPolicyTestClock()
			policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
				RequestsPerSecond: maxRequestsPerSecond,
				MaxRetries:        1,
				MaxRetryElapsed:   10 * time.Second,
				InitialBackoff:    time.Second,
				MaxBackoff:        2 * time.Second,
			})
			attempts := 0
			_, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
				attempts++
				if attempts == 1 {
					return policyAttemptResult{
						attempted: true,
						headers:   responsePolicyHeaders{retryAfter: test.retryAfter},
					}, newError(ErrorRateLimited, EndpointComments, "post1", http.StatusTooManyRequests, errUnexpectedHTTPStatus)
				}
				return policyAttemptResult{attempted: true, payload: []byte("accepted")}, nil
			})
			if test.wantAttempts == 2 && err != nil {
				t.Fatalf("policy.do() error = %v", err)
			}
			if test.wantAttempts == 1 && err == nil {
				t.Fatal("policy.do() error = nil, want original rate-limit failure")
			}
			if attempts != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, test.wantAttempts)
			}
			if got := clock.delays(); !slices.Equal(got, test.wantDelays) {
				t.Fatalf("delays = %v, want %v", got, test.wantDelays)
			}
		})
	}
}

func TestRequestPolicyRetriesTruncatedHTTPAttempt(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	var calls atomic.Int32
	httpClient := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := io.ReadCloser(io.NopCloser(strings.NewReader(`{"ok":true}`)))
		if calls.Add(1) == 1 {
			body = &failingResponseBody{readErr: io.ErrUnexpectedEOF}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})}
	payload, err := policy.do(context.Background(), EndpointComments, "post1", func(attemptCtx context.Context) (policyAttemptResult, error) {
		request, requestErr := http.NewRequestWithContext(attemptCtx, http.MethodGet, "https://example.invalid/comments/post1", nil)
		if requestErr != nil {
			return policyAttemptResult{}, requestErr
		}
		return executePayloadAttempt(attemptCtx, httpClient, request, EndpointComments, "post1", 1024, nil)
	})
	if err != nil || string(payload) != `{"ok":true}` || calls.Load() != 2 {
		t.Fatalf("policy.do() = %q, %v after %d HTTP calls", payload, err, calls.Load())
	}
	if got := policy.Snapshot(); got.HTTPAttempts != 2 || got.Retries != 1 {
		t.Fatalf("snapshot = %#v, want truncated attempt plus one replay", got)
	}
}

func TestRequestPolicyBackoffCapAndRetryExhaustion(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        3,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        2 * time.Second,
	})
	attempts := 0
	wantErr := newError(ErrorServer, EndpointComments, "post1", http.StatusServiceUnavailable, errUnexpectedHTTPStatus)
	_, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, wantErr
	})
	if err != wantErr || attempts != 4 {
		t.Fatalf("policy.do() error = %v, attempts = %d; want original error and 4 attempts", err, attempts)
	}
	if got, want := clock.delays(), []time.Duration{time.Second, 2 * time.Second, 2 * time.Second}; !slices.Equal(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	if got := policy.Snapshot(); got.HTTPAttempts != 4 || got.Retries != 3 {
		t.Fatalf("snapshot = %#v, want four attempts and three retries", got)
	}
}

func TestRequestPolicyElapsedBudgetPreventsLateAttempt(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        5,
		MaxRetryElapsed:   750 * time.Millisecond,
		InitialBackoff:    500 * time.Millisecond,
		MaxBackoff:        2 * time.Second,
	})
	wantErr := newError(ErrorServer, EndpointComments, "post1", http.StatusServiceUnavailable, errUnexpectedHTTPStatus)
	attempts := 0
	_, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, wantErr
	})
	if err != wantErr || attempts != 2 {
		t.Fatalf("policy.do() error = %v, attempts = %d; want budget stop after 2", err, attempts)
	}
	rate := float64(maxRequestsPerSecond)
	baseInterval := time.Duration(float64(time.Second) / rate)
	wantDelays := []time.Duration{500 * time.Millisecond, baseInterval - 500*time.Millisecond}
	if got := clock.delays(); !slices.Equal(got, wantDelays) {
		t.Fatalf("delays = %v, want bounded backoff and remaining limiter wait %v", got, wantDelays)
	}
}

func TestRequestPolicyInitialLimiterWaitHonorsElapsedBudget(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: 0.5,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Second,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        time.Millisecond,
	})
	attempts := 0
	attempt := func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, nil
	}
	if _, err := policy.do(context.Background(), EndpointComments, "post1", attempt); err != nil {
		t.Fatalf("first policy.do() error = %v", err)
	}

	_, err := policy.do(context.Background(), EndpointComments, "post2", attempt)
	assertClientError(t, err, ErrorTransport, EndpointComments, 0)
	if !errors.Is(err, errRetryBudget) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second policy.do() error = %v, want internal non-cancellation budget failure", err)
	}
	if attempts != 1 {
		t.Fatalf("HTTP attempts = %d, want no initial request after limiter delay exceeds session budget", attempts)
	}
	if got := clock.delays(); len(got) != 0 {
		t.Fatalf("limiter delays = %v, want refusal before an over-budget wait", got)
	}
}

func TestRequestPolicyObserverCannotConsumeBudgetThenStartBackoff(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	wantErr := newError(ErrorServer, EndpointComments, "post1", http.StatusServiceUnavailable, errUnexpectedHTTPStatus)
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   2 * time.Second,
		InitialBackoff:    500 * time.Millisecond,
		MaxBackoff:        time.Second,
		Observer: func(RetryEvent) {
			// Model a slow synchronous logging sink without a wall-clock sleep.
			clock.jump(3 * time.Second)
		},
	})
	attempts := 0
	_, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, wantErr
	})
	if err != wantErr {
		t.Fatalf("policy.do() error = %v, want original transient error", err)
	}
	if attempts != 1 {
		t.Fatalf("HTTP attempts = %d, want no replay after observer consumed budget", attempts)
	}
	if got := clock.delays(); len(got) != 0 {
		t.Fatalf("backoff delays = %v, want none after observer consumed budget", got)
	}
}

func TestRequestPolicyBackoffReceivesRemainingBudgetContext(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	wantErr := newError(ErrorTransport, EndpointComments, "post1", 0, io.ErrUnexpectedEOF)
	var sawDeadline atomic.Bool
	policy, err := newRequestPolicy(RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   2 * time.Second,
		InitialBackoff:    500 * time.Millisecond,
		MaxBackoff:        time.Second,
	}, clock.nowTime, func(waitCtx context.Context, _ time.Duration) error {
		_, hasDeadline := waitCtx.Deadline()
		sawDeadline.Store(hasDeadline)
		// Model a backoff sleeper that unblocks as its retry budget expires.
		clock.jump(3 * time.Second)
		return context.DeadlineExceeded
	}, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	_, err = policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, wantErr
	})
	if err != wantErr {
		t.Fatalf("policy.do() error = %v, want original transient error", err)
	}
	if !sawDeadline.Load() || attempts != 1 {
		t.Fatalf("backoff deadline observed = %t, attempts = %d; want true and 1", sawDeadline.Load(), attempts)
	}
}

func TestRequestPolicyBudgetExpiresWhileWaitingForSharedPermit(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   10 * time.Second,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	session := policy.newRetrySession()
	previousErr := newError(ErrorTransport, EndpointComments, "post1", 0, io.ErrUnexpectedEOF)

	// Hold the process-wide permit. The endpoint gate signal proves the replay
	// passed its initial budget check and reached the permit acquisition boundary.
	<-policy.permit
	gateEntered := make(chan struct{})
	result := make(chan error, 1)
	var attempts atomic.Int32
	go func() {
		_, err := session.doAfter(
			context.Background(),
			EndpointComments,
			"post1",
			previousErr,
			func(context.Context) (func(), error) {
				close(gateEntered)
				return func() {}, nil
			},
			func(context.Context) (policyAttemptResult, error) {
				attempts.Add(1)
				return policyAttemptResult{attempted: true}, nil
			},
		)
		result <- err
	}()
	<-gateEntered
	clock.advance(11 * time.Second)
	policy.permit <- struct{}{}

	if err := <-result; err != previousErr {
		t.Fatalf("doAfter() error = %v, want original transient error", err)
	}
	if attempts.Load() != 0 {
		t.Fatalf("HTTP attempts = %d, want no late replay", attempts.Load())
	}
}

func TestRequestPolicyBudgetBoundsReplayHTTPAttempt(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   20 * time.Millisecond,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        time.Millisecond,
	})
	previousErr := newError(ErrorTransport, EndpointComments, "post1", 0, io.ErrUnexpectedEOF)
	attempts := 0
	_, err := policy.newRetrySession().doAfter(
		context.Background(),
		EndpointComments,
		"post1",
		previousErr,
		nil,
		func(attemptCtx context.Context) (policyAttemptResult, error) {
			attempts++
			if _, ok := attemptCtx.Deadline(); !ok {
				t.Error("replay attempt context has no retry-budget deadline")
			}
			clock.advance(21 * time.Millisecond)
			return policyAttemptResult{attempted: true}, newError(ErrorTransport, EndpointComments, "post1", 0, io.ErrUnexpectedEOF)
		},
	)
	if err != previousErr {
		t.Fatalf("doAfter() error = %v, want original transient error", err)
	}
	if attempts != 1 {
		t.Fatalf("replay attempts = %d, want 1", attempts)
	}
}

func TestRequestPolicyCancellationDuringBackoff(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	ctx, cancel := context.WithCancel(context.Background())
	policy, err := newRequestPolicy(RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	}, clock.nowTime, func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		return waitCtx.Err()
	}, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	_, err = policy.do(ctx, EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, newError(ErrorTransport, EndpointComments, "post1", 0, io.ErrUnexpectedEOF)
	})
	assertClientError(t, err, ErrorCanceled, EndpointComments, 0)
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("policy.do() error = %v, attempts = %d; want canceled before replay", err, attempts)
	}
}

func TestRequestPolicyCancellationDuringLimiterWait(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	policy, err := newRequestPolicy(RequestPolicyConfig{
		RequestsPerSecond: 1,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	}, clock.nowTime, func(waitCtx context.Context, _ time.Duration) error {
		waits++
		cancel()
		return waitCtx.Err()
	}, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	attempt := func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, nil
	}
	if _, err := policy.do(ctx, EndpointComments, "post1", attempt); err != nil {
		t.Fatalf("first policy.do() error = %v", err)
	}
	_, err = policy.do(ctx, EndpointComments, "post1", attempt)
	assertClientError(t, err, ErrorCanceled, EndpointComments, 0)
	if !errors.Is(err, context.Canceled) || attempts != 1 || waits != 1 {
		t.Fatalf("second policy.do() error=%v attempts=%d waits=%d", err, attempts, waits)
	}
}

func TestRequestPolicyCallerCancellationOverridesSuccessfulAttempt(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	_, err := policy.do(ctx, EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		cancel()
		return policyAttemptResult{attempted: true, payload: []byte("must-not-accept")}, nil
	})
	assertClientError(t, err, ErrorCanceled, EndpointComments, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("policy.do() error = %v, want context cancellation", err)
	}
}

func TestRequestPolicyRateHeadersOnlySlowConfiguredCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		headers    responsePolicyHeaders
		wantSecond time.Duration
	}{
		{
			name: "remaining capacity slows schedule",
			headers: responsePolicyHeaders{
				rateUsed: "99", rateRemaining: "1", rateReset: "10",
			},
			wantSecond: 10 * time.Second,
		},
		{
			name: "exhausted capacity blocks until reset",
			headers: responsePolicyHeaders{
				rateUsed: "100", rateRemaining: "0", rateReset: "8",
			},
			wantSecond: 8 * time.Second,
		},
		{
			name: "abundant capacity cannot exceed configured ceiling",
			headers: responsePolicyHeaders{
				rateUsed: "1", rateRemaining: "100", rateReset: "1",
			},
			wantSecond: time.Second,
		},
		{
			name: "partial headers fall back",
			headers: responsePolicyHeaders{
				rateRemaining: "1", rateReset: "10",
			},
			wantSecond: time.Second,
		},
		{
			name: "malformed headers fall back",
			headers: responsePolicyHeaders{
				rateUsed: "NaN", rateRemaining: "0", rateReset: "10",
			},
			wantSecond: time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newPolicyTestClock()
			policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
				RequestsPerSecond: 1,
				MaxRetries:        0,
				MaxRetryElapsed:   time.Minute,
				InitialBackoff:    time.Second,
				MaxBackoff:        time.Second,
			})
			attempt := 0
			for attempt < 2 {
				_, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
					attempt++
					result := policyAttemptResult{attempted: true}
					if attempt == 1 {
						result.headers = test.headers
					}
					return result, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := clock.delays(); !slices.Equal(got, []time.Duration{test.wantSecond}) {
				t.Fatalf("delays = %v, want [%s]", got, test.wantSecond)
			}
			snapshot := policy.Snapshot()
			if snapshot.ThrottleWaitCount != 1 || snapshot.ThrottleWait != test.wantSecond {
				t.Fatalf("snapshot = %#v, want one %s throttle wait", snapshot, test.wantSecond)
			}
		})
	}
}

func TestRequestPolicyConcurrentHeaderUpdateTightensWaitingReservation(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	throttleEntered := make(chan struct{})
	releaseThrottle := make(chan struct{})
	var waitCalls atomic.Int32
	policy, err := newRequestPolicy(RequestPolicyConfig{
		RequestsPerSecond: 1,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	}, clock.nowTime, func(ctx context.Context, delay time.Duration) error {
		if waitCalls.Add(1) == 1 {
			close(throttleEntered)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseThrottle:
			}
		}
		clock.advance(delay)
		return nil
	}, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		t.Fatal(err)
	}

	firstAttempt := make(chan struct{})
	returnFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
			close(firstAttempt)
			<-returnFirst
			return policyAttemptResult{attempted: true, headers: responsePolicyHeaders{
				rateUsed: "100", rateRemaining: "0", rateReset: "10",
			}}, nil
		})
		firstDone <- callErr
	}()
	<-firstAttempt

	secondAttemptAt := make(chan time.Time, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, callErr := policy.do(context.Background(), EndpointComments, "post2", func(context.Context) (policyAttemptResult, error) {
			secondAttemptAt <- clock.nowTime()
			return policyAttemptResult{attempted: true}, nil
		})
		secondDone <- callErr
	}()
	<-throttleEntered
	close(returnFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first policy.do() error = %v", err)
	}
	close(releaseThrottle)
	if err := <-secondDone; err != nil {
		t.Fatalf("second policy.do() error = %v", err)
	}
	if got := (<-secondAttemptAt).Sub(time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)); got != 10*time.Second {
		t.Fatalf("second attempt started at +%s, want tightened +10s", got)
	}
	if got := clock.delays(); !slices.Equal(got, []time.Duration{time.Second, 9 * time.Second}) {
		t.Fatalf("limiter waits = %v, want base wait followed by tightened remainder", got)
	}
}

func TestRequestPolicyRetryAfterBlocksOtherLogicalRequests(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: 1,
		MaxRetries:        0,
		MaxRetryElapsed:   10 * time.Second,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	_, firstErr := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
		return policyAttemptResult{
			attempted: true,
			headers:   responsePolicyHeaders{retryAfter: "7"},
		}, newError(ErrorRateLimited, EndpointComments, "post1", http.StatusTooManyRequests, errUnexpectedHTTPStatus)
	})
	assertClientError(t, firstErr, ErrorRateLimited, EndpointComments, http.StatusTooManyRequests)

	attempts := 0
	if _, err := policy.do(context.Background(), EndpointComments, "post2", func(context.Context) (policyAttemptResult, error) {
		attempts++
		return policyAttemptResult{attempted: true}, nil
	}); err != nil {
		t.Fatalf("second policy.do() error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("second attempts = %d, want 1", attempts)
	}
	if got := clock.delays(); !slices.Equal(got, []time.Duration{7 * time.Second}) {
		t.Fatalf("shared cooldown waits = %v, want [7s]", got)
	}
}

func TestRequestPolicyOutOfOrderHeadersCannotRelaxActiveConstraint(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: 1,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	policy.observeRateHeaders(responsePolicyHeaders{rateUsed: "99", rateRemaining: "1", rateReset: "10"})
	// This logically older response finishes later. Its 2s spacing and longer
	// reset window must not relax the already-active 10s spacing.
	policy.observeRateHeaders(responsePolicyHeaders{rateUsed: "90", rateRemaining: "10", rateReset: "20"})

	for range 2 {
		if _, err := policy.do(context.Background(), EndpointComments, "post1", func(context.Context) (policyAttemptResult, error) {
			return policyAttemptResult{attempted: true}, nil
		}); err != nil {
			t.Fatalf("policy.do() error = %v", err)
		}
	}
	if got := clock.delays(); !slices.Equal(got, []time.Duration{10 * time.Second, 10 * time.Second}) {
		t.Fatalf("adaptive waits = %v, want strict cadence [10s 10s]", got)
	}
}

func TestRequestPolicyRejectsDuplicateControlHeaders(t *testing.T) {
	t.Parallel()

	header := http.Header{
		"X-Ratelimit-Used":      {"1", "2"},
		"X-Ratelimit-Remaining": {"3"},
		"X-Ratelimit-Reset":     {"4"},
		"Retry-After":           {"5", "6"},
	}
	got := policyHeaders(header)
	if got.rateUsed != "" || got.retryAfter != "" || got.rateRemaining != "3" || got.rateReset != "4" {
		t.Fatalf("policyHeaders() = %#v, want duplicate values rejected", got)
	}
}

func TestOAuthAndAPISharePolicyAndRetryOnlyAttempts(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        2,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        2 * time.Second,
	})
	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/access_token":
			clientID, secret, ok := request.BasicAuth()
			if request.Method != http.MethodPost || !ok || clientID != testOAuthClientID || secret != testOAuthClientSecret {
				t.Errorf("OAuth retry request did not recreate the confidential-client contract")
			}
			if err := request.ParseForm(); err != nil || request.Form.Get("grant_type") != "client_credentials" {
				t.Errorf("OAuth retry form = %v, error %v", request.Form, err)
			}
			if tokenCalls.Add(1) == 1 {
				response.Header().Set("Retry-After", "1")
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(response, `{"access_token":"shared-token","token_type":"bearer","expires_in":3600}`)
		case "/comments/post1":
			apiCalls.Add(1)
			_, _ = response.Write(testInitial(t, "post1",
				testComment("comment1", "visited once", "t3_post1", ""),
			))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newPolicyTestClient(t, server, policy)
	visits := 0
	stats, err := client.WalkComments(context.Background(), "post1", func(Comment) error {
		visits++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if tokenCalls.Load() != 2 || apiCalls.Load() != 1 || visits != 1 || stats.BodiesVisited != 1 {
		t.Fatalf("token calls=%d API calls=%d visits=%d stats=%#v", tokenCalls.Load(), apiCalls.Load(), visits, stats)
	}
	if got := policy.Snapshot(); got.HTTPAttempts != 3 || got.Retries != 1 {
		t.Fatalf("shared policy snapshot = %#v, want three attempts and one retry", got)
	}
}

func TestAuthenticationReplaySharesGenericRetryBudget(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        2,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/access_token" {
			call := tokenCalls.Add(1)
			_, _ = fmt.Fprintf(response, `{"access_token":"token-%d","token_type":"bearer","expires_in":3600}`, call)
			return
		}
		if request.URL.Path != "/comments/post1" {
			http.NotFound(response, request)
			return
		}
		switch apiCalls.Add(1) {
		case 1, 3, 4:
			response.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			response.WriteHeader(http.StatusUnauthorized)
		default:
			t.Error("unexpected API replay beyond bounded policy")
			_, _ = response.Write(testEmptyInitial(t, "post1"))
		}
	}))
	defer server.Close()

	client := newPolicyTestClient(t, server, policy)
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	assertClientError(t, err, ErrorServer, EndpointComments, http.StatusServiceUnavailable)
	if tokenCalls.Load() != 2 || apiCalls.Load() != 4 {
		t.Fatalf("token calls = %d, API calls = %d; want 2 and MaxRetries+2", tokenCalls.Load(), apiCalls.Load())
	}
	if got := policy.Snapshot(); got.HTTPAttempts != 6 || got.Retries != 3 {
		t.Fatalf("snapshot = %#v, want two OAuth + four API attempts and three replays", got)
	}
}

func TestAuthenticationRefreshIsBoundedByAPIReplayBudget(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   10 * time.Second,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	var apiCalls atomic.Int32
	var refreshDeadlineNanos atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/access_token":
			_, _ = io.WriteString(response, `{"access_token":"token-2","token_type":"bearer","expires_in":3600}`)
		case "/comments/post1":
			if apiCalls.Add(1) == 1 {
				clock.advance(8 * time.Second)
				response.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = response.Write(testEmptyInitial(t, "post1"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newPolicyTestClient(t, server, policy)
	primeTokenSource(client.tokenSource, "token-1")
	baseTransport := client.tokenSource.client.Transport
	client.tokenSource.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/v1/access_token" {
			deadline, ok := request.Context().Deadline()
			if !ok {
				t.Error("refresh request has no API-budget deadline")
			} else {
				refreshDeadlineNanos.Store(int64(time.Until(deadline)))
			}
		}
		return baseTransport.RoundTrip(request)
	})
	if _, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil }); err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if apiCalls.Load() != 2 {
		t.Fatalf("API calls = %d, want one 401 and one replay", apiCalls.Load())
	}
	refreshDeadline := time.Duration(refreshDeadlineNanos.Load())
	if refreshDeadline <= 0 || refreshDeadline > 2*time.Second {
		t.Fatalf("refresh deadline = %s, want remaining API budget in (0, 2s]", refreshDeadline)
	}
}

func TestAuthenticationRefreshBudgetExpiryPreservesOriginalUnauthorized(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   10 * time.Second,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	})
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/access_token":
			<-request.Context().Done()
		case "/comments/post1":
			apiCalls.Add(1)
			clock.advance(10*time.Second - time.Nanosecond)
			response.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newPolicyTestClient(t, server, policy)
	primeTokenSource(client.tokenSource, "token-1")
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	assertClientError(t, err, ErrorAuthentication, EndpointComments, http.StatusUnauthorized)
	if apiCalls.Load() != 1 {
		t.Fatalf("API calls = %d, want no bearer replay after refresh budget expiry", apiCalls.Load())
	}
}

func TestAuthenticationReplayObserverCannotStartRefreshAfterBudgetExpiry(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	var tokenCalls atomic.Int32
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   10 * time.Second,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
		Observer: func(event RetryEvent) {
			if event.StatusCode == http.StatusUnauthorized {
				clock.jump(10 * time.Second)
			}
		},
	})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/access_token":
			tokenCalls.Add(1)
			_, _ = io.WriteString(response, `{"access_token":"token-2","token_type":"bearer","expires_in":3600}`)
		case "/comments/post1":
			response.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newPolicyTestClient(t, server, policy)
	primeTokenSource(client.tokenSource, "token-1")
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	assertClientError(t, err, ErrorAuthentication, EndpointComments, http.StatusUnauthorized)
	if tokenCalls.Load() != 0 {
		t.Fatalf("token refresh calls = %d, want none after observer exhausted API budget", tokenCalls.Load())
	}
}

func TestClientDoesNotRetryAcceptedMalformedPayloadOrVisitor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		visitor func(Comment) error
		class   ErrorClass
	}{
		{name: "malformed payload", payload: []byte(`{"truncated":`), visitor: func(Comment) error { return nil }, class: ErrorProtocol},
		{
			name:    "visitor failure",
			payload: testInitial(t, "post1", testComment("comment1", "body", "t3_post1", "")),
			visitor: func(Comment) error { return errors.New("visitor failure") },
			class:   ErrorVisitor,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newPolicyTestClock()
			policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
				RequestsPerSecond: maxRequestsPerSecond,
				MaxRetries:        3,
				MaxRetryElapsed:   time.Minute,
				InitialBackoff:    time.Second,
				MaxBackoff:        time.Second,
			})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write(test.payload)
			}))
			defer server.Close()
			client := newPolicyTestClient(t, server, policy)
			primeTokenSource(client.tokenSource, testClientToken)
			_, err := client.WalkComments(context.Background(), "post1", test.visitor)
			assertClientError(t, err, test.class, EndpointComments, 0)
			if calls.Load() != 1 || policy.Snapshot().Retries != 0 {
				t.Fatalf("HTTP calls = %d, snapshot = %#v; accepted payload work must not replay", calls.Load(), policy.Snapshot())
			}
		})
	}
}

func TestMoreChildrenRetryReleasesAndReacquiresGate(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	var client *Client
	var gateReleasedDuringBackoff atomic.Bool
	var retryWaits atomic.Int32
	policy, err := newRequestPolicy(RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        1,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Second,
		MaxBackoff:        time.Second,
	}, clock.nowTime, func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if delay == time.Second {
			retryWaits.Add(1)
			gateReleasedDuringBackoff.Store(client != nil && len(client.moreGate) == 0)
		}
		clock.advance(delay)
		return nil
	}, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		t.Fatal(err)
	}

	var moreCalls atomic.Int32
	var gateHeldEveryAttempt atomic.Bool
	gateHeldEveryAttempt.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/comments/post1":
			_, _ = response.Write(testInitial(t, "post1", testMore([]string{"extra"}, 1, "t3_post1")))
		case "/api/morechildren":
			if client == nil || len(client.moreGate) != 1 {
				gateHeldEveryAttempt.Store(false)
			}
			if moreCalls.Add(1) == 1 {
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = response.Write(testMoreResponse(t, nil,
				testComment("extra", "expanded exactly once", "t3_post1", nil),
			))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client = newPolicyTestClient(t, server, policy)
	primeTokenSource(client.tokenSource, testClientToken)

	visits := 0
	stats, err := client.WalkComments(context.Background(), "post1", func(Comment) error {
		visits++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if moreCalls.Load() != 2 || stats.MoreRequests != 1 || visits != 1 {
		t.Fatalf("more calls=%d stats=%#v visits=%d", moreCalls.Load(), stats, visits)
	}
	if retryWaits.Load() != 1 || !gateReleasedDuringBackoff.Load() || !gateHeldEveryAttempt.Load() || len(client.moreGate) != 0 {
		t.Fatalf("gate state: retry waits=%d released=%t held=%t final=%d", retryWaits.Load(), gateReleasedDuringBackoff.Load(), gateHeldEveryAttempt.Load(), len(client.moreGate))
	}
}

func TestClientRequiresSharedRequestPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	config := validClientConfig(t, server)
	config.RequestPolicy = newFastTestRequestPolicy()
	if config.RequestPolicy == config.TokenSource.requestPolicy {
		t.Fatal("test setup unexpectedly reused request policy")
	}
	if _, err := newTestClient(config, server.URL); !errors.Is(err, ErrClientConfig) {
		t.Fatalf("newTestClient(mismatched policy) error = %v, want ErrClientConfig", err)
	}
}

func FuzzRetryAfter(f *testing.F) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	for _, seed := range []string{"", "0", "1", "999999999999999999999", now.Add(time.Second).Format(http.TimeFormat), "later", "\r\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		delay, valid := parseRetryAfter(value, now, time.Minute)
		if valid && (delay <= 0 || delay > time.Minute) {
			t.Fatalf("parseRetryAfter(%q) = %s, true; want delay in (0, 1m]", value, delay)
		}
	})
}

func FuzzRateLimitHeaderNumbers(f *testing.F) {
	for _, seed := range []string{"", "0", "1", "1.5", "NaN", "Inf", "-1", " 2 "} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		number, valid := parseNonNegativeFloat(value)
		if valid && number < 0 {
			t.Fatalf("parseNonNegativeFloat(%q) = %f, true", value, number)
		}
	})
}

type policyTestClock struct {
	mu     sync.Mutex
	now    time.Time
	waited []time.Duration
}

func newPolicyTestClock() *policyTestClock {
	return &policyTestClock{now: time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)}
}

func (clock *policyTestClock) nowTime() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *policyTestClock) wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.advance(delay)
	return nil
}

func (clock *policyTestClock) advance(delay time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	clock.waited = append(clock.waited, delay)
	clock.mu.Unlock()
}

func (clock *policyTestClock) jump(delay time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	clock.mu.Unlock()
}

func (clock *policyTestClock) delays() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return slices.Clone(clock.waited)
}

func mustPolicyForTest(t testing.TB, clock *policyTestClock, config RequestPolicyConfig) *RequestPolicy {
	t.Helper()
	policy, err := newRequestPolicy(config, clock.nowTime, clock.wait, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		t.Fatalf("newRequestPolicy() error = %v", err)
	}
	return policy
}

func newPolicyTestClient(t testing.TB, server *httptest.Server, policy *RequestPolicy) *Client {
	t.Helper()
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	tokenSource, err := newTestTokenSource(TokenConfig{
		ClientID:      testOAuthClientID,
		ClientSecret:  testOAuthClientSecret,
		UserAgent:     testClientUserAgent,
		HTTPClient:    httpClient,
		RequestPolicy: policy,
	}, server.URL+"/api/v1/access_token", time.Now)
	if err != nil {
		t.Fatalf("newTestTokenSource() error = %v", err)
	}
	client, err := newTestClient(ClientConfig{
		HTTPClient:    httpClient,
		TokenSource:   tokenSource,
		RequestPolicy: policy,
	}, server.URL)
	if err != nil {
		t.Fatalf("newTestClient() error = %v", err)
	}
	return client
}
