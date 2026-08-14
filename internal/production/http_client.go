package production

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/pointerm/duckwords/internal/app"
)

const (
	productionDialTimeout       = 10 * time.Second
	productionTLSHandshake      = 10 * time.Second
	productionIdleConnTimeout   = 90 * time.Second
	productionExpectContinue    = time.Second
	productionMaxResponseHeader = 64 << 10
)

var errHTTPClientConfig = errors.New("invalid production HTTP client configuration")

// newProductionHTTPClient centralizes transport ownership for source downloads,
// OAuth, and Reddit API traffic. The specialized clients copy this value to apply
// stricter redirect and cookie policy while retaining this shared connection pool.
func newProductionHTTPClient(requestTimeout time.Duration, workers int) (*http.Client, error) {
	if requestTimeout <= 0 {
		return nil, fmt.Errorf("%w: request timeout must be positive", errHTTPClientConfig)
	}
	if workers < app.MinWorkers || workers > app.MaxWorkers {
		return nil, fmt.Errorf(
			"%w: workers must be between %d and %d",
			errHTTPClientConfig,
			app.MinWorkers,
			app.MaxWorkers,
		)
	}

	connectionBudget := workers + 4 // workers, one OAuth flight, and two source loads.
	dialTimeout := min(requestTimeout, productionDialTimeout)
	tlsTimeout := min(requestTimeout, productionTLSHandshake)
	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           max(16, connectionBudget),
		MaxIdleConnsPerHost:    connectionBudget,
		MaxConnsPerHost:        connectionBudget,
		IdleConnTimeout:        productionIdleConnTimeout,
		TLSHandshakeTimeout:    tlsTimeout,
		ResponseHeaderTimeout:  requestTimeout,
		ExpectContinueTimeout:  productionExpectContinue,
		MaxResponseHeaderBytes: productionMaxResponseHeader,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
	}, nil
}
