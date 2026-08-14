package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

const maximumHTTPTimeout = 10 * time.Minute

var errRedirectPolicy = errors.New("redirect policy")

// Load validates the complete source policy before I/O, reads at most MaxBytes+1,
// and returns either one immutable document or a zero document on every failure.
func Load(ctx context.Context, spec Spec, config Config) (Document, error) {
	if ctx == nil {
		return Document{}, newLoadError(spec.Kind, ModeUnknown, ErrInvalidConfig, 0, nil)
	}
	if err := Validate(spec, config); err != nil {
		return Document{}, err
	}
	if err := ctx.Err(); err != nil {
		return Document{}, canceledError(spec.Kind, selectedMode(spec), err)
	}
	if spec.URL != "" {
		return loadHTTPS(ctx, spec, config)
	}
	return loadFile(ctx, spec, config.MaxBytes)
}

// Validate checks the complete acquisition policy without filesystem or network I/O.
// Composition code can validate every configured source before starting either load.
func Validate(spec Spec, config Config) error {
	if config.MaxBytes <= 0 || config.MaxBytes > MaximumDocumentBytes {
		return newLoadError(spec.Kind, selectedMode(spec), ErrInvalidConfig, 0, nil)
	}
	if err := ValidateSpec(spec); err != nil {
		return err
	}
	if spec.File != "" {
		return nil
	}
	if err := validateHTTPConfig(config); err != nil {
		return newLoadError(spec.Kind, ModeHTTPS, ErrInvalidConfig, 0, nil)
	}
	return nil
}

// ValidateSpec checks only the source locator: the kind, the URL/file exclusivity,
// and the fixed HTTPS host and path policy. It needs no HTTP client, so command-line
// parsing can reject an unusable source before any dependency is constructed and
// report the precise reason rather than a generic setup failure.
func ValidateSpec(spec Spec) error {
	if _, ok := AllowedHost(spec.Kind); !ok {
		return newLoadError(spec.Kind, selectedMode(spec), ErrInvalidSpec, 0, nil)
	}

	hasURL := spec.URL != ""
	hasFile := spec.File != ""
	if hasURL == hasFile {
		return newLoadError(spec.Kind, ModeUnknown, ErrInvalidSpec, 0, nil)
	}
	if hasFile {
		if len(spec.File) > maxSourceLocatorBytes || strings.IndexByte(spec.File, 0) >= 0 {
			return newLoadError(spec.Kind, ModeFile, ErrInvalidSpec, 0, nil)
		}
		return nil
	}
	_, err := validateRemoteURL(spec.Kind, spec.URL)
	return err
}

func loadFile(ctx context.Context, spec Spec, maxBytes int64) (document Document, resultErr error) {
	if err := ctx.Err(); err != nil {
		return Document{}, canceledError(spec.Kind, ModeFile, err)
	}

	before, err := os.Lstat(spec.File)
	if err != nil {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrOpen, 0, nil)
	}
	// Reject symlinks and special files before open. Apart from keeping provenance
	// stable, this avoids reading devices and the ordinary blocking behavior of FIFOs.
	if !before.Mode().IsRegular() {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrNotRegular, 0, nil)
	}
	if before.Size() > maxBytes {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrTooLarge, 0, nil)
	}

	file, err := os.Open(spec.File)
	if err != nil {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrOpen, 0, nil)
	}
	// A close failure invalidates otherwise successful bytes. When another failure
	// already owns the result, preserving that primary class is more actionable and
	// avoids retaining or logging a secondary raw filesystem error.
	defer func() {
		if err := file.Close(); err != nil && resultErr == nil {
			document = Document{}
			if ctxErr := ctx.Err(); ctxErr != nil {
				resultErr = canceledError(spec.Kind, ModeFile, ctxErr)
			} else {
				resultErr = newLoadError(spec.Kind, ModeFile, ErrClose, 0, nil)
			}
		}
	}()

	after, err := file.Stat()
	if err != nil {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrOpen, 0, nil)
	}
	if !after.Mode().IsRegular() {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrNotRegular, 0, nil)
	}
	if !os.SameFile(before, after) {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrSourceChanged, 0, nil)
	}
	if after.Size() > maxBytes {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrTooLarge, 0, nil)
	}

	data, err := readBounded(ctx, file, maxBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Document{}, canceledError(spec.Kind, ModeFile, ctxErr)
		}
		if errors.Is(err, ErrTooLarge) {
			return Document{}, newLoadError(spec.Kind, ModeFile, ErrTooLarge, 0, nil)
		}
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrRead, 0, nil)
	}
	final, err := file.Stat()
	if err != nil {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrSourceChanged, 0, nil)
	}
	// A local source is one stable snapshot. Detect common replace, truncate, append,
	// and rewrite races instead of attaching a reproducibility hash to hybrid bytes.
	if !final.Mode().IsRegular() || !os.SameFile(after, final) || final.Size() != after.Size() || final.Size() != int64(len(data)) || !final.ModTime().Equal(after.ModTime()) {
		return Document{}, newLoadError(spec.Kind, ModeFile, ErrSourceChanged, 0, nil)
	}
	return makeDocument(spec.Kind, ModeFile, "local-file", data), nil
}

func loadHTTPS(ctx context.Context, spec Spec, config Config) (Document, error) {
	parsed, err := validateRemoteURL(spec.Kind, spec.URL)
	if err != nil {
		return Document{}, err
	}
	policy, err := sourceRetryPolicyFor(config.Retry)
	if err != nil {
		return Document{}, newLoadError(spec.Kind, ModeHTTPS, ErrInvalidConfig, 0, nil)
	}
	return loadHTTPSWithRetryPolicy(ctx, spec, config, parsed.String(), policy)
}

func loadHTTPSWithRetryPolicy(
	ctx context.Context,
	spec Spec,
	config Config,
	requestURL string,
	policy sourceRetryPolicy,
) (Document, error) {
	startedAt := policy.now()
	client := restrictedClient(config.HTTPClient, spec.Kind, config.UserAgent)
	var lastErr error
	for attempt := 0; ; attempt++ {
		remaining := policy.remaining(startedAt)
		if remaining <= 0 {
			return Document{}, sourceRetryBudgetFailure(spec, lastErr)
		}
		// The source retry budget is total elapsed time, not merely replay time. Bound
		// the initial HTTP exchange as well as every replay so a long first attempt
		// cannot consume the HTTP-client timeout and then start a fresh retry session.
		attemptCtx, cancelAttempt := context.WithTimeoutCause(ctx, remaining, errSourceRetryBudget)

		document, retryable, retryAfter, attemptErr := loadHTTPSAttempt(
			attemptCtx,
			spec,
			config,
			client,
			requestURL,
		)
		budgetEnded := ctx.Err() == nil &&
			(errors.Is(context.Cause(attemptCtx), errSourceRetryBudget) || policy.remaining(startedAt) <= 0)
		cancelAttempt()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Document{}, canceledError(spec.Kind, ModeHTTPS, ctxErr)
		}
		if budgetEnded {
			return Document{}, sourceRetryBudgetFailure(spec, lastErr)
		}
		if attemptErr == nil {
			return document, nil
		}
		lastErr = attemptErr
		if !retryable || attempt >= policy.maxRetries {
			return Document{}, attemptErr
		}

		delay := policy.delay(retryAfter, attempt)
		remaining = policy.remaining(startedAt)
		// A server delay that does not fit is honored by declining the replay rather
		// than issuing it earlier than requested.
		if remaining <= 0 || delay >= remaining {
			return Document{}, attemptErr
		}
		policy.observeRetry(spec, attemptErr, attempt+2, delay)
		// Observers are synchronous by contract. Recompute after the callback so its
		// runtime cannot silently extend the total retry budget.
		remaining = policy.remaining(startedAt)
		if remaining <= 0 || delay >= remaining {
			return Document{}, attemptErr
		}
		waitCtx, cancelWait := context.WithTimeoutCause(ctx, remaining, errSourceRetryBudget)
		waitErr := policy.wait(waitCtx, delay)
		budgetEnded = ctx.Err() == nil && errors.Is(context.Cause(waitCtx), errSourceRetryBudget)
		cancelWait()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Document{}, canceledError(spec.Kind, ModeHTTPS, ctxErr)
		}
		if budgetEnded {
			return Document{}, attemptErr
		}
		if waitErr != nil {
			return Document{}, canceledError(spec.Kind, ModeHTTPS, waitErr)
		}
	}
}

// sourceRetryBudgetFailure keeps an internal policy deadline distinct from caller
// cancellation. Once a replay has started, the transient failure that justified it is
// the stable material result; an initial-attempt expiry has no earlier cause and is
// reported as a sanitized transport failure.
func sourceRetryBudgetFailure(spec Spec, previous error) error {
	if previous != nil {
		return previous
	}
	return newLoadError(spec.Kind, ModeHTTPS, ErrTransport, 0, nil)
}

func loadHTTPSAttempt(
	ctx context.Context,
	spec Spec,
	config Config,
	client *http.Client,
	requestURL string,
) (document Document, retryable bool, retryAfter string, resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrInvalidSpec, 0, nil)
	}
	request.Header.Set("Accept", "text/plain, application/octet-stream;q=0.9")
	// Identity keeps the SHA-256 tied to the resolved source representation. The
	// size sentinel still protects against a server that ignores this preference.
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", config.UserAgent)

	response, err := client.Do(request)
	if err != nil {
		// http.Client closes the response body before returning a redirect-policy
		// error. For all other errors its documented response is ignorable, so there
		// is no caller-owned body to close (and no untrusted response to retain).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Document{}, false, "", canceledError(spec.Kind, ModeHTTPS, ctxErr)
		}
		if errors.Is(err, errRedirectPolicy) {
			return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrRedirect, 0, nil)
		}
		return Document{}, transientSourceTransportError(err), "", newLoadError(spec.Kind, ModeHTTPS, ErrTransport, 0, nil)
	}
	if response.Body == nil {
		if response.StatusCode != http.StatusOK {
			return Document{}, retryableSourceStatus(response.StatusCode), sourceRetryAfter(response.Header), newLoadError(spec.Kind, ModeHTTPS, ErrHTTPStatus, response.StatusCode, nil)
		}
		return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrRead, response.StatusCode, nil)
	}
	// A close failure invalidates otherwise successful bytes. A primary status,
	// content, size, read, or cancellation error remains authoritative and sanitized.
	defer func() {
		if err := response.Body.Close(); err != nil && resultErr == nil {
			document = Document{}
			if ctxErr := ctx.Err(); ctxErr != nil {
				resultErr = canceledError(spec.Kind, ModeHTTPS, ctxErr)
			} else {
				retryable = transientSourceTransportError(err)
				resultErr = newLoadError(spec.Kind, ModeHTTPS, ErrClose, 0, nil)
			}
		}
	}()

	if response.StatusCode != http.StatusOK {
		// Only statuses eligible for replay receive a small bounded drain. This can
		// preserve connection reuse without making an arbitrary error body part of the
		// trusted source or allowing it to consume unbounded memory.
		_ = drainSourceRetryableBody(response)
		return Document{}, retryableSourceStatus(response.StatusCode), sourceRetryAfter(response.Header), newLoadError(spec.Kind, ModeHTTPS, ErrHTTPStatus, response.StatusCode, nil)
	}
	if !supportedContentEncoding(response) {
		return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrContentEncoding, response.StatusCode, nil)
	}
	if !supportedTextContentType(response.Header.Values("Content-Type")) {
		return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrContentType, response.StatusCode, nil)
	}
	if response.ContentLength > config.MaxBytes {
		return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrTooLarge, response.StatusCode, nil)
	}

	data, err := readBounded(ctx, response.Body, config.MaxBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Document{}, false, "", canceledError(spec.Kind, ModeHTTPS, ctxErr)
		}
		if errors.Is(err, ErrTooLarge) {
			return Document{}, false, "", newLoadError(spec.Kind, ModeHTTPS, ErrTooLarge, response.StatusCode, nil)
		}
		return Document{}, transientSourceTransportError(err), "", newLoadError(spec.Kind, ModeHTTPS, ErrRead, response.StatusCode, nil)
	}
	parsed, err := validateRemoteURL(spec.Kind, requestURL)
	if err != nil {
		return Document{}, false, "", err
	}
	return makeDocument(spec.Kind, ModeHTTPS, parsed.Hostname(), data), false, "", nil
}

func validateHTTPConfig(config Config) error {
	if config.HTTPClient == nil || config.HTTPClient.Transport == nil {
		return ErrInvalidConfig
	}
	if config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > maximumHTTPTimeout {
		return ErrInvalidConfig
	}
	if config.UserAgent == "" || len(config.UserAgent) > maxUserAgentBytes || strings.TrimSpace(config.UserAgent) != config.UserAgent {
		return ErrInvalidConfig
	}
	for index := 0; index < len(config.UserAgent); index++ {
		if config.UserAgent[index] < 0x20 || config.UserAgent[index] > 0x7e {
			return ErrInvalidConfig
		}
	}
	if _, err := sourceRetryPolicyFor(config.Retry); err != nil {
		return ErrInvalidConfig
	}
	return nil
}

func restrictedClient(base *http.Client, kind Kind, userAgent string) *http.Client {
	client := *base
	// Input downloads never need ambient cookies. Copying the client avoids mutating
	// caller-owned policy while retaining its bounded transport and connection pool.
	client.Jar = nil
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return errRedirectPolicy
		}
		if _, err := validateRemoteURL(kind, request.URL.String()); err != nil {
			return errRedirectPolicy
		}
		request.Header.Set("Accept", "text/plain, application/octet-stream;q=0.9")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", userAgent)
		return nil
	}
	return &client
}

func supportedContentEncoding(response *http.Response) bool {
	values := response.Header.Values("Content-Encoding")
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")
}

func supportedTextContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil {
		return false
	}
	switch strings.ToLower(mediaType) {
	case "application/octet-stream":
		return len(parameters) == 0
	case "text/plain":
		for name, value := range parameters {
			if !strings.EqualFold(name, "charset") {
				return false
			}
			if !strings.EqualFold(value, "utf-8") && !strings.EqualFold(value, "us-ascii") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func readBounded(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, error) {
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: reader}, N: maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(target []byte) (int, error) {
	select {
	case <-reader.ctx.Done():
		return 0, reader.ctx.Err()
	default:
		return reader.reader.Read(target)
	}
}

func makeDocument(kind Kind, mode Mode, origin string, data []byte) Document {
	digest := sha256.Sum256(data)
	return Document{
		data: data,
		provenance: Provenance{
			Kind:   kind,
			Mode:   mode,
			Origin: origin,
			Bytes:  int64(len(data)),
			SHA256: hex.EncodeToString(digest[:]),
		},
	}
}

func newLoadError(kind Kind, mode Mode, code error, statusCode int, cause error) *LoadError {
	return &LoadError{Kind: kind, Mode: mode, StatusCode: statusCode, code: code, cause: cause}
}

func canceledError(kind Kind, mode Mode, cause error) *LoadError {
	return newLoadError(kind, mode, ErrCanceled, 0, cause)
}

func selectedMode(spec Spec) Mode {
	switch {
	case spec.URL != "" && spec.File == "":
		return ModeHTTPS
	case spec.File != "" && spec.URL == "":
		return ModeFile
	default:
		return ModeUnknown
	}
}
