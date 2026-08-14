package main

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pointerm/duckwords/internal/acquire"
	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/buildinfo"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/logging"
	"github.com/pointerm/duckwords/internal/reddit"
)

const (
	logMessageRunStarted  = "run started"
	logMessageSource      = "source loaded"
	logMessageRetry       = "request retry scheduled"
	logMessagePostOutcome = "post processing completed"
	logMessageRunSummary  = "processing summary"
	logMessageOutput      = "result written"
	logMessageRunFailed   = "run failed"
	logMessageCancelled   = "run cancelled"
)

const (
	unknownBuildLogValue   = "unknown"
	maxCommitLogBytes      = 64
	minCommitLogBytes      = 7
	maxGoVersionLogBytes   = 96
	maxPlatformLogBytes    = 16
	buildDateLogLayout     = "2006-01-02T15:04:05Z"
	terminalStatusComplete = "complete"
	terminalStatusPartial  = "partial"
	inputProfileAssignment = "assignment-default-v1"
	inputProfileCustom     = "custom"
)

type logBuildIdentity struct {
	version   string
	commit    string
	buildDate string
	goVersion string
	goos      string
	goarch    string
}

type executionLogger struct {
	logger        *slog.Logger
	sourceRetries *atomic.Uint64
}

func newExecutionLogger(logger *slog.Logger) executionLogger {
	return executionLogger{logger: logger, sourceRetries: new(atomic.Uint64)}
}

func (log executionLogger) runStarted(ctx context.Context, cfg config.Config) {
	if log.logger == nil {
		return
	}
	traversalBudget := reddit.DefaultTraversalBudgetConfig()
	sourceRetry := productionSourceRetryConfig(cfg, nil)
	attributes := []slog.Attr{
		logging.EventAttr(logging.EventRunStarted),
		slog.Int("workers", cfg.Workers),
		slog.String("failure_mode", string(cfg.FailureMode)),
		slog.String("input_profile", executionInputProfile(cfg)),
		slog.Int("filter_count", len(cfg.Filters)),
		slog.Float64("rate_limit_rps", cfg.RateLimit),
		slog.Duration("request_timeout", cfg.RequestTimeout),
		slog.Duration("global_timeout", cfg.Timeout),
		slog.Int("max_retries", cfg.MaxRetries),
		slog.Duration("retry_budget", cfg.RetryBudget),
		slog.Int("source_max_retries", sourceRetry.MaxRetries),
		slog.Duration("source_retry_budget", sourceRetry.MaxElapsed),
		slog.Int("max_distinct_words_per_post", app.DefaultMaxDistinctWordsPerPost),
		slog.Int64("max_in_flight_response_bytes", traversalBudget.MaxInFlightResponseBytes),
		slog.Int("max_retained_things", traversalBudget.MaxRetainedThings),
	}
	attributes = appendBuildIdentityAttrs(attributes, currentLogBuildIdentity())
	log.logger.LogAttrs(ctx, slog.LevelInfo, logMessageRunStarted, attributes...)
}

func (log executionLogger) retryObserver(ctx context.Context) reddit.RetryObserver {
	return func(event reddit.RetryEvent) {
		if log.logger == nil {
			return
		}
		log.logger.WarnContext(
			ctx,
			logMessageRetry,
			logging.EventAttr(logging.EventRetry),
			slog.String(logging.KeyOperation, string(event.Endpoint)),
			slog.String(logging.KeyPostID, event.PostID),
			logging.ErrorClassAttr(string(event.Class)),
			slog.Int("http_status", event.StatusCode),
			slog.Int("attempt", event.Attempt),
			slog.Duration("delay", event.Delay),
		)
	}
}

func (log executionLogger) sourceRetryObserver(ctx context.Context) acquire.RetryObserver {
	return func(event acquire.RetryEvent) {
		if log.sourceRetries != nil {
			log.sourceRetries.Add(1)
		}
		if log.logger == nil {
			return
		}
		attributes := []slog.Attr{
			logging.EventAttr(logging.EventRetry),
			slog.String(logging.KeyOperation, "source_download"),
			slog.String(logging.KeySourceKind, event.Kind.String()),
			logging.ErrorClassAttr(string(event.Reason)),
			slog.Int("attempt", event.Attempt),
			slog.Duration("delay", event.Delay),
		}
		if event.StatusCode != 0 {
			attributes = append(attributes, slog.Int("http_status", event.StatusCode))
		}
		log.logger.LogAttrs(ctx, slog.LevelWarn, logMessageRetry, attributes...)
	}
}

func (log executionLogger) sourceRetryCount() uint64 {
	if log.sourceRetries == nil {
		return 0
	}
	return log.sourceRetries.Load()
}

func (log executionLogger) outcomes(ctx context.Context, result app.Result) {
	if log.logger == nil {
		return
	}
	for _, outcome := range result.Outcomes {
		attributes := []slog.Attr{
			logging.EventAttr(logging.EventPostOutcome),
			slog.String(logging.KeyPostID, outcome.PostID),
			slog.Int("source_line", outcome.SourceLine),
			slog.String("status", string(outcome.Status)),
			slog.Int("comments", outcome.Comments),
			slog.Int("bodies_visited", outcome.BodiesVisited),
			slog.Int("more_requests", outcome.MoreRequests),
			slog.Int("continuation_requests", outcome.ContinuationRequests),
			slog.Uint64("counted_tokens", outcome.CountedTokens),
		}
		if outcome.ErrorClass != "" {
			attributes = append(attributes, logging.ErrorClassAttr(string(outcome.ErrorClass)))
		}
		if outcome.Endpoint != "" {
			attributes = append(attributes, slog.String(logging.KeyOperation, string(outcome.Endpoint)))
		}
		if outcome.HTTPStatus != 0 {
			attributes = append(attributes, slog.Int("http_status", outcome.HTTPStatus))
		}
		log.logger.LogAttrs(ctx, slog.LevelInfo, logMessagePostOutcome, attributes...)
	}
}

func (log executionLogger) summary(
	ctx context.Context,
	cfg config.Config,
	result app.Result,
	requestStats reddit.RequestPolicySnapshot,
	postsHash string,
	postIDsHash string,
	dictionaryHash string,
	dictionaryWords int,
	duration time.Duration,
	partial bool,
) {
	if log.logger == nil {
		return
	}
	summary := result.Summary
	terminalStatus := terminalStatusComplete
	if partial {
		terminalStatus = terminalStatusPartial
	}
	attributes := []slog.Attr{
		logging.EventAttr(logging.EventRunSummary),
		slog.String("terminal_status", terminalStatus),
		slog.Bool("partial", partial),
		slog.String("failure_mode", string(cfg.FailureMode)),
		slog.Int("workers", cfg.Workers),
		slog.String("input_profile", executionInputProfile(cfg)),
		slog.Int("filter_count", len(cfg.Filters)),
		slog.Duration("duration", duration),
		slog.Int("posts_total", summary.Total),
		slog.Int("posts_completed", summary.Completed),
		slog.Int("posts_skipped", summary.Skipped),
		slog.Int("posts_failed", summary.Failed),
		slog.Int("posts_incomplete", summary.Incomplete),
		slog.Uint64("comments", summary.Comments),
		slog.Uint64("bodies_visited", summary.BodiesVisited),
		slog.Uint64("more_requests", summary.MoreRequests),
		slog.Uint64("continuation_requests", summary.ContinuationRequests),
		slog.Uint64("counted_tokens", summary.CountedTokens),
		slog.Int("distinct_words", summary.DistinctWords),
		slog.Int("dictionary_words", dictionaryWords),
		slog.Uint64("source_retries", log.sourceRetryCount()),
		slog.Uint64("reddit_http_attempts", requestStats.HTTPAttempts),
		slog.Uint64("reddit_retries", requestStats.Retries),
		slog.Uint64("throttle_waits", requestStats.ThrottleWaitCount),
		slog.Duration("throttle_wait", requestStats.ThrottleWait),
		slog.String("posts_sha256", postsHash),
		slog.String("post_ids_sha256", postIDsHash),
		slog.String("dictionary_sha256", dictionaryHash),
	}
	attributes = appendBuildIdentityAttrs(attributes, currentLogBuildIdentity())
	log.logger.LogAttrs(ctx, slog.LevelInfo, logMessageRunSummary, attributes...)
}

// executionInputProfile binds evidence to the two exact assignment locators
// without putting full URLs or local paths in operational logs. Any override is
// deliberately collapsed to "custom" and therefore cannot satisfy the final
// assignment-evidence contract.
func executionInputProfile(cfg config.Config) string {
	if cfg.Posts == (config.InputSource{Kind: config.SourceURL, Location: config.DefaultPostsURL}) &&
		cfg.Dictionary == (config.InputSource{Kind: config.SourceURL, Location: config.DefaultDictionaryURL}) {
		return inputProfileAssignment
	}
	return inputProfileCustom
}

func currentLogBuildIdentity() logBuildIdentity {
	return safeLogBuildIdentity(buildinfo.Current(), runtime.GOOS, runtime.GOARCH)
}

// safeLogBuildIdentity applies field-specific allowlists before provenance reaches
// a log. Release metadata is supplied through linker flags and therefore remains an
// input boundary even though normal release automation validates it first.
func safeLogBuildIdentity(info buildinfo.Info, goos string, goarch string) logBuildIdentity {
	return logBuildIdentity{
		version:   safeVersionLogValue(info.Version),
		commit:    safeCommitLogValue(info.Commit),
		buildDate: safeBuildDateLogValue(info.BuildDate),
		goVersion: safePrintableLogValue(info.GoVersion, maxGoVersionLogBytes),
		goos:      safePlatformLogValue(goos),
		goarch:    safePlatformLogValue(goarch),
	}
}

func appendBuildIdentityAttrs(attributes []slog.Attr, identity logBuildIdentity) []slog.Attr {
	return append(
		attributes,
		slog.String("app_version", identity.version),
		slog.String("app_commit", identity.commit),
		slog.String("app_build_date", identity.buildDate),
		slog.String("go_version", identity.goVersion),
		slog.String("goos", identity.goos),
		slog.String("goarch", identity.goarch),
	)
}

func safeVersionLogValue(value string) string {
	if !validSourceVersion(value) || !isASCIILetterOrDigit(value[0]) {
		return unknownBuildLogValue
	}
	return value
}

func safeCommitLogValue(value string) string {
	if value == unknownBuildLogValue {
		return value
	}
	if len(value) < minCommitLogBytes || len(value) > maxCommitLogBytes {
		return unknownBuildLogValue
	}
	for index := range len(value) {
		if char := value[index]; (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return unknownBuildLogValue
		}
	}
	return value
}

func safeBuildDateLogValue(value string) string {
	if value == unknownBuildLogValue {
		return value
	}
	parsed, err := time.Parse(buildDateLogLayout, value)
	if err != nil || parsed.Format(buildDateLogLayout) != value {
		return unknownBuildLogValue
	}
	return value
}

func safePrintableLogValue(value string, maximumBytes int) string {
	if value == "" || len(value) > maximumBytes || strings.TrimSpace(value) != value {
		return unknownBuildLogValue
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return unknownBuildLogValue
		}
	}
	return value
}

func safePlatformLogValue(value string) string {
	if value == "" || len(value) > maxPlatformLogBytes {
		return unknownBuildLogValue
	}
	for index := range len(value) {
		char := value[index]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return unknownBuildLogValue
		}
	}
	return value
}

func isASCIILetterOrDigit(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func (log executionLogger) outputWritten(ctx context.Context, words int, resultHash string, partial bool) {
	if log.logger == nil {
		return
	}
	log.logger.InfoContext(
		ctx,
		logMessageOutput,
		logging.EventAttr(logging.EventOutputWritten),
		slog.Bool("partial", partial),
		slog.Int("result_words", words),
		slog.String("result_sha256", resultHash),
	)
}

func (log executionLogger) cancelled(ctx context.Context, class string) {
	if log.logger == nil {
		return
	}
	log.logger.ErrorContext(
		ctx,
		logMessageCancelled,
		logging.EventAttr(logging.EventRunCancelled),
		logging.ErrorClassAttr(class),
	)
}

func (log executionLogger) failed(ctx context.Context, class string, duration time.Duration) {
	if log.logger == nil {
		return
	}
	log.logger.ErrorContext(
		ctx,
		logMessageRunFailed,
		logging.EventAttr(logging.EventRunFailed),
		logging.ErrorClassAttr(class),
		slog.Duration("duration", duration),
	)
}
