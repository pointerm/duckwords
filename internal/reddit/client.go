package reddit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	redditAPIEndpoint = "https://oauth.reddit.com"

	// A normal comment listing is much smaller than this. The default leaves room
	// for large trees while preventing one response from exhausting process memory.
	defaultMaxAPIResponseBytes  int64 = 16 << 20
	absoluteMaxAPIResponseBytes int64 = 64 << 20
	maxAPIHTTPTimeout                 = 2 * time.Minute
)

var (
	// ErrClientConfig identifies invalid or unsafe Reddit API client configuration.
	ErrClientConfig = errors.New("invalid reddit API client configuration")
	// ErrClientEndpoint identifies an endpoint that violates production or test policy.
	ErrClientEndpoint = errors.New("invalid reddit API endpoint")
	// ErrClientRedirect identifies a blocked Reddit API redirect.
	ErrClientRedirect = errors.New("reddit API redirect blocked")
)

// ClientConfig contains the HTTP, authentication, and per-post resource policy for
// a Reddit API client. A zero MaxResponseBytes or zero ThingLimits selects the
// bounded package defaults; partially specified ThingLimits are rejected.
type ClientConfig struct {
	HTTPClient  *http.Client
	TokenSource *TokenSource
	// RequestPolicy must be the same process-wide instance used by TokenSource. A
	// nil value inherits the token source's policy for source compatibility.
	RequestPolicy    *RequestPolicy
	MaxResponseBytes int64
	ThingLimits      ThingLimits
	// TraversalBudget is shared by every concurrent walk and admits accepted response
	// bodies before they are read. A nil value constructs the bounded default for this
	// Client; inject one instance into multiple clients when a process intentionally
	// owns more than one.
	TraversalBudget *TraversalBudget
}

// Client retrieves and walks authenticated Reddit comment trees. A Client is safe
// for concurrent use; all calls share one serialized morechildren gate.
type Client struct {
	httpClient       *http.Client
	tokenSource      *TokenSource
	apiBase          url.URL
	userAgent        string
	requestPolicy    *RequestPolicy
	maxResponseBytes int64
	thingLimits      ThingLimits
	traversalBudget  *TraversalBudget

	// Callers acquire by sending and release by receiving. Keeping the semaphore
	// instance on the shared Client serializes the endpoint across concurrent posts
	// without package-level mutable state.
	moreGate chan struct{}
}

// NewClient constructs a Reddit client pinned to the official OAuth API origin.
// Endpoint injection is intentionally unavailable to application configuration so
// bearer tokens cannot be redirected to an arbitrary host.
func NewClient(config ClientConfig) (*Client, error) {
	return newClient(config, redditAPIEndpoint, endpointProduction)
}

// newTestClient accepts only a loopback origin and exists solely for deterministic
// httptest contracts. It cannot weaken the fixed production endpoint policy.
func newTestClient(config ClientConfig, endpoint string) (*Client, error) {
	return newClient(config, endpoint, endpointLoopbackTest)
}

func newClient(config ClientConfig, endpoint string, policy endpointPolicy) (*Client, error) {
	maxResponseBytes, thingLimits, requestPolicy, traversalBudget, err := validateClientConfig(config)
	if err != nil {
		return nil, err
	}
	apiBase, err := validateAPIEndpoint(endpoint, policy)
	if err != nil {
		return nil, err
	}

	// Copy the client so DuckWords can enforce a strict redirect policy without
	// mutating a caller-owned dependency. The configured transport remains shared
	// and must therefore obey net/http's concurrency-safety contract.
	httpClient := *config.HTTPClient
	// Bearer API requests do not use cookies. Clearing a caller-owned jar prevents
	// unrelated session cookies from crossing this security boundary or API cookies
	// from being persisted by an external jar.
	httpClient.Jar = nil
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return ErrClientRedirect
	}

	return &Client{
		httpClient:       &httpClient,
		tokenSource:      config.TokenSource,
		apiBase:          *apiBase,
		userAgent:        config.TokenSource.userAgent,
		requestPolicy:    requestPolicy,
		maxResponseBytes: maxResponseBytes,
		thingLimits:      thingLimits,
		traversalBudget:  traversalBudget,
		moreGate:         make(chan struct{}, 1),
	}, nil
}

func validateClientConfig(config ClientConfig) (int64, ThingLimits, *RequestPolicy, *TraversalBudget, error) {
	if config.HTTPClient == nil {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: HTTP client is required", ErrClientConfig)
	}
	if config.HTTPClient.Transport == nil {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: HTTP transport must be explicit", ErrClientConfig)
	}
	if config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > maxAPIHTTPTimeout {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: HTTP timeout must be positive and at most %s", ErrClientConfig, maxAPIHTTPTimeout)
	}
	if config.TokenSource == nil || config.TokenSource.client == nil || config.TokenSource.now == nil ||
		config.TokenSource.endpoint == "" || config.TokenSource.userAgent == "" || config.TokenSource.requestPolicy == nil {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: initialized token source is required", ErrClientConfig)
	}
	requestPolicy := config.RequestPolicy
	if requestPolicy == nil {
		requestPolicy = config.TokenSource.requestPolicy
	}
	if requestPolicy != config.TokenSource.requestPolicy {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: client and token source must share one request policy", ErrClientConfig)
	}

	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxAPIResponseBytes
	}
	if maxResponseBytes < 0 || maxResponseBytes > absoluteMaxAPIResponseBytes {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: response limit must be between 1 and %d bytes, or zero for the default", ErrClientConfig, absoluteMaxAPIResponseBytes)
	}

	thingLimits := config.ThingLimits
	if thingLimits == (ThingLimits{}) {
		thingLimits = DefaultThingLimits()
	}
	if err := validateThingLimits(thingLimits); err != nil {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: %v", ErrClientConfig, err)
	}

	traversalBudget := config.TraversalBudget
	if traversalBudget == nil {
		budgetConfig := DefaultTraversalBudgetConfig()
		// A caller may intentionally raise the per-response cap. The default shared
		// budget must still admit one legal response, while custom injected budgets
		// retain their explicit independent policy.
		if budgetConfig.MaxInFlightResponseBytes < maxResponseBytes {
			budgetConfig.MaxInFlightResponseBytes = maxResponseBytes
		}
		var err error
		traversalBudget, err = NewTraversalBudget(budgetConfig)
		if err != nil {
			return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: traversal budget: %v", ErrClientConfig, err)
		}
	}
	if traversalBudget.responseBytes == nil || traversalBudget.maxResponse < maxResponseBytes || traversalBudget.maxRetained <= 0 {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: traversal budget cannot admit configured response limit", ErrClientConfig)
	}
	return maxResponseBytes, thingLimits, requestPolicy, traversalBudget, nil
}

func validateAPIEndpoint(endpoint string, policy endpointPolicy) (*url.URL, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "" {
		return nil, ErrClientEndpoint
	}

	switch policy {
	case endpointProduction:
		if parsed.Scheme != "https" || parsed.Host != "oauth.reddit.com" || endpoint != redditAPIEndpoint {
			return nil, ErrClientEndpoint
		}
	case endpointLoopbackTest:
		ip := net.ParseIP(parsed.Hostname())
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || ip == nil || !ip.IsLoopback() {
			return nil, ErrClientEndpoint
		}
	default:
		return nil, ErrClientEndpoint
	}
	return parsed, nil
}

// WalkComments retrieves a post's initial listing, expands every reachable
// morechildren placeholder, and visits each usable comment body exactly once. Any
// returned error means the tree is incomplete and the caller must discard work
// derived from earlier visitor calls.
func (c *Client) WalkComments(
	ctx context.Context,
	postID string,
	visit func(Comment) error,
) (WalkStats, error) {
	var stats WalkStats
	if c == nil || c.httpClient == nil || c.tokenSource == nil || c.requestPolicy == nil || c.traversalBudget == nil || c.moreGate == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, "", 0, ErrClientConfig)
	}
	if ctx == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, "", 0, errMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return stats, newError(ErrorCanceled, EndpointComments, validDiagnosticPostID(postID), 0, err)
	}
	if !validPostID(postID) {
		return stats, newError(ErrorInvalidInput, EndpointComments, "", 0, errInvalidPostID)
	}
	if visit == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, postID, 0, errNilCommentVisitor)
	}

	initial, err := c.requestAuthenticated(ctx, EndpointComments, postID, c.commentsRequest(postID))
	if err != nil {
		return stats, err
	}
	fetch := func(fetchCtx context.Context, childIDs []string) (responsePayload, error) {
		request, buildErr := c.moreChildrenRequest(postID, childIDs)
		if buildErr != nil {
			return responsePayload{}, buildErr
		}
		return c.requestAuthenticated(fetchCtx, EndpointMoreChildren, postID, request)
	}
	fetchContinuation := func(fetchCtx context.Context, parentCommentID string) (responsePayload, error) {
		request, buildErr := c.continuationRequest(postID, parentCommentID)
		if buildErr != nil {
			return responsePayload{}, buildErr
		}
		return c.requestAuthenticated(fetchCtx, EndpointContinuation, postID, request)
	}
	return walkDecodedCompleteWithBudget(ctx, postID, initial.take(), c.thingLimits, c.traversalBudget, fetch, fetchContinuation, visit)
}

func validDiagnosticPostID(postID string) string {
	if validPostID(postID) {
		return postID
	}
	return ""
}

// apiRequest is one fully specified Reddit API call. A non-empty form is sent as a
// urlencoded request body; only /api/morechildren uses one.
type apiRequest struct {
	method string
	url    string
	form   string
}

func (c *Client) commentsRequest(postID string) apiRequest {
	target := c.apiBase
	target.Path = "/comments/" + postID
	query := make(url.Values, 3)
	query.Set("raw_json", "1")
	query.Set("showmore", "true")
	query.Set("sort", "confidence")
	target.RawQuery = query.Encode()
	return apiRequest{method: http.MethodGet, url: target.String()}
}

// continuationRequest uses the documented focal-comment view with zero parent context
// to continue Reddit's depth-truncated t1__ placeholder. Keeping this distinct from
// morechildren preserves both wire contracts and their independent request budgets.
func (c *Client) continuationRequest(postID, parentCommentID string) (apiRequest, error) {
	if !validPostID(postID) || !validCommentID(parentCommentID) {
		return apiRequest{}, newError(ErrorInvalidInput, EndpointContinuation, validDiagnosticPostID(postID), 0, errMalformedResponse)
	}
	target := c.apiBase
	target.Path = "/comments/" + postID
	query := make(url.Values, 6)
	query.Set("comment", parentCommentID)
	query.Set("context", "0")
	query.Set("raw_json", "1")
	query.Set("showmore", "true")
	query.Set("sort", "confidence")
	target.RawQuery = query.Encode()
	return apiRequest{method: http.MethodGet, url: target.String()}, nil
}

// moreChildrenRequest builds the documented POST form for /api/morechildren. Reddit
// specifies POST for this endpoint even though it only reads data, and a batch of
// 100 comment IDs is well past a safe query-string length. Replaying it is therefore
// still safe for the retry policy. raw_json stays on the query string, where Reddit
// applies it uniformly to every request, so response bodies arrive unescaped.
func (c *Client) moreChildrenRequest(postID string, childIDs []string) (apiRequest, error) {
	if len(childIDs) == 0 || len(childIDs) > moreChildrenBatchSize {
		return apiRequest{}, newError(ErrorInvalidInput, EndpointMoreChildren, postID, 0, errMalformedResponse)
	}
	for _, childID := range childIDs {
		if !validCommentID(childID) {
			return apiRequest{}, newError(ErrorInvalidInput, EndpointMoreChildren, postID, 0, errMalformedResponse)
		}
	}

	target := c.apiBase
	target.Path = "/api/morechildren"
	target.RawQuery = "raw_json=1"
	form := make(url.Values, 5)
	form.Set("api_type", "json")
	form.Set("children", strings.Join(childIDs, ","))
	form.Set("limit_children", "false")
	form.Set("link_id", "t3_"+postID)
	form.Set("sort", "confidence")
	return apiRequest{method: http.MethodPost, url: target.String(), form: form.Encode()}, nil
}

func (c *Client) requestAuthenticated(
	ctx context.Context,
	endpoint Endpoint,
	postID string,
	request apiRequest,
) (responsePayload, error) {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return responsePayload{}, c.classifyTokenError(ctx, endpoint, postID, err)
	}
	// Initial token acquisition owns its own bounded OAuth session. Start the
	// bearer-request session only after a token is available so its elapsed budget
	// describes this logical API request and its possible authentication replay.
	session := c.requestPolicy.newRetrySession()

	payload, err := session.doAfterPayload(ctx, endpoint, postID, nil, c.requestGate(endpoint, postID), func(attemptCtx context.Context) (policyAttemptResult, error) {
		return c.doAttempt(attemptCtx, endpoint, postID, request, token)
	})
	if !isUnauthorized(err) {
		return payload.take(), err
	}

	// A 401 may mean that a still-cached token was revoked or expired early. Exact
	// invalidation avoids evicting a newer token installed by a concurrent request;
	// the logical request is replayed once and only once.
	authenticationErr := err
	c.tokenSource.Invalidate(token)
	if session.remainingBudget() <= 0 {
		return responsePayload{}, authenticationErr
	}
	session.recordAuthenticationReplay(endpoint, postID, authenticationErr)
	// The observer is synchronous and may consume the remaining session time. Never
	// start OAuth from the stale pre-observer snapshot.
	remaining := session.remainingBudget()
	if remaining <= 0 {
		return responsePayload{}, authenticationErr
	}
	// Token refresh is part of this logical authentication replay. Bound its own
	// OAuth retry session by the API session's remaining elapsed budget so nested
	// retry policies cannot turn one configured 45s request budget into roughly 90s.
	refreshCtx, cancelRefresh := context.WithTimeoutCause(ctx, remaining, errRetryBudget)
	token, err = c.tokenSource.Token(refreshCtx)
	refreshBudgetEnded := ctx.Err() == nil &&
		(errors.Is(context.Cause(refreshCtx), errRetryBudget) || session.remainingBudget() <= 0)
	cancelRefresh()
	if err != nil {
		if refreshBudgetEnded {
			return responsePayload{}, authenticationErr
		}
		return responsePayload{}, c.classifyTokenError(ctx, endpoint, postID, err)
	}
	return session.doAfterPayload(ctx, endpoint, postID, authenticationErr, c.requestGate(endpoint, postID), func(attemptCtx context.Context) (policyAttemptResult, error) {
		return c.doAttempt(attemptCtx, endpoint, postID, request, token)
	})
}

func (c *Client) requestGate(endpoint Endpoint, postID string) policyAttemptGate {
	if endpoint != EndpointMoreChildren {
		return nil
	}
	return func(ctx context.Context) (func(), error) {
		if err := c.acquireMoreGate(ctx, postID); err != nil {
			return nil, err
		}
		return c.releaseMoreGate, nil
	}
}

func (c *Client) classifyTokenError(ctx context.Context, endpoint Endpoint, postID string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return newError(ErrorCanceled, endpoint, postID, 0, ctxErr)
	}
	var adapterError *Error
	if errors.As(err, &adapterError) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newError(ErrorCanceled, endpoint, postID, 0, err)
	}
	return newError(ErrorAuthentication, EndpointOAuthToken, "", 0, err)
}

// doAttempt performs one HTTP exchange. Each attempt builds its own request, so a
// form body is recreated rather than replayed from a consumed reader.
func (c *Client) doAttempt(
	ctx context.Context,
	endpoint Endpoint,
	postID string,
	spec apiRequest,
	token string,
) (policyAttemptResult, error) {
	var body io.Reader
	if spec.form != "" {
		body = strings.NewReader(spec.form)
	}
	request, err := http.NewRequestWithContext(ctx, spec.method, spec.url, body)
	if err != nil {
		return policyAttemptResult{}, newError(ErrorInvalidInput, endpoint, postID, 0, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", c.userAgent)
	if spec.form != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	result, err := executePayloadAttempt(
		ctx,
		c.httpClient,
		request,
		endpoint,
		postID,
		c.maxResponseBytes,
		c.traversalBudget.acquireResponse,
	)
	if errors.Is(err, ErrClientRedirect) {
		return result, newError(ErrorProtocol, endpoint, postID, 0, ErrClientRedirect)
	}
	return result, err
}

func (c *Client) acquireMoreGate(ctx context.Context, postID string) error {
	select {
	case c.moreGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return newError(ErrorCanceled, EndpointMoreChildren, postID, 0, ctx.Err())
	}
}

func (c *Client) releaseMoreGate() {
	<-c.moreGate
}

func isUnauthorized(err error) bool {
	var adapterError *Error
	return errors.As(err, &adapterError) &&
		adapterError.Class == ErrorAuthentication && adapterError.StatusCode == http.StatusUnauthorized
}
