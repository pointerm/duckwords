package reddit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testOAuthClientID     = "test-client-id"
	testOAuthClientSecret = "test-client-secret"
	testOAuthUserAgent    = "darwin:duckwords:test (by /u/example)"
)

func TestTokenSourceOAuthContractAndReuse(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/access_token" || request.URL.RawQuery != "" {
			t.Errorf("request = %s %s, want exact token endpoint", request.Method, request.URL.RequestURI())
		}
		if got := request.Header.Get("User-Agent"); got != testOAuthUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, testOAuthUserAgent)
		}
		if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		clientID, secret, ok := request.BasicAuth()
		if !ok || clientID != testOAuthClientID || secret != testOAuthClientSecret {
			t.Errorf("BasicAuth() = %q, %q, %t", clientID, secret, ok)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("ReadAll(request body): %v", err)
		}
		wantBody := (url.Values{"grant_type": {"client_credentials"}}).Encode()
		if got := string(body); got != wantBody {
			t.Errorf("body = %q, want %q", got, wantBody)
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(writer, `{"access_token":"token-one","token_type":"bearer","expires_in":3600}`)
	}))
	defer server.Close()

	clock := newFakeClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	source := newTestSource(t, server, clock.Now)

	for range 2 {
		token, err := source.Token(context.Background())
		if err != nil || token != "token-one" {
			t.Fatalf("Token() = %q, %v", token, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestTokenSourceInitialHTTPAttemptHonorsRequestPolicyElapsedBudget(t *testing.T) {
	t.Parallel()

	clock := newPolicyTestClock()
	policy := mustPolicyForTest(t, clock, RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Second,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        time.Millisecond,
	})
	var requests atomic.Int32
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			t.Errorf("OAuth request deadline present = %t, remaining = %s", ok, time.Until(deadline))
		}
		clock.jump(2 * time.Second)
		const body = `{"access_token":"must-not-cache","token_type":"bearer","expires_in":3600}`
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
		}, nil
	})
	config := validTokenConfig(transport)
	config.RequestPolicy = policy
	source, err := newTestTokenSource(
		config,
		"http://127.0.0.1/api/v1/access_token",
		clock.nowTime,
	)
	if err != nil {
		t.Fatalf("newTestTokenSource() error = %v", err)
	}

	token, err := source.Token(context.Background())
	assertRedditError(t, err, ErrorTransport, 0)
	if token != "" || requests.Load() != 1 || !errors.Is(err, errRetryBudget) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Token() = %q, %v after %d requests; want internal non-cancellation budget failure", token, err, requests.Load())
	}
	source.mu.Lock()
	cached := source.token.value
	source.mu.Unlock()
	if cached != "" {
		t.Fatalf("cached token = %q, want none from an over-budget initial response", cached)
	}
}

func TestTokenSourceRefreshesAtSafetyMarginAndInvalidatesExactly(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		request := requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"access_token":"token-%d","token_type":"bearer","expires_in":100}`, request)
	}))
	defer server.Close()

	clock := newFakeClock(time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC))
	source := newTestSource(t, server, clock.Now)
	assertToken(t, source, "token-1")
	clock.Advance(89 * time.Second)
	assertToken(t, source, "token-1")
	clock.Advance(time.Second)
	assertToken(t, source, "token-2")

	source.Invalidate("token-1")
	assertToken(t, source, "token-2")
	source.Invalidate("token-2")
	assertToken(t, source, "token-3")
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestTokenSourceConcurrentCallersShareOneAcquisition(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
		}
		<-release
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"shared-token","token_type":"bearer","expires_in":3600}`)
	}))
	defer server.Close()

	source := newTestSource(t, server, time.Now)
	const callers = 32
	start := make(chan struct{})
	errorsChannel := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			<-start
			token, err := source.Token(context.Background())
			if err == nil && token != "shared-token" {
				err = fmt.Errorf("token = %q", token)
			}
			errorsChannel <- err
		}()
	}
	close(start)
	<-entered
	close(release)
	group.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("Token(): %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestTokenSourceCanceledWaiterDoesNotCancelOwner(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"owner-token","token_type":"bearer","expires_in":3600}`)
	}))
	defer server.Close()
	source := newTestSource(t, server, time.Now)

	ownerResult := make(chan error, 1)
	go func() {
		token, err := source.Token(context.Background())
		if err == nil && token != "owner-token" {
			err = fmt.Errorf("token = %q", token)
		}
		ownerResult <- err
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Token(canceled) error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-ownerResult; err != nil {
		t.Fatalf("owner Token(): %v", err)
	}
}

func TestTokenSourceOwnerCancellationDoesNotCancelIndependentWaiter(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	firstEntered := make(chan struct{})
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(firstEntered)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"waiter-token","token_type":"bearer","expires_in":3600}`)),
		}, nil
	})
	config := validTokenConfig(transport)
	source, err := NewTokenSource(config)
	if err != nil {
		t.Fatalf("NewTokenSource() error = %v", err)
	}

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() {
		_, err := source.Token(ownerCtx)
		ownerDone <- err
	}()
	<-firstEntered

	waiterDone := make(chan error, 1)
	go func() {
		token, err := source.Token(context.Background())
		if err == nil && token != "waiter-token" {
			err = fmt.Errorf("token = %q", token)
		}
		waiterDone <- err
	}()
	cancelOwner()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner Token() error = %v, want context.Canceled", err)
	} else {
		assertRedditError(t, err, ErrorCanceled, 0)
	}
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("independent waiter Token() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("independent waiter did not retry after owner cancellation")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestTokenSourcePreCanceledContextIsTyped(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected network call")
	})
	source, err := NewTokenSource(validTokenConfig(transport))
	if err != nil {
		t.Fatalf("NewTokenSource() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = source.Token(ctx)
	assertRedditError(t, err, ErrorCanceled, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Token() error = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestTokenSourceDisablesCallerCookieJar(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Cookie") != "" {
			t.Errorf("OAuth request unexpectedly sent cookies: %q", request.Header.Get("Cookie"))
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Set-Cookie", "reddit_session=must-not-persist; Path=/")
		_, _ = io.WriteString(writer, `{"access_token":"cookie-free-token","token_type":"bearer","expires_in":3600}`)
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "browser_session", Value: "must-not-send"}})
	config := validTokenConfig(server.Client().Transport)
	config.HTTPClient = server.Client()
	config.HTTPClient.Timeout = 5 * time.Second
	config.HTTPClient.Jar = jar
	source, err := newTestTokenSource(config, server.URL+"/api/v1/access_token", time.Now)
	if err != nil {
		t.Fatalf("newTestTokenSource() error = %v", err)
	}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if cookies := jar.Cookies(endpoint); len(cookies) != 1 || cookies[0].Name != "browser_session" {
		t.Fatalf("caller jar was mutated: %#v", cookies)
	}
}

func TestNewTokenSourceValidation(t *testing.T) {
	t.Parallel()

	valid := validTokenConfig(http.DefaultTransport)
	tests := []struct {
		name   string
		mutate func(*TokenConfig)
	}{
		{name: "missing client ID", mutate: func(config *TokenConfig) { config.ClientID = "" }},
		{name: "approval not confirmed", mutate: func(config *TokenConfig) { config.APIAccessApproved = false }},
		{name: "client ID colon", mutate: func(config *TokenConfig) { config.ClientID = "bad:id" }},
		{name: "missing secret", mutate: func(config *TokenConfig) { config.ClientSecret = "" }},
		{name: "secret control", mutate: func(config *TokenConfig) { config.ClientSecret = "secret\nvalue" }},
		{name: "unstructured User-Agent", mutate: func(config *TokenConfig) { config.UserAgent = "duckwords/1.0" }},
		{name: "missing contact", mutate: func(config *TokenConfig) { config.UserAgent = "cli:duckwords:1.0" }},
		{name: "missing component", mutate: func(config *TokenConfig) { config.UserAgent = "cli::1.0 (by /u/example)" }},
		{name: "extra component", mutate: func(config *TokenConfig) { config.UserAgent = "cli:duckwords:1.0:extra (by /u/example)" }},
		{name: "non-descriptive component", mutate: func(config *TokenConfig) { config.UserAgent = "cli:---:1.0 (by /u/example)" }},
		{name: "unsafe component", mutate: func(config *TokenConfig) { config.UserAgent = "cli:duck words:1.0 (by /u/example)" }},
		{name: "non-descriptive username", mutate: func(config *TokenConfig) { config.UserAgent = "cli:duckwords:1.0 (by /u/---)" }},
		{name: "unsafe username", mutate: func(config *TokenConfig) { config.UserAgent = "cli:duckwords:1.0 (by /u/ex ample)" }},
		{name: "long component", mutate: func(config *TokenConfig) {
			config.UserAgent = "cli:" + strings.Repeat("a", maxUserAgentComponentBytes+1) + ":1.0 (by /u/example)"
		}},
		{name: "User-Agent whitespace", mutate: func(config *TokenConfig) { config.UserAgent += " " }},
		{name: "nil client", mutate: func(config *TokenConfig) { config.HTTPClient = nil }},
		{name: "implicit transport", mutate: func(config *TokenConfig) { config.HTTPClient = &http.Client{Timeout: time.Second} }},
		{name: "missing timeout", mutate: func(config *TokenConfig) { config.HTTPClient.Timeout = 0 }},
		{name: "excessive timeout", mutate: func(config *TokenConfig) { config.HTTPClient.Timeout = maxOAuthHTTPTimeout + time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			clientCopy := *valid.HTTPClient
			config.HTTPClient = &clientCopy
			test.mutate(&config)
			_, err := NewTokenSource(config)
			if !errors.Is(err, ErrOAuthConfig) {
				t.Fatalf("NewTokenSource() error = %v, want ErrOAuthConfig", err)
			}
		})
	}

	if _, err := NewTokenSource(valid); err != nil {
		t.Fatalf("NewTokenSource(valid): %v", err)
	}
	var nilSource *TokenSource
	if _, err := nilSource.Token(context.Background()); !errors.Is(err, ErrOAuthConfig) {
		t.Fatalf("nil TokenSource error = %v", err)
	}
	source, err := NewTokenSource(valid)
	if err != nil {
		t.Fatal(err)
	}
	//lint:ignore SA1012 This assertion deliberately exercises the public nil-context contract.
	if _, err := source.Token(nil); !errors.Is(err, ErrOAuthConfig) {
		t.Fatalf("Token(nil) error = %v", err)
	}
}

func TestTestTokenSourceEndpointPolicy(t *testing.T) {
	t.Parallel()

	config := validTokenConfig(http.DefaultTransport)
	tests := []string{
		"http://example.com/api/v1/access_token",
		"http://localhost/api/v1/access_token",
		"http://127.0.0.1/not-token",
		"http://127.0.0.1/api/v1/access_token?redirect=1",
		"http://user:pass@127.0.0.1/api/v1/access_token",
	}
	for _, endpoint := range tests {
		if _, err := newTestTokenSource(config, endpoint, time.Now); !errors.Is(err, ErrOAuthEndpoint) {
			t.Errorf("newTestTokenSource(%q) error = %v, want ErrOAuthEndpoint", endpoint, err)
		}
	}
	if _, err := newTestTokenSource(config, "http://127.0.0.1:1234/api/v1/access_token", nil); !errors.Is(err, ErrOAuthConfig) {
		t.Fatalf("nil clock error = %v, want ErrOAuthConfig", err)
	}
}

func TestTokenSourceBlocksRedirect(t *testing.T) {
	t.Parallel()

	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/api/v1/access_token", http.StatusFound)
	}))
	defer server.Close()

	source := newTestSource(t, server, time.Now)
	_, err := source.Token(context.Background())
	if !errors.Is(err, ErrOAuthRedirect) {
		t.Fatalf("Token() error = %v, want ErrOAuthRedirect", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received credentials")
	}
}

func TestTokenSourceRejectsBadResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        error
		wantClass   ErrorClass
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, contentType: "text/plain", body: "private-response-marker " + testOAuthClientSecret, want: ErrOAuthUnexpectedStatus, wantClass: ErrorAuthentication},
		{name: "server failure", status: http.StatusServiceUnavailable, contentType: "application/json", body: `{"secret":"private-response-marker-` + testOAuthClientSecret + `"}`, want: ErrOAuthUnexpectedStatus, wantClass: ErrorServer},
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", body: `{}`, want: ErrOAuthUnexpectedContentType, wantClass: ErrorProtocol},
		{name: "malformed", status: http.StatusOK, contentType: "application/json", body: `{"access_token":`, want: ErrOAuthMalformedResponse, wantClass: ErrorProtocol},
		{name: "trailing JSON", status: http.StatusOK, contentType: "application/json", body: `{}` + `{}`, want: ErrOAuthMalformedResponse, wantClass: ErrorProtocol},
		{name: "missing token", status: http.StatusOK, contentType: "application/json", body: `{"token_type":"bearer","expires_in":60}`, want: ErrOAuthInvalidToken, wantClass: ErrorProtocol},
		{name: "bad token characters", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"secret token","token_type":"bearer","expires_in":60}`, want: ErrOAuthInvalidToken, wantClass: ErrorProtocol},
		{name: "internal token padding", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"a=b","token_type":"bearer","expires_in":60}`, want: ErrOAuthInvalidToken, wantClass: ErrorProtocol},
		{name: "leading token padding", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"=abc","token_type":"bearer","expires_in":60}`, want: ErrOAuthInvalidToken, wantClass: ErrorProtocol},
		{name: "wrong token type", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"planted-access-credential","token_type":"mac","expires_in":60}`, want: ErrOAuthInvalidToken, wantClass: ErrorProtocol},
		{name: "missing expiry", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"token","token_type":"bearer"}`, want: ErrOAuthInvalidExpiry, wantClass: ErrorProtocol},
		{name: "string expiry", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"token","token_type":"bearer","expires_in":"60"}`, want: ErrOAuthInvalidExpiry, wantClass: ErrorProtocol},
		{name: "fraction expiry", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"token","token_type":"bearer","expires_in":1.5}`, want: ErrOAuthInvalidExpiry, wantClass: ErrorProtocol},
		{name: "zero expiry", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"token","token_type":"bearer","expires_in":0}`, want: ErrOAuthInvalidExpiry, wantClass: ErrorProtocol},
		{name: "excessive expiry", status: http.StatusOK, contentType: "application/json", body: `{"access_token":"token","token_type":"bearer","expires_in":86401}`, want: ErrOAuthInvalidExpiry, wantClass: ErrorProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", test.contentType)
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()

			source := newTestSource(t, server, time.Now)
			_, err := source.Token(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("Token() error = %v, want %v", err, test.want)
			}
			assertRedditError(t, err, test.wantClass, test.status)
			assertNoSecrets(t, err)
		})
	}
}

func TestValidBearerTokenAllowsTrailingPadding(t *testing.T) {
	t.Parallel()

	if !validBearerToken("abc==") {
		t.Fatal("validBearerToken(abc==) = false, want true")
	}
	for _, token := range []string{"a=b", "=abc"} {
		if validBearerToken(token) {
			t.Errorf("validBearerToken(%q) = true, want false", token)
		}
	}
}

func TestTokenSourceRejectsOversizedResponseBeforeDecoding(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, strings.Repeat("x", int(maxOAuthResponseBytes)+1))
	}))
	defer server.Close()
	source := newTestSource(t, server, time.Now)
	_, err := source.Token(context.Background())
	if !errors.Is(err, ErrOAuthResponseTooLarge) {
		t.Fatalf("Token() error = %v, want ErrOAuthResponseTooLarge", err)
	}
	assertRedditError(t, err, ErrorResourceLimit, http.StatusOK)
}

func TestTokenSourceResponseBodyLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		contentType   string
		payload       []byte
		readErr       error
		contentLength int64
		wantClass     ErrorClass
		wantCause     error
		wantRead      bool
	}{
		{name: "status rejected before read", status: http.StatusUnauthorized, contentType: "application/json", payload: []byte("must-not-read"), wantClass: ErrorAuthentication, wantCause: ErrOAuthUnexpectedStatus},
		{name: "retryable status is bounded-drained", status: http.StatusServiceUnavailable, contentType: "application/json", payload: []byte("small error"), wantClass: ErrorServer, wantCause: ErrOAuthUnexpectedStatus, wantRead: true},
		{name: "content type rejected before read", status: http.StatusOK, contentType: "text/plain", payload: []byte("must-not-read"), wantClass: ErrorProtocol, wantCause: ErrOAuthUnexpectedContentType},
		{name: "declared oversized response is rejected before read", status: http.StatusOK, contentType: "application/json", payload: []byte("must-not-read"), contentLength: maxOAuthResponseBytes + 1, wantClass: ErrorResourceLimit, wantCause: ErrOAuthResponseTooLarge},
		{name: "oversized response is closed", status: http.StatusOK, contentType: "application/json", payload: []byte(strings.Repeat("x", int(maxOAuthResponseBytes)+1)), wantClass: ErrorResourceLimit, wantCause: ErrOAuthResponseTooLarge, wantRead: true},
		{name: "network read failure is closed", status: http.StatusOK, contentType: "application/json", readErr: testNetworkError{}, wantClass: ErrorTransport, wantCause: ErrOAuthResponseRead, wantRead: true},
		{name: "truncated response is transport failure", status: http.StatusOK, contentType: "application/json", readErr: io.ErrUnexpectedEOF, wantClass: ErrorTransport, wantCause: ErrOAuthResponseRead, wantRead: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			body := newTrackingResponseBody(test.payload, test.readErr, nil)
			transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    test.status,
					Header:        http.Header{"Content-Type": {test.contentType}},
					Body:          body,
					ContentLength: test.contentLength,
				}, nil
			})
			source, err := NewTokenSource(validTokenConfig(transport))
			if err != nil {
				t.Fatalf("NewTokenSource() error = %v", err)
			}
			_, err = source.Token(context.Background())
			assertRedditError(t, err, test.wantClass, test.status)
			if !errors.Is(err, test.wantCause) {
				t.Fatalf("Token() error = %v, want cause %v", err, test.wantCause)
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

func TestTokenSourceRejectsAmbiguousContentTypeBeforeRead(t *testing.T) {
	t.Parallel()

	body := newTrackingResponseBody([]byte(`{"access_token":"token","token_type":"bearer","expires_in":60}`), nil, nil)
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json", "text/plain"}},
			Body:       body,
		}, nil
	})
	source, err := NewTokenSource(validTokenConfig(transport))
	if err != nil {
		t.Fatalf("NewTokenSource() error = %v", err)
	}
	_, err = source.Token(context.Background())
	assertRedditError(t, err, ErrorProtocol, http.StatusOK)
	if !errors.Is(err, ErrOAuthUnexpectedContentType) {
		t.Fatalf("Token() error = %v, want unexpected content type", err)
	}
	if body.reads.Load() != 0 || body.closes.Load() != 1 {
		t.Fatalf("response body reads=%d closes=%d, want 0/1", body.reads.Load(), body.closes.Load())
	}
}

func TestTokenSourceDoesNotRetryFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	source := newTestSource(t, server, time.Now)
	if _, err := source.Token(context.Background()); err == nil {
		t.Fatal("Token() error = nil")
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want exactly 1", requests.Load())
	}
}

func TestTokenSourceTransportErrorIsSanitized(t *testing.T) {
	t.Parallel()

	const planted = "transport-secret"
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request failed with " + planted + " and " + testOAuthClientSecret)
	})
	config := validTokenConfig(transport)
	source, err := NewTokenSource(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Token(context.Background())
	if !errors.Is(err, ErrOAuthRequest) {
		t.Fatalf("Token() error = %v, want ErrOAuthRequest", err)
	}
	assertRedditError(t, err, ErrorTransport, 0)
	for _, secret := range []string{planted, testOAuthClientSecret, testOAuthClientID} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, err)
		}
	}
}

func TestTokenSourceTransportDeadlineWithLiveCallerIsSanitized(t *testing.T) {
	t.Parallel()

	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.Join(context.DeadlineExceeded, errors.New("transport exposed "+testOAuthClientSecret))
	})
	source, err := NewTokenSource(validTokenConfig(transport))
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Token(context.Background())
	if !errors.Is(err, ErrOAuthRequest) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Token() error = %v, want ErrOAuthRequest and context.DeadlineExceeded", err)
	}
	assertRedditError(t, err, ErrorTransport, 0)
	assertNoSecrets(t, err)
}

func TestTokenSourceClientTimeoutWhileReadingBodyIsTransport(t *testing.T) {
	t.Parallel()

	body := newBlockingResponseBody()
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})
	source, err := NewTokenSource(validTokenConfig(transport))
	if err != nil {
		t.Fatalf("NewTokenSource() error = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := source.Token(context.Background())
		done <- err
	}()
	<-body.entered
	body.release(context.DeadlineExceeded)
	select {
	case err := <-done:
		assertRedditError(t, err, ErrorTransport, http.StatusOK)
		if !errors.Is(err, ErrOAuthResponseRead) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Token() error = %v, want response-read transport deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Token() did not return after body-read timeout")
	}
}

func TestTokenSourceCancellationWhileReadingBody(t *testing.T) {
	t.Parallel()

	body := newBlockingResponseBody()
	transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	})
	source, err := NewTokenSource(validTokenConfig(transport))
	if err != nil {
		t.Fatalf("NewTokenSource() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := source.Token(ctx)
		done <- err
	}()
	<-body.entered
	cancel()
	body.release(context.Canceled)
	select {
	case err := <-done:
		assertRedditError(t, err, ErrorCanceled, http.StatusOK)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Token() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Token() did not return after body-read cancellation")
	}
}

func newTestSource(t *testing.T, server *httptest.Server, now func() time.Time) *TokenSource {
	t.Helper()
	config := validTokenConfig(server.Client().Transport)
	config.HTTPClient = server.Client()
	config.HTTPClient.Timeout = 5 * time.Second
	source, err := newTestTokenSource(config, server.URL+"/api/v1/access_token", now)
	if err != nil {
		t.Fatalf("newTestTokenSource(): %v", err)
	}
	return source
}

func validTokenConfig(transport http.RoundTripper) TokenConfig {
	return TokenConfig{
		ClientID:          testOAuthClientID,
		ClientSecret:      testOAuthClientSecret,
		UserAgent:         testOAuthUserAgent,
		HTTPClient:        &http.Client{Transport: transport, Timeout: 5 * time.Second},
		RequestPolicy:     newFastTestRequestPolicy(),
		APIAccessApproved: true,
	}
}

func assertToken(t *testing.T, source *TokenSource, want string) {
	t.Helper()
	got, err := source.Token(context.Background())
	if err != nil || got != want {
		t.Fatalf("Token() = %q, %v; want %q", got, err, want)
	}
}

func assertRedditError(t *testing.T, err error, class ErrorClass, status int) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("errors.As(%v, *Error) = false", err)
	}
	if typed.Class != class || typed.Endpoint != EndpointOAuthToken || typed.StatusCode != status {
		t.Fatalf("reddit error = %#v, want class=%s endpoint=%s status=%d", typed, class, EndpointOAuthToken, status)
	}
}

func assertNoSecrets(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{testOAuthClientID, testOAuthClientSecret, "private-response-marker", "planted-access-credential"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %q", secret, err)
		}
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func newFastTestRequestPolicy() *RequestPolicy {
	clock := newFakeClock(time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC))
	policy, err := newRequestPolicy(RequestPolicyConfig{
		RequestsPerSecond: maxRequestsPerSecond,
		MaxRetries:        0,
		MaxRetryElapsed:   time.Minute,
		InitialBackoff:    time.Millisecond,
		MaxBackoff:        time.Second,
	}, clock.Now, func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		clock.Advance(delay)
		return nil
	}, func(delay time.Duration) time.Duration { return delay })
	if err != nil {
		panic(err)
	}
	return policy
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
