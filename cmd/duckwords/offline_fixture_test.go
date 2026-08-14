package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/cli"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/production"
)

const (
	offlineFixtureProcessEnvironment = "DUCKWORDS_OFFLINE_FIXTURE_PROCESS"
	offlineFixtureProfileEnvironment = "DUCKWORDS_OFFLINE_FIXTURE_PROFILE"

	offlineFixtureProfileSmall     offlineFixtureProfile = ""
	offlineFixtureProfileSynthetic offlineFixtureProfile = "synthetic-demo"

	syntheticPostsURL      = "https://gist.githubusercontent.com/duckwords-fixture/synthetic/raw/posts.txt"
	syntheticDictionaryURL = "https://raw.githubusercontent.com/duckwords-fixture/synthetic/main/dictionary.txt"
)

// The synthetic fixture files are test-only inputs compiled into the Go test
// binary. They never enter the production duckwords binary.

//go:embed testdata/synthetic/posts.txt
var syntheticPostsDocument string

//go:embed testdata/synthetic/dictionary.txt
var syntheticDictionaryDocument string

//go:embed testdata/synthetic/post-synth001.json
var syntheticPostOneResponse string

//go:embed testdata/synthetic/more-synth001.json
var syntheticMoreResponse string

//go:embed testdata/synthetic/post-synth002.json
var syntheticPostTwoResponse string

//go:embed testdata/synthetic/post-synth003.json
var syntheticPostThreeResponse string

//go:embed testdata/synthetic/continuation-synth003.json
var syntheticContinuationResponse string

var errUnexpectedOfflineFixtureRequest = errors.New("unexpected offline fixture request")

type offlineFixtureProfile string

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
	return runOfflineFixtureProcessWithProfile(
		offlineFixtureProfile(os.Getenv(offlineFixtureProfileEnvironment)),
		stdout,
		stderr,
	)
}

func runOfflineFixtureProcessWithProfile(profile offlineFixtureProfile, stdout, stderr io.Writer) int {
	if profile != offlineFixtureProfileSmall && profile != offlineFixtureProfileSynthetic {
		_, _ = fmt.Fprintln(stderr, "offline fixture profile is invalid")
		return cli.ExitFailure
	}
	fixtureEnvironment := map[string]string{
		"REDDIT_API_ACCESS_APPROVED": "true",
		"REDDIT_CLIENT_ID":           "fixture-client-id",
		"REDDIT_CLIENT_SECRET":       "fixture-client-secret",
		"REDDIT_USER_AGENT":          "cli:duckwords:offline-fixture (by /u/example)",
	}
	dependencies := production.NewDependencies(
		func(name string) (string, bool) {
			value, found := fixtureEnvironment[name]
			return value, found
		},
		func(requestTimeout time.Duration, _ int) (*http.Client, error) {
			return &http.Client{
				Transport: offlineFixtureTransport{profile: profile},
				Timeout:   requestTimeout,
			}, nil
		},
		time.Now,
	)
	args := []string{
		"--workers=1",
		"--rate-limit=1.5",
		"--request-timeout=1s",
		"--timeout=5s",
		"--max-retries=0",
		"--retry-budget=1s",
	}
	if profile == offlineFixtureProfileSynthetic {
		args = []string{
			"--posts-url=" + syntheticPostsURL,
			"--dictionary-url=" + syntheticDictionaryURL,
			"--workers=4",
			"--rate-limit=1.5",
			"--request-timeout=2s",
			"--timeout=15s",
			"--max-retries=0",
			"--retry-budget=10s",
			"--log-format=json",
		}
	}
	return cli.Run(
		context.Background(),
		args,
		stdout,
		stderr,
		func(ctx context.Context, cfg config.Config, logger *slog.Logger) (app.Result, error) {
			return production.ExecuteWithDependencies(ctx, cfg, logger, dependencies)
		},
	)
}

type offlineFixtureTransport struct {
	profile offlineFixtureProfile
}

func (transport offlineFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errUnexpectedOfflineFixtureRequest
	}
	if request.Body != nil {
		defer request.Body.Close()
	}
	postsURL := config.DefaultPostsURL
	dictionaryURL := config.DefaultDictionaryURL
	postsDocument := "https://redd.it/duck123\n"
	dictionaryDocument := "duck\npond\nwater\n"
	if transport.profile == offlineFixtureProfileSynthetic {
		postsURL = syntheticPostsURL
		dictionaryURL = syntheticDictionaryURL
		postsDocument = syntheticPostsDocument
		dictionaryDocument = syntheticDictionaryDocument
	}

	switch request.URL.String() {
	case postsURL:
		if request.Method != http.MethodGet || request.UserAgent() != production.SourceDownloadUserAgent() ||
			request.Header.Get("Accept") != "text/plain, application/octet-stream;q=0.9" ||
			request.Header.Get("Accept-Encoding") != "identity" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(request, "text/plain; charset=utf-8", postsDocument)
	case dictionaryURL:
		if request.Method != http.MethodGet || request.UserAgent() != production.SourceDownloadUserAgent() ||
			request.Header.Get("Accept") != "text/plain, application/octet-stream;q=0.9" ||
			request.Header.Get("Accept-Encoding") != "identity" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(request, "text/plain; charset=utf-8", dictionaryDocument)
	case "https://www.reddit.com/api/v1/access_token":
		clientID, secret, ok := request.BasicAuth()
		if request.Method != http.MethodPost ||
			request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
			request.Header.Get("Accept") != "application/json" ||
			!ok || clientID != "fixture-client-id" || secret != "fixture-client-secret" ||
			request.UserAgent() != "cli:duckwords:offline-fixture (by /u/example)" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		if err := request.ParseForm(); err != nil || request.PostForm.Encode() != "grant_type=client_credentials" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(
			request,
			"application/json",
			`{"access_token":"offline-token","token_type":"bearer","expires_in":3600}`,
		)
	}

	if request.URL.Scheme != "https" || request.URL.Host != "oauth.reddit.com" ||
		request.Header.Get("Authorization") != "Bearer offline-token" ||
		request.Header.Get("Accept") != "application/json" ||
		request.UserAgent() != "cli:duckwords:offline-fixture (by /u/example)" {
		return nil, errUnexpectedOfflineFixtureRequest
	}
	if transport.profile == offlineFixtureProfileSynthetic {
		return syntheticOfflineFixtureResponse(request)
	}
	if request.Method != http.MethodGet || request.URL.Path != "/comments/duck123" ||
		request.URL.Query().Encode() != "raw_json=1&showmore=true&sort=confidence" {
		return nil, errUnexpectedOfflineFixtureRequest
	}
	return offlineFixtureResponse(
		request,
		"application/json",
		`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"duck123","name":"t3_duck123"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t1","data":{"id":"c1","name":"t1_c1","link_id":"t3_duck123","parent_id":"t3_duck123","body":"Duck duck water.","replies":""}}]}}]`,
	)
}

func syntheticOfflineFixtureResponse(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/api/morechildren" {
		if request.Method != http.MethodPost || request.URL.RawQuery != "raw_json=1" ||
			request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		if err := request.ParseForm(); err != nil ||
			request.PostForm.Encode() != "api_type=json&children=a4%2Ca5&limit_children=false&link_id=t3_synth001&sort=confidence" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(request, "application/json", syntheticMoreResponse)
	}
	if request.Method != http.MethodGet {
		return nil, errUnexpectedOfflineFixtureRequest
	}

	switch request.URL.Path {
	case "/comments/synth001":
		if request.URL.Query().Encode() != "raw_json=1&showmore=true&sort=confidence" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(request, "application/json", syntheticPostOneResponse)
	case "/comments/synth002":
		if request.URL.Query().Encode() != "raw_json=1&showmore=true&sort=confidence" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(request, "application/json", syntheticPostTwoResponse)
	case "/comments/synth003":
		switch request.URL.Query().Encode() {
		case "raw_json=1&showmore=true&sort=confidence":
			return offlineFixtureResponse(request, "application/json", syntheticPostThreeResponse)
		case "comment=d1&context=0&raw_json=1&showmore=true&sort=confidence":
			return offlineFixtureResponse(request, "application/json", syntheticContinuationResponse)
		default:
			return nil, errUnexpectedOfflineFixtureRequest
		}
	default:
		return nil, errUnexpectedOfflineFixtureRequest
	}
}

func offlineFixtureResponse(request *http.Request, contentType, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {contentType}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

func TestOfflineFixtureProcessProducesGoldenJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	if code := runOfflineFixtureProcessWithProfile(offlineFixtureProfileSmall, &stdout, &stderr); code != cli.ExitSuccess {
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

func TestOfflineFixtureProcessRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	if code := runOfflineFixtureProcessWithProfile("unknown", &stdout, &stderr); code != cli.ExitFailure {
		t.Fatalf("offline fixture exit code = %d, want %d", code, cli.ExitFailure)
	}
	if stdout.Len() != 0 || stderr.String() != "offline fixture profile is invalid\n" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
