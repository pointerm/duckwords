package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/config"
)

const offlineFixtureProcessEnvironment = "DUCKWORDS_OFFLINE_FIXTURE_PROCESS"

var errUnexpectedOfflineFixtureRequest = errors.New("unexpected offline fixture request")

// TestMain exposes one deterministic process fixture only from the compiled test
// binary. The production duckwords binary contains neither this environment switch
// nor injectable endpoints, so release verification cannot weaken the live approval
// gate or redirect credentials.
func TestMain(m *testing.M) {
	if os.Getenv(offlineFixtureProcessEnvironment) == "1" {
		os.Exit(runOfflineFixtureProcess(os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func runOfflineFixtureProcess(stdout, stderr io.Writer) int {
	fixtureEnvironment := map[string]string{
		envRedditAPIApproved:  "true",
		envRedditClientID:     "fixture-client-id",
		envRedditClientSecret: "fixture-client-secret",
		envRedditUserAgent:    "cli:duckwords:offline-fixture (by /u/example)",
	}
	dependencies := productionDependencies{
		lookupEnv: func(name string) (string, bool) {
			value, found := fixtureEnvironment[name]
			return value, found
		},
		newHTTP: func(time.Duration, int) (*http.Client, error) {
			return &http.Client{
				Transport: offlineFixtureTransport{},
				Timeout:   time.Second,
			}, nil
		},
		now: time.Now,
	}
	return run(
		context.Background(),
		[]string{
			"--workers=1",
			"--rate-limit=1.5",
			"--request-timeout=1s",
			"--timeout=5s",
			"--max-retries=0",
			"--retry-budget=1s",
		},
		stdout,
		stderr,
		func(ctx context.Context, cfg config.Config, logger *slog.Logger) (app.Result, error) {
			return executeProduction(ctx, cfg, logger, dependencies)
		},
	)
}

type offlineFixtureTransport struct{}

func (offlineFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errUnexpectedOfflineFixtureRequest
	}
	response := func(contentType, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	}

	switch request.URL.String() {
	case config.DefaultPostsURL:
		if request.UserAgent() != sourceDownloadUserAgent() {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return response("text/plain; charset=utf-8", "https://redd.it/duck123\n")
	case config.DefaultDictionaryURL:
		if request.UserAgent() != sourceDownloadUserAgent() {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return response("text/plain; charset=utf-8", "duck\npond\nwater\n")
	case "https://www.reddit.com/api/v1/access_token":
		clientID, secret, ok := request.BasicAuth()
		if !ok || clientID != "fixture-client-id" || secret != "fixture-client-secret" ||
			request.UserAgent() != "cli:duckwords:offline-fixture (by /u/example)" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return response(
			"application/json",
			`{"access_token":"offline-token","token_type":"bearer","expires_in":3600}`,
		)
	default:
		if request.URL.Scheme != "https" || request.URL.Host != "oauth.reddit.com" ||
			request.URL.Path != "/comments/duck123" || request.URL.Query().Get("raw_json") != "1" ||
			request.Header.Get("Authorization") != "Bearer offline-token" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return response(
			"application/json",
			`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"duck123","name":"t3_duck123"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t1","data":{"id":"c1","name":"t1_c1","link_id":"t3_duck123","parent_id":"t3_duck123","body":"Duck duck water.","replies":""}}]}}]`,
		)
	}
}

func TestOfflineFixtureProcessProducesGoldenJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	if code := runOfflineFixtureProcess(&stdout, &stderr); code != exitSuccess {
		t.Fatalf("offline fixture exit code = %d; stderr:\n%s", code, stderr.String())
	}
	const want = "[\n  {\n    \"word\": \"duck\",\n    \"count\": 2\n  },\n  {\n    \"word\": \"water\",\n    \"count\": 1\n  }\n]\n"
	if stdout.String() != want {
		t.Fatalf("offline fixture stdout mismatch:\ngot:\n%s\nwant:\n%s", stdout.String(), want)
	}
	for _, event := range []string{"event=run_started", "event=run_summary", "event=output_written"} {
		if !strings.Contains(stderr.String(), event) {
			t.Fatalf("offline fixture stderr does not contain %q:\n%s", event, stderr.String())
		}
	}
	for _, secret := range []string{"fixture-client-secret", "offline-token"} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("offline fixture stderr contains planted secret %q", secret)
		}
	}
}
