package reddit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	redditPublicEndpoint = "https://old.reddit.com"

	// A normal comment listing is much smaller than this. The default leaves room
	// for large trees while preventing one response from exhausting process memory.
	defaultMaxAPIResponseBytes  int64 = 16 << 20
	absoluteMaxAPIResponseBytes int64 = 64 << 20
	maxAPIHTTPTimeout                 = 2 * time.Minute
	minUserAgentBytes                 = 8
	maxUserAgentBytes                 = 256
)

var (
	// ErrClientConfig identifies invalid or unsafe Reddit client configuration.
	ErrClientConfig = errors.New("invalid reddit client configuration")
	// ErrClientEndpoint identifies an endpoint that violates production or test policy.
	ErrClientEndpoint = errors.New("invalid reddit endpoint")
	// ErrClientRedirect identifies a blocked Reddit redirect. Redirects are not
	// followed because they may leave the pinned public origin or lead to an HTML
	// login/interstitial page.
	ErrClientRedirect = errors.New("reddit redirect blocked")
)

// PostRef is the minimal validated input required to retrieve one post. JSONPath is
// a relative old.reddit.com permalink path ending in "/.json"; it never contains a
// query, fragment, credentials, host, or escaped bytes.
type PostRef struct {
	ID       string
	JSONPath string
}

// ClientConfig contains the HTTP identity and per-post resource policy for the fixed
// Reddit JSON client. A zero MaxResponseBytes or zero ThingLimits selects bounded
// package defaults; partially specified ThingLimits are rejected.
type ClientConfig struct {
	HTTPClient    *http.Client
	UserAgent     string
	RequestPolicy *RequestPolicy
	// BrowserSession is an explicit, immutable opt-in header profile. Nil keeps
	// the default cookie-free public JSON contract.
	BrowserSession *BrowserSession

	MaxResponseBytes int64
	ThingLimits      ThingLimits
	// TraversalBudget is shared by every concurrent walk and admits accepted response
	// bodies before they are read. A nil value constructs the bounded default.
	TraversalBudget *TraversalBudget
}

// Client retrieves and walks Reddit comment trees at the fixed origin. A Client is
// safe for concurrent use; all focal-comment expansion attempts share one gate.
type Client struct {
	httpClient       *http.Client
	apiBase          url.URL
	userAgent        string
	browserSession   *BrowserSession
	requestPolicy    *RequestPolicy
	maxResponseBytes int64
	thingLimits      ThingLimits
	traversalBudget  *TraversalBudget

	// Focal expansion requests are deliberately serialized across concurrent posts.
	expansionGate chan struct{}
}

type endpointPolicy uint8

const (
	endpointProduction endpointPolicy = iota
	endpointLoopbackTest
)

// NewClient constructs a client pinned to the old.reddit.com HTTPS origin.
// Endpoint injection is unavailable to application configuration.
func NewClient(config ClientConfig) (*Client, error) {
	return newClient(config, redditPublicEndpoint, endpointProduction)
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

	// Copy the client so the redirect and cookie policies do not mutate a
	// caller-owned dependency. The transport remains shared and must be safe for
	// concurrent use under net/http's contract.
	httpClient := *config.HTTPClient
	httpClient.Jar = nil
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return ErrClientRedirect
	}

	return &Client{
		httpClient:       &httpClient,
		apiBase:          *apiBase,
		userAgent:        config.UserAgent,
		browserSession:   config.BrowserSession,
		requestPolicy:    requestPolicy,
		maxResponseBytes: maxResponseBytes,
		thingLimits:      thingLimits,
		traversalBudget:  traversalBudget,
		expansionGate:    make(chan struct{}, 1),
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
	if !validUserAgent(config.UserAgent) {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: user agent must be %d-%d printable ASCII bytes", ErrClientConfig, minUserAgentBytes, maxUserAgentBytes)
	}
	if config.BrowserSession != nil && !config.BrowserSession.valid() {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: browser session is invalid", ErrClientConfig)
	}
	if config.RequestPolicy == nil {
		return 0, ThingLimits{}, nil, nil, fmt.Errorf("%w: request policy is required", ErrClientConfig)
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
	return maxResponseBytes, thingLimits, config.RequestPolicy, traversalBudget, nil
}

func validUserAgent(value string) bool {
	if len(value) < minUserAgentBytes || len(value) > maxUserAgentBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

func validateAPIEndpoint(endpoint string, policy endpointPolicy) (*url.URL, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "" {
		return nil, ErrClientEndpoint
	}

	switch policy {
	case endpointProduction:
		if parsed.Scheme != "https" || parsed.Host != "old.reddit.com" || endpoint != redditPublicEndpoint {
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

// WalkComments retrieves a post's initial public JSON listing, expands every
// reachable placeholder through focal GET requests, and visits each usable body
// exactly once. Any error means completeness was not proven and the caller must
// discard work derived from prior visitor calls.
func (c *Client) WalkComments(
	ctx context.Context,
	post PostRef,
	visit func(Comment) error,
) (WalkStats, error) {
	var stats WalkStats
	if c == nil || c.httpClient == nil || c.requestPolicy == nil || c.traversalBudget == nil || c.expansionGate == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, "", 0, ErrClientConfig)
	}
	if ctx == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, "", 0, errMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return stats, newError(ErrorCanceled, EndpointComments, validDiagnosticPostID(post.ID), 0, err)
	}
	if !validPostRef(post) {
		return stats, newError(ErrorInvalidInput, EndpointComments, validDiagnosticPostID(post.ID), 0, errInvalidPostRef)
	}
	if visit == nil {
		return stats, newError(ErrorInvalidInput, EndpointComments, post.ID, 0, errNilCommentVisitor)
	}

	initial, err := c.request(ctx, EndpointComments, post.ID, c.commentsRequest(post))
	if err != nil {
		return stats, err
	}
	fetchExpansion := func(fetchCtx context.Context, childID string) (responsePayload, error) {
		request, buildErr := c.focalRequest(post, childID, EndpointCommentExpansion)
		if buildErr != nil {
			return responsePayload{}, buildErr
		}
		return c.request(fetchCtx, EndpointCommentExpansion, post.ID, request)
	}
	fetchContinuation := func(fetchCtx context.Context, parentCommentID string) (continuationResponse, error) {
		request, buildErr := c.focalRequest(post, parentCommentID, EndpointContinuation)
		if buildErr != nil {
			return continuationResponse{}, buildErr
		}
		session := c.requestPolicy.newRetrySession()
		payload, requestErr := c.requestWithSession(fetchCtx, session, EndpointContinuation, post.ID, request)
		if requestErr != nil {
			return continuationResponse{}, requestErr
		}
		response := continuationResponse{payload: payload.take()}
		if session.canRetry() {
			response.canReplay = session.canRetry
			response.replay = func(retryCtx context.Context, previousErr error) (responsePayload, error) {
				fallback, fallbackErr := c.continuationFallbackRequest(post, parentCommentID)
				if fallbackErr != nil {
					return responsePayload{}, fallbackErr
				}
				return c.retryRequestWithSession(retryCtx, session, EndpointContinuation, post.ID, previousErr, fallback)
			}
		}
		return response, nil
	}
	return walkDecodedCompleteWithBudget(ctx, post.ID, initial.take(), c.thingLimits, c.traversalBudget, fetchExpansion, fetchContinuation, visit)
}

func validDiagnosticPostID(postID string) string {
	if validPostID(postID) {
		return postID
	}
	return ""
}

func validPostRef(post PostRef) bool {
	if !validPostID(post.ID) || post.ID != strings.ToLower(post.ID) || !strings.HasPrefix(post.JSONPath, "/") ||
		!strings.HasSuffix(post.JSONPath, "/.json") || strings.ContainsAny(post.JSONPath, "%?#\\") || strings.Contains(post.JSONPath, "//") {
		return false
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(post.JSONPath, "/"), "/.json")
	segments := strings.Split(trimmed, "/")
	idIndex := 1
	switch {
	case len(segments) >= 2 && len(segments) <= 3 && segments[0] == "comments":
	case len(segments) >= 4 && len(segments) <= 5 && segments[0] == "r" && segments[2] == "comments" && validSubredditName(segments[1]):
		idIndex = 3
	default:
		return false
	}
	if segments[idIndex] != post.ID {
		return false
	}
	for _, segment := range segments {
		if !validPublicPathSegment(segment) {
			return false
		}
	}
	return true
}

func validSubredditName(value string) bool {
	return len(value) >= 2 && len(value) <= 21 && validPublicPathSegment(value)
}

func validPublicPathSegment(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range []byte(value) {
		if !isASCIIAlphaNumeric(character) && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

type apiRequest struct {
	url string
}

func (c *Client) commentsRequest(post PostRef) apiRequest {
	return c.publicRequest(post.JSONPath, "")
}

func (c *Client) focalRequest(post PostRef, commentID string, endpoint Endpoint) (apiRequest, error) {
	if !validPostRef(post) || !validCommentID(commentID) {
		return apiRequest{}, newError(ErrorInvalidInput, endpoint, validDiagnosticPostID(post.ID), 0, errMalformedResponse)
	}
	return c.publicRequest(post.JSONPath, commentID), nil
}

// continuationFallbackRequest uses Reddit's equivalent path-form focal route for
// bounded semantic replays. The distinct cache key can recover a transient stale
// query-form response without changing the fixed origin or accepting an incomplete
// tree as complete.
func (c *Client) continuationFallbackRequest(post PostRef, commentID string) (apiRequest, error) {
	if !validPostRef(post) || !validCommentID(commentID) {
		return apiRequest{}, newError(ErrorInvalidInput, EndpointContinuation, validDiagnosticPostID(post.ID), 0, errMalformedResponse)
	}
	target := c.apiBase
	target.Path = "/comments/" + post.ID + "/_/" + commentID + "/.json"
	query := make(url.Values, 5)
	query.Set("context", "0")
	query.Set("limit", "500")
	query.Set("raw_json", "1")
	query.Set("showmore", "true")
	query.Set("sort", "confidence")
	target.RawQuery = query.Encode()
	return apiRequest{url: target.String()}, nil
}

func (c *Client) publicRequest(jsonPath, commentID string) apiRequest {
	target := c.apiBase
	target.Path = jsonPath
	query := make(url.Values, 6)
	if commentID != "" {
		query.Set("comment", commentID)
		query.Set("context", "0")
	}
	query.Set("limit", "500")
	query.Set("raw_json", "1")
	query.Set("showmore", "true")
	query.Set("sort", "confidence")
	target.RawQuery = query.Encode()
	return apiRequest{url: target.String()}
}

func (c *Client) request(ctx context.Context, endpoint Endpoint, postID string, spec apiRequest) (responsePayload, error) {
	session := c.requestPolicy.newRetrySession()
	return c.requestWithSession(ctx, session, endpoint, postID, spec)
}

func (c *Client) requestWithSession(ctx context.Context, session *retrySession, endpoint Endpoint, postID string, spec apiRequest) (responsePayload, error) {
	return session.doAfterPayload(ctx, endpoint, postID, nil, c.requestGate(endpoint, postID), func(attemptCtx context.Context) (policyAttemptResult, error) {
		return c.doAttempt(attemptCtx, endpoint, postID, spec)
	})
}

func (c *Client) retryRequestWithSession(
	ctx context.Context,
	session *retrySession,
	endpoint Endpoint,
	postID string,
	previousErr error,
	spec apiRequest,
) (responsePayload, error) {
	return session.retryAfterPayload(ctx, endpoint, postID, previousErr, c.requestGate(endpoint, postID), func(attemptCtx context.Context) (policyAttemptResult, error) {
		return c.doAttempt(attemptCtx, endpoint, postID, spec)
	})
}

func (c *Client) requestGate(endpoint Endpoint, postID string) policyAttemptGate {
	if endpoint != EndpointCommentExpansion {
		return nil
	}
	return func(ctx context.Context) (func(), error) {
		if err := c.acquireExpansionGate(ctx, postID); err != nil {
			return nil, err
		}
		return c.releaseExpansionGate, nil
	}
}

// doAttempt performs one GET exchange. A new request is built for every retry; the
// request always has a nil body. By default it carries neither authentication nor
// cookies; an explicitly configured BrowserSession adds only its reviewed headers.
func (c *Client) doAttempt(ctx context.Context, endpoint Endpoint, postID string, spec apiRequest) (policyAttemptResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.url, nil)
	if err != nil {
		return policyAttemptResult{}, newError(ErrorInvalidInput, endpoint, postID, 0, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	c.browserSession.apply(request)

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
		statusCode := 0
		var adapterErr *Error
		if errors.As(err, &adapterErr) {
			statusCode = adapterErr.StatusCode
		}
		return result, newError(ErrorAccess, endpoint, postID, statusCode, ErrClientRedirect)
	}
	return result, err
}

func (c *Client) acquireExpansionGate(ctx context.Context, postID string) error {
	select {
	case c.expansionGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return newError(ErrorCanceled, EndpointCommentExpansion, postID, 0, ctx.Err())
	}
}

func (c *Client) releaseExpansionGate() {
	<-c.expansionGate
}
