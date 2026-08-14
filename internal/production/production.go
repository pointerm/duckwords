package production

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
	"github.com/pointerm/duckwords/internal/reddit"
	"github.com/pointerm/duckwords/internal/runlog"
	"github.com/pointerm/duckwords/internal/source"
	"github.com/pointerm/duckwords/internal/words"
)

const (
	policyInitialBackoff = 500 * time.Millisecond
	policyMaximumBackoff = 8 * time.Second
)

var (
	// ErrConfig classifies invalid production composition without exposing secrets.
	ErrConfig            = errors.New("invalid production execution configuration")
	ErrSourceConfig      = fmt.Errorf("%w: input source", ErrConfig)
	ErrSourceAcquisition = errors.New("input acquisition failed")
	ErrSourceParsing     = errors.New("input parsing failed")
	ErrRedditSetup       = errors.New("reddit client setup failed")
	errRunnerSetup       = errors.New("application runner setup failed")
)

// Dependencies contains the process adapters used by production composition.
// Fields remain private so callers construct a complete set atomically.
type Dependencies struct {
	lookupEnv environmentLookup
	newHTTP   func(time.Duration, int) (*http.Client, error)
	now       func() time.Time
}

func defaultDependencies() Dependencies {
	return Dependencies{
		lookupEnv: os.LookupEnv,
		newHTTP:   newProductionHTTPClient,
		now:       time.Now,
	}
}

// NewDependencies constructs a complete dependency set for deterministic integration tests.
func NewDependencies(
	lookupEnv func(string) (string, bool),
	newHTTP func(time.Duration, int) (*http.Client, error),
	now func() time.Time,
) Dependencies {
	return Dependencies{lookupEnv: lookupEnv, newHTTP: newHTTP, now: now}
}

// Execute runs the production composition with operating-system dependencies.
func Execute(ctx context.Context, cfg config.Config, logger *slog.Logger) (app.Result, error) {
	return ExecuteWithDependencies(ctx, cfg, logger, defaultDependencies())
}

// ExecuteWithDependencies runs production composition with explicit process adapters.
func ExecuteWithDependencies(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	dependencies Dependencies,
) (app.Result, error) {
	return executeProduction(ctx, cfg, logger, dependencies)
}

// executeProduction assembles exactly one dictionary, request policy, public Reddit
// client, and runner. Constructors and both source specifications are
// validated before the first filesystem or network operation.
func executeProduction(
	ctx context.Context,
	cfg config.Config,
	logger *slog.Logger,
	dependencies Dependencies,
) (app.Result, error) {
	if ctx == nil || logger == nil || dependencies.lookupEnv == nil ||
		dependencies.newHTTP == nil || dependencies.now == nil {
		return app.Result{}, ErrConfig
	}
	startedAt := dependencies.now()
	log := runlog.New(logger)

	access, supplied := accessIdentityFromContext(ctx)
	var err error
	if !supplied {
		access, err = ResolveAccessIdentity(dependencies.lookupEnv)
	}
	if err != nil || !validAccessIdentity(access) {
		log.Failed(ctx, "user_agent", elapsed(dependencies.now, startedAt))
		return app.Result{}, ErrRedditSetup
	}
	httpClient, err := dependencies.newHTTP(cfg.RequestTimeout, cfg.Workers)
	if err != nil {
		log.Failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: HTTP policy", ErrConfig)
	}
	defer closeIdleConnections(httpClient)

	postLimits := source.DefaultPostListLimits()
	dictionaryLimits := words.DefaultDictionaryLimits()
	sourceUserAgent := sourceDownloadUserAgent()
	sourceRetry := productionSourceRetryConfig(cfg, log.SourceRetryObserver(ctx))
	postsSpec, postsAcquireConfig := acquisitionConfig(cfg.Posts, acquire.KindPosts, httpClient, sourceUserAgent, postLimits.MaxBytes, sourceRetry)
	dictionarySpec, dictionaryAcquireConfig := acquisitionConfig(cfg.Dictionary, acquire.KindDictionary, httpClient, sourceUserAgent, dictionaryLimits.MaxBytes, sourceRetry)
	if err := acquire.Validate(postsSpec, postsAcquireConfig); err != nil {
		log.Failed(ctx, "source_config", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: posts", ErrSourceConfig)
	}
	if err := acquire.Validate(dictionarySpec, dictionaryAcquireConfig); err != nil {
		log.Failed(ctx, "source_config", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: dictionary", ErrSourceConfig)
	}

	policy, err := reddit.NewRequestPolicy(reddit.RequestPolicyConfig{
		RequestsPerSecond: cfg.RateLimit,
		MaxRetries:        cfg.MaxRetries,
		MaxRetryElapsed:   cfg.RetryBudget,
		InitialBackoff:    policyInitialBackoff,
		MaxBackoff:        policyMaximumBackoff,
		Observer:          log.RetryObserver(ctx),
	})
	if err != nil {
		log.Failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, fmt.Errorf("%w: request policy", ErrConfig)
	}
	redditClient, err := reddit.NewClient(reddit.ClientConfig{
		HTTPClient:    httpClient,
		UserAgent:     access.UserAgent(),
		RequestPolicy: policy,
	})
	if err != nil {
		log.Failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, ErrRedditSetup
	}

	postsDocument, err := acquire.Load(ctx, postsSpec, postsAcquireConfig)
	if err != nil {
		if !errors.Is(err, acquire.ErrCanceled) {
			log.Failed(ctx, acquisitionErrorClass(err), elapsed(dependencies.now, startedAt))
		}
		return app.Result{}, preserveCancellation(err, ErrSourceAcquisition)
	}
	log.SourceLoaded(ctx, postsDocument.Provenance())
	posts, postStats, err := source.LoadPostList(postsDocument.Reader(), postLimits)
	if err != nil {
		log.Failed(ctx, "source_parse", elapsed(dependencies.now, startedAt))
		return app.Result{}, ErrSourceParsing
	}
	if postStats.SHA256 != postsDocument.Provenance().SHA256 {
		log.Failed(ctx, "source_integrity", elapsed(dependencies.now, startedAt))
		return app.Result{}, ErrSourceParsing
	}
	log.SourceParsed(ctx, "posts", postStats.Posts, postStats.SHA256, postStats.PostsSHA256)

	dictionaryDocument, err := acquire.Load(ctx, dictionarySpec, dictionaryAcquireConfig)
	if err != nil {
		if !errors.Is(err, acquire.ErrCanceled) {
			log.Failed(ctx, acquisitionErrorClass(err), elapsed(dependencies.now, startedAt))
		}
		return app.Result{}, preserveCancellation(err, ErrSourceAcquisition)
	}
	log.SourceLoaded(ctx, dictionaryDocument.Provenance())
	dictionary, dictionaryStats, err := words.LoadDictionary(dictionaryDocument.Reader(), dictionaryLimits)
	if err != nil {
		log.Failed(ctx, "source_parse", elapsed(dependencies.now, startedAt))
		return app.Result{}, ErrSourceParsing
	}
	// Acquisition and parsers intentionally compute their digests independently. A
	// mismatch would mean the bytes changed across an internal trust boundary.
	if dictionaryStats.SHA256 != dictionaryDocument.Provenance().SHA256 {
		log.Failed(ctx, "source_integrity", elapsed(dependencies.now, startedAt))
		return app.Result{}, ErrSourceParsing
	}
	log.SourceParsed(ctx, "dictionary", dictionary.Len(), dictionaryStats.SHA256, "")

	matcher, err := words.NewMatcher(cfg.Filters)
	if err != nil {
		log.Failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, errRunnerSetup
	}
	runner, err := app.New(
		app.Config{Workers: cfg.Workers, FailureMode: app.FailureMode(cfg.FailureMode)},
		redditClient,
		dictionary,
		matcher,
	)
	if err != nil {
		log.Failed(ctx, "configuration", elapsed(dependencies.now, startedAt))
		return app.Result{}, errRunnerSetup
	}

	result, runErr := runner.Run(ctx, posts)
	log.Outcomes(ctx, result)
	if runErr == nil || runErr == app.ErrPartialResult {
		log.Summary(
			ctx,
			cfg,
			runlogAccessIdentity(access),
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
		log.Failed(ctx, runErrorClass(runErr), elapsed(dependencies.now, startedAt))
	}
	return result, runErr
}

func runlogAccessIdentity(access AccessIdentity) runlog.AccessIdentity {
	return runlog.AccessIdentity{
		Profile:         access.Profile,
		Origin:          access.Origin,
		Method:          access.Method,
		Auth:            access.Auth,
		UserAgentSource: access.UserAgentSource,
		UserAgentSHA256: access.UserAgentSHA256,
	}
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
