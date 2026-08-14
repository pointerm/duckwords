package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/pointerm/duckwords/internal/acquire"
	"github.com/pointerm/duckwords/internal/app"
	"github.com/pointerm/duckwords/internal/config"
	"github.com/pointerm/duckwords/internal/logging"
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/source"
	"github.com/pointerm/duckwords/internal/words"
)

const (
	policyInitialBackoff = 500 * time.Millisecond
	policyMaximumBackoff = 8 * time.Second
)

var (
	errProductionConfig  = errors.New("invalid production execution configuration")
	errSourceAcquisition = errors.New("input acquisition failed")
	errSourceParsing     = errors.New("input parsing failed")
	errRedditSetup       = errors.New("reddit client setup failed")
	errRunnerSetup       = errors.New("application runner setup failed")
)

type productionDependencies struct {
	lookupEnv environmentLookup
	newHTTP   func(time.Duration, int) (*http.Client, error)
	now       func() time.Time
}

func defaultProductionDependencies() productionDependencies {
	return productionDependencies{
		lookupEnv: os.LookupEnv,
		newHTTP:   newProductionHTTPClient,
		now:       time.Now,
	}
}

// executeProduction assembles exactly one dictionary, token source, request policy,
// Reddit client, and runner. Constructors and both source specifications are
// validated before the first filesystem or network operation.
func executeProduction(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	dependencies productionDependencies,
) (app.Result, error) {
	if ctx == nil || logger == nil || dependencies.lookupEnv == nil ||
		dependencies.newHTTP == nil || dependencies.now == nil {
		return app.Result{}, errProductionConfig
	}
	startedAt := dependencies.now()
	log := newExecutionLogger(logger)

	credentials, err := credentialsFromEnvironment(dependencies.lookupEnv)
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: credentials or approval", errProductionConfig)
	}
	httpClient, err := dependencies.newHTTP(cfg.RequestTimeout, cfg.Workers)
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: HTTP policy", errProductionConfig)
	}
	defer closeIdleConnections(httpClient)

	postLimits := source.DefaultPostListLimits()
	dictionaryLimits := words.DefaultDictionaryLimits()
	sourceUserAgent := sourceDownloadUserAgent()
	sourceRetry := productionSourceRetryConfig(cfg, log.sourceRetryObserver(ctx))
	postsSpec, postsAcquireConfig := acquisitionConfig(cfg.Posts, acquire.KindPosts, httpClient, sourceUserAgent, postLimits.MaxBytes, sourceRetry)
	dictionarySpec, dictionaryAcquireConfig := acquisitionConfig(cfg.Dictionary, acquire.KindDictionary, httpClient, sourceUserAgent, dictionaryLimits.MaxBytes, sourceRetry)
	if err := acquire.Validate(postsSpec, postsAcquireConfig); err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: posts", errProductionConfig)
	}
	if err := acquire.Validate(dictionarySpec, dictionaryAcquireConfig); err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: dictionary", errProductionConfig)
	}

	policy, err := reddit.NewRequestPolicy(reddit.RequestPolicyConfig{
		RequestsPerSecond: cfg.RateLimit,
		MaxRetries:        cfg.MaxRetries,
		MaxRetryElapsed:   cfg.RetryBudget,
		InitialBackoff:    policyInitialBackoff,
		MaxBackoff:        policyMaximumBackoff,
		Observer:          log.retryObserver(ctx),
	})
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: request policy", errProductionConfig)
	}
	tokenSource, err := reddit.NewTokenSource(reddit.TokenConfig{
		ClientID:          credentials.clientID,
		ClientSecret:      credentials.clientSecret,
		UserAgent:         credentials.userAgent,
		HTTPClient:        httpClient,
		RequestPolicy:     policy,
		APIAccessApproved: credentials.approved,
	})
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, errRedditSetup
	}
	redditClient, err := reddit.NewClient(reddit.ClientConfig{
		HTTPClient:    httpClient,
		TokenSource:   tokenSource,
		RequestPolicy: policy,
	})
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, errRedditSetup
	}

	postsDocument, err := acquire.Load(ctx, postsSpec, postsAcquireConfig)
	if err != nil {
		if !errors.Is(err, acquire.ErrCanceled) {
			log.failed(ctx, acquisitionErrorClass(err), elapsed(dependencies.now, startedAt))
		}
		return app.Result{}, preserveCancellation(err, errSourceAcquisition)
	}
	logSourceLoaded(ctx, logger, postsDocument.Provenance())
	posts, postStats, err := source.LoadPostList(postsDocument.Reader(), postLimits)
	if err != nil {
		log.failed(ctx, "source_parse", elapsed(dependencies.now, startedAt))
		return app.Result{}, errSourceParsing
	}
	if postStats.SHA256 != postsDocument.Provenance().SHA256 {
		log.failed(ctx, "source_integrity", elapsed(dependencies.now, startedAt))
		return app.Result{}, errSourceParsing
	}
	logSourceParsed(ctx, logger, "posts", postStats.Posts, postStats.SHA256, postStats.PostsSHA256)

	dictionaryDocument, err := acquire.Load(ctx, dictionarySpec, dictionaryAcquireConfig)
	if err != nil {
		if !errors.Is(err, acquire.ErrCanceled) {
			log.failed(ctx, acquisitionErrorClass(err), elapsed(dependencies.now, startedAt))
		}
		return app.Result{}, preserveCancellation(err, errSourceAcquisition)
	}
	logSourceLoaded(ctx, logger, dictionaryDocument.Provenance())
	dictionary, dictionaryStats, err := words.LoadDictionary(dictionaryDocument.Reader(), dictionaryLimits)
	if err != nil {
		log.failed(ctx, "source_parse", elapsed(dependencies.now, startedAt))
		return app.Result{}, errSourceParsing
	}
	// Acquisition and parsers intentionally compute their digests independently. A
	// mismatch would mean the bytes changed across an internal trust boundary.
	if dictionaryStats.SHA256 != dictionaryDocument.Provenance().SHA256 {
		log.failed(ctx, "source_integrity", elapsed(dependencies.now, startedAt))
		return app.Result{}, errSourceParsing
	}
	logSourceParsed(ctx, logger, "dictionary", dictionary.Len(), dictionaryStats.SHA256, "")

	matcher, err := words.NewMatcher(cfg.Filters)
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, errRunnerSetup
	}
	runner, err := app.New(
		app.Config{Workers: cfg.Workers, FailureMode: app.FailureMode(cfg.FailureMode)},
		redditClient,
		dictionary,
		matcher,
	)
	if err != nil {
		log.failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, errRunnerSetup
	}

	result, runErr := runner.Run(ctx, posts)
	log.outcomes(ctx, result)
	if runErr == nil || runErr == app.ErrPartialResult {
		log.summary(
			ctx,
			cfg,
			result,
			policy.Snapshot(),
			postStats.SHA256,
			postStats.PostsSHA256,
			dictionaryStats.SHA256,
			dictionary.Len(),
			elapsed(dependencies.now, startedAt),
			runErr == app.ErrPartialResult,
		)
		return result, runErr
	}
	// The CLI owns the single terminal cancellation record. Check the live context
	// as well as the returned cause because an injected walker may report its
	// cancellation class without wrapping context.Canceled.
	if ctx.Err() == nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
		log.failed(ctx, runErrorClass(runErr), elapsed(dependencies.now, startedAt))
	}
	return result, runErr
}

func acquisitionConfig(
	input config.InputSource,
	kind acquire.Kind,
	httpClient *http.Client,
	sourceUserAgent string,
	maxBytes int64,
	retry acquire.RetryConfig,
) (acquire.Spec, acquire.Config) {
	spec := acquire.Spec{Kind: kind}
	switch input.Kind {
	case config.SourceURL:
		spec.URL = input.Location
	case config.SourceFile:
		spec.File = input.Location
	}
	return spec, acquire.Config{
		HTTPClient: httpClient,
		UserAgent:  sourceUserAgent,
		MaxBytes:   maxBytes,
		Retry:      &retry,
	}
}

// productionSourceRetryConfig maps the broader Reddit retry controls onto the
// intentionally smaller source-download policy. Zero retries remains an explicit
// opt-out; larger CLI values and budgets cannot expand the acquisition ceiling.
func productionSourceRetryConfig(cfg config.Config, observer acquire.RetryObserver) acquire.RetryConfig {
	retry := acquire.DefaultRetryConfig()
	if cfg.MaxRetries < retry.MaxRetries {
		retry.MaxRetries = cfg.MaxRetries
	}
	if cfg.RetryBudget < retry.MaxElapsed {
		retry.MaxElapsed = cfg.RetryBudget
	}
	retry.Observer = observer
	return retry
}

func closeIdleConnections(client *http.Client) {
	if client == nil {
		return
	}
	client.CloseIdleConnections()
}

func logSourceLoaded(ctx context.Context, logger *slog.Logger, provenance acquire.Provenance) {
	if logger == nil {
		return
	}
	logger.InfoContext(
		ctx,
		logMessageSource,
		logging.EventAttr(logging.EventSourceLoaded),
		slog.String(logging.KeySourceKind, provenance.Kind.String()),
		slog.String("source_mode", provenance.Mode.String()),
		slog.String("source_origin", provenance.Origin),
		slog.Int64("source_bytes", provenance.Bytes),
		slog.String("source_sha256", provenance.SHA256),
	)
}

func logSourceParsed(ctx context.Context, logger *slog.Logger, kind string, entries int, digest string, postsDigest string) {
	if logger == nil {
		return
	}
	attributes := []slog.Attr{
		logging.EventAttr(logging.EventSourceParsed),
		slog.String(logging.KeySourceKind, kind),
		slog.String("stage", "parsed"),
		slog.Int("entries", entries),
		slog.String("source_sha256", digest),
	}
	if kind == "posts" {
		attributes = append(attributes, slog.String("posts_sha256", postsDigest))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "source parsed", attributes...)
}

func elapsed(now func() time.Time, startedAt time.Time) time.Duration {
	if now == nil {
		return 0
	}
	duration := now().Sub(startedAt)
	if duration < 0 {
		return 0
	}
	return duration
}

func acquisitionErrorClass(err error) string {
	switch {
	case errors.Is(err, acquire.ErrCanceled):
		return "canceled"
	case errors.Is(err, acquire.ErrTooLarge):
		return "resource_limit"
	case errors.Is(err, acquire.ErrHTTPStatus):
		return "http_status"
	case errors.Is(err, acquire.ErrTransport):
		return "transport"
	default:
		return "source"
	}
}

func preserveCancellation(err, fallback error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fallback
}

func runErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, app.ErrStrictFailure):
		return "strict"
	case errors.Is(err, app.ErrNoCompletedPosts):
		return "no_completed_posts"
	default:
		return "application"
	}
}
