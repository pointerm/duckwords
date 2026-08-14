package reddit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	redditOAuthTokenEndpoint = "https://www.reddit.com/api/v1/access_token"

	// OAuth responses are small. The cap bounds both memory use and the amount of
	// untrusted data read before a malformed response is rejected.
	maxOAuthResponseBytes      int64 = 64 << 10
	maxOAuthTokenBytes               = 8 << 10
	maxOAuthCredentialBytes          = 4 << 10
	maxOAuthUserAgentBytes           = 256
	maxUserAgentComponentBytes       = 64
	maxRedditUsernameBytes           = 64
	maxOAuthHTTPTimeout              = 2 * time.Minute
	maxOAuthTokenLifetime            = 24 * time.Hour
	oauthExpirySafetyCap             = 30 * time.Second
)

var (
	// ErrOAuthConfig identifies invalid or unsafe token-source configuration.
	ErrOAuthConfig = errors.New("invalid reddit OAuth configuration")
	// ErrOAuthEndpoint identifies an endpoint that violates production or test policy.
	ErrOAuthEndpoint = errors.New("invalid reddit OAuth endpoint")
	// ErrOAuthRedirect identifies a blocked token-endpoint redirect.
	ErrOAuthRedirect = errors.New("reddit OAuth redirect blocked")
	// ErrOAuthRequest identifies a failure before a valid HTTP response was received.
	ErrOAuthRequest = errors.New("reddit OAuth request failed")
	// ErrOAuthResponseRead identifies a failure while reading or closing the response.
	ErrOAuthResponseRead = errors.New("reddit OAuth response read failed")
	// ErrOAuthResponseTooLarge identifies a response that exceeds the fixed body cap.
	ErrOAuthResponseTooLarge = errors.New("reddit OAuth response is too large")
	// ErrOAuthUnexpectedStatus identifies a non-success token response.
	ErrOAuthUnexpectedStatus = errors.New("reddit OAuth endpoint returned an unexpected status")
	// ErrOAuthUnexpectedContentType identifies a token response that is not JSON.
	ErrOAuthUnexpectedContentType = errors.New("reddit OAuth response is not JSON")
	// ErrOAuthMalformedResponse identifies syntactically invalid or trailing JSON.
	ErrOAuthMalformedResponse = errors.New("malformed reddit OAuth response")
	// ErrOAuthInvalidToken identifies a missing token or a non-bearer token response.
	ErrOAuthInvalidToken = errors.New("invalid reddit OAuth bearer token")
	// ErrOAuthInvalidExpiry identifies a missing, non-positive, excessive, or stale expiry.
	ErrOAuthInvalidExpiry = errors.New("invalid reddit OAuth token expiry")
)

// TokenConfig contains the credentials and HTTP dependency required by Reddit's
// application-only OAuth flow. Credentials must come from external configuration;
// TokenSource never logs or persists them.
type TokenConfig struct {
	ClientID     string
	ClientSecret string
	UserAgent    string
	HTTPClient   *http.Client
	// RequestPolicy is the process-wide limiter/retry policy shared with Client. A
	// nil value constructs conservative defaults; production composition should
	// pass one explicit instance to both boundaries.
	RequestPolicy *RequestPolicy
	// APIAccessApproved is an explicit safety acknowledgement that Reddit has
	// approved this application's Data API access under its current policy. It is
	// required by the production constructor; deterministic loopback tests bypass
	// it without making a live request possible.
	APIAccessApproved bool
}

type cachedOAuthToken struct {
	value     string
	refreshAt time.Time
}

type oauthFlight struct {
	done       chan struct{}
	err        error
	ownerEnded bool
}

// TokenSource acquires and caches application-only Reddit bearer tokens. It is safe
// for concurrent use. At most one caller owns an acquisition; all other callers wait
// on that acquisition or their own context cancellation.
type TokenSource struct {
	client        *http.Client
	endpoint      string
	clientID      string
	clientSecret  string
	userAgent     string
	now           func() time.Time
	requestPolicy *RequestPolicy

	mu     sync.Mutex
	token  cachedOAuthToken
	flight *oauthFlight
}

// NewTokenSource constructs a token source pinned to Reddit's official HTTPS token
// endpoint. Endpoint injection is intentionally unavailable in production config so
// credentials cannot be redirected to an arbitrary host.
func NewTokenSource(config TokenConfig) (*TokenSource, error) {
	return newTokenSource(config, redditOAuthTokenEndpoint, time.Now, endpointProduction)
}

type endpointPolicy uint8

const (
	endpointProduction endpointPolicy = iota
	endpointLoopbackTest
)

// newTestTokenSource is deliberately unexported: deterministic HTTP tests may send
// placeholder credentials to a loopback server, while application code cannot
// override the fixed production credential destination.
func newTestTokenSource(config TokenConfig, endpoint string, now func() time.Time) (*TokenSource, error) {
	return newTokenSource(config, endpoint, now, endpointLoopbackTest)
}

func newTokenSource(config TokenConfig, endpoint string, now func() time.Time, policy endpointPolicy) (*TokenSource, error) {
	if policy == endpointProduction && !config.APIAccessApproved {
		return nil, fmt.Errorf("%w: explicit Reddit Data API approval confirmation is required", ErrOAuthConfig)
	}
	if err := validateTokenConfig(config); err != nil {
		return nil, err
	}
	if err := validateOAuthEndpoint(endpoint, policy); err != nil {
		return nil, err
	}
	if now == nil {
		return nil, fmt.Errorf("%w: clock is required", ErrOAuthConfig)
	}
	requestPolicy := config.RequestPolicy
	if requestPolicy == nil {
		var err error
		requestPolicy, err = NewRequestPolicy(DefaultRequestPolicyConfig())
		if err != nil {
			return nil, fmt.Errorf("%w: request policy: %v", ErrOAuthConfig, err)
		}
	}
	if requestPolicy.now == nil || requestPolicy.wait == nil || requestPolicy.jitter == nil || requestPolicy.permit == nil {
		return nil, fmt.Errorf("%w: initialized request policy is required", ErrOAuthConfig)
	}

	// Copy the caller's client so redirect policy can be made stricter without
	// mutating a dependency that may be shared elsewhere. The transport remains
	// shared and is expected to be concurrency-safe, as required by net/http.
	client := *config.HTTPClient
	// OAuth and API calls are stateless bearer requests. A caller-supplied cookie
	// jar could attach unrelated browser/session credentials or persist Set-Cookie
	// data, so the private client deliberately disables it.
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return ErrOAuthRedirect
	}

	return &TokenSource{
		client:        &client,
		endpoint:      endpoint,
		clientID:      config.ClientID,
		clientSecret:  config.ClientSecret,
		userAgent:     config.UserAgent,
		now:           now,
		requestPolicy: requestPolicy,
	}, nil
}

func validateTokenConfig(config TokenConfig) error {
	if err := validateSecretConfigValue("client ID", config.ClientID, false); err != nil {
		return err
	}
	if err := validateSecretConfigValue("client secret", config.ClientSecret, true); err != nil {
		return err
	}
	if !validRedditUserAgent(config.UserAgent) {
		return fmt.Errorf("%w: User-Agent must use platform:app:version (by /u/name) with header-safe components", ErrOAuthConfig)
	}
	if config.HTTPClient == nil {
		return fmt.Errorf("%w: HTTP client is required", ErrOAuthConfig)
	}
	if config.HTTPClient.Transport == nil {
		return fmt.Errorf("%w: HTTP transport must be explicit", ErrOAuthConfig)
	}
	if config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > maxOAuthHTTPTimeout {
		return fmt.Errorf("%w: HTTP timeout must be positive and at most %s", ErrOAuthConfig, maxOAuthHTTPTimeout)
	}
	return nil
}

// validRedditUserAgent enforces Reddit's descriptive application identity without
// admitting whitespace, control characters, or delimiter ambiguity into a header.
// Components are deliberately ASCII and independently bounded so an otherwise
// well-formed value cannot become an oversized or misleading request identity.
func validRedditUserAgent(value string) bool {
	if value == "" || len(value) > maxOAuthUserAgentBytes || strings.TrimSpace(value) != value {
		return false
	}
	identity, contact, found := strings.Cut(value, " (by /u/")
	if !found || !strings.HasSuffix(contact, ")") {
		return false
	}
	username := strings.TrimSuffix(contact, ")")
	if username == "" || len(username) > maxRedditUsernameBytes || !validRedditUsername(username) {
		return false
	}
	components := strings.Split(identity, ":")
	if len(components) != 3 {
		return false
	}
	for _, component := range components {
		if !validUserAgentComponent(component) {
			return false
		}
	}
	return true
}

func validUserAgentComponent(value string) bool {
	if value == "" || len(value) > maxUserAgentComponentBytes {
		return false
	}
	hasAlphaNumeric := false
	for _, char := range []byte(value) {
		if isASCIIAlphaNumeric(char) {
			hasAlphaNumeric = true
			continue
		}
		if strings.ContainsRune("._-+/", rune(char)) {
			continue
		}
		return false
	}
	return hasAlphaNumeric
}

func validRedditUsername(value string) bool {
	hasAlphaNumeric := false
	for _, char := range []byte(value) {
		if isASCIIAlphaNumeric(char) {
			hasAlphaNumeric = true
			continue
		}
		if char == '_' || char == '-' {
			continue
		}
		return false
	}
	return hasAlphaNumeric
}

func isASCIIAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func validateSecretConfigValue(name, value string, allowColon bool) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s is required and must not have surrounding whitespace", ErrOAuthConfig, name)
	}
	if len(value) > maxOAuthCredentialBytes || containsControl(value) {
		return fmt.Errorf("%w: %s has an invalid format", ErrOAuthConfig, name)
	}
	if !allowColon && strings.ContainsRune(value, ':') {
		return fmt.Errorf("%w: %s has an invalid format", ErrOAuthConfig, name)
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateOAuthEndpoint(endpoint string, policy endpointPolicy) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "/api/v1/access_token" {
		return ErrOAuthEndpoint
	}

	switch policy {
	case endpointProduction:
		if parsed.Scheme != "https" || parsed.Host != "www.reddit.com" || endpoint != redditOAuthTokenEndpoint {
			return ErrOAuthEndpoint
		}
	case endpointLoopbackTest:
		ip := net.ParseIP(parsed.Hostname())
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || ip == nil || !ip.IsLoopback() {
			return ErrOAuthEndpoint
		}
	default:
		return ErrOAuthEndpoint
	}
	return nil
}

// Token returns a reusable bearer token or acquires one with the application-only
// client_credentials grant. Waiting on another acquisition remains cancelable even
// though that acquisition is owned by a different caller.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", fmt.Errorf("%w: token source is nil", ErrOAuthConfig)
	}
	if ctx == nil {
		return "", fmt.Errorf("%w: context is required", ErrOAuthConfig)
	}

	for {
		if err := ctx.Err(); err != nil {
			return "", newError(ErrorCanceled, EndpointOAuthToken, "", 0, err)
		}

		now := s.now()
		s.mu.Lock()
		if s.token.value != "" && now.Before(s.token.refreshAt) {
			token := s.token.value
			s.mu.Unlock()
			return token, nil
		}
		if s.token.value != "" {
			s.token = cachedOAuthToken{}
		}
		if s.flight != nil {
			flight := s.flight
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", newError(ErrorCanceled, EndpointOAuthToken, "", 0, ctx.Err())
			case <-flight.done:
				if err := ctx.Err(); err != nil {
					return "", newError(ErrorCanceled, EndpointOAuthToken, "", 0, err)
				}
				if flight.err != nil && !flight.ownerEnded {
					return "", flight.err
				}
				// An acquisition uses its owner's context. If only that owner was
				// canceled, an independent waiter is still entitled to acquire with
				// its own live context instead of inheriting unrelated cancellation.
				// Recheck the cache: an API caller may have invalidated this exact
				// token after the acquisition completed but before this waiter woke.
				continue
			}
		}

		flight := &oauthFlight{done: make(chan struct{})}
		s.flight = flight
		s.mu.Unlock()

		token, refreshAt, err := s.acquire(ctx)

		s.mu.Lock()
		if err == nil {
			s.token = cachedOAuthToken{value: token, refreshAt: refreshAt}
		}
		flight.err = err
		flight.ownerEnded = err != nil && ctx.Err() != nil
		s.flight = nil
		// The acquiring caller creates and exclusively closes done after both
		// the shared result and cache are visible under the mutex.
		close(flight.done)
		s.mu.Unlock()
		return token, err
	}
}

// Invalidate removes the cached token only when token exactly matches it. A delayed
// 401 for an older token therefore cannot evict a newer token acquired concurrently.
func (s *TokenSource) Invalidate(token string) {
	if s == nil || token == "" {
		return
	}
	s.mu.Lock()
	if s.token.value == token {
		s.token = cachedOAuthToken{}
	}
	s.mu.Unlock()
}

func (s *TokenSource) acquire(ctx context.Context) (string, time.Time, error) {
	startedAt := s.now()
	payload, err := s.requestPolicy.do(ctx, EndpointOAuthToken, "", func(attemptCtx context.Context) (policyAttemptResult, error) {
		attemptStartedAt := s.now()
		result, attemptErr := s.acquireAttempt(attemptCtx)
		if attemptErr == nil {
			startedAt = attemptStartedAt
		}
		return result, attemptErr
	})
	if err != nil {
		return "", time.Time{}, err
	}
	accessToken, lifetime, err := decodeOAuthToken(payload)
	if err != nil {
		return "", time.Time{}, newError(ErrorProtocol, EndpointOAuthToken, "", http.StatusOK, err)
	}
	safetyMargin := lifetime / 10
	if safetyMargin > oauthExpirySafetyCap {
		safetyMargin = oauthExpirySafetyCap
	}
	refreshAt := startedAt.Add(lifetime - safetyMargin)
	if !s.now().Before(refreshAt) {
		return "", time.Time{}, newError(ErrorProtocol, EndpointOAuthToken, "", http.StatusOK, ErrOAuthInvalidExpiry)
	}
	return accessToken, refreshAt, nil
}

// acquireAttempt recreates the client_credentials body and request for every HTTP
// attempt. Decoding occurs after policy acceptance, so malformed protocol data is
// never retried as though it were a transient transport failure.
func (s *TokenSource) acquireAttempt(ctx context.Context) (policyAttemptResult, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return policyAttemptResult{}, newError(ErrorInvalidInput, EndpointOAuthToken, "", 0, &oauthRequestError{cause: err})
	}
	request.SetBasicAuth(s.clientID, s.clientSecret)
	request.Header.Set("User-Agent", s.userAgent)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := s.client.Do(request)
	result := policyAttemptResult{attempted: true}
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, newError(ErrorCanceled, EndpointOAuthToken, "", 0, ctxErr)
		}
		if errors.Is(err, ErrOAuthRedirect) {
			return result, newError(ErrorProtocol, EndpointOAuthToken, "", 0, ErrOAuthRedirect)
		}
		return result, newError(ErrorTransport, EndpointOAuthToken, "", 0, &oauthRequestError{cause: err})
	}
	result.headers = policyHeaders(response.Header)
	if response.Body == nil {
		return result, newError(ErrorProtocol, EndpointOAuthToken, "", response.StatusCode, ErrOAuthResponseRead)
	}
	if response.StatusCode != http.StatusOK {
		drainErr := drainRetryableResponseBody(response)
		closeErr := response.Body.Close()
		cause := error(ErrOAuthUnexpectedStatus)
		if drainErr != nil {
			cause = errors.Join(cause, &oauthResponseReadError{cause: drainErr})
		}
		if closeErr != nil {
			cause = errors.Join(cause, &oauthResponseReadError{cause: closeErr})
		}
		return result, newError(oauthStatusClass(response.StatusCode), EndpointOAuthToken, "", response.StatusCode, cause)
	}
	if response.ContentLength > maxOAuthResponseBytes {
		closeErr := response.Body.Close()
		cause := error(ErrOAuthResponseTooLarge)
		if closeErr != nil {
			cause = errors.Join(cause, &oauthResponseReadError{cause: closeErr})
		}
		return result, newError(ErrorResourceLimit, EndpointOAuthToken, "", response.StatusCode, cause)
	}
	if !hasSingleJSONContentType(response.Header) {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return result, newError(ErrorProtocol, EndpointOAuthToken, "", response.StatusCode, errors.Join(ErrOAuthUnexpectedContentType, &oauthResponseReadError{cause: closeErr}))
		}
		return result, newError(ErrorProtocol, EndpointOAuthToken, "", response.StatusCode, ErrOAuthUnexpectedContentType)
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, newError(ErrorCanceled, EndpointOAuthToken, "", response.StatusCode, ctxErr)
		}
		if isTransportReadError(readErr) {
			return result, newError(ErrorTransport, EndpointOAuthToken, "", response.StatusCode, &oauthResponseReadError{cause: readErr})
		}
		return result, newError(ErrorProtocol, EndpointOAuthToken, "", response.StatusCode, &oauthResponseReadError{cause: readErr})
	}
	if closeErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, newError(ErrorCanceled, EndpointOAuthToken, "", response.StatusCode, ctxErr)
		}
		return result, newError(ErrorProtocol, EndpointOAuthToken, "", response.StatusCode, &oauthResponseReadError{cause: closeErr})
	}
	if int64(len(body)) > maxOAuthResponseBytes {
		return result, newError(ErrorResourceLimit, EndpointOAuthToken, "", response.StatusCode, ErrOAuthResponseTooLarge)
	}
	result.payload = body
	return result, nil
}

func oauthStatusClass(statusCode int) ErrorClass {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusBadRequest:
		return ErrorAuthentication
	case statusCode == http.StatusRequestTimeout:
		return ErrorTransport
	case statusCode == http.StatusForbidden:
		return ErrorForbidden
	case statusCode == http.StatusTooManyRequests:
		return ErrorRateLimited
	case statusCode >= 500 && statusCode <= 599:
		return ErrorServer
	default:
		return ErrorProtocol
	}
}

func hasSingleJSONContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

type oauthTokenResponse struct {
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
}

func decodeOAuthToken(body []byte) (string, time.Duration, error) {
	var payload oauthTokenResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return "", 0, ErrOAuthMalformedResponse
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", 0, ErrOAuthMalformedResponse
	}

	if !validBearerToken(payload.AccessToken) ||
		!strings.EqualFold(payload.TokenType, "bearer") {
		return "", 0, ErrOAuthInvalidToken
	}

	var expirySeconds int64
	if len(payload.ExpiresIn) == 0 || json.Unmarshal(payload.ExpiresIn, &expirySeconds) != nil || expirySeconds <= 0 {
		return "", 0, ErrOAuthInvalidExpiry
	}
	if expirySeconds > int64(maxOAuthTokenLifetime/time.Second) {
		return "", 0, ErrOAuthInvalidExpiry
	}
	lifetime := time.Duration(expirySeconds) * time.Second
	return payload.AccessToken, lifetime, nil
}

func validBearerToken(token string) bool {
	if token == "" || len(token) > maxOAuthTokenBytes {
		return false
	}
	padding := false
	for index, char := range []byte(token) {
		if char == '=' {
			// RFC 6750's b64token syntax permits '=' only as trailing padding,
			// never at the beginning or between token characters.
			if index == 0 {
				return false
			}
			padding = true
			continue
		}
		if padding {
			return false
		}
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-._~+/", rune(char)) {
			continue
		}
		return false
	}
	return true
}

// oauthRequestError preserves errors.Is/errors.As for cancellation and future
// transport classification while keeping arbitrary transport error text out of the
// returned Error string, where it could contain request secrets.
type oauthRequestError struct {
	cause error
}

func (e *oauthRequestError) Error() string { return ErrOAuthRequest.Error() }
func (e *oauthRequestError) Unwrap() error { return e.cause }
func (e *oauthRequestError) Is(target error) bool {
	return target == ErrOAuthRequest
}

type oauthResponseReadError struct {
	cause error
}

func (e *oauthResponseReadError) Error() string { return ErrOAuthResponseRead.Error() }
func (e *oauthResponseReadError) Unwrap() error { return e.cause }
func (e *oauthResponseReadError) Is(target error) bool {
	return target == ErrOAuthResponseRead
}
