// Package reddit retrieves and walks complete Reddit comment trees through public
// old.reddit.com JSON representations without retaining raw user content.
package reddit

import (
	"errors"
	"fmt"
)

// ErrorClass identifies a stable, policy-relevant adapter failure category.
type ErrorClass string

const (
	// ErrorInvalidInput identifies an invalid local argument or request value.
	ErrorInvalidInput ErrorClass = "invalid_input"
	// ErrorAccess identifies an HTTP 401/403 response or a blocked redirect to a
	// login/interstitial page. These responses do not prove that a post is absent.
	ErrorAccess ErrorClass = "access"
	// ErrorNotFound identifies an HTTP 404 or 410 response from Reddit.
	ErrorNotFound ErrorClass = "not_found"
	// ErrorRateLimited identifies an HTTP 429 response from Reddit.
	ErrorRateLimited ErrorClass = "rate_limited"
	// ErrorServer identifies a Reddit HTTP 5xx response.
	ErrorServer ErrorClass = "server"
	// ErrorTransport identifies a failure before a complete HTTP response arrived.
	ErrorTransport ErrorClass = "transport"
	// ErrorProtocol identifies malformed or contradictory response data.
	ErrorProtocol ErrorClass = "protocol"
	// ErrorIncomplete identifies a response that was valid enough to process but did
	// not prove that the complete reachable comment tree was returned. Callers must
	// discard every count derived from that post.
	ErrorIncomplete ErrorClass = "incomplete"
	// ErrorResourceLimit identifies work or input rejected by a configured safety bound.
	ErrorResourceLimit ErrorClass = "resource_limit"
	// ErrorCanceled identifies cancellation or deadline expiry.
	ErrorCanceled ErrorClass = "canceled"
	// ErrorVisitor identifies a failure returned by the streaming comment visitor.
	ErrorVisitor ErrorClass = "visitor"
)

// Endpoint identifies the Reddit operation that failed without including a URL or
// query string in diagnostics.
type Endpoint string

const (
	// EndpointComments identifies the initial post comment-listing operation.
	EndpointComments Endpoint = "comments"
	// EndpointContinuation identifies a focal-comment request used to continue a
	// depth-truncated branch.
	EndpointContinuation Endpoint = "continuation"
	// EndpointCommentExpansion identifies a focal-comment GET used to resolve a
	// regular kind:"more" placeholder.
	EndpointCommentExpansion Endpoint = "comment_expansion"
)

var errInvalidAdapterError = errors.New("invalid reddit adapter error")

// Error contains sanitized Reddit adapter failure metadata. Its Error method never
// formats the underlying error because HTTP errors can contain credential-bearing
// URLs; callers can still classify the cause with errors.Is and errors.As.
type Error struct {
	Class      ErrorClass
	Endpoint   Endpoint
	PostID     string
	StatusCode int
	err        error
}

// newError constructs a sanitized adapter error while retaining cause for
// errors.Is/errors.As classification.
func newError(class ErrorClass, endpoint Endpoint, postID string, statusCode int, cause error) *Error {
	if cause == nil {
		cause = errInvalidAdapterError
	}
	return &Error{
		Class:      class,
		Endpoint:   endpoint,
		PostID:     postID,
		StatusCode: statusCode,
		err:        cause,
	}
}

// Error returns only bounded, non-secret context.
func (e *Error) Error() string {
	if e == nil {
		return "reddit adapter error"
	}
	message := fmt.Sprintf("reddit %s: %s", e.Endpoint, e.Class)
	if e.PostID != "" {
		message += fmt.Sprintf(" for post %q", e.PostID)
	}
	if e.StatusCode != 0 {
		message += fmt.Sprintf(" (status %d)", e.StatusCode)
	}
	return message
}

// Unwrap exposes the original cause for programmatic classification only.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func classForStatus(statusCode int) ErrorClass {
	switch {
	case statusCode >= 300 && statusCode <= 399:
		return ErrorAccess
	case statusCode == 400:
		return ErrorInvalidInput
	case statusCode == 408:
		return ErrorTransport
	case statusCode == 401 || statusCode == 403:
		return ErrorAccess
	case statusCode == 404 || statusCode == 410:
		return ErrorNotFound
	case statusCode == 429:
		return ErrorRateLimited
	case statusCode >= 500 && statusCode <= 599:
		return ErrorServer
	default:
		return ErrorProtocol
	}
}
