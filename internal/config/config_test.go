package config

import (
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/words"
)

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Workers != defaultWorkers {
		t.Fatalf("Workers = %d, want %d", cfg.Workers, defaultWorkers)
	}
	if cfg.FailureMode != FailureModeBestEffort {
		t.Fatalf("FailureMode = %q, want %q", cfg.FailureMode, FailureModeBestEffort)
	}
	if cfg.Timeout != defaultTimeout {
		t.Fatalf("Timeout = %s, want %s", cfg.Timeout, defaultTimeout)
	}
	if cfg.Posts != (InputSource{Kind: SourceURL, Location: DefaultPostsURL}) {
		t.Fatalf("Posts = %+v, want supplied URL", cfg.Posts)
	}
	if cfg.Dictionary != (InputSource{Kind: SourceURL, Location: DefaultDictionaryURL}) {
		t.Fatalf("Dictionary = %+v, want supplied URL", cfg.Dictionary)
	}
	if cfg.RateLimit != DefaultRateLimit {
		t.Fatalf("RateLimit = %v, want %v", cfg.RateLimit, DefaultRateLimit)
	}
	if cfg.RequestTimeout != DefaultRequestTimeout {
		t.Fatalf("RequestTimeout = %s, want %s", cfg.RequestTimeout, DefaultRequestTimeout)
	}
	if cfg.MaxRetries != DefaultMaxRetries {
		t.Fatalf("MaxRetries = %d, want %d", cfg.MaxRetries, DefaultMaxRetries)
	}
	if cfg.RetryBudget != DefaultRetryBudget {
		t.Fatalf("RetryBudget = %s, want %s", cfg.RetryBudget, DefaultRetryBudget)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "text" {
		t.Fatalf("log config = %s/%s, want info/text", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.Filters != nil {
		t.Fatalf("Filters = %#v, want nil", cfg.Filters)
	}
	if cfg.ShowVersion {
		t.Fatal("ShowVersion = true, want false")
	}
	if !cfg.RunRequested {
		t.Fatal("RunRequested = false; a bare invocation must process the assignment inputs")
	}
}

func TestParseOptions(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]string{
		"--posts-file=testdata/posts.txt",
		"--dictionary-file", "testdata/words.txt",
		"--workers=12",
		"--rate-limit=1.25",
		"--request-timeout=15s",
		"--failure-mode=strict",
		"--timeout=45m",
		"--max-retries=4",
		"--retry-budget=1m",
		"--log-level=debug",
		"--log-format=json",
		"--filter=DUCK*",
		"--filter", "water",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.Workers != 12 || cfg.FailureMode != FailureModeStrict || cfg.Timeout != 45*time.Minute {
		t.Fatalf("Config = %+v, want workers=12 mode=strict timeout=45m", cfg)
	}
	if cfg.Posts != (InputSource{Kind: SourceFile, Location: "testdata/posts.txt"}) {
		t.Fatalf("Posts = %+v, want local file", cfg.Posts)
	}
	if cfg.Dictionary != (InputSource{Kind: SourceFile, Location: "testdata/words.txt"}) {
		t.Fatalf("Dictionary = %+v, want local file", cfg.Dictionary)
	}
	if cfg.RateLimit != 1.25 || cfg.RequestTimeout != 15*time.Second || cfg.MaxRetries != 4 || cfg.RetryBudget != time.Minute {
		t.Fatalf("request policy = %+v, want explicit values", cfg)
	}
	if cfg.LogLevel != "debug" || cfg.LogFormat != "json" {
		t.Fatalf("log config = %s/%s, want debug/json", cfg.LogLevel, cfg.LogFormat)
	}
	if !reflect.DeepEqual(cfg.Filters, []string{"DUCK*", "water"}) {
		t.Fatalf("Filters = %#v, want repeated filters in argument order", cfg.Filters)
	}
	if !cfg.RunRequested {
		t.Fatal("RunRequested = false, want true")
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]string{"--version"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.ShowVersion {
		t.Fatal("Parse() did not enable ShowVersion")
	}
	if cfg.RunRequested {
		t.Fatal("--version unexpectedly requested execution")
	}
}

func TestParseRejectsVersionWithProcessingOptions(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"--version", "--workers=4"})
	if !errors.Is(err, ErrConflictingMode) {
		t.Fatalf("Parse() error = %v, want ErrConflictingMode", err)
	}
}

func TestParseHelp(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"--help"})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Parse() error = %v, want flag.ErrHelp", err)
	}
}

func TestParseRejectsInvalidWorkers(t *testing.T) {
	t.Parallel()

	for _, workers := range []string{"0", "-1", "33"} {
		workers := workers
		t.Run(workers, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]string{"--workers=" + workers})
			if !errors.Is(err, ErrWorkersOutOfRange) {
				t.Fatalf("Parse() error = %v, want ErrWorkersOutOfRange", err)
			}
		})
	}
}

func TestParseAcceptsWorkerBounds(t *testing.T) {
	t.Parallel()

	for _, workers := range []string{"1", "32"} {
		workers := workers
		t.Run(workers, func(t *testing.T) {
			t.Parallel()

			if _, err := Parse([]string{"--workers=" + workers}); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseRejectsInvalidFailureMode(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"--failure-mode=continue"})
	if !errors.Is(err, ErrInvalidFailureMode) {
		t.Fatalf("Parse() error = %v, want ErrInvalidFailureMode", err)
	}
}

func TestParseRejectsInvalidTimeout(t *testing.T) {
	t.Parallel()

	for _, timeout := range []string{"0", "-1s", "999ms", "2h1ns"} {
		timeout := timeout
		t.Run(timeout, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]string{"--timeout=" + timeout})
			if !errors.Is(err, ErrTimeoutOutOfRange) {
				t.Fatalf("Parse() error = %v, want ErrTimeoutOutOfRange", err)
			}
		})
	}
}

func TestParseAcceptsTimeoutBounds(t *testing.T) {
	t.Parallel()

	for _, timeout := range []string{"1s", "2h"} {
		timeout := timeout
		t.Run(timeout, func(t *testing.T) {
			t.Parallel()

			if _, err := Parse([]string{"--timeout=" + timeout}); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseRejectsMalformedDuration(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"--timeout=forever"})
	if err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("Parse() error = %v, want duration parse error", err)
	}
}

func TestParseRejectsInvalidFilterBeforeExecution(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"--filter=duck?"})
	if !errors.Is(err, words.ErrInvalidPattern) {
		t.Fatalf("Parse() error = %v, want words.ErrInvalidPattern", err)
	}
	if !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("Parse() error = %v, want ErrInvalidFilter", err)
	}
}

func TestParseRejectsUnknownFlag(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"--unknown"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("Parse() error = %v, want unknown-flag error", err)
	}
}

func TestParseRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	_, err := Parse([]string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("Parse() error = %v, want positional-argument error", err)
	}
}

func TestParseSelectsSourcesAndRequestsExecution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		wantPosts InputSource
		wantWords InputSource
	}{
		{
			name:      "explicit post URL",
			args:      []string{"--posts-url=https://gist.githubusercontent.com/example/raw/posts.txt"},
			wantPosts: InputSource{Kind: SourceURL, Location: "https://gist.githubusercontent.com/example/raw/posts.txt"},
			wantWords: InputSource{Kind: SourceURL, Location: DefaultDictionaryURL},
		},
		{
			name:      "explicit dictionary URL",
			args:      []string{"--dictionary-url=https://raw.githubusercontent.com/example/words.txt"},
			wantPosts: InputSource{Kind: SourceURL, Location: DefaultPostsURL},
			wantWords: InputSource{Kind: SourceURL, Location: "https://raw.githubusercontent.com/example/words.txt"},
		},
		{
			name:      "local files",
			args:      []string{"--posts-file=posts.txt", "--dictionary-file=words.txt"},
			wantPosts: InputSource{Kind: SourceFile, Location: "posts.txt"},
			wantWords: InputSource{Kind: SourceFile, Location: "words.txt"},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Parse(test.args)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if cfg.Posts != test.wantPosts || cfg.Dictionary != test.wantWords {
				t.Fatalf("sources = %+v / %+v, want %+v / %+v", cfg.Posts, cfg.Dictionary, test.wantPosts, test.wantWords)
			}
			if !cfg.RunRequested {
				t.Fatal("explicit source option did not request execution")
			}
		})
	}
}

func TestParseRejectsConflictingSources(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"--posts-url=https://example.test/posts.txt", "--posts-file=posts.txt"},
		{"--dictionary-url=https://example.test/words.txt", "--dictionary-file=words.txt"},
	}
	for _, args := range tests {
		args := args
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()

			_, err := Parse(args)
			if !errors.Is(err, ErrConflictingSources) {
				t.Fatalf("Parse() error = %v, want ErrConflictingSources", err)
			}
		})
	}
}

func TestParseRejectsDuplicateSourceOptionWithoutEchoingValue(t *testing.T) {
	t.Parallel()

	const plantedValue = "https://example.test/planted-sensitive-location"
	_, err := Parse([]string{
		"--posts-url=https://example.test/posts.txt",
		"--posts-url=" + plantedValue,
	})
	if !errors.Is(err, ErrDuplicateOption) {
		t.Fatalf("Parse() error = %v, want ErrDuplicateOption", err)
	}
	if strings.Contains(err.Error(), plantedValue) {
		t.Fatalf("Parse() error exposed source value: %v", err)
	}
}

func TestParseRejectsInvalidSourcesWithoutEchoingValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "empty URL", args: []string{"--posts-url="}},
		{name: "HTTP URL", args: []string{"--posts-url=http://example.test/planted-location"}},
		{name: "URL credentials", args: []string{"--dictionary-url=https://planted:secret@example.test/words.txt"}},
		{name: "URL fragment", args: []string{"--posts-url=https://example.test/posts.txt#planted-location"}},
		{name: "URL port", args: []string{"--dictionary-url=https://example.test:443/words.txt"}},
		{name: "escaped URL path", args: []string{"--dictionary-url=https://example.test/%77ords.txt"}},
		{name: "surrounding URL whitespace", args: []string{"--posts-url= https://example.test/posts.txt"}},
		{name: "empty file", args: []string{"--dictionary-file=  "}},
		{name: "padded file", args: []string{"--dictionary-file= planted-path "}},
		{name: "NUL file", args: []string{"--posts-file=planted\x00path"}},
		{name: "long file", args: []string{"--posts-file=" + strings.Repeat("p", 4<<10+1)}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(test.args)
			if !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("Parse() error = %v, want ErrInvalidSource", err)
			}
			if strings.Contains(err.Error(), "planted") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("Parse() error exposed source value: %v", err)
			}
		})
	}
}

func TestParseValidatesRateLimit(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"NaN", "+Inf", "-Inf", "0", "0.09", "1.51", "10"} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]string{"--rate-limit=" + value})
			if !errors.Is(err, ErrRateLimitOutOfRange) {
				t.Fatalf("Parse() error = %v, want ErrRateLimitOutOfRange", err)
			}
		})
	}
	for _, value := range []string{"0.1", "1.5"} {
		value := value
		t.Run("bound_"+value, func(t *testing.T) {
			t.Parallel()

			if _, err := Parse([]string{"--rate-limit=" + value}); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseValidatesRequestTimeout(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"0", "999ms", "2m1ns"} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]string{"--request-timeout=" + value})
			if !errors.Is(err, ErrRequestTimeoutOutOfRange) {
				t.Fatalf("Parse() error = %v, want ErrRequestTimeoutOutOfRange", err)
			}
		})
	}
	for _, value := range []string{"1s", "2m"} {
		value := value
		t.Run("bound_"+value, func(t *testing.T) {
			t.Parallel()

			if _, err := Parse([]string{"--request-timeout=" + value}); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseValidatesRetryPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr error
	}{
		{name: "negative retries", args: []string{"--max-retries=-1"}, wantErr: ErrMaxRetriesOutOfRange},
		{name: "excess retries", args: []string{"--max-retries=6"}, wantErr: ErrMaxRetriesOutOfRange},
		{name: "zero budget", args: []string{"--retry-budget=0"}, wantErr: ErrRetryBudgetOutOfRange},
		{name: "short budget", args: []string{"--retry-budget=999ms"}, wantErr: ErrRetryBudgetOutOfRange},
		{name: "long budget", args: []string{"--retry-budget=5m1ns"}, wantErr: ErrRetryBudgetOutOfRange},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(test.args)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Parse() error = %v, want %v", err, test.wantErr)
			}
		})
	}

	for _, args := range [][]string{
		{"--max-retries=0", "--retry-budget=1s"},
		{"--max-retries=5", "--retry-budget=5m"},
	} {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()

			if _, err := Parse(args); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseValidatesLogging(t *testing.T) {
	t.Parallel()

	for _, level := range []string{"debug", "info", "warn", "error"} {
		level := level
		t.Run("level_"+level, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]string{"--log-level=" + level}); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
	for _, format := range []string{"text", "json"} {
		format := format
		t.Run("format_"+format, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]string{"--log-format=" + format}); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}

	if _, err := Parse([]string{"--log-level=trace"}); !errors.Is(err, ErrInvalidLogLevel) {
		t.Fatalf("Parse() error = %v, want ErrInvalidLogLevel", err)
	}
	if _, err := Parse([]string{"--log-format=yaml"}); !errors.Is(err, ErrInvalidLogFormat) {
		t.Fatalf("Parse() error = %v, want ErrInvalidLogFormat", err)
	}
}

func TestWriteUsage(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	if err := WriteUsage(&output); err != nil {
		t.Fatalf("WriteUsage() error = %v", err)
	}
	for _, want := range []string{
		"Usage:",
		"--posts-url",
		"--posts-file",
		"--dictionary-url",
		"--dictionary-file",
		"--workers",
		"--rate-limit",
		"--request-timeout",
		"--failure-mode",
		"--timeout",
		"--max-retries",
		"--retry-budget",
		"--filter",
		"--log-level",
		"--log-format",
		"--version",
		"--help",
		"REDDIT_USER_AGENT",
		"REDDIT_BROWSER_COOKIE",
		"REDDIT_BROWSER_ACCEPT_LANGUAGE",
		"REDDIT_BROWSER_SEC_CH_UA",
		"REDDIT_BROWSER_SEC_CH_UA_MOBILE",
		"REDDIT_BROWSER_SEC_CH_UA_PLATFORM",
		"--filter 'duck*'",
		"gist.githubusercontent.com",
		"raw.githubusercontent.com",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("usage does not contain %q:\n%s", want, output.String())
		}
	}
	for _, legacy := range []string{"REDDIT_API_ACCESS_APPROVED", "REDDIT_CLIENT_ID", "REDDIT_CLIENT_SECRET"} {
		if strings.Contains(output.String(), legacy) {
			t.Fatalf("usage still advertises legacy variable %q:\n%s", legacy, output.String())
		}
	}
}

// TestParseRejectsSourceURLOutsideOriginAllowlist keeps the acquisition host policy
// visible at the configuration boundary rather than deferring it to acquisition.
func TestParseRejectsSourceURLOutsideOriginAllowlist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		host string
	}{
		{name: "posts", args: []string{"--posts-url=https://example.test/posts.txt"}, host: "gist.githubusercontent.com"},
		{name: "dictionary", args: []string{"--dictionary-url=https://example.test/words.txt"}, host: "raw.githubusercontent.com"},
		{name: "posts host swapped", args: []string{"--posts-url=https://raw.githubusercontent.com/a/posts.txt"}, host: "gist.githubusercontent.com"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(test.args)
			if !errors.Is(err, ErrSourceHostNotAllowed) {
				t.Fatalf("Parse() error = %v, want ErrSourceHostNotAllowed", err)
			}
			if !strings.Contains(err.Error(), test.host) {
				t.Fatalf("error %q does not name the allowed host %q", err, test.host)
			}
		})
	}
}

// TestParseSeparatesHostFromPathRejection keeps the two source-URL failures distinct.
// Reporting a path problem as a wrong host sends the reader to the one part of the
// URL that was already correct.
func TestParseSeparatesHostFromPathRejection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want error
	}{
		{name: "wrong host", args: []string{"--posts-url=https://example.test/posts.txt"}, want: ErrSourceHostNotAllowed},
		{name: "right host trailing slash", args: []string{"--posts-url=https://gist.githubusercontent.com/a/b/"}, want: ErrSourcePathNotSupported},
		{name: "right host empty segment", args: []string{"--posts-url=https://gist.githubusercontent.com/a//b.txt"}, want: ErrSourcePathNotSupported},
		{name: "right host relative segment", args: []string{"--posts-url=https://gist.githubusercontent.com/a/../b.txt"}, want: ErrSourcePathNotSupported},
		{name: "dictionary wrong host", args: []string{"--dictionary-url=https://gist.githubusercontent.com/a/w.txt"}, want: ErrSourceHostNotAllowed},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(test.args)
			if !errors.Is(err, test.want) {
				t.Fatalf("Parse() error = %v, want %v", err, test.want)
			}
			if errors.Is(err, ErrSourcePathNotSupported) && errors.Is(err, ErrSourceHostNotAllowed) {
				t.Fatalf("Parse() error is both a host and a path rejection: %v", err)
			}
		})
	}
}

// TestParseAcceptsQueryOnAllowedHost covers raw-gist links whose query is part of the
// resource identity.
func TestParseAcceptsQueryOnAllowedHost(t *testing.T) {
	t.Parallel()

	const raw = "https://gist.githubusercontent.com/owner/id/raw/posts.txt?token=value"
	cfg, err := Parse([]string{"--posts-url=" + raw})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Posts.Location != raw {
		t.Fatalf("Posts.Location = %q, want the query preserved", cfg.Posts.Location)
	}
}
