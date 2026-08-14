// Package cli owns DuckWords command-line parsing, process-independent lifecycle,
// stdout/stderr separation, and exit-code mapping.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/buildinfo"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/logging"
	"github.com/pointerm/duckwords/internal/production"
	"github.com/pointerm/duckwords/internal/runlog"
)

const (
	exitSuccess     = 0
	exitFailure     = 1
	exitUsage       = 2
	exitPartial     = 3
	exitInterrupted = 130

	// ExitSuccess and the other exported exit codes form the process boundary used
	// by the tiny command package and the offline fixture executable.
	ExitSuccess     = exitSuccess
	ExitFailure     = exitFailure
	ExitUsage       = exitUsage
	ExitPartial     = exitPartial
	ExitInterrupted = exitInterrupted
)

var (
	errNilExecutor       = errors.New("nil application executor")
	errNilParentContext  = errors.New("nil parent context")
	errNilTimeoutFactory = errors.New("nil timeout context factory")
	errNilTimeoutContext = errors.New("nil execution context or cancel function")
)

// Executor runs one validated DuckWords configuration.
type Executor func(context.Context, config.Config, *slog.Logger) (app.Result, error)

type timeoutContextFactory func(context.Context, time.Duration) (context.Context, context.CancelFunc)

// Run parses args, executes the application, and returns a stable process exit code.
func Run(
	parent context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	execute Executor,
) int {
	return runWithTimeoutContext(parent, args, stdout, stderr, execute, context.WithTimeout)
}

func run(
	parent context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	execute Executor,
) int {
	return Run(parent, args, stdout, stderr, execute)
}

func runWithTimeoutContext(
	parent context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	execute Executor,
	withTimeout timeoutContextFactory,
) int {
	if parent == nil {
		if writeErr := writeExecutionFailure(stderr, errNilParentContext); writeErr != nil {
			return exitFailure
		}
		return exitFailure
	}
	if withTimeout == nil {
		if writeErr := writeExecutionFailure(stderr, errNilTimeoutFactory); writeErr != nil {
			return exitFailure
		}
		return exitFailure
	}
	cfg, err := config.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		if writeErr := config.WriteUsage(stdout); writeErr != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if err != nil {
		if writeErr := writeDiagnostic(stderr, configDiagnostic(err)); writeErr != nil {
			return exitFailure
		}
		return exitUsage
	}

	if cfg.ShowVersion {
		if _, err := fmt.Fprintln(stdout, buildinfo.Current().String()); err != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if execute == nil {
		if writeErr := writeExecutionFailure(stderr, errNilExecutor); writeErr != nil {
			return exitFailure
		}
		return exitFailure
	}
	if stderr == nil {
		return exitFailure
	}
	logSink, err := logging.New(stderr, logging.Options{Level: cfg.LogLevel, Format: cfg.LogFormat})
	if err != nil || logSink == nil || logSink.Logger() == nil {
		if writeErr := writeExecutionFailure(stderr, err); writeErr != nil {
			return exitFailure
		}
		return exitFailure
	}

	executionCtx, cancel := withTimeout(parent, cfg.Timeout)
	if executionCtx == nil || cancel == nil {
		// A sink already exists, so JSON mode must stay pure NDJSON on stderr. The
		// cause is a fixed internal sentinel and is safe to name in text mode.
		message := "execution failed: " + errNilTimeoutContext.Error()
		if writeErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, message); writeErr != nil {
			return exitFailure
		}
		return exitFailure
	}
	defer cancel()
	if code, stopped := cancellationExit(parent, executionCtx); stopped {
		if cfg.LogFormat == logging.FormatJSON {
			class := "timeout"
			if code == exitInterrupted {
				class = "interrupted"
			}
			runlog.New(logSink.Logger()).Cancelled(executionCtx, class)
			if logErr := logSink.Err(); logErr != nil {
				return exitFailure
			}
		}
		if writeErr := writeRuntimeCancellation(stderr, cfg.LogFormat, code); writeErr != nil {
			return exitFailure
		}
		return code
	}
	access, err := production.ResolveAccessIdentity(os.LookupEnv)
	if err != nil {
		runlog.New(logSink.Logger()).Failed(executionCtx, "reddit_environment", 0)
		if logErr := logSink.Err(); logErr != nil {
			return exitFailure
		}
		if writeErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, executionFailureDiagnostic(production.ErrRedditSetup)); writeErr != nil {
			return exitFailure
		}
		return exitFailure
	}
	accessLog := runlog.AccessIdentity{
		Profile:         access.Profile,
		Origin:          access.Origin,
		Method:          access.Method,
		Auth:            access.Auth,
		UserAgentSource: access.UserAgentSource,
		UserAgentSHA256: access.UserAgentSHA256,
	}
	executionCtx = production.ContextWithAccessIdentity(executionCtx, access)
	runlog.New(logSink.Logger()).RunStarted(executionCtx, cfg, accessLog)
	if logErr := logSink.Err(); logErr != nil {
		return exitFailure
	}

	result, executionErr := execute(executionCtx, cfg, logSink.Logger())
	if logErr := logSink.Err(); logErr != nil {
		return exitFailure
	}
	if code, stopped := cancellationExit(parent, executionCtx); stopped {
		class := "timeout"
		if code == exitInterrupted {
			class = "interrupted"
		}
		runlog.New(logSink.Logger()).Cancelled(executionCtx, class)
		if logErr := logSink.Err(); logErr != nil {
			return exitFailure
		}
		if writeErr := writeRuntimeCancellation(stderr, cfg.LogFormat, code); writeErr != nil {
			return exitFailure
		}
		return code
	}

	if executionErr == nil {
		resultHash, err := writeWords(stdout, result.Words)
		if err != nil {
			runlog.New(logSink.Logger()).Failed(executionCtx, "output", 0)
			logErr := logSink.Err()
			if diagnosticErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, "output failed"); diagnosticErr != nil {
				return exitFailure
			}
			if logErr != nil {
				return exitFailure
			}
			return exitFailure
		}
		runlog.New(logSink.Logger()).OutputWritten(executionCtx, len(result.Words), resultHash, false)
		if logErr := logSink.Err(); logErr != nil {
			return exitFailure
		}
		return exitSuccess
	}
	// Only the exact application sentinel authorizes partial stdout. A wrapped or
	// joined error could contain an additional fatal cause and must fail closed.
	if executionErr == app.ErrPartialResult {
		resultHash, err := writeWords(stdout, result.Words)
		if err != nil {
			runlog.New(logSink.Logger()).Failed(executionCtx, "output", 0)
			logErr := logSink.Err()
			if diagnosticErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, "output failed"); diagnosticErr != nil {
				return exitFailure
			}
			if logErr != nil {
				return exitFailure
			}
			return exitFailure
		}
		runlog.New(logSink.Logger()).OutputWritten(executionCtx, len(result.Words), resultHash, true)
		if logErr := logSink.Err(); logErr != nil {
			return exitFailure
		}
		if err := writeRuntimeDiagnostic(stderr, cfg.LogFormat, "processing completed with a partial result"); err != nil {
			return exitFailure
		}
		return exitPartial
	}

	if err := writeRuntimeDiagnostic(stderr, cfg.LogFormat, executionFailureDiagnostic(executionErr)); err != nil {
		return exitFailure
	}
	return exitFailure
}

func cancellationExit(parent, execution context.Context) (int, bool) {
	if errors.Is(parent.Err(), context.Canceled) {
		return exitInterrupted, true
	}
	if parent.Err() != nil || execution.Err() != nil {
		return exitFailure, true
	}
	return 0, false
}

func writeWords(w io.Writer, words []aggregate.WordCount) (string, error) {
	if words == nil {
		words = []aggregate.WordCount{}
	}
	payload, err := json.MarshalIndent(words, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	payload = append(payload, '\n')

	// Prepare the complete document before this single write so workers and encoding
	// failures can never expose an incomplete JSON fragment on stdout.
	written, err := w.Write(payload)
	if err != nil {
		return "", fmt.Errorf("write result: %w", err)
	}
	if written != len(payload) {
		return "", fmt.Errorf("write result: %w", io.ErrShortWrite)
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func writeCancellation(stderr io.Writer, code int) error {
	if code == exitInterrupted {
		return writeDiagnostic(stderr, "interrupted")
	}
	return writeDiagnostic(stderr, "execution timed out")
}

// writeRuntimeCancellation keeps JSON operational logs valid NDJSON once their
// structured sink exists. Text mode retains the concise human-facing diagnostic.
func writeRuntimeCancellation(stderr io.Writer, format logging.Format, code int) error {
	if format == logging.FormatJSON {
		return nil
	}
	return writeCancellation(stderr, code)
}

// writeRuntimeDiagnostic prevents plain text from corrupting a JSON log stream.
// Configuration errors emitted before a sink exists still use writeDiagnostic.
func writeRuntimeDiagnostic(stderr io.Writer, format logging.Format, message string) error {
	if format == logging.FormatJSON {
		return nil
	}
	return writeDiagnostic(stderr, message)
}

// executionFailureDiagnostic keeps the human-facing cause specific so an unrelated
// source or HTTP-policy failure is not mistaken for a Reddit access problem.
func executionFailureDiagnostic(err error) string {
	switch {
	case errors.Is(err, production.ErrSourceConfig):
		return "input source rejected before any download; check --posts-url/--posts-file " +
			"and --dictionary-url/--dictionary-file"
	case errors.Is(err, production.ErrSourceAcquisition):
		return "could not download an input source; check network access and the source URLs"
	case errors.Is(err, production.ErrSourceParsing):
		return "an input source was downloaded but could not be parsed"
	case errors.Is(err, production.ErrRedditSetup):
		return "Reddit client setup failed; check the optional REDDIT_USER_AGENT and REDDIT_BROWSER_* values documented by --help"
	case errors.Is(err, production.ErrConfig):
		return "production setup failed; check the configuration reported in the log"
	}
	return "execution failed"
}

func configDiagnostic(err error) string {
	switch {
	case errors.Is(err, config.ErrWorkersOutOfRange):
		return "invalid configuration: workers must be between 1 and 32"
	case errors.Is(err, config.ErrInvalidFailureMode):
		return "invalid configuration: failure mode must be best-effort or strict"
	case errors.Is(err, config.ErrTimeoutOutOfRange):
		return "invalid configuration: timeout must be between 1s and 2h"
	case errors.Is(err, config.ErrConflictingSources):
		return "invalid configuration: choose one URL or file for each input"
	case errors.Is(err, config.ErrDuplicateOption):
		return "invalid configuration: source options may be supplied only once"
	case errors.Is(err, config.ErrInvalidSource):
		return "invalid configuration: sources must be safe HTTPS URLs or non-empty local paths"
	case errors.Is(err, config.ErrSourcePathNotSupported):
		return "invalid configuration: the source URL host is correct, but its path or query is not; " +
			"the path must be absolute, unescaped, and without empty or relative segments"
	case errors.Is(err, config.ErrSourceHostNotAllowed):
		return "invalid configuration: --posts-url accepts only gist.githubusercontent.com and " +
			"--dictionary-url only raw.githubusercontent.com; use --posts-file/--dictionary-file for other origins"
	case errors.Is(err, config.ErrRateLimitOutOfRange):
		return "invalid configuration: rate limit must be between 0.1 and 1.5 requests per second"
	case errors.Is(err, config.ErrRequestTimeoutOutOfRange):
		return "invalid configuration: request timeout must be between 1s and 2m"
	case errors.Is(err, config.ErrMaxRetriesOutOfRange):
		return "invalid configuration: max retries must be between 0 and 5"
	case errors.Is(err, config.ErrRetryBudgetOutOfRange):
		return "invalid configuration: retry budget must be between 1s and 5m"
	case errors.Is(err, config.ErrInvalidLogLevel):
		return "invalid configuration: log level must be debug, info, warn, or error"
	case errors.Is(err, config.ErrInvalidLogFormat):
		return "invalid configuration: log format must be text or json"
	case errors.Is(err, config.ErrInvalidFilter):
		return "invalid configuration: filter must use only letters and '*' within documented limits"
	case errors.Is(err, config.ErrConflictingMode):
		return "invalid configuration: --version cannot be combined with processing options"
	default:
		return "invalid command-line arguments; use --help"
	}
}

// writeExecutionFailure reports a lifecycle failure that happened before a
// structured log sink existed. The cause is a fixed internal sentinel, so its text
// is safe to show and tells the operator which precondition was violated.
func writeExecutionFailure(stderr io.Writer, err error) error {
	if err == nil {
		return writeDiagnostic(stderr, "execution failed")
	}
	return writeDiagnostic(stderr, "execution failed: "+err.Error())
}

func writeDiagnostic(stderr io.Writer, message string) error {
	_, err := fmt.Fprintf(stderr, "duckwords: %s\n", message)
	return err
}
