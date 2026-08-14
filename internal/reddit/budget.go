package reddit

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/semaphore"
)

const (
	// Two default-sized API responses may be read and decoded concurrently. Additional
	// walks wait before body allocation instead of multiplying transient response and
	// decoder state by the worker count.
	defaultMaxInFlightResponseBytes int64 = 32 << 20
	// A deliberate shared cardinality ceiling prevents four workers from independently
	// approaching their per-post comment and more-ID limits.
	defaultMaxRetainedThings = defaultMaxThings

	absoluteMaxInFlightResponseBytes int64 = 256 << 20
	absoluteMaxRetainedThings              = absoluteMaxThings
)

var (
	// ErrTraversalBudgetConfig identifies an invalid shared traversal budget.
	ErrTraversalBudgetConfig  = errors.New("invalid reddit traversal budget configuration")
	errInFlightResponseBudget = errors.New("reddit in-flight response budget exceeded")
	errRetainedThingBudget    = errors.New("reddit retained traversal-state budget exceeded")
)

// TraversalBudgetConfig bounds response admission and retained graph state across
// all concurrent walks that share a TraversalBudget. MaxInFlightResponseBytes
// counts the configured response-byte capacity reserved before each accepted body
// is read and held until its decoded tree is consumed. It is not an estimate of Go
// heap use. MaxRetainedThings counts first insertions into the retained comment,
// more-ID, and continuation sets until their walk returns; one reserved record may
// have entries in multiple indexes.
type TraversalBudgetConfig struct {
	MaxInFlightResponseBytes int64
	MaxRetainedThings        int
}

// DefaultTraversalBudgetConfig returns a process-conservative traversal budget. It
// permits two default-sized responses to be read/decoded concurrently and caps the
// total retained traversal cardinality below worker-multiplied per-post ceilings.
func DefaultTraversalBudgetConfig() TraversalBudgetConfig {
	return TraversalBudgetConfig{
		MaxInFlightResponseBytes: defaultMaxInFlightResponseBytes,
		MaxRetainedThings:        defaultMaxRetainedThings,
	}
}

// TraversalBudget is a concurrency-safe weighted budget shared by Reddit clients or
// walks. Construct one instance and inject it wherever a process owns more than one
// Client; a single Client shares its configured instance automatically.
type TraversalBudget struct {
	responseBytes *semaphore.Weighted
	maxResponse   int64

	retainedMu  sync.Mutex
	retained    int
	maxRetained int
}

// NewTraversalBudget validates config and constructs an empty shared budget.
func NewTraversalBudget(config TraversalBudgetConfig) (*TraversalBudget, error) {
	if config.MaxInFlightResponseBytes <= 0 || config.MaxInFlightResponseBytes > absoluteMaxInFlightResponseBytes {
		return nil, fmt.Errorf(
			"%w: MaxInFlightResponseBytes must be between 1 and %d",
			ErrTraversalBudgetConfig,
			absoluteMaxInFlightResponseBytes,
		)
	}
	if config.MaxRetainedThings <= 0 || config.MaxRetainedThings > absoluteMaxRetainedThings {
		return nil, fmt.Errorf(
			"%w: MaxRetainedThings must be between 1 and %d",
			ErrTraversalBudgetConfig,
			absoluteMaxRetainedThings,
		)
	}
	return &TraversalBudget{
		responseBytes: semaphore.NewWeighted(config.MaxInFlightResponseBytes),
		maxResponse:   config.MaxInFlightResponseBytes,
		maxRetained:   config.MaxRetainedThings,
	}, nil
}

// acquireResponse waits for the requested response-capacity weight. A request that
// can never fit fails immediately; ordinary contention waits fairly inside
// semaphore.Weighted and remains cancelable through the caller's context.
func (budget *TraversalBudget) acquireResponse(ctx context.Context, bytes int64) (func(), error) {
	if budget == nil || budget.responseBytes == nil || ctx == nil || bytes < 0 {
		return nil, ErrTraversalBudgetConfig
	}
	weight := bytes
	if weight > budget.maxResponse {
		return nil, errInFlightResponseBudget
	}
	if err := budget.responseBytes.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			budget.responseBytes.Release(weight)
		})
	}, nil
}

// reserveRetained fails closed instead of waiting. A walk progressively discovers
// graph nodes, so waiting while holding earlier reservations could deadlock all
// active walks. The walk lease releases every successful reservation on return.
func (budget *TraversalBudget) reserveRetained(additional int) error {
	if budget == nil || additional < 0 {
		return ErrTraversalBudgetConfig
	}
	budget.retainedMu.Lock()
	defer budget.retainedMu.Unlock()
	if additional > budget.maxRetained-budget.retained {
		return errRetainedThingBudget
	}
	budget.retained += additional
	return nil
}

func (budget *TraversalBudget) releaseRetained(count int) {
	if budget == nil || count <= 0 {
		return
	}
	budget.retainedMu.Lock()
	budget.retained -= count
	budget.retainedMu.Unlock()
}

type retainedLease struct {
	budget *TraversalBudget
	count  int
}

func (lease *retainedLease) reserve() error {
	if lease == nil || lease.budget == nil {
		return ErrTraversalBudgetConfig
	}
	if err := lease.budget.reserveRetained(1); err != nil {
		return err
	}
	lease.count++
	return nil
}

func (lease *retainedLease) release() {
	if lease == nil || lease.budget == nil {
		return
	}
	lease.budget.releaseRetained(lease.count)
	lease.count = 0
}
