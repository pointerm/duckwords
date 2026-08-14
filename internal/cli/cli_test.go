package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/logging"
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/runlog"
)

func TestRunInformationalPathsDoNotExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "help", args: []string{"--help"}, want: "Usage:"},
		{name: "short help", args: []string{"-h"}, want: "Usage:"},
		{name: "version", args: []string{"--version"}, want: "duckwords version="},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder
			code := run(
				context.Background(),
				test.args,
				&stdout,
				&stderr,
				func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
					t.Fatal("executor was called")
					return app.Result{}, nil
				},
			)

			if code != exitSuccess {
				t.Fatalf("run() exit code = %d, want %d", code, exitSuccess)
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("stdout does not contain %q:\n%s", test.want, stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunInformationalPathsDoNotReadBrowserSession(t *testing.T) {
	t.Setenv("REDDIT_BROWSER_COOKIE", "invalid browser session that has no equals sign")

	for _, arguments := range [][]string{{"--help"}, {"--version"}} {
		var stdout, stderr strings.Builder
		code := run(
			context.Background(),
			arguments,
			&stdout,
			&stderr,
			func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
				t.Fatal("executor was called on an informational path")
				return app.Result{}, nil
			},
		)
		if code != exitSuccess || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("run(%v) = code %d, stdout %q, stderr %q", arguments, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunRejectsInvalidArgumentsBeforeExecution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args       []string
		diagnostic string
	}{
		{args: []string{"--unknown"}, diagnostic: "invalid command-line arguments; use --help"},
		{args: []string{"--workers=0"}, diagnostic: "invalid configuration: workers must be between 1 and 32"},
		{args: []string{"--failure-mode=continue"}, diagnostic: "invalid configuration: failure mode must be best-effort or strict"},
		{args: []string{"--timeout=0"}, diagnostic: "invalid configuration: timeout must be between 1s and 2h"},
		{args: []string{"--filter=duck?"}, diagnostic: "invalid configuration: filter must use only letters and '*' within documented limits"},
		{args: []string{"--posts-url=https://example.test/posts", "--posts-file=posts.txt"}, diagnostic: "invalid configuration: choose one URL or file for each input"},
		{args: []string{"--rate-limit=NaN"}, diagnostic: "invalid configuration: rate limit must be between 0.1 and 1.5 requests per second"},
		{args: []string{"--request-timeout=0"}, diagnostic: "invalid configuration: request timeout must be between 1s and 2m"},
		{args: []string{"--max-retries=99"}, diagnostic: "invalid configuration: max retries must be between 0 and 5"},
		{args: []string{"--retry-budget=0"}, diagnostic: "invalid configuration: retry budget must be between 1s and 5m"},
		{args: []string{"--log-level=trace"}, diagnostic: "invalid configuration: log level must be debug, info, warn, or error"},
		{args: []string{"--log-format=yaml"}, diagnostic: "invalid configuration: log format must be text or json"},
		{args: []string{"--version", "--workers=4"}, diagnostic: "invalid configuration: --version cannot be combined with processing options"},
	}
	for _, test := range tests {
		test := test
		t.Run(strings.Join(test.args, "_"), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder
			code := run(
				context.Background(),
				test.args,
				&stdout,
				&stderr,
				func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
					t.Fatal("executor was called")
					return app.Result{}, nil
				},
			)

			if code != exitUsage {
				t.Fatalf("run() exit code = %d, want %d", code, exitUsage)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if stderr.String() != "duckwords: "+test.diagnostic+"\n" {
				t.Fatalf("stderr = %q, want stable configuration diagnostic", stderr.String())
			}
		})
	}
}

func TestRunPassesValidatedConfigAndTimeoutContext(t *testing.T) {
	t.Parallel()

	var got config.Config
	executorCalled := false
	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{
			"--workers=4",
			"--failure-mode=strict",
			"--timeout=1m",
			"--filter=duck*",
			"--filter=water",
		},
		&stdout,
		&stderr,
		func(ctx context.Context, cfg config.Config, _ *slog.Logger) (app.Result, error) {
			executorCalled = true
			got = cfg
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("execution context has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > time.Minute {
				t.Fatalf("execution deadline remaining = %s, want (0, 1m]", remaining)
			}
			return app.Result{}, nil
		},
	)

	if code != exitSuccess {
		t.Fatalf("run() exit code = %d, want %d; stderr=%q", code, exitSuccess, stderr.String())
	}
	if !executorCalled {
		t.Fatal("executor was not called")
	}
	if got.Workers != 4 || got.FailureMode != config.FailureModeStrict || got.Timeout != time.Minute {
		t.Fatalf("executor config = %+v", got)
	}
	if strings.Join(got.Filters, ",") != "duck*,water" {
		t.Fatalf("Filters = %#v, want argument order", got.Filters)
	}
	if stdout.String() != "[]\n" {
		t.Fatalf("stdout = %q, want empty JSON array", stdout.String())
	}
	for _, want := range []string{
		"event=run_started",
		"workers=4",
		"source_max_retries=2",
		"source_retry_budget=15s",
		"max_distinct_words_per_post=50000",
		"max_in_flight_response_bytes=33554432",
		"max_retained_things=500000",
		"event=output_written",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want structured lifecycle field %q", stderr.String(), want)
		}
	}
}

func TestCLIAndApplicationWorkerDefaultsStayAligned(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatalf("config.Parse() error = %v", err)
	}
	if cfg.Workers != app.DefaultWorkers {
		t.Fatalf("CLI workers = %d, application default = %d", cfg.Workers, app.DefaultWorkers)
	}
}

func TestRunSuccessWritesOneCompletePrettyJSONDocument(t *testing.T) {
	t.Parallel()

	stdout := &recordingWriter{}
	var stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1"},
		stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			return app.Result{Words: []aggregate.WordCount{
				{Word: "duck", Count: 7},
				{Word: "water", Count: 2},
			}}, nil
		},
	)

	const want = "[\n  {\n    \"word\": \"duck\",\n    \"count\": 7\n  },\n  {\n    \"word\": \"water\",\n    \"count\": 2\n  }\n]\n"
	if code != exitSuccess {
		t.Fatalf("run() exit code = %d, want %d", code, exitSuccess)
	}
	if stdout.writes != 1 {
		t.Fatalf("stdout Write calls = %d, want 1", stdout.writes)
	}
	if stdout.String() != want {
		t.Fatalf("stdout mismatch:\ngot:\n%s\nwant:\n%s", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "event=run_started") || !strings.Contains(stderr.String(), "event=output_written") {
		t.Fatalf("stderr = %q, want structured start and output lifecycle", stderr.String())
	}
	if wantHash := fmt.Sprintf("result_sha256=%x", sha256.Sum256([]byte(want))); !strings.Contains(stderr.String(), wantHash) {
		t.Fatalf("stderr = %q, want exact output digest %q", stderr.String(), wantHash)
	}
}

func TestRunPartialWritesJSONAndReturnsPartialExit(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--failure-mode=best-effort"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			return app.Result{Words: []aggregate.WordCount{{Word: "duck", Count: 1}}}, app.ErrPartialResult
		},
	)

	if code != exitPartial {
		t.Fatalf("run() exit code = %d, want %d", code, exitPartial)
	}
	if !strings.Contains(stdout.String(), `"word": "duck"`) {
		t.Fatalf("stdout does not contain result: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "event=run_started") ||
		!strings.Contains(stderr.String(), "event=output_written") ||
		!strings.HasSuffix(stderr.String(), "duckwords: processing completed with a partial result\n") {
		t.Fatalf("stderr = %q, want lifecycle and partial diagnostic", stderr.String())
	}
}

func TestRunPassesStructuredLoggerWithSelectedFormat(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1", "--log-format=json", "--log-level=debug"},
		&stdout,
		&stderr,
		func(ctx context.Context, _ config.Config, logger *slog.Logger) (app.Result, error) {
			if logger == nil {
				t.Fatal("executor received nil logger")
			}
			logger.DebugContext(ctx, "executor diagnostic", "event", "executor_test")
			return app.Result{}, nil
		},
	)
	if code != exitSuccess {
		t.Fatalf("run() exit code = %d, stderr=%q", code, stderr.String())
	}
	for _, want := range []string{`"event":"run_started"`, `"event":"executor_test"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("JSON log %q does not contain %q", stderr.String(), want)
		}
	}
	if stdout.String() != "[]\n" {
		t.Fatalf("stdout = %q, want result JSON only", stdout.String())
	}
}

func TestRunPartialJSONLogRemainsNDJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--failure-mode=best-effort", "--log-format=json"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			return app.Result{Words: []aggregate.WordCount{{Word: "duck", Count: 1}}}, app.ErrPartialResult
		},
	)

	if code != exitPartial {
		t.Fatalf("run() exit code = %d, want %d", code, exitPartial)
	}
	records := decodeJSONLogRecords(t, stderr.String())
	if got := records[len(records)-1]["event"]; got != "output_written" {
		t.Fatalf("terminal event = %v, want output_written; log=%q", got, stderr.String())
	}
	if got := records[len(records)-1]["partial"]; got != true {
		t.Fatalf("terminal partial = %v, want true", got)
	}
}

func TestRunFatalJSONLogNeverAppendsPlainDiagnostic(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1", "--log-format=json"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			return app.Result{}, errors.New("injected fatal error")
		},
	)

	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	decodeJSONLogRecords(t, stderr.String())
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunCancellationJSONLogRemainsNDJSON(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr strings.Builder
	code := run(
		parent,
		[]string{"--workers=1", "--log-format=json"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called")
			return app.Result{}, nil
		},
	)

	if code != exitInterrupted {
		t.Fatalf("run() exit code = %d, want %d", code, exitInterrupted)
	}
	records := decodeJSONLogRecords(t, stderr.String())
	if got := records[len(records)-1]["event"]; got != "run_cancelled" {
		t.Fatalf("terminal event = %v, want run_cancelled; log=%q", got, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunOutputFailureJSONLogRemainsNDJSON(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1", "--log-format=json"},
		errorWriter{},
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			return app.Result{Words: []aggregate.WordCount{{Word: "duck", Count: 1}}}, nil
		},
	)

	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	records := decodeJSONLogRecords(t, stderr.String())
	if got := records[len(records)-1]["event"]; got != "run_failed" {
		t.Fatalf("terminal event = %v, want run_failed; log=%q", got, stderr.String())
	}
	if got := records[len(records)-1]["error_class"]; got != "output" {
		t.Fatalf("terminal error_class = %v, want output", got)
	}
}

func TestRunFatalErrorsNeverWriteStdout(t *testing.T) {
	t.Parallel()

	for _, executionErr := range []error{
		errors.New("fatal dependency error"),
		app.ErrStrictFailure,
		app.ErrNoCompletedPosts,
		errors.Join(errors.New("fatal dependency error"), app.ErrPartialResult),
		context.Canceled,
		context.DeadlineExceeded,
	} {
		executionErr := executionErr
		t.Run(executionErr.Error(), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder
			code := run(
				context.Background(),
				[]string{"--workers=1"},
				&stdout,
				&stderr,
				func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
					return app.Result{Words: []aggregate.WordCount{{Word: "must-not-leak", Count: 1}}}, executionErr
				},
			)

			if code != exitFailure {
				t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "event=run_started") ||
				!strings.HasSuffix(stderr.String(), "duckwords: execution failed\n") {
				t.Fatalf("stderr = %q, want lifecycle and stable fatal diagnostic", stderr.String())
			}
		})
	}
}

func TestRunCanceledParentMapsToInterruptedWithoutExecution(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr strings.Builder
	code := run(
		parent,
		[]string{"--workers=1"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called")
			return app.Result{}, nil
		},
	)

	if code != exitInterrupted {
		t.Fatalf("run() exit code = %d, want %d", code, exitInterrupted)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.String() != "duckwords: interrupted\n" {
		t.Fatalf("stderr = %q, want interrupt diagnostic", stderr.String())
	}
}

func TestRunCancellationDuringExecutionMapsToInterrupted(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr strings.Builder
	code := run(
		parent,
		[]string{"--workers=1"},
		&stdout,
		&stderr,
		func(ctx context.Context, _ config.Config, _ *slog.Logger) (app.Result, error) {
			cancel()
			<-ctx.Done()
			return app.Result{}, ctx.Err()
		},
	)

	if code != exitInterrupted {
		t.Fatalf("run() exit code = %d, want %d", code, exitInterrupted)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "event=run_cancelled") ||
		!strings.HasSuffix(stderr.String(), "duckwords: interrupted\n") {
		t.Fatalf("stderr = %q, want terminal cancellation event and diagnostic", stderr.String())
	}
}

func TestRunGlobalTimeoutMapsToFailure(t *testing.T) {
	t.Parallel()

	var gotTimeout time.Duration
	var stdout, stderr strings.Builder
	code := runWithTimeoutContext(
		context.Background(),
		[]string{"--workers=1", "--timeout=17s"},
		&stdout,
		&stderr,
		func(ctx context.Context, _ config.Config, _ *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called after global timeout")
			return app.Result{}, nil
		},
		func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
			gotTimeout = timeout
			return context.WithDeadline(parent, time.Now().Add(-time.Second))
		},
	)

	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.String() != "duckwords: execution timed out\n" {
		t.Fatalf("stderr = %q, want timeout diagnostic", stderr.String())
	}
	if gotTimeout != 17*time.Second {
		t.Fatalf("timeout factory duration = %s, want 17s", gotTimeout)
	}
}

func TestRunParentDeadlineMapsToFailure(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	var stdout, stderr strings.Builder
	code := run(
		parent,
		[]string{"--workers=1"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called")
			return app.Result{}, nil
		},
	)

	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	if stderr.String() != "duckwords: execution timed out\n" {
		t.Fatalf("stderr = %q, want timeout diagnostic", stderr.String())
	}
}

func TestRunRejectsInvalidUserAgentBeforeExecutor(t *testing.T) {
	t.Setenv("REDDIT_USER_AGENT", "")

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called with an invalid User-Agent override")
			return app.Result{}, nil
		},
	)

	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), "event=run_started") ||
		!strings.Contains(stderr.String(), "event=run_failed") ||
		!strings.HasSuffix(stderr.String(), "duckwords: Reddit client setup failed; check the optional REDDIT_USER_AGENT and REDDIT_BROWSER_* values documented by --help\n") {
		t.Fatalf("stderr = %q, want fail-closed user-agent diagnostic", stderr.String())
	}
}

func TestRunRejectsInvalidBrowserSessionWithoutEchoingIt(t *testing.T) {
	const canary = "browser-session-private-canary"
	t.Setenv("REDDIT_BROWSER_COOKIE", "reddit_session="+canary+"\r\nAuthorization: planted")

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called with an invalid browser session")
			return app.Result{}, nil
		},
	)

	if code != exitFailure || stdout.Len() != 0 {
		t.Fatalf("run() exit code = %d, stdout = %q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "event=run_failed") ||
		!strings.HasSuffix(stderr.String(), "duckwords: Reddit client setup failed; check the optional REDDIT_USER_AGENT and REDDIT_BROWSER_* values documented by --help\n") ||
		strings.Contains(stderr.String(), canary) {
		t.Fatalf("stderr = %q, want sanitized browser-session rejection", stderr.String())
	}
}

func TestRunInvalidUserAgentFailureIsPureJSON(t *testing.T) {
	t.Setenv("REDDIT_USER_AGENT", "must-not-appear\r\nX-Test: secret")

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1", "--log-format=json"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called with an invalid User-Agent override")
			return app.Result{}, nil
		},
	)

	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	records := decodeJSONLogRecords(t, stderr.String())
	if got := records[len(records)-1]["event"]; got != "run_failed" {
		t.Fatalf("terminal event = %v, want run_failed; log=%q", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "must-not-appear") {
		t.Fatalf("JSON log exposed an environment value: %q", stderr.String())
	}
}

func TestRunOutputFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		writer       io.Writer
		executionErr error
	}{
		{name: "complete write error", writer: errorWriter{}},
		{name: "complete short write", writer: shortWriter{}},
		{name: "partial write error", writer: errorWriter{}, executionErr: app.ErrPartialResult},
		{name: "partial short write", writer: shortWriter{}, executionErr: app.ErrPartialResult},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stderr strings.Builder
			code := run(
				context.Background(),
				[]string{"--workers=1"},
				test.writer,
				&stderr,
				func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
					return app.Result{Words: []aggregate.WordCount{{Word: "duck", Count: 1}}}, test.executionErr
				},
			)
			if code != exitFailure {
				t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
			}
			if !strings.Contains(stderr.String(), "event=run_started") ||
				!strings.HasSuffix(stderr.String(), "duckwords: output failed\n") {
				t.Fatalf("stderr = %q, want lifecycle and output failure without a partial-success claim", stderr.String())
			}
		})
	}
}

func TestRunFailsWhenTerminalApplicationLogCannotBeWritten(t *testing.T) {
	t.Parallel()

	stderr := &failOnWriteWriter{failAt: 2}
	var stdout strings.Builder
	code := run(
		context.Background(),
		[]string{"--workers=1"},
		&stdout,
		stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			return app.Result{Words: []aggregate.WordCount{{Word: "duck", Count: 1}}}, nil
		},
	)
	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stdout.String(), `"word": "duck"`) {
		t.Fatalf("stdout = %q, want the already-written atomic result", stdout.String())
	}
	if stderr.calls != 2 || !strings.Contains(stderr.String(), "event=run_started") ||
		strings.Contains(stderr.String(), "event=output_written") {
		t.Fatalf("stderr calls=%d output=%q, want detected terminal log failure", stderr.calls, stderr.String())
	}
}

func TestRunReturnsFailureWhenDiagnosticWriterFails(t *testing.T) {
	t.Parallel()

	code := run(
		context.Background(),
		[]string{"--unknown"},
		&strings.Builder{},
		errorWriter{},
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called")
			return app.Result{}, nil
		},
	)
	if code != exitFailure {
		t.Fatalf("run() exit code = %d, want %d", code, exitFailure)
	}
}

func TestRunRejectsInvalidLifecycleDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		parent      context.Context
		withTimeout timeoutContextFactory
	}{
		{name: "nil parent", withTimeout: context.WithTimeout},
		{name: "nil timeout factory", parent: context.Background()},
		{
			name:   "nil timeout context",
			parent: context.Background(),
			withTimeout: func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
				return nil, func() {}
			},
		},
		{
			name:   "nil timeout cancel",
			parent: context.Background(),
			withTimeout: func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
				return parent, nil
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder
			code := runWithTimeoutContext(
				test.parent,
				[]string{"--workers=1"},
				&stdout,
				&stderr,
				func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
					t.Fatal("executor was called")
					return app.Result{}, nil
				},
				test.withTimeout,
			)
			if code != exitFailure {
				t.Fatalf("runWithTimeoutContext() exit code = %d, want %d", code, exitFailure)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if !strings.HasPrefix(stderr.String(), "duckwords: execution failed: ") {
				t.Fatalf("stderr = %q, want a lifecycle diagnostic naming the cause", stderr.String())
			}
		})
	}
}

type recordingWriter struct {
	bytes.Buffer
	writes int
}

func (writer *recordingWriter) Write(payload []byte) (int, error) {
	writer.writes++
	return writer.Buffer.Write(payload)
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected write failure")
}

type shortWriter struct{}

func (shortWriter) Write(payload []byte) (int, error) {
	return max(0, len(payload)-1), nil
}

type failOnWriteWriter struct {
	strings.Builder
	calls  int
	failAt int
}

func decodeJSONLogRecords(t *testing.T, document string) []map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSuffix(document, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("JSON log is empty: %q", document)
	}
	records := make([]map[string]any, 0, len(lines))
	for lineNumber, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("JSON log line %d is invalid: %v; line=%q", lineNumber+1, err, line)
		}
		records = append(records, record)
	}
	return records
}

func (writer *failOnWriteWriter) Write(payload []byte) (int, error) {
	writer.calls++
	if writer.calls == writer.failAt {
		return 0, errors.New("injected terminal log failure")
	}
	return writer.Builder.Write(payload)
}

// TestRunWithoutArgumentsProcesses locks in the out-of-the-box contract: a bare
// invocation runs the assignment configuration instead of printing usage.
func TestRunWithoutArgumentsProcesses(t *testing.T) {
	t.Parallel()

	executed := false
	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		nil,
		&stdout,
		&stderr,
		func(_ context.Context, cfg config.Config, _ *slog.Logger) (app.Result, error) {
			executed = true
			if cfg.Posts.Location != config.DefaultPostsURL || cfg.Dictionary.Location != config.DefaultDictionaryURL {
				t.Fatalf("sources = %+v / %+v, want assignment defaults", cfg.Posts, cfg.Dictionary)
			}
			if cfg.Workers != config.DefaultWorkers || cfg.FailureMode != config.FailureModeBestEffort {
				t.Fatalf("config = %+v, want documented defaults", cfg)
			}
			return app.Result{Words: []aggregate.WordCount{{Word: "duck", Count: 2}}}, nil
		},
	)

	if !executed {
		t.Fatal("a bare invocation did not execute")
	}
	if code != exitSuccess {
		t.Fatalf("run() exit code = %d, want %d", code, exitSuccess)
	}
	const want = "[\n  {\n    \"word\": \"duck\",\n    \"count\": 2\n  }\n]\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestConfigDiagnosticNamesTheDisallowedHost proves the usage-exit path explains the
// origin allowlist rather than deferring to a generic setup failure.
func TestConfigDiagnosticNamesTheDisallowedHost(t *testing.T) {
	t.Parallel()

	var stdout, stderr strings.Builder
	code := run(
		context.Background(),
		[]string{"--posts-url=https://example.test/posts.txt"},
		&stdout,
		&stderr,
		func(context.Context, config.Config, *slog.Logger) (app.Result, error) {
			t.Fatal("executor was called for an invalid configuration")
			return app.Result{}, nil
		},
	)

	if code != exitUsage {
		t.Fatalf("run() exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "gist.githubusercontent.com") ||
		strings.Contains(stderr.String(), "REDDIT_API_ACCESS_APPROVED") {
		t.Fatalf("stderr = %q, want the allowed host and no legacy-access misdirection", stderr.String())
	}
}

// TestSkippedOutcomeIsLoggedWithStableFields proves a missing post remains
// machine-readable in the application log without contributing counts.
func TestSkippedOutcomeIsLoggedWithStableFields(t *testing.T) {
	t.Parallel()

	var stderr strings.Builder
	sink, err := logging.New(&stderr, logging.Options{Level: logging.LevelInfo, Format: logging.FormatJSON})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	result := app.Result{
		Words: []aggregate.WordCount{{Word: "duck", Count: 2}},
		Summary: app.Summary{
			Total: 2, Completed: 1, Skipped: 1,
			Comments: 1, BodiesVisited: 1, CountedTokens: 2, DistinctWords: 1,
		},
		Outcomes: []app.PostOutcome{
			{PostID: "duck123", SourceLine: 1, Status: app.OutcomeCompleted, Comments: 1, BodiesVisited: 1, CountedTokens: 2},
			{PostID: "gone456", SourceLine: 2, Status: app.OutcomeSkipped, ErrorClass: reddit.ErrorNotFound, Endpoint: reddit.EndpointComments},
		},
	}
	runlog.New(sink.Logger()).Outcomes(context.Background(), result)
	if err := sink.Err(); err != nil {
		t.Fatalf("sink.Err() = %v", err)
	}

	var skipped map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("operational log line is not JSON: %q", line)
		}
		if record["event"] == "post_outcome" && record["status"] == "skipped" {
			skipped = record
		}
	}
	if skipped == nil {
		t.Fatalf("no skipped post_outcome record:\n%s", stderr.String())
	}
	// The comments endpoint proves absence without a synthetic HTTP status.
	if skipped["error_class"] != "not_found" || skipped["operation"] != "comments" {
		t.Fatalf("skipped record = %#v, want not_found on the comments endpoint", skipped)
	}
	if _, present := skipped["http_status"]; present {
		t.Fatalf("skipped record carries an HTTP status: %#v", skipped)
	}
	if skipped["counted_tokens"] != float64(0) {
		t.Fatalf("skipped record contributed tokens: %#v", skipped)
	}
}
