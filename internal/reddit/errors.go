// Package reddit retrieves and walks complete Reddit comment trees through the
// authenticated Data API without retaining raw user content.
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
	// ErrorAuthentication identifies rejected or unavailable OAuth credentials.
	ErrorAuthentication ErrorClass = "authentication"
	// ErrorForbidden identifies an HTTP 403 response from Reddit.
	ErrorForbidden ErrorClass = "forbidden"
	// ErrorNotFound identifies an HTTP 404 response from Reddit.
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
	// EndpointOAuthToken identifies the application-only token operation.
	EndpointOAuthToken Endpoint = "oauth_token"
	// EndpointComments identifies the initial post comment-listing operation.
	EndpointComments Endpoint = "comments"
	// EndpointContinuation identifies a focal-comment request used to continue a
	// depth-truncated branch.
	EndpointContinuation Endpoint = "continuation"
	// EndpointMoreChildren identifies a comment-placeholder expansion operation.
	EndpointMoreChildren Endpoint = "morechildren"
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
	case statusCode == 400:
		return ErrorInvalidInput
	case statusCode == 408:
		return ErrorTransport
	case statusCode == 401:
		return ErrorAuthentication
	case statusCode == 403:
		return ErrorForbidden
	case statusCode == 404:
		return ErrorNotFound
	case statusCode == 429:
		return ErrorRateLimited
	case statusCode >= 500 && statusCode <= 599:
		return ErrorServer
	default:
		return ErrorProtocol
	}
}
