package reddit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecuteJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		limit       int64
		wantClass   ErrorClass
		wantCause   error
	}{
		{name: "success", status: 200, contentType: "application/json; charset=utf-8", body: `{"value":"ok"}`, limit: 64},
		{name: "wrong content type", status: 200, contentType: "text/html", body: `{}`, limit: 64, wantClass: ErrorProtocol, wantCause: errUnexpectedContentType},
		{name: "malformed", status: 200, contentType: "application/json", body: `{`, limit: 64, wantClass: ErrorProtocol, wantCause: errMalformedJSON},
		{name: "second value", status: 200, contentType: "application/json", body: `{} {}`, limit: 64, wantClass: ErrorProtocol, wantCause: errMalformedJSON},
		{name: "oversized", status: 200, contentType: "application/json", body: `{"value":"too large"}`, limit: 4, wantClass: ErrorResourceLimit, wantCause: errResponseTooLarge},
		{name: "created is not the endpoint contract", status: 201, contentType: "application/json", body: `{"value":"ok"}`, limit: 64, wantClass: ErrorProtocol, wantCause: errUnexpectedHTTPStatus},
		{name: "accepted is not the endpoint contract", status: 202, contentType: "application/json", body: `{"value":"ok"}`, limit: 64, wantClass: ErrorProtocol, wantCause: errUnexpectedHTTPStatus},
		{name: "partial content cannot prove completeness", status: 206, contentType: "application/json", body: `{"value":"ok"}`, limit: 64, wantClass: ErrorProtocol, wantCause: errUnexpectedHTTPStatus},
		{name: "authentication", status: 401, contentType: "text/html", body: "planted-secret-body", limit: 64, wantClass: ErrorAuthentication},
		{name: "forbidden", status: 403, contentType: "application/json", body: `{}`, limit: 64, wantClass: ErrorForbidden},
		{name: "not found", status: 404, contentType: "application/json", body: `{}`, limit: 64, wantClass: ErrorNotFound},
		{name: "rate limited", status: 429, contentType: "application/json", body: `{}`, limit: 64, wantClass: ErrorRateLimited},
		{name: "server", status: 503, contentType: "application/json", body: `{}`, limit: 64, wantClass: ErrorServer},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", test.contentType)
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext() error = %v", err)
			}
			var decoded struct{ Value string }
			err = executeJSON(context.Background(), server.Client(), request, EndpointComments, "abc123", test.limit, &decoded)
			if test.wantClass == "" {
				if err != nil || decoded.Value != "ok" {
					t.Fatalf("executeJSON() = decoded %+v, error %v", decoded, err)
				}
				return
			}
			var adapterErr *Error
			if !errors.As(err, &adapterErr) || adapterErr.Class != test.wantClass {
				t.Fatalf("executeJSON() error = %v, want class %q", err, test.wantClass)
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("executeJSON() error = %v, want cause %v", err, test.wantCause)
			}
			if strings.Contains(err.Error(), test.body) && test.body != "" {
				t.Fatalf("executeJSON() error leaked body: %q", err)
			}
		})
	}
}

func TestExecuteJSONHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	err = executeJSON(ctx, http.DefaultClient, request, EndpointComments, "abc123", 64, &struct{}{})
	var adapterErr *Error
	if !errors.As(err, &adapterErr) || adapterErr.Class != ErrorCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("executeJSON() error = %v, want canceled adapter error", err)
	}
}

func TestExecutePayloadValidatesBeforeNetwork(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		called.Store(true)
		return nil, errors.New("unexpected network call")
	})}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	tests := []struct {
		name    string
		ctx     context.Context
		client  *http.Client
		request *http.Request
		limit   int64
		class   ErrorClass
	}{
		{name: "nil context", client: client, request: request, limit: 1, class: ErrorInvalidInput},
		{name: "nil client", ctx: context.Background(), request: request, limit: 1, class: ErrorInvalidInput},
		{name: "nil request", ctx: context.Background(), client: client, limit: 1, class: ErrorInvalidInput},
		{name: "invalid limit", ctx: context.Background(), client: client, request: request, class: ErrorResourceLimit},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := executePayload(test.ctx, test.client, test.request, EndpointComments, "abc123", test.limit)
			var adapterErr *Error
			if !errors.As(err, &adapterErr) || adapterErr.Class != test.class {
				t.Fatalf("executePayload() error = %v, want class %q", err, test.class)
			}
		})
	}
	t.Cleanup(func() {
		if called.Load() {
			t.Fatal("executePayload() made a network call for invalid input")
		}
	})
}

func TestExecutePayloadReportsReadAndCloseFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      io.ReadCloser
		wantCause error
	}{
		{name: "read", body: &failingResponseBody{readErr: errors.New("planted read detail")}, wantCause: errResponseRead},
		{name: "close", body: &failingResponseBody{payload: []byte(`{}`), closeErr: errors.New("planted close detail")}, wantCause: errResponseClose},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       test.body,
				}, nil
			})}
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext() error = %v", err)
			}
			_, err = executePayload(context.Background(), client, request, EndpointComments, "abc123", 64)
			if !errors.Is(err, test.wantCause) {
				t.Fatalf("executePayload() error = %v, want cause %v", err, test.wantCause)
			}
			if strings.Contains(err.Error(), "planted") {
				t.Fatalf("executePayload() leaked failure detail: %q", err)
			}
		})
	}
}

func TestExecutePayloadRejectsAmbiguousContentTypeBeforeRead(t *testing.T) {
	t.Parallel()

	body := newTrackingResponseBody([]byte(`{}`), nil, nil)
	client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json", "text/plain"}},
			Body:       body,
		}, nil
	})}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	_, err = executePayload(context.Background(), client, request, EndpointComments, "abc123", 64)
	assertErrorClass(t, err, ErrorProtocol)
	if !errors.Is(err, errUnexpectedContentType) {
		t.Fatalf("executePayload() error = %v, want unexpected content type", err)
	}
	if body.reads.Load() != 0 || body.closes.Load() != 1 {
		t.Fatalf("response body reads=%d closes=%d, want 0/1", body.reads.Load(), body.closes.Load())
	}
}

func TestExecutePayloadResponseBodyLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		contentType   string
		payload       []byte
		readErr       error
		contentLength int64
		limit         int64
		wantClass     ErrorClass
		wantRead      bool
	}{
		{name: "status rejected before read", status: http.StatusUnauthorized, contentType: "application/json", payload: []byte("must-not-read"), limit: 64, wantClass: ErrorAuthentication},
		{name: "retryable status is bounded-drained", status: http.StatusServiceUnavailable, contentType: "application/json", payload: []byte("small error"), limit: 64, wantClass: ErrorServer, wantRead: true},
		{name: "declared large retryable body is not drained", status: http.StatusServiceUnavailable, contentType: "application/json", payload: []byte("must-not-read"), contentLength: maxRetryableResponseDrainBytes + 1, limit: 64, wantClass: ErrorServer},
		{name: "unselected server status is not drained", status: http.StatusNotImplemented, contentType: "application/json", payload: []byte("must-not-read"), limit: 64, wantClass: ErrorServer},
		{name: "partial content rejected before read", status: http.StatusPartialContent, contentType: "application/json", payload: []byte(`{"looks":"complete"}`), limit: 64, wantClass: ErrorProtocol},
		{name: "content type rejected before read", status: http.StatusOK, contentType: "text/plain", payload: []byte("must-not-read"), limit: 64, wantClass: ErrorProtocol},
		{name: "declared oversized response is rejected before read", status: http.StatusOK, contentType: "application/json", payload: []byte("must-not-read"), contentLength: 5, limit: 4, wantClass: ErrorResourceLimit},
		{name: "oversized response is closed", status: http.StatusOK, contentType: "application/json", payload: []byte("12345"), limit: 4, wantClass: ErrorResourceLimit, wantRead: true},
		{name: "network read failure is closed", status: http.StatusOK, contentType: "application/json", readErr: testNetworkError{}, limit: 64, wantClass: ErrorTransport, wantRead: true},
		{name: "truncated response is transport failure", status: http.StatusOK, contentType: "application/json", readErr: io.ErrUnexpectedEOF, limit: 64, wantClass: ErrorTransport, wantRead: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			body := newTrackingResponseBody(test.payload, test.readErr, nil)
			client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    test.status,
					Header:        http.Header{"Content-Type": {test.contentType}},
					Body:          body,
					ContentLength: test.contentLength,
				}, nil
			})}
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext() error = %v", err)
			}
			_, err = executePayload(context.Background(), client, request, EndpointComments, "abc123", test.limit)
			var adapterErr *Error
			if !errors.As(err, &adapterErr) || adapterErr.Class != test.wantClass {
				t.Fatalf("executePayload() error = %v, want class %q", err, test.wantClass)
			}
			if got := body.reads.Load(); test.wantRead && got == 0 {
				t.Fatal("response body was not read")
			} else if !test.wantRead && got != 0 {
				t.Fatalf("response body reads = %d, want 0", got)
			}
			if got := body.closes.Load(); got != 1 {
				t.Fatalf("response body closes = %d, want 1", got)
			}
		})
	}
}

func TestExecutePayloadTransportFailureIsSanitizedAndClassifiable(t *testing.T) {
	t.Parallel()

	const planted = "planted-authorization-detail"
	cause := errors.Join(context.DeadlineExceeded, errors.New(planted))
	client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	_, err = executePayload(context.Background(), client, request, EndpointComments, "abc123", 64)
	var adapterErr *Error
	if !errors.As(err, &adapterErr) || adapterErr.Class != ErrorTransport {
		t.Fatalf("executePayload() error = %v, want transport class", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("executePayload() error = %v, want preserved cause", err)
	}
	if strings.Contains(err.Error(), planted) {
		t.Fatalf("executePayload() leaked transport detail: %q", err)
	}
}

func TestExecutePayloadClientTimeoutWhileReadingBodyIsTransport(t *testing.T) {
	t.Parallel()

	body := newBlockingResponseBody()
	client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := executePayload(context.Background(), client, request, EndpointComments, "abc123", 64)
		done <- err
	}()
	<-body.entered
	body.release(context.DeadlineExceeded)
	select {
	case err := <-done:
		var adapterErr *Error
		if !errors.As(err, &adapterErr) || adapterErr.Class != ErrorTransport || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("executePayload() error = %v, want transport deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executePayload() did not return after body-read timeout")
	}
}

func TestExecutePayloadCancellationWhileReadingBody(t *testing.T) {
	t.Parallel()

	body := newBlockingResponseBody()
	client := &http.Client{Transport: httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.invalid", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := executePayload(ctx, client, request, EndpointComments, "abc123", 64)
		done <- err
	}()
	<-body.entered
	cancel()
	body.release(context.Canceled)
	select {
	case err := <-done:
		var adapterErr *Error
		if !errors.As(err, &adapterErr) || adapterErr.Class != ErrorCanceled || !errors.Is(err, context.Canceled) {
			t.Fatalf("executePayload() error = %v, want canceled adapter error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("executePayload() did not return after body-read cancellation")
	}
}

type httpRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn httpRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type failingResponseBody struct {
	payload  []byte
	readErr  error
	closeErr error
	read     bool
}

func (body *failingResponseBody) Read(destination []byte) (int, error) {
	if body.read {
		return 0, body.readErr
	}
	body.read = true
	n := copy(destination, body.payload)
	if body.readErr != nil {
		return n, body.readErr
	}
	return n, io.EOF
}

func (body *failingResponseBody) Close() error {
	return body.closeErr
}

type blockingResponseBody struct {
	entered chan struct{}
	done    chan struct{}
	once    sync.Once
	err     error
}

func newBlockingResponseBody() *blockingResponseBody {
	return &blockingResponseBody{entered: make(chan struct{}), done: make(chan struct{})}
}

func (body *blockingResponseBody) Read([]byte) (int, error) {
	body.once.Do(func() { close(body.entered) })
	<-body.done
	return 0, body.err
}

func (body *blockingResponseBody) Close() error { return nil }

func (body *blockingResponseBody) release(err error) {
	body.err = err
	close(body.done)
}

type trackingResponseBody struct {
	payload  []byte
	readErr  error
	closeErr error
	offset   int
	reads    atomic.Int32
	closes   atomic.Int32
}

func newTrackingResponseBody(payload []byte, readErr, closeErr error) *trackingResponseBody {
	return &trackingResponseBody{payload: payload, readErr: readErr, closeErr: closeErr}
}

func (body *trackingResponseBody) Read(destination []byte) (int, error) {
	body.reads.Add(1)
	if body.readErr != nil {
		return 0, body.readErr
	}
	if body.offset == len(body.payload) {
		return 0, io.EOF
	}
	n := copy(destination, body.payload[body.offset:])
	body.offset += n
	return n, nil
}

func (body *trackingResponseBody) Close() error {
	body.closes.Add(1)
	return body.closeErr
}

type testNetworkError struct{}

func (testNetworkError) Error() string   { return "synthetic network read failure" }
func (testNetworkError) Timeout() bool   { return false }
func (testNetworkError) Temporary() bool { return false }
