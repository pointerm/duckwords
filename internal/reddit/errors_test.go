package reddit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestErrorIsSanitizedAndUnwraps(t *testing.T) {
	t.Parallel()

	const secret = "planted-client-secret"
	cause := errors.Join(context.DeadlineExceeded, errors.New("url contained "+secret))
	err := newError(ErrorTransport, EndpointComments, "abc123", 503, cause)

	for _, want := range []string{"comments", "transport", "abc123", "503"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Error() = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Error() leaked secret: %q", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(%v, DeadlineExceeded) = false", err)
	}

	var typed *Error
	if !errors.As(err, &typed) || typed.Class != ErrorTransport {
		t.Fatalf("errors.As() = %#v, want transport Error", typed)
	}
}

func TestNilErrorMethods(t *testing.T) {
	t.Parallel()

	var err *Error
	if err.Error() != "reddit adapter error" || err.Unwrap() != nil {
		t.Fatalf("nil Error methods returned unexpected values")
	}
}

func TestClassForStatus(t *testing.T) {
	t.Parallel()

	tests := map[int]ErrorClass{
		400: ErrorInvalidInput,
		401: ErrorAuthentication,
		403: ErrorForbidden,
		404: ErrorNotFound,
		408: ErrorTransport,
		429: ErrorRateLimited,
		500: ErrorServer,
		503: ErrorServer,
		418: ErrorProtocol,
		600: ErrorProtocol,
	}
	for status, want := range tests {
		if got := classForStatus(status); got != want {
			t.Errorf("classForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}
