package aggregate

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

func TestMerge(t *testing.T) {
	t.Parallel()

	destination := map[string]uint64{"duck": 2, "zero": 0}
	source := map[string]uint64{"duck": 3, "bird": 4, "ignored": 0}

	if err := Merge(destination, source); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	want := map[string]uint64{"duck": 5, "bird": 4, "zero": 0}
	if !reflect.DeepEqual(destination, want) {
		t.Fatalf("destination = %#v, want %#v", destination, want)
	}
}

func TestMergeIsAtomicOnOverflow(t *testing.T) {
	t.Parallel()

	destination := map[string]uint64{"bird": 1, "duck": math.MaxUint64}
	source := map[string]uint64{"bird": 5, "duck": 1}
	want := map[string]uint64{"bird": 1, "duck": math.MaxUint64}

	err := Merge(destination, source)
	if !errors.Is(err, ErrMergeOverflow) {
		t.Fatalf("Merge() error = %v, want ErrMergeOverflow", err)
	}
	if !reflect.DeepEqual(destination, want) {
		t.Fatalf("destination changed after failed merge: %#v", destination)
	}
}

func TestMergeRejectsInvalidInputAtomically(t *testing.T) {
	t.Parallel()

	if err := Merge(nil, map[string]uint64{"duck": 1}); !errors.Is(err, ErrNilDestination) {
		t.Fatalf("Merge() error = %v, want ErrNilDestination", err)
	}

	destination := map[string]uint64{"duck": 1}
	err := Merge(destination, map[string]uint64{"bird": 2, "": 1})
	if !errors.Is(err, ErrInvalidCountEntry) {
		t.Fatalf("Merge() error = %v, want ErrInvalidCountEntry", err)
	}
	if !reflect.DeepEqual(destination, map[string]uint64{"duck": 1}) {
		t.Fatalf("destination changed after failed merge: %#v", destination)
	}
}

func TestMergeErrorClassificationIsDeterministic(t *testing.T) {
	t.Parallel()

	for range 100 {
		destination := map[string]uint64{"goose": math.MaxUint64, "duck": math.MaxUint64}
		source := map[string]uint64{"goose": 1, "duck": 1, "": 1}
		if err := Merge(destination, source); !errors.Is(err, ErrInvalidCountEntry) {
			t.Fatalf("Merge() error = %v, want ErrInvalidCountEntry", err)
		}

		delete(source, "")
		if err := Merge(destination, source); err == nil || err.Error() != `merged word count overflow for "duck"` {
			t.Fatalf("Merge() error = %v, want deterministic overflow word", err)
		}
	}
}

func TestTopNDeterministicOrdering(t *testing.T) {
	t.Parallel()

	counts := map[string]uint64{
		"water":    3,
		"duck":     7,
		"bird":     1,
		"goose":    1,
		"duckling": 1,
		"ignored":  0,
	}

	got, err := TopN(counts, 4)
	if err != nil {
		t.Fatalf("TopN() error = %v", err)
	}
	want := []WordCount{
		{Word: "duck", Count: 7},
		{Word: "water", Count: 3},
		{Word: "bird", Count: 1},
		{Word: "duckling", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TopN() = %#v, want %#v", got, want)
	}
}

func TestTopNBoundaries(t *testing.T) {
	t.Parallel()

	empty, err := TopN(nil, 10)
	if err != nil {
		t.Fatalf("TopN(nil) error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("TopN(nil) = %#v, want non-nil empty slice", empty)
	}
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal(empty) error = %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("json.Marshal(empty) = %s, want []", encoded)
	}

	zero, err := TopN(map[string]uint64{"duck": 1}, 0)
	if err != nil {
		t.Fatalf("TopN(limit=0) error = %v", err)
	}
	if zero == nil || len(zero) != 0 {
		t.Fatalf("TopN(limit=0) = %#v, want non-nil empty slice", zero)
	}

	if _, err := TopN(nil, -1); !errors.Is(err, ErrInvalidTopLimit) {
		t.Fatalf("TopN(limit=-1) error = %v, want ErrInvalidTopLimit", err)
	}
	if _, err := TopN(map[string]uint64{"": 1}, 10); !errors.Is(err, ErrInvalidCountEntry) {
		t.Fatalf("TopN(invalid) error = %v, want ErrInvalidCountEntry", err)
	}
}

func TestTopNDoesNotDependOnInsertionOrder(t *testing.T) {
	t.Parallel()

	words := []string{"wing", "bird", "pond", "goose", "duck"}
	var baseline []WordCount
	for rotation := range words {
		counts := make(map[string]uint64, len(words))
		for offset := range words {
			counts[words[(rotation+offset)%len(words)]] = 1
		}
		got, err := TopN(counts, len(words))
		if err != nil {
			t.Fatalf("TopN() error = %v", err)
		}
		if rotation == 0 {
			baseline = got
			continue
		}
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("rotation %d result = %#v, want %#v", rotation, got, baseline)
		}
	}
}

func TestTopNBoundedHeapMatchesFullSortReference(t *testing.T) {
	t.Parallel()

	random := rand.New(rand.NewPCG(0x746f706e, 0x68656170))
	for iteration := range 1_000 {
		entries := random.IntN(100)
		limit := random.IntN(20)
		counts := make(map[string]uint64, entries)
		for index := range entries {
			counts[benchmarkAggregateWord(iteration*100+index)] = random.Uint64N(20)
		}

		got, err := TopN(counts, limit)
		if err != nil {
			t.Fatalf("iteration %d TopN() error = %v", iteration, err)
		}
		want := make([]WordCount, 0, len(counts))
		for word, count := range counts {
			if count != 0 {
				want = append(want, WordCount{Word: word, Count: count})
			}
		}
		sort.Slice(want, func(left, right int) bool { return betterRank(want[left], want[right]) })
		want = want[:min(limit, len(want))]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d TopN() = %#v, want %#v", iteration, got, want)
		}
	}
}

func TestMergeAssociativeAndCommutative(t *testing.T) {
	t.Parallel()

	random := rand.New(rand.NewPCG(0x6475636b, 0x776f7264))
	for iteration := range 1_000 {
		parts := [3]map[string]uint64{}
		for part := range parts {
			parts[part] = make(map[string]uint64)
			for _, word := range []string{"bird", "duck", "goose", "pond", "water"} {
				parts[part][word] = random.Uint64N(1_000)
			}
		}

		left := make(map[string]uint64)
		mustMerge(t, left, parts[0], parts[1], parts[2])
		right := make(map[string]uint64)
		mustMerge(t, right, parts[2], parts[0], parts[1])
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("iteration %d is not commutative: left=%#v right=%#v", iteration, left, right)
		}

		firstPair := make(map[string]uint64)
		mustMerge(t, firstPair, parts[0], parts[1])
		associatedLeft := make(map[string]uint64)
		mustMerge(t, associatedLeft, firstPair, parts[2])
		secondPair := make(map[string]uint64)
		mustMerge(t, secondPair, parts[1], parts[2])
		associatedRight := make(map[string]uint64)
		mustMerge(t, associatedRight, parts[0], secondPair)
		if !reflect.DeepEqual(associatedLeft, associatedRight) {
			t.Fatalf("iteration %d is not associative: left=%#v right=%#v", iteration, associatedLeft, associatedRight)
		}
	}
}

func TestTopNDoesNotMutateCounts(t *testing.T) {
	t.Parallel()

	counts := map[string]uint64{"duck": 5, "water": 3, "bird": 1, "zero": 0}
	want := map[string]uint64{"duck": 5, "water": 3, "bird": 1, "zero": 0}
	for range 10 {
		if _, err := TopN(counts, 2); err != nil {
			t.Fatalf("TopN() error = %v", err)
		}
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("TopN() mutated input: got %#v, want %#v", counts, want)
	}
}

func mustMerge(t *testing.T, destination map[string]uint64, sources ...map[string]uint64) {
	t.Helper()

	for _, source := range sources {
		if err := Merge(destination, source); err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
	}
}
