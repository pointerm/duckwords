package acquire

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSourceRetryClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection reset", err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}, want: true},
		{name: "connection closed before response", err: io.EOF, want: true},
		{name: "timeout", err: context.DeadlineExceeded, want: true},
		{name: "temporary DNS", err: &net.DNSError{IsTemporary: true}, want: true},
		{name: "opaque", err: errors.New("opaque")},
		{name: "DNS not found", err: &net.DNSError{IsNotFound: true}},
		{name: "certificate", err: &tls.CertificateVerificationError{Err: errors.New("unknown authority")}},
		{name: "proxy", err: &net.OpError{Op: "proxyconnect", Err: syscall.ECONNREFUSED}},
		{name: "canceled", err: context.Canceled},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := transientSourceTransportError(test.err); got != test.want {
				t.Fatalf("transientSourceTransportError() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestParseSourceRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		value     string
		cap       time.Duration
		want      time.Duration
		wantValid bool
	}{
		{name: "seconds", value: "2", cap: 10 * time.Second, want: 2 * time.Second, wantValid: true},
		{name: "date", value: now.Add(3 * time.Second).Format(http.TimeFormat), cap: 10 * time.Second, want: 3 * time.Second, wantValid: true},
		{name: "capped", value: "60", cap: 10 * time.Second, want: 10 * time.Second, wantValid: true},
		{name: "overflow capped", value: strings.Repeat("9", 30), cap: 10 * time.Second, want: 10 * time.Second, wantValid: true},
		{name: "zero", value: "0", cap: 10 * time.Second},
		{name: "past date", value: now.Add(-time.Second).Format(http.TimeFormat), cap: 10 * time.Second},
		{name: "malformed", value: "later", cap: 10 * time.Second},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, valid := parseSourceRetryAfter(test.value, now, test.cap)
			if got != test.want || valid != test.wantValid {
				t.Fatalf("parseSourceRetryAfter(%q) = %s, %t; want %s, %t", test.value, got, valid, test.want, test.wantValid)
			}
		})
	}
}

func TestSourceRetryPolicyHonorsRetryAfterAndThenSucceeds(t *testing.T) {
	t.Parallel()

	const payload = "alpha\nbeta\n"
	var calls atomic.Int32
	retryBody := &observedBody{reader: strings.NewReader("temporary")}
	client := clientWithTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.UserAgent() != testUserAgent {
			t.Errorf("User-Agent = %q", request.UserAgent())
		}
		if calls.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Retry-After": {"2"}},
				Body:       retryBody,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
		}, nil
	}))
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	var retryEvents []RetryEvent
	policy := defaultSourceRetryPolicy()
	policy.observer = func(event RetryEvent) {
		retryEvents = append(retryEvents, event)
	}
	policy.now = func() time.Time { return now }
	policy.wait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, delay)
		now = now.Add(delay)
		return nil
	}
	spec := Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}
	document, err := loadHTTPSWithRetryPolicy(context.Background(), spec, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   int64(len(payload)),
	}, spec.URL, policy)
	if err != nil {
		t.Fatalf("loadHTTPSWithRetryPolicy() error = %v", err)
	}
	if string(document.Bytes()) != payload || calls.Load() != 2 {
		t.Fatalf("document = %q, calls = %d", document.Bytes(), calls.Load())
	}
	if len(waits) != 1 || waits[0] != 2*time.Second {
		t.Fatalf("retry waits = %v, want [2s]", waits)
	}
	if len(retryEvents) != 1 || retryEvents[0] != (RetryEvent{
		Kind:       KindPosts,
		Reason:     RetryReasonHTTPStatus,
		StatusCode: http.StatusServiceUnavailable,
		Attempt:    2,
		Delay:      2 * time.Second,
	}) {
		t.Fatalf("retry events = %#v, want one sanitized status replay event", retryEvents)
	}
	if retryBody.reads.Load() == 0 || retryBody.closes.Load() != 1 {
		t.Fatalf("retry body reads/closes = %d/%d, want bounded drain and one close", retryBody.reads.Load(), retryBody.closes.Load())
	}
}

func TestExplicitSourceRetryConfigDisablesReplays(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	var observed atomic.Int32
	client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("temporary")),
		}, nil
	}))
	retry := RetryConfig{
		MaxRetries: 0,
		MaxElapsed: time.Second,
		Observer:   func(RetryEvent) { observed.Add(1) },
	}
	document, err := Load(context.Background(), Spec{Kind: KindDictionary, URL: "https://" + dictionaryHost + "/input"}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   32,
		Retry:      &retry,
	})
	if !errors.Is(err, ErrHTTPStatus) || calls.Load() != 1 || observed.Load() != 0 {
		t.Fatalf("document = %+v, error = %v, calls = %d, events = %d; want one attempt and no retry event", document, err, calls.Load(), observed.Load())
	}
}

func TestSourceRetryPolicyStopsAfterTwoRetries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("temporary")),
		}, nil
	}))
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	var waits []time.Duration
	policy := defaultSourceRetryPolicy()
	policy.now = func() time.Time { return now }
	policy.wait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, delay)
		now = now.Add(delay)
		return nil
	}
	spec := Spec{Kind: KindDictionary, URL: "https://" + dictionaryHost + "/input"}
	document, err := loadHTTPSWithRetryPolicy(context.Background(), spec, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   32,
	}, spec.URL, policy)
	if !errors.Is(err, ErrHTTPStatus) || calls.Load() != maxSourceRetries+1 {
		t.Fatalf("document = %+v, error = %v, calls = %d", document, err, calls.Load())
	}
	wantWaits := []time.Duration{sourceInitialBackoff, 2 * sourceInitialBackoff}
	if len(waits) != len(wantWaits) || waits[0] != wantWaits[0] || waits[1] != wantWaits[1] {
		t.Fatalf("retry waits = %v, want %v", waits, wantWaits)
	}
}

func TestSourceRetryPolicyFailsFastForPermanentTransportErrors(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, &net.DNSError{IsNotFound: true}
	}))
	document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   32,
	})
	if !errors.Is(err, ErrTransport) || calls.Load() != 1 {
		t.Fatalf("document = %+v, error = %v, calls = %d", document, err, calls.Load())
	}
}

func TestSourceRetryPolicyCancellationInterruptsBackoff(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"5"}},
			Body:       io.NopCloser(strings.NewReader("slow down")),
		}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	policy := defaultSourceRetryPolicy()
	policy.wait = func(waitCtx context.Context, _ time.Duration) error {
		cancel()
		<-waitCtx.Done()
		return waitCtx.Err()
	}
	spec := Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}
	document, err := loadHTTPSWithRetryPolicy(ctx, spec, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   32,
	}, spec.URL, policy)
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("document = %+v, error = %v, calls = %d", document, err, calls.Load())
	}
}

func TestSourceRetryElapsedBudgetIncludesInitialAndReplayAttempts(t *testing.T) {
	t.Parallel()

	const payload = "alpha\n"
	tests := []struct {
		name       string
		first      func() *http.Response
		wantCode   error
		wantStatus int
		wantCalls  int32
	}{
		{
			name:      "initial attempt cannot outlive total budget",
			wantCode:  ErrTransport,
			wantCalls: 1,
		},
		{
			name: "expired replay preserves prior transient status",
			first: func() *http.Response {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("temporary")),
				}
			},
			wantCode:   ErrHTTPStatus,
			wantStatus: http.StatusServiceUnavailable,
			wantCalls:  2,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
			startedAt := now
			var calls atomic.Int32
			client := clientWithTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				call := calls.Add(1)
				deadline, hasDeadline := request.Context().Deadline()
				if !hasDeadline || time.Until(deadline) > time.Second {
					t.Errorf("attempt %d deadline present = %t, remaining = %s", call, hasDeadline, time.Until(deadline))
				}
				if call == 1 && test.first != nil {
					return test.first(), nil
				}
				// Advancing the injected monotonic policy clock makes exhaustion fully
				// deterministic without sleeping for a wall-clock deadline.
				now = startedAt.Add(time.Second)
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": {"text/plain"}},
					Body:          io.NopCloser(strings.NewReader(payload)),
					ContentLength: int64(len(payload)),
				}, nil
			}))
			policy := defaultSourceRetryPolicy()
			policy.maxElapsed = time.Second
			policy.initialBackoff = time.Nanosecond
			policy.maxBackoff = time.Nanosecond
			policy.now = func() time.Time { return now }
			policy.wait = func(ctx context.Context, delay time.Duration) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				now = now.Add(delay)
				return nil
			}

			spec := Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}
			document, err := loadHTTPSWithRetryPolicy(context.Background(), spec, Config{
				HTTPClient: client,
				UserAgent:  testUserAgent,
				MaxBytes:   int64(len(payload)),
			}, spec.URL, policy)
			if document.Len() != 0 || !errors.Is(err, test.wantCode) || errors.Is(err, ErrCanceled) ||
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || calls.Load() != test.wantCalls {
				t.Fatalf("document bytes = %d, error = %v, calls = %d; want %v, non-cancellation, %d calls", document.Len(), err, calls.Load(), test.wantCode, test.wantCalls)
			}
			var loadErr *LoadError
			if !errors.As(err, &loadErr) || loadErr.StatusCode != test.wantStatus {
				t.Fatalf("load error = %#v, want status %d", loadErr, test.wantStatus)
			}
		})
	}
}

func TestSourceRetryInitialAttemptDeadlineIsNotCallerCancellation(t *testing.T) {
	var calls atomic.Int32
	client := clientWithTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	policy := defaultSourceRetryPolicy()
	policy.maxElapsed = 10 * time.Millisecond
	spec := Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}

	startedAt := time.Now()
	document, err := loadHTTPSWithRetryPolicy(context.Background(), spec, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   32,
	}, spec.URL, policy)
	elapsed := time.Since(startedAt)

	if document.Len() != 0 || !errors.Is(err, ErrTransport) || errors.Is(err, ErrCanceled) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("document bytes = %d, error = %v, calls = %d; want sanitized transport expiry", document.Len(), err, calls.Load())
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("initial attempt elapsed = %s, want short total retry budget", elapsed)
	}
}
