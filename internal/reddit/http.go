package reddit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
)

const maxRetryableResponseDrainBytes int64 = 32 << 10

var (
	errUnexpectedContentType = errors.New("unexpected response content type")
	errResponseTooLarge      = errors.New("response exceeds byte limit")
	errMalformedJSON         = errors.New("malformed JSON response")
	errInvalidHTTPCall       = errors.New("invalid HTTP call")
	errMissingResponseBody   = errors.New("HTTP response has no body")
	errUnexpectedHTTPStatus  = errors.New("unexpected HTTP response status")
	errResponseRead          = errors.New("read HTTP response")
	errResponseClose         = errors.New("close HTTP response")
)

func hasSingleJSONContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func executeJSON(
	ctx context.Context,
	httpClient *http.Client,
	request *http.Request,
	endpoint Endpoint,
	postID string,
	maxResponseBytes int64,
	destination any,
) error {
	payload, err := executePayload(ctx, httpClient, request, endpoint, postID, maxResponseBytes)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return newError(ErrorProtocol, endpoint, postID, http.StatusOK, errMalformedJSON)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return newError(ErrorProtocol, endpoint, postID, http.StatusOK, errMalformedJSON)
	}
	return nil
}

func executePayload(
	ctx context.Context,
	httpClient *http.Client,
	request *http.Request,
	endpoint Endpoint,
	postID string,
	maxResponseBytes int64,
) ([]byte, error) {
	result, err := executePayloadAttempt(ctx, httpClient, request, endpoint, postID, maxResponseBytes, nil)
	return result.payload, err
}

type responseAdmission func(context.Context, int64) (func(), error)

// executePayloadAttempt performs exactly one HTTP exchange. It returns only the
// payload, its optional admission lease, and the allowlisted scheduling headers
// needed by RequestPolicy. The attempted bit lets policy metrics exclude validation
// or gate failures that occur before Client.Do.
func executePayloadAttempt(
	ctx context.Context,
	httpClient *http.Client,
	request *http.Request,
	endpoint Endpoint,
	postID string,
	maxResponseBytes int64,
	admitResponse responseAdmission,
) (policyAttemptResult, error) {
	if ctx == nil || httpClient == nil || request == nil {
		return policyAttemptResult{}, newError(ErrorInvalidInput, endpoint, postID, 0, errInvalidHTTPCall)
	}
	if maxResponseBytes <= 0 {
		return policyAttemptResult{}, newError(ErrorResourceLimit, endpoint, postID, 0, errResponseTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return policyAttemptResult{}, newError(ErrorCanceled, endpoint, postID, 0, err)
	}
	// http.Client.Timeout does not cancel the caller's context while code is waiting
	// between response headers and a body read. Mirror it with an explicit child
	// context so response-budget admission is covered by the same request deadline.
	requestCtx := ctx
	cancelRequest := func() {}
	if httpClient.Timeout > 0 {
		requestCtx, cancelRequest = context.WithTimeout(ctx, httpClient.Timeout)
		request = request.Clone(requestCtx)
	}
	defer cancelRequest()

	response, err := httpClient.Do(request)
	result := policyAttemptResult{attempted: true}
	if response != nil {
		result.statusCode = response.StatusCode
	}
	if err != nil {
		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		if response != nil && response.Body != nil {
			// A response returned alongside an error is unusable according to the
			// net/http contract. Closing it is best-effort cleanup; the transport
			// error remains the actionable failure.
			_ = response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, newError(ErrorCanceled, endpoint, postID, 0, ctxErr)
		}
		if errors.Is(err, ErrClientRedirect) {
			return result, newError(ErrorAccess, endpoint, postID, statusCode, ErrClientRedirect)
		}
		return result, newError(ErrorTransport, endpoint, postID, 0, &httpRequestError{cause: err})
	}
	result.headers = policyHeaders(response.Header)
	if response.Body == nil {
		return result, newError(ErrorProtocol, endpoint, postID, response.StatusCode, errMissingResponseBody)
	}

	// Reddit's JSON endpoints have an exact 200 contract. Treat every other status,
	// including a syntactically valid-looking 206 Partial Content, as unusable so a
	// truncated representation can never be reported as a complete comment tree.
	if response.StatusCode != http.StatusOK {
		drainErr := drainRetryableResponseBody(response)
		closeErr := response.Body.Close()
		cause := error(errUnexpectedHTTPStatus)
		if drainErr != nil {
			cause = errors.Join(cause, &httpResponseError{kind: errResponseRead, cause: drainErr})
		}
		if closeErr != nil {
			cause = errors.Join(cause, &httpResponseError{kind: errResponseClose, cause: closeErr})
		}
		return result, newError(classForStatus(response.StatusCode), endpoint, postID, response.StatusCode, cause)
	}
	if response.ContentLength > maxResponseBytes {
		closeErr := response.Body.Close()
		cause := error(errResponseTooLarge)
		if closeErr != nil {
			cause = errors.Join(cause, &httpResponseError{kind: errResponseClose, cause: closeErr})
		}
		return result, newError(ErrorResourceLimit, endpoint, postID, response.StatusCode, cause)
	}
	if !hasSingleJSONContentType(response.Header) {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return result, newError(ErrorProtocol, endpoint, postID, response.StatusCode, errors.Join(errUnexpectedContentType, &httpResponseError{kind: errResponseClose, cause: closeErr}))
		}
		return result, newError(ErrorProtocol, endpoint, postID, response.StatusCode, errUnexpectedContentType)
	}

	// Reserve the configured response capacity before allocating or reading an
	// accepted body. Non-200 attempts and invalid content types never hold this lease,
	// so retry backoff cannot starve unrelated decoders. Ownership transfers through
	// policyAttemptResult only after the whole body has been read successfully.
	var releasePayload func()
	if admitResponse != nil {
		releasePayload, err = admitResponse(requestCtx, maxResponseBytes)
		if err != nil {
			closeErr := response.Body.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, newError(ErrorCanceled, endpoint, postID, response.StatusCode, ctxErr)
			}
			cause := err
			if closeErr != nil {
				cause = errors.Join(cause, &httpResponseError{kind: errResponseClose, cause: closeErr})
			}
			if requestCtx.Err() != nil {
				return result, newError(ErrorTransport, endpoint, postID, response.StatusCode, &httpResponseError{kind: errResponseRead, cause: cause})
			}
			return result, newError(ErrorResourceLimit, endpoint, postID, response.StatusCode, cause)
		}
		defer func() {
			if releasePayload != nil {
				releasePayload()
			}
		}()
	}

	limited := &io.LimitedReader{R: response.Body, N: maxResponseBytes + 1}
	payload, readErr := io.ReadAll(limited)
	closeErr := response.Body.Close()
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, newError(ErrorCanceled, endpoint, postID, response.StatusCode, ctxErr)
		}
		if isTransportReadError(readErr) {
			return result, newError(ErrorTransport, endpoint, postID, response.StatusCode, &httpResponseError{kind: errResponseRead, cause: readErr})
		}
		return result, newError(ErrorProtocol, endpoint, postID, response.StatusCode, &httpResponseError{kind: errResponseRead, cause: readErr})
	}
	if closeErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, newError(ErrorCanceled, endpoint, postID, response.StatusCode, ctxErr)
		}
		return result, newError(ErrorProtocol, endpoint, postID, response.StatusCode, &httpResponseError{kind: errResponseClose, cause: closeErr})
	}
	if int64(len(payload)) > maxResponseBytes {
		return result, newError(ErrorResourceLimit, endpoint, postID, response.StatusCode, errResponseTooLarge)
	}
	result.payload = payload
	result.releasePayload = releasePayload
	releasePayload = nil
	return result, nil
}

// drainRetryableResponseBody consumes only a small status body before close. A body
// that reaches EOF within this cap preserves HTTP/1.1 connection reuse; a larger or
// slow body remains bounded by both this byte limit and the request context/client
// timeout. Permanent status failures are closed without reading attacker-controlled
// data.
func drainRetryableResponseBody(response *http.Response) error {
	if response == nil || response.Body == nil || !statusShouldBeDrained(response.StatusCode) {
		return nil
	}
	if response.ContentLength > maxRetryableResponseDrainBytes {
		return nil
	}
	_, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxRetryableResponseDrainBytes+1))
	return err
}

func statusShouldBeDrained(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// isTransportReadError separates request/client timeouts and network failures from
// malformed or truncated response streams. Only cancellation observed on the
// caller's context is classified as ErrorCanceled by the call sites.
func isTransportReadError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

// httpRequestError retains errors.Is/errors.As classification while ensuring that
// neither an error chain nor a future caller accidentally formats a credential-
// bearing request URL or Authorization header.
type httpRequestError struct {
	cause error
}

func (e *httpRequestError) Error() string { return "Reddit HTTP request failed" }
func (e *httpRequestError) Unwrap() error { return e.cause }

// httpResponseError provides the same redaction boundary for arbitrary errors from
// an untrusted response body implementation.
type httpResponseError struct {
	kind  error
	cause error
}

func (e *httpResponseError) Error() string { return e.kind.Error() }
func (e *httpResponseError) Unwrap() error { return errors.Join(e.kind, e.cause) }
