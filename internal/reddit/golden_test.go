package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPhase2GoldenFixtureVisitsCompleteTreeExactlyOnce(t *testing.T) {
	t.Parallel()

	initial := readPhase2Fixture(t, "initial.json")
	responses := map[string][]byte{
		"c4,c5": readPhase2Fixture(t, "more_c4_c5.json"),
		"c8":    readPhase2Fixture(t, "more_c8.json"),
	}
	var expected struct {
		Bodies []string `json:"bodies"`
		Stats  struct {
			Things            int `json:"things"`
			Comments          int `json:"comments"`
			BodiesVisited     int `json:"bodies_visited"`
			BodiesSkipped     int `json:"bodies_skipped"`
			DuplicateComments int `json:"duplicate_comments"`
			MoreIDs           int `json:"more_ids"`
			UniqueMoreIDs     int `json:"unique_more_ids"`
			DuplicateMoreIDs  int `json:"duplicate_more_ids"`
			MoreRequests      int `json:"more_requests"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(readPhase2Fixture(t, "expected.json"), &expected); err != nil {
		t.Fatalf("decode expected fixture: %v", err)
	}

	var bodies []string
	stats, err := walkDecoded(context.Background(), "duck123", initial, func(_ context.Context, ids []string) ([]byte, error) {
		response, ok := responses[strings.Join(ids, ",")]
		if !ok {
			return nil, errors.New("unexpected synthetic morechildren batch")
		}
		return response, nil
	}, func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}
	if !reflect.DeepEqual(bodies, expected.Bodies) {
		t.Fatalf("visited bodies = %#v, want %#v", bodies, expected.Bodies)
	}
	if stats.Things != expected.Stats.Things || stats.Comments != expected.Stats.Comments ||
		stats.BodiesVisited != expected.Stats.BodiesVisited || stats.BodiesSkipped != expected.Stats.BodiesSkipped ||
		stats.DuplicateComments != expected.Stats.DuplicateComments || stats.MoreIDs != expected.Stats.MoreIDs ||
		stats.UniqueMoreIDs != expected.Stats.UniqueMoreIDs || stats.DuplicateMoreIDs != expected.Stats.DuplicateMoreIDs ||
		stats.MoreRequests != expected.Stats.MoreRequests || stats.BodyBytes != 106 || stats.ResponseBytes <= 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestPhase2GoldenFixtureMakesMissingMoreChildExplicit(t *testing.T) {
	t.Parallel()

	var bodies []string
	stats, err := walkDecoded(
		context.Background(),
		"duck123",
		readPhase2Fixture(t, "initial.json"),
		func(context.Context, []string) ([]byte, error) {
			return readPhase2Fixture(t, "more_incomplete.json"), nil
		},
		func(comment Comment) error {
			bodies = append(bodies, comment.Body)
			return nil
		},
	)
	var adapterErr *Error
	if !errors.As(err, &adapterErr) || adapterErr.Class != ErrorIncomplete || adapterErr.Endpoint != EndpointMoreChildren {
		t.Fatalf("walkDecoded() error = %v, want explicit incomplete morechildren failure", err)
	}
	if !errors.Is(err, errIncompleteTree) {
		t.Fatalf("walkDecoded() error = %v, want incomplete-tree cause", err)
	}
	if stats.MoreRequests != 1 || len(bodies) == 0 {
		t.Fatalf("partial evidence = stats %#v, bodies %#v; fixture did not exercise discard boundary", stats, bodies)
	}
}

func readPhase2Fixture(tb testing.TB, name string) []byte {
	tb.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "testdata", "phase2", name))
	if err != nil {
		tb.Fatalf("read Phase 2 fixture %q: %v", name, err)
	}
	return payload
}
