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

//go:embed testdata/synthetic/expansion-synth001-a4.json
var syntheticExpansionA4Response string

//go:embed testdata/synthetic/expansion-synth001-a5.json
var syntheticExpansionA5Response string

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
// nor injectable endpoints, so release verification cannot redirect public requests.
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
	access, err := production.ResolveAccessIdentity(os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "offline fixture User-Agent is invalid")
		return cli.ExitFailure
	}
	dependencies := production.NewDependencies(
		func(string) (string, bool) {
			panic("production re-resolved the CLI-owned access identity")
		},
		func(requestTimeout time.Duration, _ int) (*http.Client, error) {
			return &http.Client{
				Transport: offlineFixtureTransport{profile: profile, redditUserAgent: access.UserAgent()},
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
	profile         offlineFixtureProfile
	redditUserAgent string
}

func (transport offlineFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errUnexpectedOfflineFixtureRequest
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
	}

	if request.Method != http.MethodGet || request.Body != nil || request.Response != nil ||
		request.URL.Scheme != "https" || request.URL.Host != production.RedditOrigin ||
		request.URL.User != nil || request.URL.Fragment != "" ||
		request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" ||
		request.Header.Get("Content-Type") != "" ||
		request.Header.Get("Accept") != "application/json" ||
		request.UserAgent() != transport.redditUserAgent {
		return nil, errUnexpectedOfflineFixtureRequest
	}
	if transport.profile == offlineFixtureProfileSynthetic {
		return syntheticOfflineFixtureResponse(request)
	}
	if request.URL.Path != "/comments/duck123/.json" ||
		request.URL.Query().Encode() != "limit=500&raw_json=1&showmore=true&sort=confidence" {
		return nil, errUnexpectedOfflineFixtureRequest
	}
	return offlineFixtureResponse(
		request,
		"application/json",
		`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"duck123","name":"t3_duck123"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t1","data":{"id":"c1","name":"t1_c1","link_id":"t3_duck123","parent_id":"t3_duck123","body":"Duck duck water.","replies":""}}]}}]`,
	)
}

func syntheticOfflineFixtureResponse(request *http.Request) (*http.Response, error) {
	switch request.URL.Path {
	case "/comments/synth001/.json":
		switch request.URL.Query().Encode() {
		case "limit=500&raw_json=1&showmore=true&sort=confidence":
			return offlineFixtureResponse(request, "application/json", syntheticPostOneResponse)
		case "comment=a4&context=0&limit=500&raw_json=1&showmore=true&sort=confidence":
			return offlineFixtureResponse(request, "application/json", syntheticExpansionA4Response)
		case "comment=a5&context=0&limit=500&raw_json=1&showmore=true&sort=confidence":
			return offlineFixtureResponse(request, "application/json", syntheticExpansionA5Response)
		default:
			return nil, errUnexpectedOfflineFixtureRequest
		}
	case "/r/duck/comments/synth002/synthetic_fixture/.json":
		if request.URL.Query().Encode() != "limit=500&raw_json=1&showmore=true&sort=confidence" {
			return nil, errUnexpectedOfflineFixtureRequest
		}
		return offlineFixtureResponse(request, "application/json", syntheticPostTwoResponse)
	case "/comments/synth003/synthetic_fixture/.json":
		switch request.URL.Query().Encode() {
		case "limit=500&raw_json=1&showmore=true&sort=confidence":
			return offlineFixtureResponse(request, "application/json", syntheticPostThreeResponse)
		case "comment=d1&context=0&limit=500&raw_json=1&showmore=true&sort=confidence":
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
	for _, event := range []string{
		"event=run_started", "event=http_attempt", "result=success", "event=run_summary", "event=output_written",
		"access_profile=old-reddit-public-json-v1", "reddit_origin=old.reddit.com",
		"reddit_method=GET", "reddit_auth=none", "ua_source=", "ua_sha256=",
	} {
		if !strings.Contains(stderr.String(), event) {
			t.Fatalf("offline fixture stderr does not contain %q:\n%s", event, stderr.String())
		}
	}
	if got := strings.Count(stderr.String(), "event=http_attempt"); got != 1 {
		t.Fatalf("HTTP attempt event count = %d, want 1:\n%s", got, stderr.String())
	}
	access, err := production.ResolveAccessIdentity(os.LookupEnv)
	if err != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", err)
	}
	if got := strings.Count(stderr.String(), "ua_sha256="+access.UserAgentSHA256); got != 2 {
		t.Fatalf("safe User-Agent digest occurrence count = %d, want matching start and summary records", got)
	}
	if got := strings.Count(stderr.String(), "ua_source="+access.UserAgentSource); got != 2 {
		t.Fatalf("User-Agent source occurrence count = %d, want matching start and summary records", got)
	}
	if strings.Contains(stderr.String(), access.UserAgent()) {
		t.Fatal("offline fixture stderr contains the raw Reddit User-Agent")
	}
}

func TestOfflineFixtureTransportRejectsNonPublicRequestShapes(t *testing.T) {
	t.Parallel()

	access, err := production.ResolveAccessIdentity(os.LookupEnv)
	if err != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", err)
	}
	transport := offlineFixtureTransport{redditUserAgent: access.UserAgent()}
	newRequest := func(t *testing.T) *http.Request {
		t.Helper()
		request, requestErr := http.NewRequest(
			http.MethodGet,
			"https://old.reddit.com/comments/duck123/.json?limit=500&raw_json=1&showmore=true&sort=confidence",
			nil,
		)
		if requestErr != nil {
			t.Fatalf("http.NewRequest() error = %v", requestErr)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", access.UserAgent())
		return request
	}
	tests := []struct {
		name   string
		change func(*http.Request)
	}{
		{name: "wrong host", change: func(request *http.Request) { request.URL.Host = "oauth.reddit.com" }},
		{name: "post", change: func(request *http.Request) { request.Method = http.MethodPost }},
		{name: "body", change: func(request *http.Request) { request.Body = io.NopCloser(strings.NewReader("token=secret")) }},
		{name: "authorization", change: func(request *http.Request) { request.Header.Set("Authorization", "Bearer secret") }},
		{name: "cookie", change: func(request *http.Request) { request.Header.Set("Cookie", "session=secret") }},
		{name: "content type", change: func(request *http.Request) { request.Header.Set("Content-Type", "application/x-www-form-urlencoded") }},
		{name: "redirect follow", change: func(request *http.Request) { request.Response = &http.Response{StatusCode: http.StatusFound} }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := newRequest(t)
			test.change(request)
			response, requestErr := transport.RoundTrip(request)
			if response != nil || !errors.Is(requestErr, errUnexpectedOfflineFixtureRequest) {
				t.Fatalf("RoundTrip() response = %#v, error = %v", response, requestErr)
			}
		})
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
