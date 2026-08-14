package reddit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const browserSessionTestCookie = "reddit_session=planted-session; loid=planted-loid"

func TestNewBrowserSessionValidatesBoundedAllowlist(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name   string
		config BrowserSessionConfig
	}{
		{name: "minimal", config: BrowserSessionConfig{Cookie: "session=value"}},
		{name: "multiple cookie pairs", config: BrowserSessionConfig{Cookie: "session=value; loid=abc-._~"}},
		{name: "opaque browser JSON cookie", config: BrowserSessionConfig{Cookie: `session=value; g_state={"i_l":0,"i_b":"a+b/c=="}`}},
		{
			name: "full",
			config: BrowserSessionConfig{
				Cookie:          browserSessionTestCookie,
				AcceptLanguage:  "en-US,en;q=0.9",
				SecCHUA:         `"Chromium";v="126", "Not.A/Brand";v="24"`,
				SecCHUAMobile:   "?0",
				SecCHUAPlatform: `"macOS"`,
			},
		},
		{
			name: "maximum lengths",
			config: BrowserSessionConfig{
				Cookie:          "a=" + strings.Repeat("x", maxBrowserCookieBytes-2),
				AcceptLanguage:  strings.Repeat("a", maxAcceptLanguageBytes),
				SecCHUA:         strings.Repeat("a", maxSecCHUABytes),
				SecCHUAMobile:   "?1",
				SecCHUAPlatform: strings.Repeat("a", maxSecCHUAPlatformBytes),
			},
		},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			session, err := NewBrowserSession(test.config)
			if err != nil {
				t.Fatalf("NewBrowserSession() error = %v", err)
			}
			if session == nil || !session.Valid() {
				t.Fatal("NewBrowserSession() returned an invalid session")
			}
		})
	}

	invalid := []struct {
		name   string
		config BrowserSessionConfig
	}{
		{name: "empty cookie", config: BrowserSessionConfig{}},
		{name: "oversized cookie", config: BrowserSessionConfig{Cookie: "a=" + strings.Repeat("x", maxBrowserCookieBytes-1)}},
		{name: "cookie surrounding whitespace", config: BrowserSessionConfig{Cookie: " session=planted"}},
		{name: "cookie control", config: BrowserSessionConfig{Cookie: "session=planted\nvalue"}},
		{name: "cookie non ASCII", config: BrowserSessionConfig{Cookie: "session=planted-качка"}},
		{name: "cookie missing equals", config: BrowserSessionConfig{Cookie: "planted-session"}},
		{name: "cookie empty name", config: BrowserSessionConfig{Cookie: "=planted"}},
		{name: "cookie invalid name", config: BrowserSessionConfig{Cookie: "bad name=planted"}},
		{name: "cookie empty trailing pair", config: BrowserSessionConfig{Cookie: "session=planted;"}},
		{name: "cookie invalid value", config: BrowserSessionConfig{Cookie: "session=planted\x7fvalue"}},
		{name: "accept language whitespace", config: BrowserSessionConfig{Cookie: "session=value", AcceptLanguage: " en-US"}},
		{name: "accept language control", config: BrowserSessionConfig{Cookie: "session=value", AcceptLanguage: "planted\nvalue"}},
		{name: "accept language oversized", config: BrowserSessionConfig{Cookie: "session=value", AcceptLanguage: strings.Repeat("a", maxAcceptLanguageBytes+1)}},
		{name: "client hint control", config: BrowserSessionConfig{Cookie: "session=value", SecCHUA: "planted\rvalue"}},
		{name: "client hint oversized", config: BrowserSessionConfig{Cookie: "session=value", SecCHUA: strings.Repeat("a", maxSecCHUABytes+1)}},
		{name: "invalid mobile hint", config: BrowserSessionConfig{Cookie: "session=value", SecCHUAMobile: "?2"}},
		{name: "platform whitespace", config: BrowserSessionConfig{Cookie: "session=value", SecCHUAPlatform: "planted "}},
		{name: "platform oversized", config: BrowserSessionConfig{Cookie: "session=value", SecCHUAPlatform: strings.Repeat("a", maxSecCHUAPlatformBytes+1)}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			session, err := NewBrowserSession(test.config)
			if session != nil || !errors.Is(err, errBrowserSessionConfig) {
				t.Fatalf("NewBrowserSession() = %#v, %v; want nil session and configuration error", session, err)
			}
			if strings.Contains(err.Error(), "planted") || strings.Contains(err.Error(), "качка") {
				t.Fatalf("configuration error leaked rejected value: %v", err)
			}
		})
	}

	var nilSession *BrowserSession
	if nilSession.Valid() {
		t.Fatal("nil BrowserSession.Valid() = true")
	}
}

func TestBrowserSessionFormattingRedactsEveryValue(t *testing.T) {
	t.Parallel()

	config := fullBrowserSessionTestConfig()
	session, err := NewBrowserSession(config)
	if err != nil {
		t.Fatalf("NewBrowserSession() error = %v", err)
	}
	formatted := []string{
		fmt.Sprintf("%v", config),
		fmt.Sprintf("%+v", &config),
		fmt.Sprintf("%#v", config),
		fmt.Sprintf("%v", session),
		fmt.Sprintf("%+v", session),
		fmt.Sprintf("%#v", session),
	}
	values := []string{config.Cookie, config.AcceptLanguage, config.SecCHUA, config.SecCHUAMobile, config.SecCHUAPlatform}
	for _, output := range formatted {
		if !strings.Contains(strings.ToLower(output), "redacted") {
			t.Fatalf("formatted session is not visibly redacted: %q", output)
		}
		for _, value := range values {
			if strings.Contains(output, value) {
				t.Fatalf("formatted session exposed a configured value")
			}
		}
	}

	invalid := BrowserSessionConfig{Cookie: "planted-cookie\r\nInjected: planted-value"}
	_, err = NewBrowserSession(invalid)
	if err == nil {
		t.Fatal("NewBrowserSession() unexpectedly accepted an injected cookie")
	}
	if strings.Contains(err.Error(), "planted-cookie") || strings.Contains(err.Error(), "planted-value") {
		t.Fatalf("NewBrowserSession() error leaked rejected input: %v", err)
	}
}

func TestBrowserSessionAppliesExactAllowlistedHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config *BrowserSessionConfig
	}{
		{name: "nil default"},
		{name: "minimal", config: &BrowserSessionConfig{Cookie: browserSessionTestCookie}},
		{name: "full", config: browserSessionConfigPointer(fullBrowserSessionTestConfig())},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var session *BrowserSession
			if test.config != nil {
				var err error
				session, err = NewBrowserSession(*test.config)
				if err != nil {
					t.Fatalf("NewBrowserSession() error = %v", err)
				}
			}
			payload := testInitial(t, testPostRef.ID)
			var requests atomic.Int32
			transport := httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				assertExactBrowserHeaders(t, request.Header, test.config)
				return browserSessionJSONResponse(request, http.StatusOK, payload), nil
			})
			client := newBrowserSessionProductionClient(t, session, transport, 0)
			if _, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil }); err != nil {
				t.Fatalf("WalkComments() error = %v", err)
			}
			if requests.Load() != 1 {
				t.Fatalf("request count = %d, want 1", requests.Load())
			}
		})
	}
}

func TestBrowserSessionIsAppliedToInitialRetryAndTraversalRequests(t *testing.T) {
	t.Parallel()

	config := fullBrowserSessionTestConfig()
	session, err := NewBrowserSession(config)
	if err != nil {
		t.Fatalf("NewBrowserSession() error = %v", err)
	}

	t.Run("retry", func(t *testing.T) {
		t.Parallel()

		payload := testInitial(t, testPostRef.ID)
		var attempts atomic.Int32
		transport := httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			attempt := attempts.Add(1)
			assertExactBrowserHeaders(t, request.Header, &config)
			if attempt == 1 {
				return browserSessionJSONResponse(request, http.StatusServiceUnavailable, []byte(`{}`)), nil
			}
			return browserSessionJSONResponse(request, http.StatusOK, payload), nil
		})
		client := newBrowserSessionProductionClient(t, session, transport, 1)
		if _, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil }); err != nil {
			t.Fatalf("WalkComments() error = %v", err)
		}
		if attempts.Load() != 2 {
			t.Fatalf("attempt count = %d, want 2", attempts.Load())
		}
	})

	tests := []struct {
		name              string
		focalComment      string
		initial           func(*testing.T) []byte
		focal             func(*testing.T) []byte
		wantExpansions    int
		wantContinuations int
	}{
		{
			name:         "comment expansion",
			focalComment: "child",
			initial: func(t *testing.T) []byte {
				return testInitial(t, "post1",
					testComment("root", "root body", "t3_post1", testListing(
						testMore([]string{"child"}, 1, "t1_root"),
					)),
				)
			},
			focal: func(t *testing.T) []byte {
				return testInitial(t, "post1", testComment("child", "child body", "t1_root", ""))
			},
			wantExpansions: 1,
		},
		{
			name:         "continuation",
			focalComment: "root",
			initial: func(t *testing.T) []byte {
				return testInitial(t, "post1",
					testComment("root", "root body", "t3_post1", testListing(
						testMore([]string{}, 0, "t1_root"),
					)),
				)
			},
			focal: func(t *testing.T) []byte {
				return testInitial(t, "post1",
					testComment("root", "root body", "t3_post1", testListing(
						testComment("child", "child body", "t1_root", ""),
					)),
				)
			},
			wantContinuations: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			transport := httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				assertExactBrowserHeaders(t, request.Header, &config)
				commentID := request.URL.Query().Get("comment")
				switch commentID {
				case "":
					return browserSessionJSONResponse(request, http.StatusOK, test.initial(t)), nil
				case test.focalComment:
					return browserSessionJSONResponse(request, http.StatusOK, test.focal(t)), nil
				default:
					t.Errorf("unexpected focal comment %q", commentID)
					return browserSessionJSONResponse(request, http.StatusBadRequest, []byte(`{}`)), nil
				}
			})
			client := newBrowserSessionProductionClient(t, session, transport, 0)
			stats, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil })
			if err != nil {
				t.Fatalf("WalkComments() error = %v", err)
			}
			if requests.Load() != 2 || stats.ExpansionRequests != test.wantExpansions ||
				stats.ContinuationRequests != test.wantContinuations {
				t.Fatalf("requests = %d, stats = %#v", requests.Load(), stats)
			}
		})
	}
}

func TestBrowserSessionIgnoresJarAndDoesNotPersistSetCookie(t *testing.T) {
	t.Parallel()

	config := BrowserSessionConfig{Cookie: browserSessionTestCookie}
	session, err := NewBrowserSession(config)
	if err != nil {
		t.Fatalf("NewBrowserSession() error = %v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Cookie") != config.Cookie || strings.Contains(request.Header.Get("Cookie"), "ambient") ||
			strings.Contains(request.Header.Get("Cookie"), "learned") {
			t.Error("request cookie differs from the fixed browser session")
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Add("Set-Cookie", "learned=must-not-persist; Path=/")
		_, _ = response.Write(testInitial(t, testPostRef.ID))
	}))
	defer server.Close()
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	jar.SetCookies(origin, []*http.Cookie{{Name: "ambient", Value: "must-not-send"}})
	clientConfig := validPublicClientConfig(t, server)
	clientConfig.HTTPClient.Jar = jar
	clientConfig.BrowserSession = session
	client := newPublicTestClient(t, clientConfig, server.URL)

	for range 2 {
		if _, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil }); err != nil {
			t.Fatalf("WalkComments() error = %v", err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("request count = %d, want 2", requests.Load())
	}
	if clientConfig.HTTPClient.Jar != jar {
		t.Fatal("client construction mutated the caller-owned Jar")
	}
	cookies := jar.Cookies(origin)
	if len(cookies) != 1 || cookies[0].Name != "ambient" || cookies[0].Value != "must-not-send" {
		t.Fatalf("caller cookie jar changed; cookie count = %d", len(cookies))
	}
}

func TestBrowserSessionDoesNotWeakenRedirectBlocking(t *testing.T) {
	t.Parallel()

	config := BrowserSessionConfig{Cookie: browserSessionTestCookie}
	session, err := NewBrowserSession(config)
	if err != nil {
		t.Fatalf("NewBrowserSession() error = %v", err)
	}
	var targetReached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/login" {
			targetReached.Store(true)
			response.WriteHeader(http.StatusOK)
			return
		}
		if request.Header.Get("Cookie") != config.Cookie {
			t.Error("initial redirecting request lacks the configured browser session")
		}
		http.Redirect(response, request, "/login", http.StatusFound)
	}))
	defer server.Close()
	clientConfig := validPublicClientConfig(t, server)
	clientConfig.BrowserSession = session
	client := newPublicTestClient(t, clientConfig, server.URL)
	_, err = client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil })
	assertClientError(t, err, ErrorAccess, EndpointComments, http.StatusFound)
	if !errors.Is(err, ErrClientRedirect) {
		t.Fatalf("WalkComments() error = %v, want ErrClientRedirect", err)
	}
	if targetReached.Load() {
		t.Fatal("redirect target was requested")
	}
}

func TestBrowserSessionCopiesConfigurationBeforeCallerMutation(t *testing.T) {
	t.Parallel()

	config := fullBrowserSessionTestConfig()
	want := config
	session, err := NewBrowserSession(config)
	if err != nil {
		t.Fatalf("NewBrowserSession() error = %v", err)
	}
	config.Cookie = "mutated=must-not-send"
	config.AcceptLanguage = "mutated-language"
	config.SecCHUA = "mutated-client-hint"
	config.SecCHUAMobile = "?1"
	config.SecCHUAPlatform = "mutated-platform"

	payload := testInitial(t, testPostRef.ID)
	transport := httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		assertExactBrowserHeaders(t, request.Header, &want)
		for _, mutated := range []string{config.Cookie, config.AcceptLanguage, config.SecCHUA, config.SecCHUAMobile, config.SecCHUAPlatform} {
			for _, values := range request.Header {
				if slices.Contains(values, mutated) {
					t.Error("request observed a post-construction configuration mutation")
				}
			}
		}
		return browserSessionJSONResponse(request, http.StatusOK, payload), nil
	})
	client := newBrowserSessionProductionClient(t, session, transport, 0)
	if _, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil }); err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
}

func TestClientRejectsForgedInvalidBrowserSessionWithoutLeakingIt(t *testing.T) {
	t.Parallel()

	invalid := &BrowserSession{cookie: "planted-cookie\nInjected: planted-value"}
	transport := httpRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("transport called for invalid browser session")
		return nil, nil
	})
	_, err := NewClient(ClientConfig{
		HTTPClient:     &http.Client{Transport: transport, Timeout: time.Second},
		UserAgent:      testClientUserAgent,
		RequestPolicy:  newInstantTestRequestPolicy(t),
		BrowserSession: invalid,
	})
	if !errors.Is(err, ErrClientConfig) {
		t.Fatalf("NewClient() error = %v, want ErrClientConfig", err)
	}
	if strings.Contains(err.Error(), "planted-cookie") || strings.Contains(err.Error(), "planted-value") {
		t.Fatalf("NewClient() error leaked browser session input: %v", err)
	}
}

func fullBrowserSessionTestConfig() BrowserSessionConfig {
	return BrowserSessionConfig{
		Cookie:          browserSessionTestCookie,
		AcceptLanguage:  "en-US,en;q=0.9",
		SecCHUA:         `"Chromium";v="126", "Not.A/Brand";v="24"`,
		SecCHUAMobile:   "?0",
		SecCHUAPlatform: `"macOS"`,
	}
}

func browserSessionConfigPointer(config BrowserSessionConfig) *BrowserSessionConfig {
	return &config
}

func newBrowserSessionProductionClient(
	tb testing.TB,
	session *BrowserSession,
	transport http.RoundTripper,
	maxRetries int,
) *Client {
	tb.Helper()
	client, err := NewClient(ClientConfig{
		HTTPClient:     &http.Client{Transport: transport, Timeout: time.Second},
		UserAgent:      testClientUserAgent,
		RequestPolicy:  newInstantTestRequestPolicyWithRetries(tb, maxRetries),
		BrowserSession: session,
	})
	if err != nil {
		tb.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func assertExactBrowserHeaders(tb testing.TB, got http.Header, config *BrowserSessionConfig) {
	tb.Helper()
	want := make(http.Header)
	want.Set("Accept", "application/json")
	want.Set("User-Agent", testClientUserAgent)
	if config != nil {
		want.Set("Cookie", config.Cookie)
		if config.AcceptLanguage != "" {
			want.Set("Accept-Language", config.AcceptLanguage)
		}
		if config.SecCHUA != "" {
			want.Set("Sec-CH-UA", config.SecCHUA)
		}
		if config.SecCHUAMobile != "" {
			want.Set("Sec-CH-UA-Mobile", config.SecCHUAMobile)
		}
		if config.SecCHUAPlatform != "" {
			want.Set("Sec-CH-UA-Platform", config.SecCHUAPlatform)
		}
		want.Set("Sec-Fetch-Dest", browserFetchDestination)
		want.Set("Sec-Fetch-Mode", browserFetchMode)
		want.Set("Sec-Fetch-Site", browserFetchSite)
	}
	if len(got) != len(want) {
		tb.Errorf("header key count = %d, want %d", len(got), len(want))
	}
	for key, wantValues := range want {
		gotValues, exists := got[key]
		if !exists || !slices.Equal(gotValues, wantValues) {
			tb.Errorf("header %q is missing or differs from the allowlisted value", key)
		}
	}
	for key := range got {
		if _, allowed := want[key]; !allowed {
			tb.Errorf("unexpected request header %q", key)
		}
	}
}

func browserSessionJSONResponse(request *http.Request, status int, payload []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Request:    request,
	}
}
