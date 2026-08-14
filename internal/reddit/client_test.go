package reddit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testClientUserAgent = "duckwords/test (+https://github.com/pointerm/duckwords)"

var testPostRef = PostRef{ID: "post1", JSONPath: "/r/duck/comments/post1/title/.json"}

func TestClientWalkCommentsUsesExactPublicGETContractAndNestedFocalExpansion(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assertPublicRequest(t, request)
		response.Header().Set("Content-Type", "application/json; charset=utf-8")

		commentID := request.URL.Query().Get("comment")
		switch commentID {
		case "":
			_, _ = response.Write(testInitial(t, "post1",
				testComment("root", "first body", "t3_post1", testListing(
					testMore([]string{"child"}, 1, "t1_root"),
				)),
			))
		case "child":
			_, _ = response.Write(testInitial(t, "post1",
				testComment("child", "second body", "t1_root", testListing(
					testMore([]string{"leaf"}, 1, "t1_child"),
				)),
			))
		case "leaf":
			_, _ = response.Write(testInitial(t, "post1",
				testComment("leaf", "third body", "t1_child", ""),
			))
		default:
			t.Errorf("unexpected focal comment %q", commentID)
			http.Error(response, "unexpected", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "session", Value: "must-not-send"}})

	config := validPublicClientConfig(t, server)
	config.HTTPClient.Jar = jar
	client := newPublicTestClient(t, config, server.URL)
	var bodies []string
	stats, err := client.WalkComments(context.Background(), testPostRef, func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if !slices.Equal(bodies, []string{"first body", "second body", "third body"}) {
		t.Fatalf("bodies = %q", bodies)
	}
	if stats.Comments != 3 || stats.BodiesVisited != 3 || stats.ExpansionRequests != 2 || stats.ContinuationRequests != 0 {
		t.Fatalf("stats = %#v", stats)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests = %d, want 3", requests.Load())
	}
}

func TestClientDisablesCallerCookieJarWithoutMutatingIt(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertPublicRequest(t, request)
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Set-Cookie", "reddit_session=must-not-persist; Path=/")
		_, _ = response.Write(testInitial(t, "post1"))
	}))
	defer server.Close()

	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	jar.SetCookies(origin, []*http.Cookie{{Name: "browser_session", Value: "must-not-send"}})
	config := validPublicClientConfig(t, server)
	config.HTTPClient.Jar = jar
	client := newPublicTestClient(t, config, server.URL)

	if _, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil }); err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if config.HTTPClient.Jar != jar {
		t.Fatal("NewClient mutated the caller-owned HTTP client's Jar field")
	}
	cookies := jar.Cookies(origin)
	if len(cookies) != 1 || cookies[0].Name != "browser_session" || cookies[0].Value != "must-not-send" {
		t.Fatalf("caller cookie jar was mutated by Reddit response: %#v", cookies)
	}
}

func TestClientWalkCommentsContinuesDepthTruncatedBranchWithPublicFocalGET(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assertPublicRequest(t, request)
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("comment") == "" {
			_, _ = response.Write(testInitial(t, "post1",
				testComment("root", "root body", "t3_post1", testListing(
					testMore([]string{}, 0, "t1_root"),
				)),
			))
			return
		}
		if request.URL.Query().Get("comment") != "root" {
			t.Errorf("comment = %q, want root", request.URL.Query().Get("comment"))
		}
		_, _ = response.Write(testInitial(t, "post1",
			testComment("root", "root body", "t3_post1", testListing(
				testComment("child", "child body", "t1_root", ""),
			)),
		))
	}))
	defer server.Close()

	client := newPublicTestClient(t, validPublicClientConfig(t, server), server.URL)
	var bodies []string
	stats, err := client.WalkComments(context.Background(), testPostRef, func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if !slices.Equal(bodies, []string{"root body", "child body"}) || stats.ContinuationRequests != 1 || stats.ExpansionRequests != 0 {
		t.Fatalf("bodies=%q stats=%#v", bodies, stats)
	}
}

func TestClientWalkCommentsFailsClosedWhenFocalListingOmitsRequestedComment(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("comment") == "" {
			_, _ = response.Write(testInitial(t, "post1", testMore([]string{"missing"}, 1, "t3_post1")))
			return
		}
		_, _ = response.Write(testInitial(t, "post1"))
	}))
	defer server.Close()

	client := newPublicTestClient(t, validPublicClientConfig(t, server), server.URL)
	stats, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil })
	assertClientError(t, err, ErrorIncomplete, EndpointCommentExpansion, 0)
	if stats.ExpansionRequests != 1 || stats.UniqueMoreIDs != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestClientSerializesFocalExpansionAcrossConcurrentPosts(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	var peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		postID := "posta"
		if request.URL.Path == "/comments/postb/.json" {
			postID = "postb"
		}
		if request.URL.Query().Get("comment") == "" {
			_, _ = response.Write(testInitial(t, postID, testClientMore(postID+"child", "t3_"+postID)))
			return
		}
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
		_, _ = response.Write(testInitial(t, postID, testClientComment(postID+"child", "body", postID, "t3_"+postID, "")))
	}))
	defer server.Close()

	client := newPublicTestClient(t, validPublicClientConfig(t, server), server.URL)
	posts := []PostRef{
		{ID: "posta", JSONPath: "/comments/posta/.json"},
		{ID: "postb", JSONPath: "/comments/postb/.json"},
	}
	errorsChannel := make(chan error, len(posts))
	var group sync.WaitGroup
	for _, post := range posts {
		post := post
		group.Add(1)
		go func() {
			defer group.Done()
			_, walkErr := client.WalkComments(context.Background(), post, func(Comment) error { return nil })
			errorsChannel <- walkErr
		}()
	}
	group.Wait()
	close(errorsChannel)
	for walkErr := range errorsChannel {
		if walkErr != nil {
			t.Fatalf("WalkComments() error = %v", walkErr)
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("peak concurrent expansion requests = %d, want 1", peak.Load())
	}
}

func TestClientExpansionGateWaiterHonorsCancellation(t *testing.T) {
	t.Parallel()

	client := &Client{expansionGate: make(chan struct{}, 1)}
	if err := client.acquireExpansionGate(context.Background(), "posta"); err != nil {
		t.Fatalf("first acquireExpansionGate() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.acquireExpansionGate(ctx, "postb")
	assertClientError(t, err, ErrorCanceled, EndpointCommentExpansion, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireExpansionGate() error = %v, want context.Canceled", err)
	}
	if got := len(client.expansionGate); got != 1 {
		t.Fatalf("expansion gate permits held after canceled waiter = %d, want 1", got)
	}

	client.releaseExpansionGate()
	if err := client.acquireExpansionGate(context.Background(), "postb"); err != nil {
		t.Fatalf("acquireExpansionGate() after release error = %v", err)
	}
	client.releaseExpansionGate()
}

func TestClientRetriesTransientPublicGETWithoutChangingWireContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "server unavailable", status: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			payload := testInitial(t, "post1")
			var attempts atomic.Int32
			transport := httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempt := attempts.Add(1)
				assertRequestURL(t, request.URL.String(), testPostRef.JSONPath, url.Values{
					"limit": {"500"}, "raw_json": {"1"}, "showmore": {"true"}, "sort": {"confidence"},
				})
				if request.Method != http.MethodGet || request.Body != nil || request.GetBody != nil || request.ContentLength != 0 {
					t.Errorf("attempt %d method/body contract = method %q body=%#v hasGetBody=%t length=%d", attempt, request.Method, request.Body, request.GetBody != nil, request.ContentLength)
				}
				if request.Header.Get("Accept") != "application/json" || request.Header.Get("User-Agent") != testClientUserAgent {
					t.Errorf("attempt %d public headers = %#v", attempt, request.Header)
				}
				if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Content-Type") != "" {
					t.Errorf("attempt %d carried ambient authority or a body content type: %#v", attempt, request.Header)
				}

				if attempt == 1 {
					header := http.Header{"Content-Type": {"application/json"}}
					if test.status == http.StatusTooManyRequests {
						header.Set("Retry-After", "0")
					}
					return &http.Response{
						StatusCode:    test.status,
						Header:        header,
						Body:          io.NopCloser(strings.NewReader(`{}`)),
						ContentLength: 2,
						Request:       request,
					}, nil
				}
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": {"application/json"}},
					Body:          io.NopCloser(bytes.NewReader(payload)),
					ContentLength: int64(len(payload)),
					Request:       request,
				}, nil
			})
			policy := newInstantTestRequestPolicyWithRetries(t, 1)
			client, err := NewClient(ClientConfig{
				HTTPClient:    &http.Client{Transport: transport, Timeout: time.Second},
				UserAgent:     testClientUserAgent,
				RequestPolicy: policy,
			})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			stats, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil })
			if err != nil {
				t.Fatalf("WalkComments() error = %v", err)
			}
			if stats.Comments != 0 || attempts.Load() != 2 {
				t.Fatalf("stats = %#v, attempts = %d; want empty complete tree after two attempts", stats, attempts.Load())
			}
			snapshot := policy.Snapshot()
			if snapshot.HTTPAttempts != 2 || snapshot.Retries != 1 {
				t.Fatalf("request policy snapshot = %#v, want two attempts and one retry", snapshot)
			}
		})
	}
}

func TestClientClassifiesPublicAccessStatusAndProtocolFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		wantClass   ErrorClass
	}{
		{name: "redirect without location", status: http.StatusFound, contentType: "text/html", wantClass: ErrorAccess},
		{name: "unauthorized", status: http.StatusUnauthorized, contentType: "application/json", wantClass: ErrorAccess},
		{name: "forbidden", status: http.StatusForbidden, contentType: "application/json", wantClass: ErrorAccess},
		{name: "gone", status: http.StatusGone, contentType: "application/json", wantClass: ErrorNotFound},
		{name: "HTML success", status: http.StatusOK, contentType: "text/html", wantClass: ErrorProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", test.contentType)
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte("not-json private marker"))
			}))
			defer server.Close()
			client := newPublicTestClient(t, validPublicClientConfig(t, server), server.URL)
			_, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil })
			assertClientError(t, err, test.wantClass, EndpointComments, test.status)
			if err != nil && (strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), server.URL)) {
				t.Fatalf("sanitized error leaked response or URL: %v", err)
			}
		})
	}
}

func TestClientBuildsGETWithNilBodyAndNoAmbientAuthority(t *testing.T) {
	t.Parallel()

	payload := testInitial(t, "post1")
	transport := httpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.Body != nil || request.GetBody != nil || request.ContentLength != 0 {
			t.Errorf("request method/body contract = method %q body=%#v hasGetBody=%t length=%d", request.Method, request.Body, request.GetBody != nil, request.ContentLength)
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("request carried ambient authority: %#v", request.Header)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(payload)),
			ContentLength: int64(len(payload)),
			Request:       request,
		}, nil
	})
	policy := newInstantTestRequestPolicy(t)
	client, err := newTestClient(ClientConfig{
		HTTPClient: &http.Client{Transport: transport, Timeout: time.Second},
		UserAgent:  testClientUserAgent, RequestPolicy: policy,
	}, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("newTestClient() error = %v", err)
	}
	if _, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil }); err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
}

func TestClientBlocksRedirectAsAccessFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/login" {
			t.Fatal("redirect target must never be requested")
		}
		http.Redirect(response, request, "/login", http.StatusFound)
	}))
	defer server.Close()
	client := newPublicTestClient(t, validPublicClientConfig(t, server), server.URL)
	_, err := client.WalkComments(context.Background(), testPostRef, func(Comment) error { return nil })
	assertClientError(t, err, ErrorAccess, EndpointComments, http.StatusFound)
	if !errors.Is(err, ErrClientRedirect) {
		t.Fatalf("error = %v, want ErrClientRedirect", err)
	}
}

func TestClientValidatesConfigurationEndpointAndPostReference(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	valid := validPublicClientConfig(t, server)

	tests := []struct {
		name   string
		mutate func(*ClientConfig)
	}{
		{name: "nil HTTP client", mutate: func(config *ClientConfig) { config.HTTPClient = nil }},
		{name: "implicit transport", mutate: func(config *ClientConfig) { config.HTTPClient.Transport = nil }},
		{name: "zero timeout", mutate: func(config *ClientConfig) { config.HTTPClient.Timeout = 0 }},
		{name: "nil policy", mutate: func(config *ClientConfig) { config.RequestPolicy = nil }},
		{name: "short user agent", mutate: func(config *ClientConfig) { config.UserAgent = "short" }},
		{name: "control user agent", mutate: func(config *ClientConfig) { config.UserAgent = "duckwords\ninvalid" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			clientCopy := *valid.HTTPClient
			config.HTTPClient = &clientCopy
			test.mutate(&config)
			if _, err := newTestClient(config, server.URL); !errors.Is(err, ErrClientConfig) {
				t.Fatalf("newTestClient() error = %v, want ErrClientConfig", err)
			}
		})
	}

	for _, endpoint := range []string{
		"https://old.reddit.com/path", "https://old.reddit.com?query=1", "https://example.com",
		"http://example.com", "http://localhost:1234", "http://127.0.0.1:1/path",
	} {
		if _, err := newTestClient(valid, endpoint); !errors.Is(err, ErrClientEndpoint) {
			t.Errorf("newTestClient(%q) error = %v, want ErrClientEndpoint", endpoint, err)
		}
	}
	if client, err := NewClient(valid); err != nil || client.apiBase.String() != redditPublicEndpoint {
		t.Fatalf("NewClient() client=%#v error=%v", client, err)
	}

	client := newPublicTestClient(t, valid, server.URL)
	invalidRefs := []PostRef{
		{},
		{ID: "POST1", JSONPath: "/comments/post1/.json"},
		{ID: "post1", JSONPath: "https://evil.test/comments/post1/.json"},
		{ID: "post1", JSONPath: "/comments/other/.json"},
		{ID: "post1", JSONPath: "/comments/post1/.json?raw_json=0"},
		{ID: "post1", JSONPath: "/r/duck/comments/post1/title/comment/.json"},
	}
	for _, post := range invalidRefs {
		if _, err := client.WalkComments(context.Background(), post, func(Comment) error { return nil }); err == nil {
			t.Errorf("WalkComments(%#v) error = nil", post)
		}
	}
	var nilContext context.Context
	if _, err := client.WalkComments(nilContext, testPostRef, func(Comment) error { return nil }); err == nil {
		t.Error("WalkComments(nil context) error = nil")
	}
	if _, err := client.WalkComments(context.Background(), testPostRef, nil); err == nil {
		t.Error("WalkComments(nil visitor) error = nil")
	}
}

func TestPublicRequestBuilderPinsPathAndExactQuery(t *testing.T) {
	t.Parallel()
	client := &Client{apiBase: url.URL{Scheme: "https", Host: "old.reddit.com"}}
	initial := client.commentsRequest(testPostRef)
	assertRequestURL(t, initial.url, testPostRef.JSONPath, url.Values{
		"limit": {"500"}, "raw_json": {"1"}, "showmore": {"true"}, "sort": {"confidence"},
	})
	focal, err := client.focalRequest(testPostRef, "child", EndpointCommentExpansion)
	if err != nil {
		t.Fatalf("focalRequest() error = %v", err)
	}
	assertRequestURL(t, focal.url, testPostRef.JSONPath, url.Values{
		"comment": {"child"}, "context": {"0"}, "limit": {"500"},
		"raw_json": {"1"}, "showmore": {"true"}, "sort": {"confidence"},
	})
}

func validPublicClientConfig(tb testing.TB, server *httptest.Server) ClientConfig {
	tb.Helper()
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	return ClientConfig{
		HTTPClient:    httpClient,
		UserAgent:     testClientUserAgent,
		RequestPolicy: newInstantTestRequestPolicy(tb),
	}
}

func newPublicTestClient(tb testing.TB, config ClientConfig, endpoint string) *Client {
	tb.Helper()
	client, err := newTestClient(config, endpoint)
	if err != nil {
		tb.Fatalf("newTestClient() error = %v", err)
	}
	return client
}

func newInstantTestRequestPolicy(tb testing.TB) *RequestPolicy {
	return newInstantTestRequestPolicyWithRetries(tb, 0)
}

func newInstantTestRequestPolicyWithRetries(tb testing.TB, maxRetries int) *RequestPolicy {
	tb.Helper()
	var mu sync.Mutex
	current := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	wait := func(ctx context.Context, duration time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mu.Lock()
		current = current.Add(duration)
		mu.Unlock()
		return nil
	}
	config := DefaultRequestPolicyConfig()
	config.MaxRetries = maxRetries
	policy, err := newRequestPolicy(config, now, wait, func(duration time.Duration) time.Duration { return duration })
	if err != nil {
		tb.Fatalf("newRequestPolicy() error = %v", err)
	}
	return policy
}

func assertPublicRequest(tb testing.TB, request *http.Request) {
	tb.Helper()
	if request.Method != http.MethodGet {
		tb.Errorf("method = %q, want GET", request.Method)
	}
	if request.URL.Path != testPostRef.JSONPath && request.URL.Path != "/comments/posta/.json" && request.URL.Path != "/comments/postb/.json" {
		tb.Errorf("path = %q", request.URL.Path)
	}
	wantQuery := url.Values{"limit": {"500"}, "raw_json": {"1"}, "showmore": {"true"}, "sort": {"confidence"}}
	if commentID := request.URL.Query().Get("comment"); commentID != "" {
		wantQuery.Set("comment", commentID)
		wantQuery.Set("context", "0")
	}
	if got := request.URL.Query().Encode(); got != wantQuery.Encode() {
		tb.Errorf("query = %q, want %q", got, wantQuery.Encode())
	}
	if request.Header.Get("Accept") != "application/json" || request.Header.Get("User-Agent") != testClientUserAgent {
		tb.Errorf("headers = %#v", request.Header)
	}
	if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
		tb.Errorf("request carried auth/cookie headers: %#v", request.Header)
	}
	if request.ContentLength != 0 {
		tb.Errorf("ContentLength = %d, want 0", request.ContentLength)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil || len(body) != 0 {
		tb.Errorf("body = %q, error = %v; want empty", body, err)
	}
}

func assertRequestURL(tb testing.TB, rawURL, wantPath string, wantQuery url.Values) {
	tb.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		tb.Fatalf("url.Parse() error = %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "old.reddit.com" || parsed.Path != wantPath || parsed.Query().Encode() != wantQuery.Encode() {
		tb.Fatalf("URL = %q, want pinned origin path=%q query=%q", rawURL, wantPath, wantQuery.Encode())
	}
}

func assertClientError(tb testing.TB, err error, class ErrorClass, endpoint Endpoint, status int) {
	tb.Helper()
	if err == nil {
		tb.Fatalf("error = nil, want class %q", class)
	}
	var adapterError *Error
	if !errors.As(err, &adapterError) {
		tb.Fatalf("error = %v, want *Error", err)
	}
	if adapterError.Class != class || adapterError.Endpoint != endpoint || adapterError.StatusCode != status {
		tb.Fatalf("error = %#v, want class=%q endpoint=%q status=%d", adapterError, class, endpoint, status)
	}
}

func testClientComment(id, body, postID, parent string, replies any) map[string]any {
	return map[string]any{
		"kind": "t1",
		"data": map[string]any{
			"id": id, "name": "t1_" + id, "link_id": "t3_" + postID,
			"parent_id": parent, "body": body, "replies": replies,
		},
	}
}

func testClientMore(id, parent string) map[string]any {
	return map[string]any{"kind": "more", "data": map[string]any{
		"id": id, "name": "t1_" + id, "children": []string{id}, "count": 1, "parent_id": parent,
	}}
}
