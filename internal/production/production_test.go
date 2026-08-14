package production

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/acquire"
	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/logging"
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/runlog"
)

func TestExecuteProductionRejectsInvalidUserAgentBeforeHTTPOrSourceIO(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	sink := mustTestLogSink(t, &logs)
	httpCalls := 0
	result, err := executeProduction(context.Background(), productionTestConfig(), sink.Logger(), Dependencies{
		lookupEnv: mapLookup(map[string]string{envRedditUserAgent: ""}),
		newHTTP: func(time.Duration, int) (*http.Client, error) {
			httpCalls++
			return nil, errors.New("must not run")
		},
		now: fixedProductionClock(),
	})
	if !errors.Is(err, ErrRedditSetup) || httpCalls != 0 {
		t.Fatalf("result = %+v, error = %v, HTTP factory calls = %d", result, err, httpCalls)
	}
	if strings.Contains(logs.String(), "event=run_started") || !strings.Contains(logs.String(), "event=run_failed") ||
		strings.Contains(logs.String(), "REDDIT_USER_AGENT") {
		t.Fatalf("logs = %q, want production failure without duplicating the CLI-owned start event", logs.String())
	}
}

func TestExecuteProductionRejectsBothSourcePoliciesBeforeFilesystemIO(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "must-not-open")
	cfg := productionTestConfig()
	cfg.Posts = config.InputSource{Kind: config.SourceFile, Location: missing}
	cfg.Dictionary = config.InputSource{Kind: config.SourceURL, Location: "https://example.invalid/words.txt"}

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("network request occurred")
		return nil, nil
	}), Timeout: time.Second}
	result, err := executeProduction(context.Background(), cfg, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), Dependencies{
		lookupEnv: mapLookup(validProductionEnvironment()),
		newHTTP:   func(time.Duration, int) (*http.Client, error) { return client, nil },
		now:       fixedProductionClock(),
	})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("result = %+v, error = %v, want production config error", result, err)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("source path unexpectedly changed: %v", statErr)
	}
}

func TestExecuteProductionLocalSourcesFailOnlyAtFixedRedditEndpoint(t *testing.T) {
	postsFile := filepath.Join(t.TempDir(), "posts.txt")
	dictionaryFile := filepath.Join(t.TempDir(), "dictionary.txt")
	if err := os.WriteFile(postsFile, []byte("https://redd.it/duck123\n"), 0o600); err != nil {
		t.Fatalf("write posts: %v", err)
	}
	if err := os.WriteFile(dictionaryFile, []byte("duck\npond\nwater\n"), 0o600); err != nil {
		t.Fatalf("write dictionary: %v", err)
	}

	cfg := productionTestConfig()
	cfg.Posts = config.InputSource{Kind: config.SourceFile, Location: postsFile}
	cfg.Dictionary = config.InputSource{Kind: config.SourceFile, Location: dictionaryFile}
	cfg.MaxRetries = 0
	cfg.RateLimit = config.MaxRateLimit
	var logs strings.Builder
	sink := mustTestLogSink(t, &logs)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != RedditOrigin ||
			request.URL.Path != "/comments/duck123/.json" {
			t.Fatalf("unexpected network destination %s", request.URL.Redacted())
		}
		return nil, errors.New("injected offline transport failure")
	})
	client := &http.Client{Transport: transport, Timeout: time.Second}

	result, err := executeProduction(context.Background(), cfg, sink.Logger(), Dependencies{
		lookupEnv: mapLookup(validProductionEnvironment()),
		newHTTP:   func(time.Duration, int) (*http.Client, error) { return client, nil },
		now:       fixedProductionClock(),
	})
	if err == nil || len(result.Words) != 0 {
		t.Fatalf("result = %+v, error = %v, want fail-closed Reddit transport failure", result, err)
	}
	for _, want := range []string{"event=source_loaded", "source_kind=posts", "source_kind=dictionary", "source_mode=file", "source_origin=local-file", "event=post_outcome", "post_id=duck123", "event=run_failed"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs do not contain %q:\n%s", want, logs.String())
		}
	}
	for _, canary := range []string{postsFile, dictionaryFile, "injected offline transport failure"} {
		if strings.Contains(logs.String(), canary) || strings.Contains(err.Error(), canary) {
			t.Fatalf("secret %q leaked: error=%q logs=%q", canary, err, logs.String())
		}
	}
}

func TestExecuteProductionComposesOfflineEndToEnd(t *testing.T) {
	postsFile := filepath.Join(t.TempDir(), "posts.txt")
	dictionaryFile := filepath.Join(t.TempDir(), "dictionary.txt")
	if err := os.WriteFile(postsFile, []byte("https://redd.it/duck123\n"), 0o600); err != nil {
		t.Fatalf("write posts: %v", err)
	}
	if err := os.WriteFile(dictionaryFile, []byte("duck\npond\nwater\n"), 0o600); err != nil {
		t.Fatalf("write dictionary: %v", err)
	}

	const initial = `[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"duck123","name":"t3_duck123"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t1","data":{"id":"c1","name":"t1_c1","link_id":"t3_duck123","parent_id":"t3_duck123","body":"Duck duck water.","replies":""}}]}}]`
	var requests []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.Host+request.URL.Path)
		header := http.Header{"Content-Type": {"application/json"}}
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != RedditOrigin ||
			request.URL.Path != "/comments/duck123/.json" ||
			request.URL.Query().Encode() != "limit=500&raw_json=1&showmore=true&sort=confidence" ||
			request.Header.Get("Accept") != "application/json" ||
			request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" ||
			request.Body != nil || request.UserAgent() != validProductionEnvironment()[envRedditUserAgent] {
			t.Fatalf("unexpected public Reddit request %s", request.URL.Redacted())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(initial))}, nil
	})
	client := &http.Client{Transport: transport, Timeout: time.Second}
	access, accessErr := ResolveAccessIdentity(mapLookup(validProductionEnvironment()))
	if accessErr != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", accessErr)
	}
	lookupCalls := 0
	dependencies := Dependencies{
		lookupEnv: func(string) (string, bool) {
			lookupCalls++
			return "", false
		},
		newHTTP: func(time.Duration, int) (*http.Client, error) { return client, nil },
		now:     time.Now,
	}
	cfg := productionTestConfig()
	cfg.Posts = config.InputSource{Kind: config.SourceFile, Location: postsFile}
	cfg.Dictionary = config.InputSource{Kind: config.SourceFile, Location: dictionaryFile}
	cfg.RateLimit = config.MaxRateLimit
	var logs strings.Builder
	sink := mustTestLogSink(t, &logs)
	ctx := ContextWithAccessIdentity(context.Background(), access)
	result, err := executeProduction(ctx, cfg, sink.Logger(), dependencies)
	if err != nil {
		t.Fatalf("executeProduction() error = %v; logs:\n%s", err, logs.String())
	}
	if len(result.Words) != 2 || result.Words[0].Word != "duck" || result.Words[0].Count != 2 ||
		result.Words[1].Word != "water" || result.Words[1].Count != 1 {
		t.Fatalf("Words = %#v, want duck=2 and water=1", result.Words)
	}
	if result.Summary.Total != 1 || result.Summary.Completed != 1 || result.Summary.Comments != 1 || result.Summary.Partial {
		t.Fatalf("Summary = %#v, want one complete post", result.Summary)
	}
	if len(requests) != 1 || requests[0] != "old.reddit.com/comments/duck123/.json" {
		t.Fatalf("requests = %v, want one fixed public JSON endpoint", requests)
	}
	if lookupCalls != 0 {
		t.Fatalf("environment lookup calls = %d, want identity resolved exactly once before composition", lookupCalls)
	}
	for _, want := range []string{
		"event=source_parsed", "event=post_outcome", "status=completed", "event=run_summary",
		"source_retries=0", "reddit_http_attempts=1", "reddit_retries=0",
		"access_profile=old-reddit-public-json-v1", "reddit_origin=old.reddit.com",
		"reddit_method=GET", "reddit_auth=none", "ua_source=override", "ua_sha256=",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs do not contain %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), validProductionEnvironment()[envRedditUserAgent]) {
		t.Fatalf("logs leaked raw User-Agent: %s", logs.String())
	}
}

func TestExecuteProductionLeavesTerminalCancellationLoggingToCLI(t *testing.T) {
	postsFile := filepath.Join(t.TempDir(), "posts.txt")
	dictionaryFile := filepath.Join(t.TempDir(), "dictionary.txt")
	if err := os.WriteFile(postsFile, []byte("https://redd.it/duck123\n"), 0o600); err != nil {
		t.Fatalf("write posts: %v", err)
	}
	if err := os.WriteFile(dictionaryFile, []byte("duck\npond\nwater\n"), 0o600); err != nil {
		t.Fatalf("write dictionary: %v", err)
	}

	cfg := productionTestConfig()
	cfg.Posts = config.InputSource{Kind: config.SourceFile, Location: postsFile}
	cfg.Dictionary = config.InputSource{Kind: config.SourceFile, Location: dictionaryFile}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	}), Timeout: time.Second}

	var logs strings.Builder
	sink := mustTestLogSink(t, &logs)
	_, err := executeProduction(ctx, cfg, sink.Logger(), Dependencies{
		lookupEnv: mapLookup(validProductionEnvironment()),
		newHTTP:   func(time.Duration, int) (*http.Client, error) { return client, nil },
		now:       fixedProductionClock(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeProduction() error = %v, want context cancellation", err)
	}
	if strings.Contains(logs.String(), "event=run_failed") || strings.Contains(logs.String(), "event=run_cancelled") {
		t.Fatalf("production emitted a terminal cancellation event owned by the CLI: %q", logs.String())
	}
}

func TestExecuteProductionRejectsInvalidDependencies(t *testing.T) {
	t.Parallel()

	dependencies := defaultDependencies()
	tests := []struct {
		name   string
		ctx    context.Context
		logger *slog.Logger
		change func(*Dependencies)
	}{
		{name: "nil context", logger: slog.Default()},
		{name: "nil logger", ctx: context.Background()},
		{name: "nil environment", ctx: context.Background(), logger: slog.Default(), change: func(deps *Dependencies) { deps.lookupEnv = nil }},
		{name: "nil HTTP factory", ctx: context.Background(), logger: slog.Default(), change: func(deps *Dependencies) { deps.newHTTP = nil }},
		{name: "nil clock", ctx: context.Background(), logger: slog.Default(), change: func(deps *Dependencies) { deps.now = nil }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			deps := dependencies
			if test.change != nil {
				test.change(&deps)
			}
			result, err := executeProduction(test.ctx, productionTestConfig(), test.logger, deps)
			if result.Words != nil || !errors.Is(err, ErrConfig) {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestProductionSourceRetryConfigClampsCLIControls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		maxRetries  int
		retryBudget time.Duration
		wantRetries int
		wantBudget  time.Duration
	}{
		{name: "explicit opt-out", maxRetries: 0, retryBudget: time.Second, wantRetries: 0, wantBudget: time.Second},
		{name: "within source bounds", maxRetries: 1, retryBudget: 10 * time.Second, wantRetries: 1, wantBudget: 10 * time.Second},
		{name: "source ceilings", maxRetries: config.MaxMaxRetries, retryBudget: config.MaxRetryBudget, wantRetries: 2, wantBudget: 15 * time.Second},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := productionTestConfig()
			cfg.MaxRetries = test.maxRetries
			cfg.RetryBudget = test.retryBudget
			observed := false
			got := productionSourceRetryConfig(cfg, func(acquire.RetryEvent) { observed = true })
			if got.MaxRetries != test.wantRetries || got.MaxElapsed != test.wantBudget || got.Observer == nil {
				t.Fatalf("productionSourceRetryConfig() = %+v; want retries=%d budget=%s observer", got, test.wantRetries, test.wantBudget)
			}
			got.Observer(acquire.RetryEvent{})
			if !observed {
				t.Fatal("productionSourceRetryConfig() did not preserve observer")
			}
		})
	}
}

func TestSourceRetryLoggingIsSanitizedAndCountedInSummary(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	log := runlog.New(mustTestLogSink(t, &logs).Logger())
	log.SourceRetryObserver(context.Background())(acquire.RetryEvent{
		Kind:       acquire.KindPosts,
		Reason:     acquire.RetryReasonHTTPStatus,
		StatusCode: http.StatusServiceUnavailable,
		Attempt:    2,
		Delay:      250 * time.Millisecond,
	})
	log.Summary(
		context.Background(),
		productionTestConfig(),
		runlogTestAccessIdentity(),
		app.Result{},
		reddit.RequestPolicySnapshot{HTTPAttempts: 7, Retries: 2},
		"posts-digest",
		"post-ids-digest",
		"dictionary-digest",
		42,
		time.Second,
		false,
	)

	for _, want := range []string{
		"event=request_retry",
		"operation=source_download",
		"source_kind=posts",
		"error_class=http_status",
		"http_status=503",
		"attempt=2",
		"delay=250ms",
		"event=run_summary",
		"terminal_status=complete",
		"failure_mode=best-effort",
		"workers=1",
		"input_profile=assignment-default-v1",
		"filter_count=0",
		"dictionary_words=42",
		"source_retries=1",
		"reddit_http_attempts=7",
		"reddit_retries=2",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs do not contain %q:\n%s", want, logs.String())
		}
	}
}

func productionTestConfig() config.Config {
	return config.Config{
		Posts:          config.InputSource{Kind: config.SourceURL, Location: config.DefaultPostsURL},
		Dictionary:     config.InputSource{Kind: config.SourceURL, Location: config.DefaultDictionaryURL},
		Workers:        1,
		RateLimit:      config.DefaultRateLimit,
		RequestTimeout: time.Second,
		Timeout:        time.Minute,
		MaxRetries:     0,
		RetryBudget:    time.Second,
		FailureMode:    config.FailureModeBestEffort,
		LogLevel:       logging.LevelInfo,
		LogFormat:      logging.FormatText,
	}
}

func validProductionEnvironment() map[string]string {
	return map[string]string{
		envRedditUserAgent: "duckwords-test/1.0 (+https://github.com/pointerm/duckwords)",
	}
}

func runlogTestAccessIdentity() runlog.AccessIdentity {
	access, err := ResolveAccessIdentity(mapLookup(validProductionEnvironment()))
	if err != nil {
		panic(err)
	}
	return runlogAccessIdentity(access)
}

func fixedProductionClock() func() time.Time {
	return func() time.Time { return time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC) }
}

func mustTestLogSink(t *testing.T, output *strings.Builder) *logging.Sink {
	t.Helper()
	sink, err := logging.New(output, logging.Options{
		Format: logging.FormatText,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	return sink
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
