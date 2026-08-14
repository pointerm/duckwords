// Command duckwords analyzes dictionary words in Reddit comment trees.
package main

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
	"os/signal"
	"syscall"
	"time"

	"github.com/pointerm/duckwords/internal/aggregate"
	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/buildinfo"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/logging"
)

const (
	exitSuccess     = 0
	exitFailure     = 1
	exitUsage       = 2
	exitPartial     = 3
	exitInterrupted = 130
)

var (
	errNilExecutor       = errors.New("nil application executor")
	errNilParentContext  = errors.New("nil parent context")
	errNilTimeoutFactory = errors.New("nil timeout context factory")
)

type executor func(context.Context, config.Config, *slog.Logger) (app.Result, error)

type timeoutContextFactory func(context.Context, time.Duration) (context.Context, context.CancelFunc)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return run(ctx, args, stdout, stderr, mainExecutor)
}

// mainExecutor is the production composition point. It remains fail-closed unless
// the environment explicitly confirms Reddit API approval and supplies credentials.
func mainExecutor(ctx context.Context, cfg config.Config, logger *slog.Logger) (app.Result, error) {
	return executeProduction(ctx, cfg, logger, defaultProductionDependencies())
}

func run(
	parent context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	execute executor,
) int {
	return runWithTimeoutContext(parent, args, stdout, stderr, execute, context.WithTimeout)
}

func runWithTimeoutContext(
	parent context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	execute executor,
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
	if !cfg.RunRequested {
		if err := config.WriteUsage(stdout); err != nil {
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
		if writeErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, "execution failed"); writeErr != nil {
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
			newExecutionLogger(logSink.Logger()).cancelled(executionCtx, class)
			if logErr := logSink.Err(); logErr != nil {
				return exitFailure
			}
		}
		if writeErr := writeRuntimeCancellation(stderr, cfg.LogFormat, code); writeErr != nil {
			return exitFailure
		}
		return code
	}
	newExecutionLogger(logSink.Logger()).runStarted(executionCtx, cfg)
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
		newExecutionLogger(logSink.Logger()).cancelled(executionCtx, class)
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
			newExecutionLogger(logSink.Logger()).failed(executionCtx, "output", 0)
			logErr := logSink.Err()
			if diagnosticErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, "output failed"); diagnosticErr != nil {
				return exitFailure
			}
			if logErr != nil {
				return exitFailure
			}
			return exitFailure
		}
		newExecutionLogger(logSink.Logger()).outputWritten(executionCtx, len(result.Words), resultHash, false)
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
			newExecutionLogger(logSink.Logger()).failed(executionCtx, "output", 0)
			logErr := logSink.Err()
			if diagnosticErr := writeRuntimeDiagnostic(stderr, cfg.LogFormat, "output failed"); diagnosticErr != nil {
				return exitFailure
			}
			if logErr != nil {
				return exitFailure
			}
			return exitFailure
		}
		newExecutionLogger(logSink.Logger()).outputWritten(executionCtx, len(result.Words), resultHash, true)
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

func executionFailureDiagnostic(err error) string {
	if errors.Is(err, errProductionConfig) {
		return "production setup failed; confirm Reddit approval and required environment configuration (see README)"
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

func writeExecutionFailure(stderr io.Writer, err error) error {
	return writeDiagnostic(stderr, "execution failed")
}

func writeDiagnostic(stderr io.Writer, message string) error {
	_, err := fmt.Fprintf(stderr, "duckwords: %s\n", message)
	return err
}
