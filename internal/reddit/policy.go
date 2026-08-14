package reddit

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultRequestsPerSecond = 0.8
	defaultMaxRetries        = 3
	defaultMaxRetryElapsed   = 45 * time.Second
	defaultInitialBackoff    = 500 * time.Millisecond
	defaultMaxBackoff        = 8 * time.Second

	// Keep the constructor-level invariant aligned with the production CLI. This is
	// the last defense against a non-CLI composition accidentally exceeding the
	// deliberately conservative Reddit request ceiling.
	minRequestsPerSecond = 0.1
	maxRequestsPerSecond = 1.5
	maxConfiguredRetries = 5
	maxRetryElapsed      = 5 * time.Minute
	maxBackoff           = time.Minute
	maxRateReset         = time.Hour
	maxPolicyHeaderBytes = 128
)

var (
	// ErrRequestPolicyConfig identifies an invalid or unsafe request-policy value.
	ErrRequestPolicyConfig = errors.New("invalid reddit request policy configuration")
	errRetryBudget         = errors.New("reddit retry budget exhausted")
)

// RequestPolicyConfig defines the process-wide Reddit request ceiling and bounded
// retry behavior. MaxRetryElapsed is the hard total budget measured from the start
// of one logical request session. It covers limiter and endpoint-gate waits, the
// initial HTTP attempt, backoff, and every replay. An internal expiry is distinct
// from caller cancellation. A completely zero config selects
// DefaultRequestPolicyConfig; otherwise all duration and rate fields must be set
// explicitly. MaxRetries may be zero to disable generic retries while retaining
// request pacing.
type RequestPolicyConfig struct {
	RequestsPerSecond float64
	MaxRetries        int
	MaxRetryElapsed   time.Duration
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	Observer          RetryObserver
}

// DefaultRequestPolicyConfig returns conservative, finite production defaults. The
// configured rate is a ceiling: valid runtime headers may reduce it but never raise
// it.
func DefaultRequestPolicyConfig() RequestPolicyConfig {
	return RequestPolicyConfig{
		RequestsPerSecond: defaultRequestsPerSecond,
		MaxRetries:        defaultMaxRetries,
		MaxRetryElapsed:   defaultMaxRetryElapsed,
		InitialBackoff:    defaultInitialBackoff,
		MaxBackoff:        defaultMaxBackoff,
	}
}

// RetryEvent contains only allowlisted, bounded metadata for an HTTP replay. It
// deliberately excludes URLs, headers, bodies, tokens, and underlying error text.
type RetryEvent struct {
	Endpoint   Endpoint
	PostID     string
	Class      ErrorClass
	StatusCode int
	// Attempt is the one-based number of the HTTP attempt that will run next.
	Attempt int
	Delay   time.Duration
}

// RetryObserver receives sanitized retry events synchronously. Calls from
// independent requests may overlap, so implementations must be concurrency-safe,
// return quickly, and not call back into the same RequestPolicy.
type RetryObserver func(RetryEvent)

// RequestPolicySnapshot is a race-safe point-in-time view of process-wide request
// activity. Retry count includes the single controlled 401 replay when it occurs.
type RequestPolicySnapshot struct {
	HTTPAttempts      uint64
	Retries           uint64
	ThrottleWaitCount uint64
	ThrottleWait      time.Duration
}

type policyWait func(context.Context, time.Duration) error
type policyJitter func(time.Duration) time.Duration

// RequestPolicy coordinates every OAuth and bearer-API HTTP attempt in a process.
// It is safe for concurrent use. Each logical session is bounded from its first
// limiter wait through its final HTTP attempt. The size-one permit serializes only
// scheduling; the permit is released before network I/O, so independent requests
// may overlap.
type RequestPolicy struct {
	baseInterval    time.Duration
	maxRetries      int
	maxRetryElapsed time.Duration
	initialBackoff  time.Duration
	maxBackoff      time.Duration
	observer        RetryObserver
	now             func() time.Time
	wait            policyWait
	jitter          policyJitter

	permit chan struct{}

	mu               sync.Mutex
	nextAllowed      time.Time
	adaptiveInterval time.Duration
	adaptiveUntil    time.Time
	blockedUntil     time.Time

	httpAttempts      atomic.Uint64
	retries           atomic.Uint64
	throttleWaitCount atomic.Uint64
	throttleWaitNanos atomic.Int64
}

// NewRequestPolicy constructs a shareable production request policy using the
// system clock, interruptible timers, and bounded randomized jitter.
func NewRequestPolicy(config RequestPolicyConfig) (*RequestPolicy, error) {
	return newRequestPolicy(config, time.Now, waitWithTimer, defaultRetryJitter)
}

// newRequestPolicy injects all timing behavior so unit tests can advance virtual
// time without sleeping. The wait function must advance now by at least the
// requested duration when it reports success.
func newRequestPolicy(
	config RequestPolicyConfig,
	now func() time.Time,
	wait policyWait,
	jitter policyJitter,
) (*RequestPolicy, error) {
	if zeroRequestPolicyConfig(config) {
		config = DefaultRequestPolicyConfig()
	}
	if err := validateRequestPolicyConfig(config); err != nil {
		return nil, err
	}
	if now == nil || wait == nil || jitter == nil {
		return nil, fmt.Errorf("%w: timing dependencies are required", ErrRequestPolicyConfig)
	}
	interval := time.Duration(float64(time.Second) / config.RequestsPerSecond)
	if interval <= 0 {
		return nil, fmt.Errorf("%w: request interval is too small", ErrRequestPolicyConfig)
	}
	policy := &RequestPolicy{
		baseInterval:    interval,
		maxRetries:      config.MaxRetries,
		maxRetryElapsed: config.MaxRetryElapsed,
		initialBackoff:  config.InitialBackoff,
		maxBackoff:      config.MaxBackoff,
		observer:        config.Observer,
		now:             now,
		wait:            wait,
		jitter:          jitter,
		permit:          make(chan struct{}, 1),
	}
	policy.permit <- struct{}{}
	return policy, nil
}

func zeroRequestPolicyConfig(config RequestPolicyConfig) bool {
	return config.RequestsPerSecond == 0 && config.MaxRetries == 0 &&
		config.MaxRetryElapsed == 0 && config.InitialBackoff == 0 &&
		config.MaxBackoff == 0 && config.Observer == nil
}

func validateRequestPolicyConfig(config RequestPolicyConfig) error {
	if math.IsNaN(config.RequestsPerSecond) || math.IsInf(config.RequestsPerSecond, 0) ||
		config.RequestsPerSecond < minRequestsPerSecond || config.RequestsPerSecond > maxRequestsPerSecond {
		return fmt.Errorf("%w: requests per second must be between %.1f and %.1f", ErrRequestPolicyConfig, minRequestsPerSecond, maxRequestsPerSecond)
	}
	if config.MaxRetries < 0 || config.MaxRetries > maxConfiguredRetries {
		return fmt.Errorf("%w: max retries must be between 0 and %d", ErrRequestPolicyConfig, maxConfiguredRetries)
	}
	if config.MaxRetryElapsed <= 0 || config.MaxRetryElapsed > maxRetryElapsed {
		return fmt.Errorf("%w: retry elapsed limit must be positive and at most %s", ErrRequestPolicyConfig, maxRetryElapsed)
	}
	if config.InitialBackoff <= 0 || config.InitialBackoff > maxBackoff {
		return fmt.Errorf("%w: initial backoff must be positive and at most %s", ErrRequestPolicyConfig, maxBackoff)
	}
	if config.MaxBackoff < config.InitialBackoff || config.MaxBackoff > maxBackoff {
		return fmt.Errorf("%w: max backoff must be between initial backoff and %s", ErrRequestPolicyConfig, maxBackoff)
	}
	return nil
}

// Snapshot returns counters suitable for the sanitized final run summary.
func (p *RequestPolicy) Snapshot() RequestPolicySnapshot {
	if p == nil {
		return RequestPolicySnapshot{}
	}
	return RequestPolicySnapshot{
		HTTPAttempts:      p.httpAttempts.Load(),
		Retries:           p.retries.Load(),
		ThrottleWaitCount: p.throttleWaitCount.Load(),
		ThrottleWait:      time.Duration(p.throttleWaitNanos.Load()),
	}
}

type responsePolicyHeaders struct {
	rateUsed      string
	rateRemaining string
	rateReset     string
	retryAfter    string
}

type policyAttemptResult struct {
	payload        []byte
	releasePayload func()
	headers        responsePolicyHeaders
	attempted      bool
}

// responsePayload transfers ownership of an accepted response and its admission
// lease from the retry layer to the decoder. releaseNow is deliberately idempotent
// when the underlying TraversalBudget lease is idempotent, but callers still clear
// local ownership so large byte slices do not remain strongly referenced.
type responsePayload struct {
	data    []byte
	release func()
}

func (payload *responsePayload) releaseNow() {
	if payload == nil {
		return
	}
	payload.data = nil
	if payload.release != nil {
		payload.release()
		payload.release = nil
	}
}

func (payload *responsePayload) take() responsePayload {
	if payload == nil {
		return responsePayload{}
	}
	owned := *payload
	*payload = responsePayload{}
	return owned
}

func (result *policyAttemptResult) releaseResponse() {
	if result == nil {
		return
	}
	result.payload = nil
	if result.releasePayload != nil {
		result.releasePayload()
	}
	result.releasePayload = nil
}

func (result *policyAttemptResult) takeResponse() responsePayload {
	if result == nil {
		return responsePayload{}
	}
	payload := responsePayload{data: result.payload, release: result.releasePayload}
	result.payload = nil
	result.releasePayload = nil
	return payload
}

type policyAttempt func(context.Context) (policyAttemptResult, error)
type policyAttemptGate func(context.Context) (func(), error)

type retrySession struct {
	policy         *RequestPolicy
	startedAt      time.Time
	httpAttempts   int
	genericRetries int
}

func (p *RequestPolicy) newRetrySession() *retrySession {
	return &retrySession{policy: p, startedAt: p.now()}
}

func (p *RequestPolicy) do(
	ctx context.Context,
	endpoint Endpoint,
	postID string,
	attempt policyAttempt,
) ([]byte, error) {
	return p.newRetrySession().do(ctx, endpoint, postID, attempt)
}

// do retries only the supplied HTTP attempt. Callers decode or traverse after this
// function succeeds, so neither visitor side effects nor accepted payloads can be
// replayed.
func (session *retrySession) do(
	ctx context.Context,
	endpoint Endpoint,
	postID string,
	attempt policyAttempt,
) ([]byte, error) {
	return session.doAfter(ctx, endpoint, postID, nil, nil, attempt)
}

func (session *retrySession) doAfter(
	ctx context.Context,
	endpoint Endpoint,
	postID string,
	previousErr error,
	gate policyAttemptGate,
	attempt policyAttempt,
) ([]byte, error) {
	payload, err := session.doAfterPayload(ctx, endpoint, postID, previousErr, gate, attempt)
	if err != nil {
		return nil, err
	}
	// The byte-only facade is used by OAuth and policy tests, whose attempts never
	// reserve traversal capacity. Fail closed if a future caller tries to discard an
	// owned lease through this compatibility path.
	if payload.release != nil {
		payload.releaseNow()
		return nil, newError(ErrorInvalidInput, endpoint, validDiagnosticPostID(postID), 0, ErrTraversalBudgetConfig)
	}
	return payload.data, nil
}

func (session *retrySession) doAfterPayload(
	ctx context.Context,
	endpoint Endpoint,
	postID string,
	previousErr error,
	gate policyAttemptGate,
	attempt policyAttempt,
) (responsePayload, error) {
	if session == nil || session.policy == nil || ctx == nil || attempt == nil {
		return responsePayload{}, newError(ErrorInvalidInput, endpoint, validDiagnosticPostID(postID), 0, ErrRequestPolicyConfig)
	}
	p := session.policy
	lastErr := previousErr
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, ctxErr)
		}
		remaining := session.remainingBudget()
		if remaining <= 0 {
			return responsePayload{}, retryBudgetFailure(endpoint, postID, lastErr)
		}
		// One child context covers both scheduling and network I/O. Creating it for
		// the initial attempt is essential: a saturated limiter or slow first HTTP
		// exchange must not outlive the logical-session budget merely because no
		// replay has occurred yet.
		attemptCtx, cancelAttempt := context.WithTimeoutCause(ctx, remaining, errRetryBudget)
		release, slotErr := session.acquireAttemptSlot(attemptCtx, gate)
		if slotErr != nil {
			budgetEnded := session.budgetEnded(attemptCtx) || errors.Is(slotErr, errRetryBudget)
			cancelAttempt()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, ctxErr)
			}
			if budgetEnded {
				return responsePayload{}, retryBudgetFailure(endpoint, postID, lastErr)
			}
			return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, slotErr)
		}
		// Recheck the injected monotonic clock after scheduling. A deterministic
		// test wait can advance it without firing the wall-clock context timer; in
		// production this also closes the narrow boundary race before Client.Do.
		if ctxErr := ctx.Err(); ctxErr != nil {
			release()
			cancelAttempt()
			return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, ctxErr)
		}
		if session.budgetEnded(attemptCtx) {
			release()
			cancelAttempt()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, ctxErr)
			}
			return responsePayload{}, retryBudgetFailure(endpoint, postID, lastErr)
		}

		result, err := attempt(attemptCtx)
		release()
		budgetEnded := session.budgetEnded(attemptCtx)
		cancelAttempt()
		if result.attempted {
			session.httpAttempts++
			p.httpAttempts.Add(1)
			p.observeRateHeaders(result.headers)
			p.observeSharedRetryAfter(err, result.headers.retryAfter)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.releaseResponse()
			var adapterErr *Error
			if errors.As(err, &adapterErr) && adapterErr.Class == ErrorCanceled && errors.Is(err, ctxErr) {
				return responsePayload{}, err
			}
			return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, ctxErr)
		}
		if budgetEnded {
			result.releaseResponse()
			return responsePayload{}, retryBudgetFailure(endpoint, postID, lastErr)
		}
		if err == nil {
			return result.takeResponse(), nil
		}
		result.releaseResponse()
		lastErr = err
		if !result.attempted || !retryableHTTPError(err) || session.genericRetries >= p.maxRetries {
			return responsePayload{}, err
		}

		delay := p.retryDelay(result.headers.retryAfter, session.genericRetries)
		remaining = session.remainingBudget()
		if remaining <= 0 || delay >= remaining {
			return responsePayload{}, err
		}
		session.genericRetries++
		p.recordRetry(endpoint, postID, err, session.httpAttempts+1, delay)
		budgetEnded, waitErr := session.waitForRetry(ctx, delay)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, ctxErr)
		}
		if budgetEnded {
			return responsePayload{}, err
		}
		if waitErr != nil {
			return responsePayload{}, newError(ErrorCanceled, endpoint, validDiagnosticPostID(postID), 0, waitErr)
		}
	}
}

// waitForRetry rechecks elapsed time after the synchronous observer and gives the
// backoff wait a child context bounded by the remaining retry-session budget. An
// internal budget deadline is policy exhaustion, not caller cancellation, so the
// caller can return the original transient error that prompted the replay.
func (session *retrySession) waitForRetry(ctx context.Context, delay time.Duration) (bool, error) {
	remaining := session.remainingBudget()
	if remaining <= 0 || delay >= remaining {
		return true, nil
	}
	waitCtx, cancel := context.WithTimeoutCause(ctx, remaining, errRetryBudget)
	waitErr := session.policy.waitForRetry(waitCtx, delay)
	waitCause := context.Cause(waitCtx)
	cancel()

	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(waitCause, errRetryBudget) || session.remainingBudget() <= 0 {
		return true, nil
	}
	return false, waitErr
}

func (session *retrySession) acquireAttemptSlot(
	ctx context.Context,
	gate policyAttemptGate,
) (func(), error) {
	if gate == nil {
		budget := session.remainingBudget()
		if budget <= 0 {
			return nil, errRetryBudget
		}
		if err := session.policy.waitForPermit(ctx, budget); err != nil {
			return nil, err
		}
		return func() {}, nil
	}

	// A morechildren attempt may have to wait for both the endpoint gate and the
	// process-wide rate slot. If the rate slot is not ready after acquiring the
	// gate, release the gate before sleeping and try both again. This prevents a
	// retry backoff or limiter timer from blocking unrelated morechildren cleanup.
	for {
		release, err := gate(ctx)
		if err != nil {
			return nil, err
		}
		if session.remainingBudget() <= 0 {
			release()
			return nil, errRetryBudget
		}
		delay, granted, err := session.policy.tryPermit(ctx)
		if err != nil {
			release()
			return nil, err
		}
		if granted {
			return release, nil
		}
		release()
		remaining := session.remainingBudget()
		if remaining <= 0 || delay >= remaining {
			return nil, errRetryBudget
		}
		if err := session.policy.waitForThrottle(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (session *retrySession) remainingBudget() time.Duration {
	elapsed := session.policy.now().Sub(session.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return session.policy.maxRetryElapsed - elapsed
}

func (session *retrySession) budgetEnded(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errRetryBudget) || session.remainingBudget() <= 0
}

// retryBudgetFailure preserves the material error that justified an authentication
// or generic replay. When the initial scheduling/HTTP attempt alone exhausts the
// session, no prior server result exists, so expose one sanitized non-cancellation
// transport failure while retaining errRetryBudget for errors.Is classification.
func retryBudgetFailure(endpoint Endpoint, postID string, previous error) error {
	if previous != nil {
		return previous
	}
	return newError(ErrorTransport, endpoint, validDiagnosticPostID(postID), 0, errRetryBudget)
}

// recordAuthenticationReplay accounts for the one token-lifecycle replay without
// consuming the generic retry allowance shared by both bearer-token attempts.
func (session *retrySession) recordAuthenticationReplay(endpoint Endpoint, postID string, err error) {
	session.policy.recordRetry(endpoint, postID, err, session.httpAttempts+1, 0)
}

func retryableHTTPError(err error) bool {
	var adapterError *Error
	if !errors.As(err, &adapterError) {
		return false
	}
	switch adapterError.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return adapterError.Class == ErrorTransport && transientNetworkError(err)
}

// transientNetworkError is deliberately a whitelist. Certificate validation,
// DNS-not-found, proxy setup, malformed URL, and other opaque configuration errors
// cannot improve on replay and therefore fail immediately. Timeouts, truncated
// streams, closed connections, and selected socket failures may succeed on a fresh
// idempotent request.
func transientNetworkError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if permanentTLSError(err) {
		return false
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return !dnsError.IsNotFound && (dnsError.IsTimeout || dnsError.IsTemporary)
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) && strings.EqualFold(operationError.Op, "proxyconnect") {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETDOWN) || errors.Is(err, syscall.ENETRESET) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func permanentTLSError(err error) bool {
	var verificationError *tls.CertificateVerificationError
	var recordHeaderError tls.RecordHeaderError
	var unknownAuthorityError x509.UnknownAuthorityError
	var hostnameError x509.HostnameError
	var certificateInvalidError x509.CertificateInvalidError
	var systemRootsError x509.SystemRootsError
	return errors.As(err, &verificationError) || errors.As(err, &recordHeaderError) ||
		errors.As(err, &unknownAuthorityError) || errors.As(err, &hostnameError) ||
		errors.As(err, &certificateInvalidError) || errors.As(err, &systemRootsError)
}

func (p *RequestPolicy) retryDelay(retryAfter string, retryNumber int) time.Duration {
	// A valid server-directed delay is not an exponential-backoff value and must
	// never be shortened to MaxBackoff. Cap only at the whole retry-session budget;
	// the caller refuses the replay when that delay no longer fits.
	if delay, ok := parseRetryAfter(retryAfter, p.now(), p.maxRetryElapsed); ok {
		return delay
	}
	delay := p.initialBackoff
	for index := 0; index < retryNumber && delay < p.maxBackoff; index++ {
		if delay > p.maxBackoff/2 {
			delay = p.maxBackoff
			break
		}
		delay *= 2
	}
	if delay > p.maxBackoff {
		delay = p.maxBackoff
	}
	jittered := p.jitter(delay)
	minimum := delay / 2
	if minimum <= 0 {
		minimum = time.Nanosecond
	}
	maximum := delay + delay/2
	if maximum > p.maxBackoff {
		maximum = p.maxBackoff
	}
	if jittered < minimum {
		return minimum
	}
	if jittered > maximum {
		return maximum
	}
	return jittered
}

func parseRetryAfter(value string, now time.Time, capDelay time.Duration) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxPolicyHeaderBytes {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds == 0 {
			return 0, false
		}
		if seconds > uint64(capDelay/time.Second) {
			return capDelay, true
		}
		delay := time.Duration(seconds) * time.Second
		if delay > capDelay {
			delay = capDelay
		}
		return delay, true
	} else if decimalDigits(value) {
		// A syntactically decimal value that overflows uint64 still communicates a
		// long wait. Cap it instead of falling back to a shorter exponential delay.
		return capDelay, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if delay <= 0 {
		return 0, false
	}
	if delay > capDelay {
		delay = capDelay
	}
	return delay, true
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (p *RequestPolicy) recordRetry(
	endpoint Endpoint,
	postID string,
	err error,
	attempt int,
	delay time.Duration,
) {
	p.retries.Add(1)
	if p.observer == nil {
		return
	}
	class := ErrorProtocol
	status := 0
	var adapterError *Error
	if errors.As(err, &adapterError) {
		class = adapterError.Class
		status = adapterError.StatusCode
	}
	p.observer(RetryEvent{
		Endpoint:   endpoint,
		PostID:     validDiagnosticPostID(postID),
		Class:      class,
		StatusCode: status,
		Attempt:    attempt,
		Delay:      delay,
	})
}

func (p *RequestPolicy) waitForRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.wait(ctx, delay)
}

// waitForPermit holds the size-one scheduling permit while waiting, then releases
// it before network I/O. Rechecking after each wait lets a concurrent response
// tighten runtime rate limits without leaving a queue of stale reservations.
func (p *RequestPolicy) waitForPermit(ctx context.Context, sessionBudget time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.permit:
	}
	defer func() { p.permit <- struct{}{} }()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := p.now()
		p.mu.Lock()
		p.expireAdaptationLocked(now)
		target := p.nextAllowed
		if p.blockedUntil.After(target) {
			target = p.blockedUntil
		}
		if !target.After(now) {
			interval := p.baseInterval
			if p.adaptiveInterval > interval && now.Before(p.adaptiveUntil) {
				interval = p.adaptiveInterval
			}
			p.nextAllowed = now.Add(interval)
			p.mu.Unlock()
			return nil
		}
		delay := target.Sub(now)
		p.mu.Unlock()

		if sessionBudget > 0 && delay >= sessionBudget {
			return errRetryBudget
		}
		if err := p.waitForThrottle(ctx, delay); err != nil {
			return err
		}
		if sessionBudget > 0 {
			sessionBudget -= delay
			if sessionBudget <= 0 {
				return errRetryBudget
			}
		}
	}
}

func (p *RequestPolicy) waitForThrottle(ctx context.Context, delay time.Duration) error {
	p.throttleWaitCount.Add(1)
	startedAt := p.now()
	err := p.wait(ctx, delay)
	elapsed := p.now().Sub(startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	// A clock jump must not inflate operational metrics beyond the bounded wait
	// requested by policy. Early cancellation records only time actually elapsed.
	if elapsed > delay {
		elapsed = delay
	}
	p.throttleWaitNanos.Add(int64(elapsed))
	return err
}

// tryPermit reserves a request slot only when it is ready at the current instant.
// It never sleeps, which lets a caller release an endpoint gate before waiting.
func (p *RequestPolicy) tryPermit(ctx context.Context) (time.Duration, bool, error) {
	select {
	case <-ctx.Done():
		return 0, false, ctx.Err()
	case <-p.permit:
	}
	defer func() { p.permit <- struct{}{} }()

	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireAdaptationLocked(now)
	target := p.nextAllowed
	if p.blockedUntil.After(target) {
		target = p.blockedUntil
	}
	if target.After(now) {
		return target.Sub(now), false, nil
	}
	interval := p.baseInterval
	if p.adaptiveInterval > interval && now.Before(p.adaptiveUntil) {
		interval = p.adaptiveInterval
	}
	p.nextAllowed = now.Add(interval)
	return 0, true, nil
}

func (p *RequestPolicy) expireAdaptationLocked(now time.Time) {
	if !p.adaptiveUntil.IsZero() && !now.Before(p.adaptiveUntil) {
		p.adaptiveInterval = 0
		p.adaptiveUntil = time.Time{}
	}
	if !p.blockedUntil.IsZero() && !now.Before(p.blockedUntil) {
		p.blockedUntil = time.Time{}
	}
}

// observeRateHeaders applies a complete, finite header triplet atomically. Partial,
// malformed, or implausibly large values leave the conservative configured ceiling
// unchanged. Used is validated even though Remaining/Reset determine scheduling.
func (p *RequestPolicy) observeRateHeaders(headers responsePolicyHeaders) {
	used, usedOK := parseNonNegativeFloat(headers.rateUsed)
	remaining, remainingOK := parseNonNegativeFloat(headers.rateRemaining)
	resetSeconds, resetOK := parseNonNegativeFloat(headers.rateReset)
	if !usedOK || !remainingOK || !resetOK || used < 0 || resetSeconds <= 0 ||
		resetSeconds > maxRateReset.Seconds() {
		return
	}
	now := p.now()
	reset := time.Duration(resetSeconds * float64(time.Second))
	if reset <= 0 || reset > maxRateReset {
		return
	}
	until := now.Add(reset)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireAdaptationLocked(now)
	if remaining < 1 {
		if until.After(p.blockedUntil) {
			p.blockedUntil = until
		}
		if until.After(p.nextAllowed) {
			p.nextAllowed = until
		}
		return
	}
	interval := time.Duration(float64(reset) / remaining)
	if interval > reset {
		interval = reset
	}
	if interval <= p.baseInterval {
		return
	}
	// Overlapping responses may finish body reads out of request order. Active
	// constraints therefore evolve monotonically: an older, looser response cannot
	// clear or shorten a stricter window learned from a newer response. Keeping the
	// stricter interval until every observed restrictive window expires is safely
	// conservative and avoids needing response sequence metadata.
	if interval > p.adaptiveInterval {
		p.adaptiveInterval = interval
	}
	if until.After(p.adaptiveUntil) {
		p.adaptiveUntil = until
	}
	if target := now.Add(p.adaptiveInterval); target.After(p.nextAllowed) {
		p.nextAllowed = target
	}
}

func (p *RequestPolicy) observeSharedRetryAfter(err error, value string) {
	var adapterError *Error
	if !errors.As(err, &adapterError) || adapterError.StatusCode != http.StatusTooManyRequests {
		return
	}
	now := p.now()
	delay, ok := parseRetryAfter(value, now, maxRateReset)
	if !ok {
		return
	}
	until := now.Add(delay)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireAdaptationLocked(now)
	if until.After(p.blockedUntil) {
		p.blockedUntil = until
	}
	if until.After(p.nextAllowed) {
		p.nextAllowed = until
	}
}

func parseNonNegativeFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxPolicyHeaderBytes {
		return 0, false
	}
	number, err := strconv.ParseFloat(value, 64)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0
}

func policyHeaders(header http.Header) responsePolicyHeaders {
	return responsePolicyHeaders{
		rateUsed:      singlePolicyHeader(header, "X-Ratelimit-Used"),
		rateRemaining: singlePolicyHeader(header, "X-Ratelimit-Remaining"),
		rateReset:     singlePolicyHeader(header, "X-Ratelimit-Reset"),
		retryAfter:    singlePolicyHeader(header, "Retry-After"),
	}
}

func singlePolicyHeader(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) != 1 {
		return ""
	}
	value := strings.TrimSpace(values[0])
	if value == "" || len(value) > maxPolicyHeaderBytes || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func waitWithTimer(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// defaultRetryJitter uses equal jitter in [delay/2, delay]. A cryptographic source
// avoids package-level pseudo-random state; failure safely falls back to delay.
func defaultRetryJitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	minimum := delay / 2
	span := uint64(delay-minimum) + 1
	var random [8]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return delay
	}
	return minimum + time.Duration(binary.LittleEndian.Uint64(random[:])%span)
}
