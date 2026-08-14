package production

import (
	"crypto/tls"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/app"
)

func TestNewProductionHTTPClientUsesFiniteSharedTransportPolicy(t *testing.T) {
	t.Parallel()

	client, err := newProductionHTTPClient(17*time.Second, 8)
	if err != nil {
		t.Fatalf("newProductionHTTPClient() error = %v", err)
	}
	if client.Timeout != 17*time.Second {
		t.Fatalf("Timeout = %s, want 17s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil || transport.Proxy == nil {
		t.Fatal("transport lacks explicit dial or proxy policy")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false")
	}
	if transport.MaxConnsPerHost != 10 || transport.MaxIdleConnsPerHost != 10 {
		t.Fatalf(
			"connection bounds = max %d, idle %d, want 10",
			transport.MaxConnsPerHost,
			transport.MaxIdleConnsPerHost,
		)
	}
	if transport.ResponseHeaderTimeout != 17*time.Second ||
		transport.TLSHandshakeTimeout != productionTLSHandshake ||
		transport.IdleConnTimeout != productionIdleConnTimeout {
		t.Fatalf("unexpected timeout policy: %+v", transport)
	}
	if transport.MaxResponseHeaderBytes != productionMaxResponseHeader {
		t.Fatalf("MaxResponseHeaderBytes = %d", transport.MaxResponseHeaderBytes)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS minimum = %#v, want TLS 1.2", transport.TLSClientConfig)
	}
}

func TestNewProductionHTTPClientCapsStageTimeoutsAtRequestTimeout(t *testing.T) {
	t.Parallel()

	client, err := newProductionHTTPClient(time.Second, 1)
	if err != nil {
		t.Fatalf("newProductionHTTPClient() error = %v", err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.TLSHandshakeTimeout != time.Second || transport.ResponseHeaderTimeout != time.Second {
		t.Fatalf("stage timeouts exceed request timeout: %+v", transport)
	}
}

func TestNewProductionHTTPClientRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		workers int
	}{
		{name: "zero timeout", workers: app.MinWorkers},
		{name: "negative timeout", timeout: -time.Second, workers: app.MinWorkers},
		{name: "too few workers", timeout: time.Second, workers: app.MinWorkers - 1},
		{name: "too many workers", timeout: time.Second, workers: app.MaxWorkers + 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client, err := newProductionHTTPClient(test.timeout, test.workers)
			if client != nil || !errors.Is(err, errHTTPClientConfig) {
				t.Fatalf("client = %#v, error = %v, want nil/config error", client, err)
			}
		})
	}
}
