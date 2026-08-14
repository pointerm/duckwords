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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testClientUserAgent = "darwin:duckwords:test-client (by /u/example)"
	testClientToken     = "api-token"
)

func TestClientWalkCommentsExactHTTPContractAndNestedExpansion(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assertAPIHeaders(t, request, testClientToken)
		response.Header().Set("Content-Type", "application/json; charset=utf-8")

		switch request.URL.Path {
		case "/comments/post1":
			assertExactRequest(t, request, http.MethodGet, url.Values{
				"raw_json": {"1"}, "showmore": {"true"}, "sort": {"confidence"},
			})
			_, _ = response.Write(testInitial(t, "post1",
				testComment("root", "first body", "t3_post1", testListing(
					testComment("nested", "nested body", "t1_root", ""),
				)),
				testMore([]string{"extra"}, 1, "t3_post1"),
			))
		case "/api/morechildren":
			assertExactFormRequest(t, request,
				url.Values{"raw_json": {"1"}},
				url.Values{
					"api_type":       {"json"},
					"children":       {"extra"},
					"limit_children": {"false"},
					"link_id":        {"t3_post1"},
					"sort":           {"confidence"},
				})
			_, _ = response.Write(testMoreResponse(t, nil,
				testComment("extra", "expanded body", "t3_post1", nil),
			))
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newClientForServer(t, server, 0, DefaultThingLimits())
	var bodies []string
	stats, err := client.WalkComments(context.Background(), "post1", func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if got, want := strings.Join(bodies, "|"), "first body|nested body|expanded body"; got != want {
		t.Fatalf("visited bodies = %q, want %q", got, want)
	}
	if stats.Comments != 3 || stats.BodiesVisited != 3 || stats.MoreRequests != 1 {
		t.Fatalf("stats = %#v, want three visited comments and one expansion", stats)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

func TestClientWalkCommentsContinuesDepthTruncatedBranch(t *testing.T) {
	t.Parallel()

	continuation := map[string]any{
		"kind": "more",
		"data": map[string]any{
			"id": "_", "name": "t1__", "count": 0,
			"children": []string{}, "parent_id": "t1_parent",
		},
	}
	parent := testComment("parent", "parent body", "t3_post1", testListing(continuation))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		assertAPIHeaders(t, request, testClientToken)
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/comments/post1" {
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(response, request)
			return
		}

		if request.URL.Query().Get("comment") == "" {
			assertExactRequest(t, request, http.MethodGet, url.Values{
				"raw_json": {"1"}, "showmore": {"true"}, "sort": {"confidence"},
			})
			_, _ = response.Write(testInitial(t, "post1", parent))
			return
		}
		assertExactRequest(t, request, http.MethodGet, url.Values{
			"comment": {"parent"}, "context": {"0"}, "raw_json": {"1"},
			"showmore": {"true"}, "sort": {"confidence"},
		})
		_, _ = response.Write(testInitial(t, "post1",
			testComment("parent", "parent body", "t3_post1", testListing(
				testComment("child", "continued body", "t1_parent", ""),
			)),
		))
	}))
	defer server.Close()

	client := newClientForServer(t, server, 0, DefaultThingLimits())
	var bodies []string
	stats, err := client.WalkComments(context.Background(), "post1", func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if got, want := strings.Join(bodies, "|"), "parent body|continued body"; got != want {
		t.Fatalf("visited bodies = %q, want %q", got, want)
	}
	if stats.Things != 6 || stats.Comments != 2 || stats.DuplicateComments != 1 || stats.ContinuationRequests != 1 {
		t.Fatalf("stats = %#v, want one bounded continuation and no duplicate visit", stats)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want initial plus continuation", got)
	}
}

func TestClientWalkCommentsTreatsValidEmptyListingAsComplete(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/comments/post1" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(testEmptyInitial(t, "post1"))
	}))
	defer server.Close()

	client := newClientForServer(t, server, 0, DefaultThingLimits())
	visits := 0
	stats, err := client.WalkComments(context.Background(), "post1", func(Comment) error {
		visits++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if visits != 0 || stats.Things != 1 || stats.Comments != 0 || stats.BodiesVisited != 0 || stats.MoreRequests != 0 {
		t.Fatalf("visits = %d, stats = %#v; want a complete empty tree", visits, stats)
	}
}

func TestClientWalkCommentsSkipsDeletedAndRemovedBodiesButVisitsDescendants(t *testing.T) {
	t.Parallel()

	deleted := testComment("deleted", "[deleted]", "t3_post1", testListing(
		testComment("live", "surviving reply", "t1_deleted", ""),
	))
	removed := testComment("removed", "[removed]", "t3_post1", "")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/comments/post1" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(testInitial(t, "post1", deleted, removed))
	}))
	defer server.Close()

	client := newClientForServer(t, server, 0, DefaultThingLimits())
	var bodies []string
	stats, err := client.WalkComments(context.Background(), "post1", func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if got, want := strings.Join(bodies, "|"), "surviving reply"; got != want {
		t.Fatalf("visited bodies = %q, want %q", got, want)
	}
	if stats.Comments != 3 || stats.BodiesVisited != 1 || stats.BodiesSkipped != 2 {
		t.Fatalf("stats = %#v, want three comments with two unavailable bodies", stats)
	}
}

func TestClientWalkCommentsBatchesMoreChildrenAtOneHundred(t *testing.T) {
	t.Parallel()

	children := make([]string, 205)
	for index := range children {
		children[index] = fmt.Sprintf("x%03d", index)
	}
	var moreRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/comments/post1":
			_, _ = response.Write(testInitial(t, "post1", testMore(children, len(children), "t3_post1")))
		case "/api/morechildren":
			moreRequests.Add(1)
			batch := strings.Split(morePostForm(t, request).Get("children"), ",")
			if len(batch) == 0 || len(batch) > moreChildrenBatchSize {
				t.Errorf("children batch size = %d, want 1..%d", len(batch), moreChildrenBatchSize)
			}
			things := make([]any, 0, len(batch))
			for _, id := range batch {
				things = append(things, testComment(id, "body "+id, "t3_post1", nil))
			}
			_, _ = response.Write(testMoreResponse(t, nil, things...))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newClientForServer(t, server, 0, DefaultThingLimits())
	stats, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	if err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if stats.Comments != len(children) || stats.MoreRequests != 3 || moreRequests.Load() != 3 {
		t.Fatalf("stats = %#v, HTTP more requests = %d", stats, moreRequests.Load())
	}
}

func TestClientWalkCommentsReplaysOneUnauthorizedRequest(t *testing.T) {
	t.Parallel()

	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/access_token":
			call := tokenCalls.Add(1)
			_, _ = fmt.Fprintf(response, `{"access_token":"token-%d","token_type":"bearer","expires_in":3600}`, call)
		case "/comments/post1":
			call := apiCalls.Add(1)
			if call == 1 {
				if got := request.Header.Get("Authorization"); got != "Bearer token-1" {
					t.Errorf("first Authorization = %q", got)
				}
				response.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(response, `{}`)
				return
			}
			if got := request.Header.Get("Authorization"); got != "Bearer token-2" {
				t.Errorf("replayed Authorization = %q", got)
			}
			_, _ = response.Write(testEmptyInitial(t, "post1"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client := newClientWithSharedServer(t, server, 0, DefaultThingLimits())
	if _, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil }); err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if tokenCalls.Load() != 2 || apiCalls.Load() != 2 {
		t.Fatalf("token calls = %d, API calls = %d, want 2 each", tokenCalls.Load(), apiCalls.Load())
	}
}

func TestClientConcurrentUnauthorizedRequestsShareOneRefresh(t *testing.T) {
	t.Parallel()

	postIDs := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7"}
	const delayedPostID = "p0"

	var tokenCalls atomic.Int32
	var staleArrivals atomic.Int32
	allStaleArrived := make(chan struct{})
	newTokenObserved := make(chan struct{})
	var newTokenObservedOnce sync.Once
	var attemptsMu sync.Mutex
	attempts := make(map[string]int, len(postIDs))

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/access_token" {
			call := tokenCalls.Add(1)
			_, _ = fmt.Fprintf(response, `{"access_token":"token-%d","token_type":"bearer","expires_in":3600}`, call)
			return
		}

		postID := strings.TrimPrefix(request.URL.Path, "/comments/")
		attemptsMu.Lock()
		attempts[postID]++
		attemptsMu.Unlock()

		switch request.Header.Get("Authorization") {
		case "Bearer token-1":
			if staleArrivals.Add(1) == int32(len(postIDs)) {
				close(allStaleArrived)
			}
			select {
			case <-allStaleArrived:
			case <-request.Context().Done():
				return
			}
			// Hold one stale-token response until another logical request has
			// already obtained and used token-2. Its delayed 401 must not evict
			// that newer cached token.
			if postID == delayedPostID {
				select {
				case <-newTokenObserved:
				case <-request.Context().Done():
					return
				}
			}
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{}`)
		case "Bearer token-2":
			newTokenObservedOnce.Do(func() { close(newTokenObserved) })
			_, _ = response.Write(testEmptyInitial(t, postID))
		default:
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{}`)
		}
	}))
	defer server.Close()

	client := newClientWithSharedServer(t, server, 0, DefaultThingLimits())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	errorsFound := make(chan error, len(postIDs))
	var group sync.WaitGroup
	group.Add(len(postIDs))
	for _, postID := range postIDs {
		go func(id string) {
			defer group.Done()
			<-start
			_, err := client.WalkComments(ctx, id, func(Comment) error { return nil })
			errorsFound <- err
		}(postID)
	}
	close(start)
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("WalkComments() error = %v", err)
		}
	}

	if token, err := client.tokenSource.Token(context.Background()); err != nil || token != "token-2" {
		t.Fatalf("cached Token() = %q, %v; want token-2", token, err)
	}
	if got := tokenCalls.Load(); got != 2 {
		t.Fatalf("token endpoint calls = %d, want one initial acquisition plus one shared refresh", got)
	}
	attemptsMu.Lock()
	defer attemptsMu.Unlock()
	for _, postID := range postIDs {
		if got := attempts[postID]; got != 2 {
			t.Errorf("API attempts for %s = %d, want exactly 2", postID, got)
		}
	}
}

func TestClientWalkCommentsDoesNotReplayPersistentUnauthorized(t *testing.T) {
	t.Parallel()

	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/access_token" {
			call := tokenCalls.Add(1)
			_, _ = fmt.Fprintf(response, `{"access_token":"token-%d","token_type":"bearer","expires_in":3600}`, call)
			return
		}
		apiCalls.Add(1)
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(response, `{}`)
	}))
	defer server.Close()

	client := newClientWithSharedServer(t, server, 0, DefaultThingLimits())
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	assertClientError(t, err, ErrorAuthentication, EndpointComments, http.StatusUnauthorized)
	if tokenCalls.Load() != 2 || apiCalls.Load() != 2 {
		t.Fatalf("token calls = %d, API calls = %d, want exactly 2 each", tokenCalls.Load(), apiCalls.Load())
	}
}

func TestClientWalkCommentsReportsTokenAcquisitionFailureBeforeAPIRequest(t *testing.T) {
	t.Parallel()

	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/access_token" {
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{"error":"invalid_client","private":"must-not-leak"}`)
			return
		}
		apiCalls.Add(1)
		http.NotFound(response, request)
	}))
	defer server.Close()

	client := newClientWithSharedServer(t, server, 0, DefaultThingLimits())
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	assertClientError(t, err, ErrorAuthentication, EndpointOAuthToken, http.StatusUnauthorized)
	if apiCalls.Load() != 0 {
		t.Fatalf("API calls = %d, want zero after token failure", apiCalls.Load())
	}
	if strings.Contains(err.Error(), "must-not-leak") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("token failure leaked response or endpoint: %v", err)
	}
}

func TestClientClassifyTokenError(t *testing.T) {
	t.Parallel()

	client := &Client{}
	t.Run("typed adapter error is preserved", func(t *testing.T) {
		cause := newError(ErrorServer, EndpointOAuthToken, "", http.StatusServiceUnavailable, errors.New("token service failed"))
		if got := client.classifyTokenError(context.Background(), EndpointComments, "post1", cause); got != cause {
			t.Fatalf("classifyTokenError() = %v, want original typed error", got)
		}
	})
	t.Run("caller cancellation takes precedence", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := client.classifyTokenError(ctx, EndpointComments, "post1", errors.New("opaque failure"))
		assertClientError(t, err, ErrorCanceled, EndpointComments, 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation cause", err)
		}
	})
	t.Run("token cancellation with live caller remains cancellation", func(t *testing.T) {
		err := client.classifyTokenError(context.Background(), EndpointComments, "post1", context.DeadlineExceeded)
		assertClientError(t, err, ErrorCanceled, EndpointComments, 0)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want token deadline cause", err)
		}
	})
	t.Run("unexpected token failure is authentication", func(t *testing.T) {
		cause := errors.New("opaque token failure")
		err := client.classifyTokenError(context.Background(), EndpointComments, "post1", cause)
		assertClientError(t, err, ErrorAuthentication, EndpointOAuthToken, 0)
		if !errors.Is(err, cause) || strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("error = %v, want redacted unwrap-able cause", err)
		}
	})
}

func TestClientMoreGateSerializesOwnershipAndHonorsContext(t *testing.T) {
	t.Parallel()

	client := &Client{moreGate: make(chan struct{}, 1)}
	if err := client.acquireMoreGate(context.Background(), "posta"); err != nil {
		t.Fatalf("first acquireMoreGate() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.acquireMoreGate(ctx, "postb")
	assertClientError(t, err, ErrorCanceled, EndpointMoreChildren, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireMoreGate() error = %v, want context.Canceled", err)
	}
	if got := len(client.moreGate); got != 1 {
		t.Fatalf("more gate permits held after rejected waiter = %d, want 1", got)
	}

	// Only the current owner releases the permit. A subsequent caller can acquire
	// immediately after that release and becomes responsible for the next release.
	client.releaseMoreGate()
	if err := client.acquireMoreGate(context.Background(), "postb"); err != nil {
		t.Fatalf("second acquireMoreGate() after release error = %v", err)
	}
	client.releaseMoreGate()
}

func TestClientConcurrentWalkCommentsSerializeMoreChildren(t *testing.T) {
	t.Parallel()

	postIDs := []string{"posta", "postb"}
	rootIDs := map[string]string{"posta": "roota", "postb": "rootb"}
	childIDs := map[string]string{"posta": "childa", "postb": "childb"}
	initialPayloads := make(map[string][]byte, len(postIDs))
	morePayloads := make(map[string][]byte, len(postIDs))
	for _, postID := range postIDs {
		rootID := rootIDs[postID]
		childID := childIDs[postID]
		initialPayloads[postID] = testInitial(t, postID,
			testClientComment(rootID, "root body "+postID, postID, "t3_"+postID, ""),
			testMore([]string{childID}, 1, "t3_"+postID),
		)
		morePayloads[childID] = testMoreResponse(t, nil,
			testClientComment(childID, "child body "+postID, postID, "t3_"+postID, ""),
		)
	}

	firstMoreEntered := make(chan string, 1)
	secondMoreEntered := make(chan string, 1)
	releaseFirstMore := make(chan struct{})
	var releaseOnce sync.Once
	var moreCalls atomic.Int32
	var inFlight atomic.Int32
	var peakInFlight atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/comments/posta", "/comments/postb":
			postID := strings.TrimPrefix(request.URL.Path, "/comments/")
			_, _ = response.Write(initialPayloads[postID])
		case "/api/morechildren":
			childID := morePostForm(t, request).Get("children")
			payload, ok := morePayloads[childID]
			if !ok {
				http.Error(response, "unexpected synthetic child", http.StatusBadRequest)
				return
			}

			current := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				peak := peakInFlight.Load()
				if current <= peak || peakInFlight.CompareAndSwap(peak, current) {
					break
				}
			}

			switch moreCalls.Add(1) {
			case 1:
				firstMoreEntered <- childID
				select {
				case <-releaseFirstMore:
				case <-request.Context().Done():
					return
				}
			case 2:
				secondMoreEntered <- childID
			default:
				http.Error(response, "too many synthetic expansions", http.StatusInternalServerError)
				return
			}
			_, _ = response.Write(payload)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer releaseOnce.Do(func() { close(releaseFirstMore) })

	client := newClientForServer(t, server, 0, DefaultThingLimits())
	rootVisitorsReady := make(chan struct{})
	var rootVisitors atomic.Int32
	type walkResult struct {
		postID string
		bodies []string
		stats  WalkStats
		err    error
	}
	results := make(chan walkResult, len(postIDs))
	start := make(chan struct{})
	for _, postID := range postIDs {
		go func(id string) {
			<-start
			result := walkResult{postID: id}
			result.stats, result.err = client.WalkComments(ctx, id, func(comment Comment) error {
				if comment.ID == rootIDs[id] {
					if rootVisitors.Add(1) == int32(len(postIDs)) {
						close(rootVisitorsReady)
					}
					select {
					case <-rootVisitorsReady:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				result.bodies = append(result.bodies, comment.Body)
				return nil
			})
			results <- result
		}(postID)
	}
	close(start)

	select {
	case <-firstMoreEntered:
	case <-ctx.Done():
		t.Fatalf("first morechildren request did not enter: %v", ctx.Err())
	}

	// Both walks pass the root-comment barrier before either can expand its stub.
	// Keeping the first handler open briefly therefore proves that the second walk
	// cannot enter the endpoint while the shared client's gate is owned.
	observation := time.NewTimer(100 * time.Millisecond)
	select {
	case childID := <-secondMoreEntered:
		observation.Stop()
		t.Fatalf("second morechildren handler entered concurrently for child %q", childID)
	case <-observation.C:
	}
	releaseOnce.Do(func() { close(releaseFirstMore) })

	seen := make(map[string]walkResult, len(postIDs))
	for range postIDs {
		select {
		case result := <-results:
			seen[result.postID] = result
		case <-ctx.Done():
			t.Fatalf("concurrent walks did not finish: %v", ctx.Err())
		}
	}
	for _, postID := range postIDs {
		result := seen[postID]
		if result.err != nil {
			t.Fatalf("WalkComments(%s) error = %v", postID, result.err)
		}
		wantBodies := []string{"root body " + postID, "child body " + postID}
		if !slices.Equal(result.bodies, wantBodies) {
			t.Fatalf("WalkComments(%s) bodies = %q, want %q", postID, result.bodies, wantBodies)
		}
		if result.stats.Comments != 2 || result.stats.MoreRequests != 1 {
			t.Fatalf("WalkComments(%s) stats = %#v", postID, result.stats)
		}
	}
	if calls, peak := moreCalls.Load(), peakInFlight.Load(); calls != 2 || peak != 1 {
		t.Fatalf("morechildren calls = %d, peak concurrent handlers = %d; want 2 and 1", calls, peak)
	}
}

func TestClientWalkCommentsFaultClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		limit       int64
		wantClass   ErrorClass
		wantStatus  int
	}{
		{name: "forbidden", status: http.StatusForbidden, contentType: "application/json", body: `{}`, wantClass: ErrorForbidden, wantStatus: http.StatusForbidden},
		{name: "not found", status: http.StatusNotFound, contentType: "application/json", body: `{}`, wantClass: ErrorNotFound, wantStatus: http.StatusNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests, contentType: "application/json", body: `{}`, wantClass: ErrorRateLimited, wantStatus: http.StatusTooManyRequests},
		{name: "server", status: http.StatusServiceUnavailable, contentType: "application/json", body: `{}`, wantClass: ErrorServer, wantStatus: http.StatusServiceUnavailable},
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", body: `{}`, wantClass: ErrorProtocol, wantStatus: http.StatusOK},
		{name: "oversized", status: http.StatusOK, contentType: "application/json", body: `123456789`, limit: 8, wantClass: ErrorResourceLimit, wantStatus: http.StatusOK},
		{name: "malformed", status: http.StatusOK, contentType: "application/json", body: `{`, wantClass: ErrorProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", test.contentType)
				response.WriteHeader(test.status)
				_, _ = io.WriteString(response, test.body)
			}))
			defer server.Close()
			client := newClientForServer(t, server, test.limit, DefaultThingLimits())
			_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
			assertClientError(t, err, test.wantClass, EndpointComments, test.wantStatus)
		})
	}
}

func TestClientWalkCommentsRedirectIsBlocked(t *testing.T) {
	t.Parallel()

	destinationCalls := atomic.Int32{}
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL, http.StatusFound)
	}))
	defer origin.Close()

	client := newClientForServer(t, origin, 0, DefaultThingLimits())
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	assertClientError(t, err, ErrorProtocol, EndpointComments, 0)
	if !errors.Is(err, ErrClientRedirect) {
		t.Fatalf("error = %v, want ErrClientRedirect", err)
	}
	if destinationCalls.Load() != 0 {
		t.Fatalf("redirect destination calls = %d, want 0", destinationCalls.Load())
	}
}

func TestClientWalkCommentsCancellationAndVisitorFailure(t *testing.T) {
	t.Parallel()

	t.Run("HTTP cancellation", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		}))
		defer server.Close()
		client := newClientForServer(t, server, 0, DefaultThingLimits())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := client.WalkComments(ctx, "post1", func(Comment) error { return nil })
			done <- err
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			assertClientError(t, err, ErrorCanceled, EndpointComments, 0)
		case <-time.After(2 * time.Second):
			t.Fatal("WalkComments() did not honor cancellation")
		}
	})

	t.Run("visitor", func(t *testing.T) {
		visitorErr := errors.New("private comment processing failure")
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(testInitial(t, "post1", testComment("c1", "body", "t3_post1", nil)))
		}))
		defer server.Close()
		client := newClientForServer(t, server, 0, DefaultThingLimits())
		_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return visitorErr })
		assertClientError(t, err, ErrorVisitor, EndpointComments, 0)
		if !errors.Is(err, visitorErr) {
			t.Fatalf("error = %v, want wrapped visitor cause", err)
		}
	})
}

func TestClientErrorsDoNotExposeAuthorizationOrResponseBody(t *testing.T) {
	t.Parallel()

	const secretBody = "response-secret-that-must-not-escape"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(response, secretBody)
	}))
	defer server.Close()
	client := newClientForServer(t, server, 0, DefaultThingLimits())
	_, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil })
	if err == nil {
		t.Fatal("WalkComments() error = nil")
	}
	formatted := fmt.Sprintf("%v", err)
	for _, forbidden := range []string{testClientToken, secretBody, "Authorization", server.URL} {
		if strings.Contains(formatted, forbidden) {
			t.Fatalf("formatted error %q leaks %q", formatted, forbidden)
		}
	}
}

func TestClientDisablesCallerCookieJar(t *testing.T) {
	t.Parallel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Cookie") != "" {
			t.Errorf("API request unexpectedly sent cookies: %q", request.Header.Get("Cookie"))
		}
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
	config := validClientConfig(t, server)
	config.HTTPClient.Jar = jar
	client, err := newTestClient(config, server.URL)
	if err != nil {
		t.Fatalf("newTestClient() error = %v", err)
	}
	// Prime after construction: this test exercises only the API client's private
	// client copy and never performs OAuth against the API fixture server.
	primeTokenSource(client.tokenSource, testClientToken)
	if _, err := client.WalkComments(context.Background(), "post1", func(Comment) error { return nil }); err != nil {
		t.Fatalf("WalkComments() error = %v", err)
	}
	if cookies := jar.Cookies(origin); len(cookies) != 1 || cookies[0].Name != "browser_session" {
		t.Fatalf("caller jar was mutated: %#v", cookies)
	}
}

func TestNewClientValidationAndArgumentErrors(t *testing.T) {
	t.Parallel()

	baseServer := httptest.NewServer(http.NotFoundHandler())
	defer baseServer.Close()
	valid := validClientConfig(t, baseServer)
	tests := []struct {
		name   string
		mutate func(*ClientConfig)
	}{
		{name: "nil HTTP client", mutate: func(config *ClientConfig) { config.HTTPClient = nil }},
		{name: "implicit transport", mutate: func(config *ClientConfig) { config.HTTPClient = &http.Client{Timeout: time.Second} }},
		{name: "missing timeout", mutate: func(config *ClientConfig) { config.HTTPClient.Timeout = 0 }},
		{name: "excessive timeout", mutate: func(config *ClientConfig) { config.HTTPClient.Timeout = maxAPIHTTPTimeout + time.Second }},
		{name: "nil token source", mutate: func(config *ClientConfig) { config.TokenSource = nil }},
		{name: "negative response limit", mutate: func(config *ClientConfig) { config.MaxResponseBytes = -1 }},
		{name: "excessive response limit", mutate: func(config *ClientConfig) { config.MaxResponseBytes = absoluteMaxAPIResponseBytes + 1 }},
		{name: "partial thing limits", mutate: func(config *ClientConfig) { config.ThingLimits.MaxThings = 1 }},
		{name: "uninitialized traversal budget", mutate: func(config *ClientConfig) { config.TraversalBudget = &TraversalBudget{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			clientCopy := *valid.HTTPClient
			config.HTTPClient = &clientCopy
			test.mutate(&config)
			if _, err := newTestClient(config, baseServer.URL); !errors.Is(err, ErrClientConfig) {
				t.Fatalf("newTestClient() error = %v, want ErrClientConfig", err)
			}
		})
	}

	for _, endpoint := range []string{
		"http://example.test", "https://oauth.reddit.com", "http://127.0.0.1:1/path",
		"http://user:password@127.0.0.1:1", "http://127.0.0.1:1?query=1",
	} {
		if _, err := newTestClient(valid, endpoint); !errors.Is(err, ErrClientEndpoint) {
			t.Errorf("newTestClient(%q) error = %v, want ErrClientEndpoint", endpoint, err)
		}
	}

	client := newClientForServer(t, baseServer, 0, DefaultThingLimits())
	//lint:ignore SA1012 This assertion deliberately exercises the public nil-context contract.
	if _, err := client.WalkComments(nil, "post1", func(Comment) error { return nil }); err == nil {
		t.Error("WalkComments(nil context) error = nil")
	} else {
		assertClientError(t, err, ErrorInvalidInput, EndpointComments, 0)
	}
	if _, err := client.WalkComments(context.Background(), "INVALID-ID", func(Comment) error { return nil }); err == nil {
		t.Error("WalkComments(invalid post ID) error = nil")
	} else {
		assertClientError(t, err, ErrorInvalidInput, EndpointComments, 0)
		if strings.Contains(err.Error(), "INVALID-ID") {
			t.Fatalf("invalid input error leaks untrusted post ID: %v", err)
		}
	}
	if _, err := client.WalkComments(context.Background(), "post1", nil); err == nil {
		t.Error("WalkComments(nil visitor) error = nil")
	} else {
		assertClientError(t, err, ErrorInvalidInput, EndpointComments, 0)
	}
	var nilClient *Client
	if _, err := nilClient.WalkComments(context.Background(), "post1", func(Comment) error { return nil }); err == nil {
		t.Error("nil Client WalkComments() error = nil")
	}
}

func newClientForServer(tb testing.TB, server *httptest.Server, responseLimit int64, limits ThingLimits) *Client {
	tb.Helper()
	config := validClientConfig(tb, server)
	config.MaxResponseBytes = responseLimit
	config.ThingLimits = limits
	client, err := newTestClient(config, server.URL)
	if err != nil {
		tb.Fatalf("newTestClient() error = %v", err)
	}
	primeTokenSource(client.tokenSource, testClientToken)
	return client
}

func testEmptyInitial(tb testing.TB, postID string) []byte {
	tb.Helper()
	return mustJSON(tb, []any{
		testListing(testPost(postID)),
		map[string]any{"kind": "Listing", "data": map[string]any{"children": []any{}}},
	})
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

func newClientWithSharedServer(tb testing.TB, server *httptest.Server, responseLimit int64, limits ThingLimits) *Client {
	tb.Helper()
	config := validClientConfig(tb, server)
	config.MaxResponseBytes = responseLimit
	config.ThingLimits = limits
	tokenSource, err := newTestTokenSource(TokenConfig{
		ClientID: "client-id", ClientSecret: "client-secret", UserAgent: testClientUserAgent,
		HTTPClient: config.HTTPClient, RequestPolicy: config.RequestPolicy,
	}, server.URL+"/api/v1/access_token", time.Now)
	if err != nil {
		tb.Fatalf("newTestTokenSource() error = %v", err)
	}
	config.TokenSource = tokenSource
	client, err := newTestClient(config, server.URL)
	if err != nil {
		tb.Fatalf("newTestClient() error = %v", err)
	}
	return client
}

func validClientConfig(tb testing.TB, server *httptest.Server) ClientConfig {
	tb.Helper()
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	tokenSource, err := newTestTokenSource(TokenConfig{
		ClientID: "client-id", ClientSecret: "client-secret", UserAgent: testClientUserAgent,
		HTTPClient: httpClient, RequestPolicy: newFastTestRequestPolicy(),
	}, server.URL+"/api/v1/access_token", time.Now)
	if err != nil {
		tb.Fatalf("newTestTokenSource() error = %v", err)
	}
	return ClientConfig{HTTPClient: httpClient, TokenSource: tokenSource, RequestPolicy: tokenSource.requestPolicy}
}

func primeTokenSource(source *TokenSource, token string) {
	source.mu.Lock()
	source.token = cachedOAuthToken{value: token, refreshAt: time.Now().Add(time.Hour)}
	source.mu.Unlock()
}

func assertAPIHeaders(tb testing.TB, request *http.Request, token string) {
	tb.Helper()
	if got := request.Header.Get("Accept"); got != "application/json" {
		tb.Errorf("Accept = %q, want application/json", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer "+token {
		tb.Errorf("Authorization = %q, want bearer token", got)
	}
	if got := request.Header.Get("User-Agent"); got != testClientUserAgent {
		tb.Errorf("User-Agent = %q, want %q", got, testClientUserAgent)
	}
}

func assertExactRequest(tb testing.TB, request *http.Request, method string, query url.Values) {
	tb.Helper()
	if request.Method != method {
		tb.Errorf("method = %q, want %q", request.Method, method)
	}
	if got, want := request.URL.Query().Encode(), query.Encode(); got != want {
		tb.Errorf("query = %q, want %q", got, want)
	}
}

// assertExactFormRequest checks a urlencoded POST. Reddit documents
// /api/morechildren as POST, so its parameters travel in the request body while
// raw_json stays on the query string.
func assertExactFormRequest(tb testing.TB, request *http.Request, query, form url.Values) {
	tb.Helper()
	assertExactRequest(tb, request, http.MethodPost, query)
	if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		tb.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", got)
	}
	if got, want := morePostForm(tb, request).Encode(), form.Encode(); got != want {
		tb.Errorf("form = %q, want %q", got, want)
	}
}

// morePostForm parses the request body exactly once and returns only its posted
// values, so a query parameter cannot satisfy an assertion about the body.
func morePostForm(tb testing.TB, request *http.Request) url.Values {
	tb.Helper()
	if err := request.ParseForm(); err != nil {
		tb.Fatalf("ParseForm() error = %v", err)
	}
	return request.PostForm
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

// TestMoreChildrenRequestUsesDocumentedPostForm locks the wire contract for the one
// endpoint Reddit documents as POST. A full 100-ID batch also shows why: the same
// parameters on a query string would produce an impractically long URL.
func TestMoreChildrenRequestUsesDocumentedPostForm(t *testing.T) {
	t.Parallel()

	client := &Client{apiBase: url.URL{Scheme: "https", Host: "oauth.reddit.com"}}

	request, err := client.moreChildrenRequest("post1", []string{"aaa", "bbb"})
	if err != nil {
		t.Fatalf("moreChildrenRequest() error = %v", err)
	}
	if request.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", request.method)
	}
	if request.url != "https://oauth.reddit.com/api/morechildren?raw_json=1" {
		t.Fatalf("url = %q; raw_json must stay on the query string", request.url)
	}
	form, err := url.ParseQuery(request.form)
	if err != nil {
		t.Fatalf("ParseQuery(form) error = %v", err)
	}
	want := url.Values{
		"api_type":       {"json"},
		"children":       {"aaa,bbb"},
		"limit_children": {"false"},
		"link_id":        {"t3_post1"},
		"sort":           {"confidence"},
	}
	if form.Encode() != want.Encode() {
		t.Fatalf("form = %q, want %q", form.Encode(), want.Encode())
	}

	batch := make([]string, moreChildrenBatchSize)
	for index := range batch {
		batch[index] = fmt.Sprintf("c%07d", index)
	}
	full, err := client.moreChildrenRequest("post1", batch)
	if err != nil {
		t.Fatalf("moreChildrenRequest(full batch) error = %v", err)
	}
	if len(full.url) > 128 {
		t.Fatalf("url grew with the batch (%d bytes); IDs belong in the body", len(full.url))
	}
	if !strings.Contains(full.form, batch[moreChildrenBatchSize-1]) {
		t.Fatal("form does not carry the whole batch")
	}

	if _, err := client.moreChildrenRequest("post1", nil); err == nil {
		t.Fatal("moreChildrenRequest(empty) error = nil")
	}
	if _, err := client.moreChildrenRequest("post1", []string{"bad id"}); err == nil {
		t.Fatal("moreChildrenRequest(invalid id) error = nil")
	}
}
