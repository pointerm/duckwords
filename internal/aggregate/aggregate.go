// Package aggregate merges post-local word counts and produces deterministic ranks.
package aggregate

import (
	"container/heap"
	"errors"
	"fmt"
	"math"
	"sort"
)

var (
	// ErrNilDestination indicates that merge output has no owned map.
	ErrNilDestination = errors.New("nil destination count map")
	// ErrMergeOverflow indicates that combined counts exceed uint64.
	ErrMergeOverflow = errors.New("merged word count overflow")
	// ErrInvalidTopLimit indicates a negative ranking limit.
	ErrInvalidTopLimit = errors.New("invalid top-N limit")
	// ErrInvalidCountEntry indicates a positive count with no word key.
	ErrInvalidCountEntry = errors.New("invalid count entry")
)

// WordCount is one JSON-ready ranked word and its occurrence count.
type WordCount struct {
	Word  string `json:"word"`
	Count uint64 `json:"count"`
}

// Merge atomically adds src into dst. It performs a complete validation pass before
// mutation, so overflow or malformed input cannot leave a partially merged post.
func Merge(dst, src map[string]uint64) error {
	if dst == nil {
		return ErrNilDestination
	}

	invalidEntry := false
	overflowWord := ""
	hasOverflow := false
	for word, count := range src {
		if word == "" && count > 0 {
			invalidEntry = true
			continue
		}
		if math.MaxUint64-dst[word] < count {
			if !hasOverflow || word < overflowWord {
				overflowWord = word
				hasOverflow = true
			}
		}
	}
	if invalidEntry {
		return ErrInvalidCountEntry
	}
	if hasOverflow {
		return fmt.Errorf("%w for %q", ErrMergeOverflow, overflowWord)
	}
	for word, count := range src {
		if count > 0 {
			dst[word] += count
		}
	}
	return nil
}

// TopN returns at most limit positive entries ordered by count descending and then
// word ascending. It retains only limit candidates while scanning, so selecting a
// small result from a large dictionary does not allocate or sort the whole map. It
// never depends on Go map iteration order.
func TopN(counts map[string]uint64, limit int) ([]WordCount, error) {
	if limit < 0 {
		return nil, ErrInvalidTopLimit
	}

	ranked := make(worstFirstHeap, 0, min(limit, len(counts)))
	for word, count := range counts {
		if count == 0 {
			continue
		}
		if word == "" {
			return nil, ErrInvalidCountEntry
		}
		if limit == 0 {
			continue
		}
		candidate := WordCount{Word: word, Count: count}
		if len(ranked) < limit {
			heap.Push(&ranked, candidate)
			continue
		}
		if betterRank(candidate, ranked[0]) {
			ranked[0] = candidate
			heap.Fix(&ranked, 0)
		}
	}

	sort.Slice(ranked, func(i, j int) bool {
		return betterRank(ranked[i], ranked[j])
	})
	if ranked == nil {
		return []WordCount{}, nil
	}
	return []WordCount(ranked), nil
}

func betterRank(left, right WordCount) bool {
	if left.Count != right.Count {
		return left.Count > right.Count
	}
	return left.Word < right.Word
}

// worstFirstHeap keeps the least desirable retained result at index zero so one
// better candidate can replace it without growing state with input cardinality.
type worstFirstHeap []WordCount

func (ranked worstFirstHeap) Len() int { return len(ranked) }
func (ranked worstFirstHeap) Less(left, right int) bool {
	return betterRank(ranked[right], ranked[left])
}
func (ranked worstFirstHeap) Swap(left, right int) {
	ranked[left], ranked[right] = ranked[right], ranked[left]
}
func (ranked *worstFirstHeap) Push(value any) { *ranked = append(*ranked, value.(WordCount)) }
func (ranked *worstFirstHeap) Pop() any {
	items := *ranked
	last := len(items) - 1
	value := items[last]
	items[last] = WordCount{}
	*ranked = items[:last]
	return value
}
