package words

import (
	"errors"
	"maps"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCounterAddText(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "duck\nbird\nwater\n")
	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := map[string]uint64{"bird": 2}
	stats, err := counter.AddText("Duck duck, bird! unknown an water42", counts)
	if err != nil {
		t.Fatalf("AddText() error = %v", err)
	}
	want := map[string]uint64{"duck": 2, "bird": 3, "water": 1}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %#v, want %#v", counts, want)
	}
	if stats.Sequences != 6 || stats.EligibleTokens != 4 || stats.DictionaryMatches != 4 || stats.CountedTokens != 4 || stats.TooShort != 1 || stats.TooLong != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestCounterAppliesFilter(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "duck\nduckling\nbird\n")
	matcher, err := NewMatcher([]string{"duck*"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := make(map[string]uint64)
	stats, err := counter.AddText("duck duckling bird", counts)
	if err != nil {
		t.Fatalf("AddText() error = %v", err)
	}
	want := map[string]uint64{"duck": 1, "duckling": 1}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %#v, want %#v", counts, want)
	}
	if stats.DictionaryMatches != 3 || stats.CountedTokens != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestCounterErrors(t *testing.T) {
	t.Parallel()

	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	if _, err := NewCounter(Dictionary{}, matcher); !errors.Is(err, ErrEmptyDictionary) {
		t.Fatalf("NewCounter() error = %v, want ErrEmptyDictionary", err)
	}

	counter, err := NewCounter(mustLoadDictionary(t, "duck\n"), matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}
	if _, err := counter.AddText("duck", nil); !errors.Is(err, ErrNilCounts) {
		t.Fatalf("AddText() error = %v, want ErrNilCounts", err)
	}

	counts := map[string]uint64{"duck": math.MaxUint64}
	if _, err := counter.AddText("duck", counts); !errors.Is(err, ErrCountOverflow) {
		t.Fatalf("AddText() error = %v, want ErrCountOverflow", err)
	}
}

func TestCounterBoundsTokensByDictionary(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "duck\nbird\n")
	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}
	if counter.tokenizer.maxTokenRunes != 4 {
		t.Fatalf("counter token limit = %d, want longest dictionary word length 4", counter.tokenizer.maxTokenRunes)
	}

	counts := make(map[string]uint64)
	stats, err := counter.AddText(strings.Repeat("a", 100_000)+" duck", counts)
	if err != nil {
		t.Fatalf("AddText() error = %v", err)
	}
	if !reflect.DeepEqual(counts, map[string]uint64{"duck": 1}) || stats.TooLong != 1 {
		t.Fatalf("bounded count result = %#v, stats %+v", counts, stats)
	}
}

func TestCounterOverflowInvalidatesPostLocalMap(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "bird\nduck\n")
	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := map[string]uint64{"duck": math.MaxUint64}
	stats, err := counter.AddText("bird duck", counts)
	if !errors.Is(err, ErrCountOverflow) {
		t.Fatalf("AddText() error = %v, want ErrCountOverflow", err)
	}
	if counts["bird"] != 1 {
		t.Fatalf("counts = %#v, want earlier increment to demonstrate discard contract", counts)
	}
	if stats.Sequences != 2 || stats.EligibleTokens != 2 || stats.DictionaryMatches != 2 || stats.CountedTokens != 1 {
		t.Fatalf("overflow stats = %+v, want delivered candidates with one successful count", stats)
	}
}

func TestCounterDistinctWordLimit(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "bird\nduck\nwater\n")
	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := map[string]uint64{"duck": 2}
	stats, err := counter.AddTextLimited("DUCK bird", counts, 1)
	if !errors.Is(err, ErrDistinctWordLimit) {
		t.Fatalf("AddTextLimited() error = %v, want ErrDistinctWordLimit", err)
	}
	if strings.Contains(err.Error(), "bird") {
		t.Fatalf("AddTextLimited() error exposed input token: %v", err)
	}
	if got, want := counts, (map[string]uint64{"duck": 3}); !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %#v, want %#v", got, want)
	}
	if stats.Sequences != 2 || stats.EligibleTokens != 2 || stats.DictionaryMatches != 2 || stats.CountedTokens != 1 {
		t.Fatalf("limit stats = %+v, want both candidates with one successful count", stats)
	}
}

func TestCounterDistinctWordLimitOnUnicodeFallback(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "café\nduck\n")
	matcher, err := NewMatcher([]string{"*"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := map[string]uint64{"café": 1}
	stats, err := counter.AddTextLimited("CAFÉ duck", counts, 1)
	if !errors.Is(err, ErrDistinctWordLimit) {
		t.Fatalf("AddTextLimited() error = %v, want ErrDistinctWordLimit", err)
	}
	if got, want := counts, (map[string]uint64{"café": 2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %#v, want %#v", got, want)
	}
	if stats.Sequences != 2 || stats.EligibleTokens != 2 || stats.DictionaryMatches != 2 || stats.CountedTokens != 1 {
		t.Fatalf("Unicode limit stats = %+v, want both candidates with one successful count", stats)
	}
}

func TestCounterRejectsInvalidDistinctWordLimit(t *testing.T) {
	t.Parallel()

	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(mustLoadDictionary(t, "duck\n"), matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := make(map[string]uint64)
	for _, maximum := range []int{0, -1} {
		stats, limitErr := counter.AddTextLimited("duck", counts, maximum)
		if !errors.Is(limitErr, ErrInvalidDistinctWordLimit) {
			t.Fatalf("AddTextLimited(maximum %d) error = %v, want ErrInvalidDistinctWordLimit", maximum, limitErr)
		}
		if stats != (CountStats{}) || len(counts) != 0 {
			t.Fatalf("invalid limit mutated result: stats %+v, counts %#v", stats, counts)
		}
	}
}

func TestCounterRejectsInitiallyOverLimitMapWithoutMutation(t *testing.T) {
	t.Parallel()

	matcher, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(mustLoadDictionary(t, "bird\nduck\nwater\n"), matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	counts := map[string]uint64{"bird": 2, "duck": 3}
	want := maps.Clone(counts)
	stats, err := counter.AddTextLimited("duck", counts, 1)
	if !errors.Is(err, ErrDistinctWordLimit) {
		t.Fatalf("AddTextLimited() error = %v, want ErrDistinctWordLimit", err)
	}
	if stats != (CountStats{}) || !reflect.DeepEqual(counts, want) {
		t.Fatalf("over-limit input mutated result: stats %+v, counts %#v; want %#v", stats, counts, want)
	}
}

func TestCounterASCIIHotPathAllocations(t *testing.T) {
	dictionary := mustLoadDictionary(t, "duck\nduckling\nwater\n")
	matcher, err := NewMatcher([]string{"duck*"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	t.Run("dictionary misses", func(t *testing.T) {
		counts := make(map[string]uint64, 1)
		allocations := testing.AllocsPerRun(100, func() {
			if _, countErr := counter.AddText("unknown unknown unknown", counts); countErr != nil {
				t.Fatalf("AddText() error = %v", countErr)
			}
		})
		if allocations != 0 {
			t.Fatalf("allocations per miss-only scan = %g, want 0", allocations)
		}
	})

	t.Run("lowercase repeated hit", func(t *testing.T) {
		counts := map[string]uint64{"duck": 1}
		allocations := testing.AllocsPerRun(100, func() {
			if _, countErr := counter.AddText("duck duck duck", counts); countErr != nil {
				t.Fatalf("AddText() error = %v", countErr)
			}
		})
		if allocations != 0 {
			t.Fatalf("allocations per lowercase hit scan = %g, want 0", allocations)
		}
	})

	t.Run("mixed-case repeated hit", func(t *testing.T) {
		counts := map[string]uint64{"duck": 1}
		allocations := testing.AllocsPerRun(100, func() {
			if _, countErr := counter.AddText("DUCK Duck dUcK", counts); countErr != nil {
				t.Fatalf("AddText() error = %v", countErr)
			}
		})
		if allocations != 0 {
			t.Fatalf("allocations per mixed-case hit scan = %g, want 0", allocations)
		}
	})

	t.Run("first distinct hit in preallocated map", func(t *testing.T) {
		counts := make(map[string]uint64, 1)
		counts["warmup"] = 1
		clear(counts)
		allocations := testing.AllocsPerRun(100, func() {
			clear(counts)
			if _, countErr := counter.AddText("DUCK", counts); countErr != nil {
				t.Fatalf("AddText() error = %v", countErr)
			}
		})
		if allocations != 0 {
			t.Fatalf("allocations per first distinct hit = %g, want 0", allocations)
		}
	})
}

func FuzzCounterASCIIPathMatchesUnicodeFallback(f *testing.F) {
	for _, seed := range []struct {
		text  string
		limit uint8
	}{
		{text: "", limit: 1},
		{text: "DUCK Duck dUcK duckling", limit: 4},
		{text: "d_uck duck-water water42 bird's", limit: 3},
		{text: strings.Repeat("A", 128) + " duck", limit: 2},
		{text: "bird pond duck water wings", limit: 1},
		{text: "duck water pond", limit: 2},
	} {
		f.Add([]byte(seed.text), seed.limit)
	}

	dictionary, _, err := LoadDictionary(
		strings.NewReader("bird\nduck\nduckling\npond\nwater\nwings\n"),
		DefaultDictionaryLimits(),
	)
	if err != nil {
		f.Fatalf("LoadDictionary() error = %v", err)
	}
	allMatcher, err := NewMatcher(nil)
	if err != nil {
		f.Fatalf("NewMatcher(all) error = %v", err)
	}
	filteredMatcher, err := NewMatcher([]string{"d*", "water", "wings"})
	if err != nil {
		f.Fatalf("NewMatcher(filtered) error = %v", err)
	}
	allCounter, err := NewCounter(dictionary, allMatcher)
	if err != nil {
		f.Fatalf("NewCounter(all) error = %v", err)
	}
	filteredCounter, err := NewCounter(dictionary, filteredMatcher)
	if err != nil {
		f.Fatalf("NewCounter(filtered) error = %v", err)
	}

	f.Fuzz(func(t *testing.T, raw []byte, rawLimit uint8) {
		if len(raw) > 4<<10 {
			return
		}
		ascii := append([]byte(nil), raw...)
		for index := range ascii {
			ascii[index] &= 0x7f
		}
		text := string(ascii)
		limit := int(rawLimit%4) + 1

		for _, configured := range []struct {
			name    string
			counter Counter
		}{
			{name: "all", counter: allCounter},
			{name: "filtered", counter: filteredCounter},
		} {
			assertCounterPathsEqual(t, configured.name+"/unlimited", configured.counter, text, nil, 0)
			assertCounterPathsEqual(t, configured.name+"/limited", configured.counter, text, map[string]uint64{"duck": 1}, limit)
			assertCounterPathsEqual(t, configured.name+"/overflow", configured.counter, text, map[string]uint64{"duck": math.MaxUint64}, limit)
		}
	})
}

func assertCounterPathsEqual(
	t *testing.T,
	label string,
	counter Counter,
	text string,
	initial map[string]uint64,
	maxDistinct int,
) {
	t.Helper()
	optimized := maps.Clone(initial)
	reference := maps.Clone(initial)
	if optimized == nil {
		optimized = make(map[string]uint64)
		reference = make(map[string]uint64)
	}

	gotStats, gotErr := counter.addASCIIText(text, optimized, maxDistinct)
	wantStats, wantErr := counter.addTextUnicode(text, reference, maxDistinct)
	gotKind, wantKind := counterErrorKind(gotErr), counterErrorKind(wantErr)
	if gotKind == "unexpected" || wantKind == "unexpected" || gotKind != wantKind {
		t.Fatalf("%s: ASCII error = %v (%s), Unicode reference error = %v (%s)", label, gotErr, gotKind, wantErr, wantKind)
	}
	if gotStats != wantStats {
		t.Fatalf("%s: ASCII stats = %+v, Unicode reference stats = %+v", label, gotStats, wantStats)
	}
	if !reflect.DeepEqual(optimized, reference) {
		t.Fatalf("%s: ASCII counts = %#v, Unicode reference counts = %#v", label, optimized, reference)
	}
}

func counterErrorKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrDistinctWordLimit):
		return "distinct_word_limit"
	case errors.Is(err, ErrCountOverflow):
		return "count_overflow"
	default:
		return "unexpected"
	}
}

func TestCounterSupportsConcurrentReadersWithLocalMaps(t *testing.T) {
	t.Parallel()

	dictionary := mustLoadDictionary(t, "bird\nduck\nwater\n")
	matcher, err := NewMatcher([]string{"*"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	counter, err := NewCounter(dictionary, matcher)
	if err != nil {
		t.Fatalf("NewCounter() error = %v", err)
	}

	const workers = 32
	errorsByWorker := make(chan error, workers)
	results := make(chan map[string]uint64, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			counts := make(map[string]uint64)
			_, err := counter.AddText("Duck bird water duck", counts)
			errorsByWorker <- err
			results <- counts
		}()
	}
	group.Wait()
	close(errorsByWorker)
	close(results)

	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent AddText() error = %v", err)
		}
	}
	want := map[string]uint64{"duck": 2, "bird": 1, "water": 1}
	for counts := range results {
		if !reflect.DeepEqual(counts, want) {
			t.Fatalf("concurrent counts = %#v, want %#v", counts, want)
		}
	}
}

func mustLoadDictionary(t *testing.T, source string) Dictionary {
	t.Helper()

	dictionary, _, err := LoadDictionary(strings.NewReader(source), DefaultDictionaryLimits())
	if err != nil {
		t.Fatalf("LoadDictionary() error = %v", err)
	}
	return dictionary
}
