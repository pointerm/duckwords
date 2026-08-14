package acquire

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// Source downloads are idempotent and happen only twice per run. Two replays
	// absorb short GitHub/CDN failures without turning startup into an open-ended
	// retry loop; the elapsed budget includes both backoff and replay attempts.
	maxSourceRetries                 = 2
	sourceRetryElapsedLimit          = 15 * time.Second
	sourceInitialBackoff             = 250 * time.Millisecond
	sourceMaximumBackoff             = time.Second
	maxSourcePolicyHeaderBytes       = 128
	maxSourceErrorResponseDrainBytes = 32 << 10
)

var errSourceRetryBudget = errors.New("source retry budget exhausted")

type sourceRetryWait func(context.Context, time.Duration) error

type sourceRetryPolicy struct {
	maxRetries     int
	maxElapsed     time.Duration
	initialBackoff time.Duration
	maxBackoff     time.Duration
	now            func() time.Time
	wait           sourceRetryWait
	observer       RetryObserver
}

// DefaultRetryConfig returns the bounded HTTPS replay policy used when Config.Retry
// is nil. Callers that need to disable retries must pass an explicit config with
// MaxRetries set to zero and a positive MaxElapsed.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: maxSourceRetries,
		MaxElapsed: sourceRetryElapsedLimit,
	}
}

func defaultSourceRetryPolicy() sourceRetryPolicy {
	return sourceRetryPolicy{
		maxRetries:     maxSourceRetries,
		maxElapsed:     sourceRetryElapsedLimit,
		initialBackoff: sourceInitialBackoff,
		maxBackoff:     sourceMaximumBackoff,
		now:            time.Now,
		wait:           waitForSourceRetry,
	}
}

func sourceRetryPolicyFor(config *RetryConfig) (sourceRetryPolicy, error) {
	resolved := DefaultRetryConfig()
	if config != nil {
		resolved = *config
	}
	if resolved.MaxRetries < 0 || resolved.MaxRetries > maxSourceRetries ||
		resolved.MaxElapsed <= 0 || resolved.MaxElapsed > sourceRetryElapsedLimit {
		return sourceRetryPolicy{}, ErrInvalidConfig
	}

	policy := defaultSourceRetryPolicy()
	policy.maxRetries = resolved.MaxRetries
	policy.maxElapsed = resolved.MaxElapsed
	policy.observer = resolved.Observer
	return policy, nil
}

func (policy sourceRetryPolicy) observeRetry(spec Spec, err error, attempt int, delay time.Duration) {
	if policy.observer == nil {
		return
	}
	event := RetryEvent{
		Kind:    spec.Kind,
		Reason:  sourceRetryReason(err),
		Attempt: attempt,
		Delay:   delay,
	}
	var loadErr *LoadError
	if errors.As(err, &loadErr) && loadErr.StatusCode >= 100 && loadErr.StatusCode <= 599 {
		event.StatusCode = loadErr.StatusCode
	}
	policy.observer(event)
}

func sourceRetryReason(err error) RetryReason {
	switch {
	case errors.Is(err, ErrHTTPStatus):
		return RetryReasonHTTPStatus
	case errors.Is(err, ErrRead):
		return RetryReasonRead
	case errors.Is(err, ErrClose):
		return RetryReasonClose
	default:
		return RetryReasonTransport
	}
}

func (policy sourceRetryPolicy) remaining(startedAt time.Time) time.Duration {
	elapsed := policy.now().Sub(startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return policy.maxElapsed - elapsed
}

func (policy sourceRetryPolicy) delay(headerValue string, retryNumber int) time.Duration {
	if delay, ok := parseSourceRetryAfter(headerValue, policy.now(), policy.maxElapsed); ok {
		return delay
	}
	delay := policy.initialBackoff
	for index := 0; index < retryNumber && delay < policy.maxBackoff; index++ {
		if delay > policy.maxBackoff/2 {
			return policy.maxBackoff
		}
		delay *= 2
	}
	if delay > policy.maxBackoff {
		return policy.maxBackoff
	}
	return delay
}

func waitForSourceRetry(ctx context.Context, delay time.Duration) error {
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

func retryableSourceStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// drainSourceRetryableBody reads only enough to preserve reuse for a small error
// response. Unknown or dishonest lengths remain bounded by the explicit sentinel;
// known large bodies are closed without reading.
func drainSourceRetryableBody(response *http.Response) error {
	if response == nil || response.Body == nil || !retryableSourceStatus(response.StatusCode) {
		return nil
	}
	if response.ContentLength > maxSourceErrorResponseDrainBytes {
		return nil
	}
	_, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxSourceErrorResponseDrainBytes+1))
	return err
}

func sourceRetryAfter(header http.Header) string {
	values := header.Values("Retry-After")
	if len(values) != 1 {
		return ""
	}
	value := strings.TrimSpace(values[0])
	if value == "" || len(value) > maxSourcePolicyHeaderBytes || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func parseSourceRetryAfter(value string, now time.Time, capDelay time.Duration) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxSourcePolicyHeaderBytes || capDelay <= 0 {
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
	} else if sourceDecimalDigits(value) {
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

func sourceDecimalDigits(value string) bool {
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

// transientSourceTransportError mirrors the source policy's intentionally narrow
// replay contract. Permanent certificate, DNS-not-found, proxy, URL, and opaque
// configuration failures cannot improve when repeated.
func transientSourceTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if permanentSourceTLSError(err) {
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

func permanentSourceTLSError(err error) bool {
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
