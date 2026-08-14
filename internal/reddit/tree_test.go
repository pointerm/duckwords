package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestWalkDecodedTraversesNestedRepliesAndSkipsUnavailableBodies(t *testing.T) {
	t.Parallel()

	postID := "post1"
	deep := testComment("c3", "charlie", "t1_c2", "")
	deleted := testComment("c2", "[deleted]", "t1_c1", testListing(deep))
	removed := testComment("c4", "[removed]", "t1_c1", nil)
	top := testComment("c1", "alpha", "t3_"+postID, testListing(deleted, removed))
	empty := testComment("c5", " \n\t", "t3_"+postID, "")
	missingBody := testComment("c6", "unused", "t3_"+postID, nil)
	delete(missingBody["data"].(map[string]any), "body")

	var visited []Comment
	stats, err := walkDecoded(
		context.Background(),
		postID,
		testInitial(t, postID, top, empty, missingBody),
		unexpectedFetch(t),
		func(comment Comment) error {
			visited = append(visited, comment)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}

	want := []Comment{
		{ID: "c1", Fullname: "t1_c1", Body: "alpha"},
		{ID: "c3", Fullname: "t1_c3", Body: "charlie"},
	}
	if !slices.Equal(visited, want) {
		t.Fatalf("visited = %#v, want %#v", visited, want)
	}
	if stats.Things != 7 || stats.Comments != 6 || stats.BodiesVisited != 2 || stats.BodiesSkipped != 4 || stats.BodyBytes != 33 || stats.ResponseBytes <= 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestWalkDecodedDeduplicatesBodiesButTraversesDuplicateReplies(t *testing.T) {
	t.Parallel()

	postID := "post1"
	first := testComment("same", "first", "t3_"+postID, "")
	child := testComment("child", "child body", "t1_same", "")
	duplicate := testComment("same", "must not visit", "t3_"+postID, testListing(child))

	var bodies []string
	stats, err := walkDecoded(context.Background(), postID, testInitial(t, postID, first, duplicate), unexpectedFetch(t), func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}
	if !slices.Equal(bodies, []string{"first", "child body"}) {
		t.Fatalf("bodies = %q", bodies)
	}
	if stats.Comments != 2 || stats.DuplicateComments != 1 || stats.BodiesVisited != 2 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestWalkDecodedRequiresCommentReferences(t *testing.T) {
	t.Parallel()

	postID := "post1"
	comment := testComment("c1", "body", "t3_"+postID, "")
	data := comment["data"].(map[string]any)
	delete(data, "link_id")
	delete(data, "parent_id")

	_, err := walkDecoded(context.Background(), postID, testInitial(t, postID, comment), unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorProtocol)
}

func TestWalkDecodedTreatsEmptyHTTP200PostListingAsIncomplete(t *testing.T) {
	t.Parallel()

	payload := mustJSON(t, []any{testListing(), testListing()})
	_, err := walkDecoded(context.Background(), "post1", payload, unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorIncomplete)
	var adapterErr *Error
	if !errors.As(err, &adapterErr) || adapterErr.Endpoint != EndpointComments || adapterErr.StatusCode != 0 {
		t.Fatalf("error = %#v, want statusless initial-comments incomplete", adapterErr)
	}
}

func TestWalkDecodedExpandsMoreIDsIndividuallyInFirstSeenOrder(t *testing.T) {
	t.Parallel()

	postID := "post1"
	ids := make([]string, 105)
	for index := range ids {
		ids[index] = fmt.Sprintf("m%d", index)
	}
	children := append(append([]string(nil), ids...), ids[0])
	continuation := testMore(children, len(children), "t3_"+postID)

	var batches [][]string
	var bodies []string
	stats, err := walkDecoded(context.Background(), postID, testInitial(t, postID, continuation), func(_ context.Context, batch []string) ([]byte, error) {
		batches = append(batches, append([]string(nil), batch...))
		if len(batch) != 1 {
			t.Fatalf("focal expansion batch = %q, want exactly one ID", batch)
		}
		return testFocalResponse(t, postID, testComment(batch[0], "body "+batch[0], "t3_"+postID, "")), nil
	}, func(comment Comment) error {
		bodies = append(bodies, comment.Body)
		return nil
	})
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}

	if len(batches) != len(ids) {
		t.Fatalf("expansion calls = %d, want %d", len(batches), len(ids))
	}
	for index, batch := range batches {
		if !slices.Equal(batch, []string{ids[index]}) {
			t.Fatalf("expansion %d = %q, want %q", index, batch, ids[index])
		}
	}
	if len(bodies) != 105 || bodies[0] != "body m0" || bodies[104] != "body m104" {
		t.Fatalf("visited %d bodies, first/last = %q/%q", len(bodies), bodies[0], bodies[len(bodies)-1])
	}
	if stats.Things != 212 || stats.Comments != 105 || stats.BodiesVisited != 105 || stats.MoreIDs != 106 ||
		stats.UniqueMoreIDs != 105 || stats.DuplicateMoreIDs != 1 || stats.ExpansionRequests != 105 ||
		stats.BodyBytes != 835 || stats.ResponseBytes <= 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestWalkDecodedAcceptsUnrequestedMoreDescendants(t *testing.T) {
	t.Parallel()

	postID := "post1"
	requested := testComment("wanted", "wanted body", "t3_"+postID, testListing(
		testComment("extra", "extra body", "t1_wanted", ""),
	))
	var ids []string
	stats, err := walkDecoded(context.Background(), postID, testInitial(t, postID, testMore([]string{"wanted"}, 1, "t3_"+postID)), func(context.Context, []string) ([]byte, error) {
		return testFocalResponse(t, postID, requested), nil
	}, func(comment Comment) error {
		ids = append(ids, comment.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}
	if !slices.Equal(ids, []string{"wanted", "extra"}) {
		t.Fatalf("visited IDs = %q", ids)
	}
	if stats.Comments != 2 || stats.MoreIDs != 1 || stats.UniqueMoreIDs != 1 || stats.ExpansionRequests != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestWalkDecodedExpandsMoreReturnedByFocalListing(t *testing.T) {
	t.Parallel()

	postID := "post1"
	var batches [][]string
	var ids []string
	stats, err := walkDecoded(context.Background(), postID, testInitial(t, postID, testMore([]string{"first"}, 1, "t3_"+postID)), func(_ context.Context, batch []string) ([]byte, error) {
		batches = append(batches, append([]string(nil), batch...))
		switch len(batches) {
		case 1:
			return testFocalResponse(t, postID,
				testComment("first", "first body", "t3_"+postID, testListing(
					testMore([]string{"second"}, 1, "t1_first"),
				)),
			), nil
		case 2:
			return testFocalResponse(t, postID, testComment("second", "second body", "t1_first", "")), nil
		default:
			t.Fatalf("unexpected batch %q", batch)
			return nil, errors.New("unexpected batch")
		}
	}, func(comment Comment) error {
		ids = append(ids, comment.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}
	wantBatches := [][]string{{"first"}, {"second"}}
	if !slices.EqualFunc(batches, wantBatches, func(left, right []string) bool { return slices.Equal(left, right) }) ||
		!slices.Equal(ids, []string{"first", "second"}) {
		t.Fatalf("batches = %q, visited IDs = %q", batches, ids)
	}
	if stats.Things != 7 || stats.Comments != 2 || stats.BodiesVisited != 2 || stats.MoreIDs != 2 ||
		stats.UniqueMoreIDs != 2 || stats.ExpansionRequests != 2 || stats.BodyBytes != 21 || stats.ResponseBytes <= 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestWalkDecodedMoreCompletenessFailures(t *testing.T) {
	t.Parallel()

	postID := "post1"
	tests := []struct {
		name     string
		more     any
		response []byte
		fetchErr error
		want     ErrorClass
	}{
		{
			name:     "missing requested child",
			more:     testMore([]string{"wanted"}, 1, "t3_"+postID),
			response: testFocalResponse(t, postID, testComment("other", "body", "t3_"+postID, "")),
			want:     ErrorIncomplete,
		},
		{
			name:     "empty returned things",
			more:     testMore([]string{"wanted"}, 1, "t3_"+postID),
			response: testInitial(t, postID),
			want:     ErrorIncomplete,
		},
		{
			name:     "legacy API envelope",
			more:     testMore([]string{"wanted"}, 1, "t3_"+postID),
			response: testMoreResponse(t, []any{[]any{"BAD_REQUEST", "bad", "children"}}),
			want:     ErrorProtocol,
		},
		{
			name:     "continuation count without children",
			more:     testMore([]string{}, 1, "t3_"+postID),
			response: testMoreResponse(t, nil),
			want:     ErrorIncomplete,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			_, err := walkDecoded(context.Background(), postID, testInitial(t, postID, test.more), func(context.Context, []string) ([]byte, error) {
				calls++
				return test.response, test.fetchErr
			}, func(Comment) error { return nil })
			assertErrorClass(t, err, test.want)
			if test.name == "continuation count without children" && calls != 0 {
				t.Fatalf("fetch calls = %d, want 0", calls)
			}
		})
	}
}

func TestWalkDecodedMakesDepthTruncationExplicit(t *testing.T) {
	t.Parallel()

	postID := "post1"
	continuation := map[string]any{
		"kind": "more",
		"data": map[string]any{
			"id": "_", "name": "t1__", "count": 0,
			"children": []string{}, "parent_id": "t1_parent",
		},
	}
	parent := testComment("parent", "parent body", "t3_"+postID, testListing(continuation))
	called := false
	_, err := walkDecoded(
		context.Background(),
		postID,
		testInitial(t, postID, parent),
		func(context.Context, []string) ([]byte, error) {
			called = true
			return nil, nil
		},
		func(Comment) error { return nil },
	)
	assertErrorClass(t, err, ErrorIncomplete)
	if !errors.Is(err, errIncompleteTree) {
		t.Fatalf("walkDecoded() error = %v, want explicit incomplete-tree cause", err)
	}
	if called {
		t.Fatal("depth-truncation placeholder was incorrectly sent as a child expansion")
	}
}

func TestWalkDecodedCompleteContinuesDepthTruncatedBranch(t *testing.T) {
	t.Parallel()

	postID := "post1"
	continuation := map[string]any{
		"kind": "more",
		"data": map[string]any{
			"id": "_", "name": "t1__", "count": 0,
			"children": []string{}, "parent_id": "t1_parent",
		},
	}
	parent := testComment("parent", "parent body", "t1_ancestor", testListing(continuation))
	ancestor := testComment("ancestor", "ancestor body", "t3_"+postID, testListing(parent))
	var continuationIDs []string
	var visited []string
	stats, err := walkDecodedCompleteWithLimits(
		context.Background(), postID, testInitial(t, postID, ancestor), DefaultThingLimits(), unexpectedFetch(t),
		func(_ context.Context, parentID string) ([]byte, error) {
			continuationIDs = append(continuationIDs, parentID)
			return testInitial(t, postID,
				testComment("parent", "duplicate parent body", "t1_ancestor", testListing(
					testComment("child", "child body", "t1_parent", ""),
				)),
			), nil
		},
		func(comment Comment) error {
			visited = append(visited, comment.ID)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("walkDecodedCompleteWithLimits() error = %v", err)
	}
	if !slices.Equal(continuationIDs, []string{"parent"}) || !slices.Equal(visited, []string{"ancestor", "parent", "child"}) {
		t.Fatalf("continuations = %q, visited = %q", continuationIDs, visited)
	}
	if stats.Things != 7 || stats.Comments != 3 || stats.DuplicateComments != 1 || stats.ContinuationRequests != 1 ||
		stats.BodyBytes != int64(len("ancestor body")+len("parent body")+len("duplicate parent body")+len("child body")) {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestWalkDecodedCompleteRejectsDuplicateOnlyContinuation(t *testing.T) {
	t.Parallel()

	postID := "post1"
	continuation := map[string]any{
		"kind": "more",
		"data": map[string]any{
			"id": "_", "name": "t1__", "count": 0,
			"children": []string{}, "parent_id": "t1_parent",
		},
	}
	child := testComment("child", "child body", "t1_parent", "")
	parent := testComment("parent", "parent body", "t3_"+postID, testListing(child, continuation))
	_, err := walkDecodedCompleteWithLimits(
		context.Background(), postID, testInitial(t, postID, parent), DefaultThingLimits(), unexpectedFetch(t),
		func(context.Context, string) ([]byte, error) {
			return testInitial(t, postID,
				testComment("parent", "duplicate parent", "t3_"+postID, testListing(
					testComment("child", "duplicate child", "t1_parent", ""),
				)),
			), nil
		},
		func(Comment) error { return nil },
	)
	assertErrorClass(t, err, ErrorIncomplete)
	var adapterErr *Error
	if !errors.Is(err, errIncompleteTree) || !errors.As(err, &adapterErr) || adapterErr.Endpoint != EndpointContinuation {
		t.Fatalf("error = %v, want explicit continuation no-progress failure", err)
	}
}

func TestWalkDecodedCompleteContinuationFailures(t *testing.T) {
	t.Parallel()

	postID := "post1"
	continuation := map[string]any{
		"kind": "more",
		"data": map[string]any{
			"id": "_", "name": "t1__", "count": 0,
			"children": []string{}, "parent_id": "t1_parent",
		},
	}
	parent := testComment("parent", "body", "t3_"+postID, testListing(continuation))
	initial := testInitial(t, postID, parent)
	tests := []struct {
		name     string
		limits   ThingLimits
		fetch    continuationFetcher
		class    ErrorClass
		endpoint Endpoint
		cause    error
	}{
		{
			name:   "malformed focal response",
			limits: DefaultThingLimits(),
			fetch:  func(context.Context, string) ([]byte, error) { return []byte(`{`), nil },
			class:  ErrorProtocol, endpoint: EndpointContinuation,
		},
		{
			name:   "malformed replies value",
			limits: DefaultThingLimits(),
			fetch: func(context.Context, string) ([]byte, error) {
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, 7)), nil
			},
			class: ErrorProtocol, endpoint: EndpointContinuation,
		},
		{
			name:   "nested replies cursor",
			limits: DefaultThingLimits(),
			fetch: func(context.Context, string) ([]byte, error) {
				replies := testListing(testComment("child", "body", "t1_parent", ""))
				replies["data"].(map[string]any)["after"] = "next"
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, replies)), nil
			},
			class: ErrorIncomplete, endpoint: EndpointContinuation,
			cause: errIncompleteTree,
		},
		{
			name:   "no progress",
			limits: DefaultThingLimits(),
			fetch: func(context.Context, string) ([]byte, error) {
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, "")), nil
			},
			class: ErrorIncomplete, endpoint: EndpointContinuation,
			cause: errIncompleteTree,
		},
		{
			name:   "repeated sentinel",
			limits: DefaultThingLimits(),
			fetch: func(context.Context, string) ([]byte, error) {
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, testListing(continuation))), nil
			},
			class: ErrorIncomplete, endpoint: EndpointContinuation,
			cause: errIncompleteTree,
		},
		{
			name: "continuation post counts toward thing limit",
			limits: withThingLimits(DefaultThingLimits(), func(limits *ThingLimits) {
				// The initial t3, parent t1, and depth sentinel exactly consume
				// this budget. The continuation response's t3 must exceed it.
				limits.MaxThings = 3
				limits.MaxComments = 3
			}),
			fetch: func(context.Context, string) ([]byte, error) {
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, testListing(
					testComment("child", "body", "t1_parent", ""),
				))), nil
			},
			class: ErrorResourceLimit, endpoint: EndpointContinuation,
			cause: errThingLimit,
		},
		{
			name: "request limit",
			limits: withThingLimits(DefaultThingLimits(), func(limits *ThingLimits) {
				limits.MaxContinuationRequests = 1
			}),
			fetch: func(context.Context, string) ([]byte, error) {
				nested := map[string]any{
					"kind": "more",
					"data": map[string]any{
						"id": "_", "name": "t1__", "count": 0,
						"children": []string{}, "parent_id": "t1_child",
					},
				}
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, testListing(
					testComment("child", "body", "t1_parent", testListing(nested)),
				))), nil
			},
			class: ErrorResourceLimit, endpoint: EndpointContinuation,
			cause: errContinuationRequestLimit,
		},
		{
			name:   "canceled fetch",
			limits: DefaultThingLimits(),
			fetch:  func(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded },
			class:  ErrorCanceled, endpoint: EndpointContinuation,
			cause: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := walkDecodedCompleteWithLimits(context.Background(), postID, initial, test.limits, unexpectedFetch(t), test.fetch, func(Comment) error { return nil })
			assertErrorClass(t, err, test.class)
			var adapterErr *Error
			if !errors.As(err, &adapterErr) || adapterErr.Endpoint != test.endpoint {
				t.Fatalf("error = %v, want endpoint %q", err, test.endpoint)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("error = %v, want cause %v", err, test.cause)
			}
		})
	}
}

func TestWalkDecodedKeepsMoreStubBatchesSeparate(t *testing.T) {
	t.Parallel()

	postID := "post1"
	root := testComment("root", "root body", "t3_"+postID, testListing(
		testMore([]string{"nested"}, 1, "t1_root"),
	))
	var batches [][]string
	_, err := walkDecoded(
		context.Background(), postID,
		testInitial(t, postID, testMore([]string{"top"}, 1, "t3_"+postID), root),
		func(_ context.Context, ids []string) ([]byte, error) {
			batches = append(batches, append([]string(nil), ids...))
			parent := "t3_" + postID
			if ids[0] == "nested" {
				parent = "t1_root"
			}
			return testFocalResponse(t, postID, testComment(ids[0], "body", parent, "")), nil
		},
		func(Comment) error { return nil },
	)
	if err != nil {
		t.Fatalf("walkDecoded() error = %v", err)
	}
	want := [][]string{{"top"}, {"nested"}}
	if !slices.EqualFunc(batches, want, func(left, right []string) bool { return slices.Equal(left, right) }) {
		t.Fatalf("batches = %q, want %q", batches, want)
	}
}

func TestWalkDecodedRejectsRelationshipContradictions(t *testing.T) {
	t.Parallel()

	postID := "post1"
	tests := []struct {
		name    string
		initial func(*testing.T) []byte
		fetch   expansionFetcher
	}{
		{
			name: "duplicate comment under different parent",
			initial: func(t *testing.T) []byte {
				first := testComment("same", "first", "t3_"+postID, "")
				other := testComment("other", "other", "t3_"+postID, testListing(testComment("same", "second", "t1_other", "")))
				return testInitial(t, postID, first, other)
			},
			fetch: unexpectedFetch(t),
		},
		{
			name: "cycle",
			initial: func(t *testing.T) []byte {
				return testInitial(t, postID, testMore([]string{"wanted"}, 1, "t3_"+postID))
			},
			fetch: func(context.Context, []string) ([]byte, error) {
				return testFocalResponse(t, postID,
					testComment("wanted", "wanted", "t3_"+postID, testListing(
						testComment("one", "one", "t1_two", ""),
						testComment("two", "two", "t1_one", ""),
					)),
				), nil
			},
		},
		{
			name: "orphan parent",
			initial: func(t *testing.T) []byte {
				return testInitial(t, postID, testMore([]string{"wanted"}, 1, "t3_"+postID))
			},
			fetch: func(context.Context, []string) ([]byte, error) {
				return testFocalResponse(t, postID,
					testComment("wanted", "wanted", "t3_"+postID, testListing(
						testComment("child", "body", "t1_missing", ""),
					)),
				), nil
			},
		},
		{
			name: "same more child under different parents",
			initial: func(t *testing.T) []byte {
				parent := testComment("parent", "body", "t3_"+postID, testListing(testMore([]string{"wanted"}, 1, "t1_parent")))
				return testInitial(t, postID, testMore([]string{"wanted"}, 1, "t3_"+postID), parent)
			},
			fetch: unexpectedFetch(t),
		},
		{
			name: "requested child returned under wrong parent",
			initial: func(t *testing.T) []byte {
				return testInitial(t, postID, testMore([]string{"wanted"}, 1, "t3_"+postID))
			},
			fetch: func(context.Context, []string) ([]byte, error) {
				return testFocalResponse(t, postID, testComment("wanted", "body", "t1_other", "")), nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := walkDecoded(context.Background(), postID, test.initial(t), test.fetch, func(Comment) error { return nil })
			assertErrorClass(t, err, ErrorProtocol)
			if test.name == "cycle" || test.name == "orphan parent" {
				var adapterErr *Error
				if !errors.As(err, &adapterErr) || adapterErr.Endpoint != EndpointCommentExpansion {
					t.Fatalf("error = %v, want comment-expansion origin", err)
				}
			}
		})
	}
}

func TestValidateCommentGraphUsesStableInsertionOrder(t *testing.T) {
	postID := "post1"
	parents := map[string]string{
		"first":  "t1_missinga",
		"second": "t1_missingb",
	}
	origins := map[string]Endpoint{
		"first":  EndpointComments,
		"second": EndpointCommentExpansion,
	}

	for range 100 {
		endpoint, err := validateCommentGraph(context.Background(), parents, origins, []string{"first", "second"}, postID)
		if !errors.Is(err, errMalformedResponse) || endpoint != EndpointComments {
			t.Fatalf("first insertion order = endpoint %q, error %v", endpoint, err)
		}
		endpoint, err = validateCommentGraph(context.Background(), parents, origins, []string{"second", "first"}, postID)
		if !errors.Is(err, errMalformedResponse) || endpoint != EndpointCommentExpansion {
			t.Fatalf("second insertion order = endpoint %q, error %v", endpoint, err)
		}
	}
}

func TestWalkDecodedRejectsMalformedRegularMoreIdentityAndCount(t *testing.T) {
	t.Parallel()

	postID := "post1"
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing ID", mutate: func(data map[string]any) { delete(data, "id") }},
		{name: "name mismatch", mutate: func(data map[string]any) { data["name"] = "t1_other" }},
		{name: "ID not first child", mutate: func(data map[string]any) { data["id"], data["name"] = "other", "t1_other" }},
		{name: "zero count with children", mutate: func(data map[string]any) { data["count"] = 0 }},
		{name: "count below children", mutate: func(data map[string]any) { data["count"] = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			thing := testMore([]string{"one", "two"}, 2, "t3_"+postID)
			test.mutate(thing["data"].(map[string]any))
			_, err := walkDecoded(context.Background(), postID, testInitial(t, postID, thing), unexpectedFetch(t), func(Comment) error { return nil })
			assertErrorClass(t, err, ErrorProtocol)
		})
	}
}

func TestWalkDecodedRejectsMoreWithContradictoryParent(t *testing.T) {
	t.Parallel()

	postID := "post1"
	continuation := testMore([]string{"child"}, 1, "t1_other")
	parent := testComment("parent", "parent body", "t3_"+postID, testListing(continuation))
	_, err := walkDecoded(context.Background(), postID, testInitial(t, postID, parent), unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorProtocol)
}

func TestWalkDecodedRejectsMalformedProtocol(t *testing.T) {
	t.Parallel()

	postID := "post1"
	valid := testComment("c1", "body", "t3_"+postID, "")
	tests := []struct {
		name    string
		payload func(*testing.T) []byte
	}{
		{name: "empty response", payload: func(*testing.T) []byte { return nil }},
		{name: "single listing", payload: func(t *testing.T) []byte { return mustJSON(t, []any{testListing(testPost(postID))}) }},
		{name: "third listing", payload: func(t *testing.T) []byte {
			return mustJSON(t, []any{testListing(testPost(postID)), testListing(), testListing()})
		}},
		{name: "second JSON value", payload: func(t *testing.T) []byte { return append(testInitial(t, postID), []byte(" {}")...) }},
		{name: "invalid UTF-8", payload: func(t *testing.T) []byte { return append(testInitial(t, postID), 0xff) }},
		{name: "wrong post ID", payload: func(t *testing.T) []byte { return mustJSON(t, []any{testListing(testPost("other")), testListing()}) }},
		{name: "two post things", payload: func(t *testing.T) []byte {
			return mustJSON(t, []any{testListing(testPost(postID), testPost(postID)), testListing()})
		}},
		{name: "unknown kind", payload: func(t *testing.T) []byte {
			return testInitial(t, postID, map[string]any{"kind": "t9", "data": map[string]any{}})
		}},
		{name: "comment missing ID", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			delete(thing["data"].(map[string]any), "id")
			return testInitial(t, postID, thing)
		}},
		{name: "comment name mismatch", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			thing["data"].(map[string]any)["name"] = "t1_other"
			return testInitial(t, postID, thing)
		}},
		{name: "foreign link ID", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			thing["data"].(map[string]any)["link_id"] = "t3_other"
			return testInitial(t, postID, thing)
		}},
		{name: "wrong parent", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			thing["data"].(map[string]any)["parent_id"] = "t1_other"
			return testInitial(t, postID, thing)
		}},
		{name: "non-string body", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			thing["data"].(map[string]any)["body"] = 7
			return testInitial(t, postID, thing)
		}},
		{name: "missing replies", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			delete(thing["data"].(map[string]any), "replies")
			return testInitial(t, postID, thing)
		}},
		{name: "nonempty string replies", payload: func(t *testing.T) []byte {
			thing := cloneThing(t, valid)
			thing["data"].(map[string]any)["replies"] = "unexpected"
			return testInitial(t, postID, thing)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := walkDecoded(context.Background(), postID, test.payload(t), unexpectedFetch(t), func(Comment) error { return nil })
			assertErrorClass(t, err, ErrorProtocol)
		})
	}
}

func TestWalkDecodedListingCursorContract(t *testing.T) {
	t.Parallel()

	postID := "post1"
	t.Run("absent null and empty cursors are complete", func(t *testing.T) {
		t.Parallel()
		replies := testListing(testComment("child", "child body", "t1_parent", ""))
		replies["data"].(map[string]any)["after"] = nil
		replies["data"].(map[string]any)["before"] = ""
		parent := testComment("parent", "parent body", "t3_"+postID, replies)
		postListing := testListing(testPost(postID))
		postListing["data"].(map[string]any)["after"] = ""
		commentListing := testListing(parent)
		commentListing["data"].(map[string]any)["before"] = nil

		stats, err := walkDecoded(context.Background(), postID, mustJSON(t, []any{postListing, commentListing}), unexpectedFetch(t), func(Comment) error { return nil })
		if err != nil || stats.Comments != 2 {
			t.Fatalf("walkDecoded() stats = %#v, error = %v", stats, err)
		}
	})

	tests := []struct {
		name       string
		payload    func(*testing.T) []byte
		incomplete bool
	}{
		{
			name: "post listing after",
			payload: func(t *testing.T) []byte {
				postListing := testListing(testPost(postID))
				postListing["data"].(map[string]any)["after"] = "next"
				return mustJSON(t, []any{postListing, testListing()})
			},
			incomplete: true,
		},
		{
			name: "comment listing before",
			payload: func(t *testing.T) []byte {
				comments := testListing()
				comments["data"].(map[string]any)["before"] = "previous"
				return mustJSON(t, []any{testListing(testPost(postID)), comments})
			},
			incomplete: true,
		},
		{
			name: "nested replies after",
			payload: func(t *testing.T) []byte {
				replies := testListing(testComment("child", "body", "t1_parent", ""))
				replies["data"].(map[string]any)["after"] = "next"
				return testInitial(t, postID, testComment("parent", "body", "t3_"+postID, replies))
			},
			incomplete: true,
		},
		{
			name: "cursor has invalid type",
			payload: func(t *testing.T) []byte {
				comments := testListing()
				comments["data"].(map[string]any)["after"] = 7
				return mustJSON(t, []any{testListing(testPost(postID)), comments})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := walkDecoded(context.Background(), postID, test.payload(t), unexpectedFetch(t), func(Comment) error { return nil })
			wantClass := ErrorProtocol
			if test.incomplete {
				wantClass = ErrorIncomplete
			}
			assertErrorClass(t, err, wantClass)
			if got := errors.Is(err, errIncompleteTree); got != test.incomplete {
				t.Fatalf("errors.Is(incomplete) = %t, want %t (error %v)", got, test.incomplete, err)
			}
		})
	}
}

func TestWalkDecodedContinuationRejectsListingCursor(t *testing.T) {
	t.Parallel()

	postID := "post1"
	sentinel := map[string]any{"kind": "more", "data": map[string]any{
		"id": "_", "name": "t1__", "count": 0, "children": []string{}, "parent_id": "t1_parent",
	}}
	initial := testInitial(t, postID, testComment("parent", "body", "t3_"+postID, testListing(sentinel)))

	_, err := walkDecodedCompleteWithLimits(
		context.Background(), postID, initial, DefaultThingLimits(), unexpectedFetch(t),
		func(context.Context, string) ([]byte, error) {
			comments := testListing(testComment("parent", "body", "t3_"+postID, testListing(
				testComment("child", "body", "t1_parent", ""),
			)))
			comments["data"].(map[string]any)["after"] = "next"
			return mustJSON(t, []any{testListing(testPost(postID)), comments}), nil
		},
		func(Comment) error { return nil },
	)
	assertErrorClass(t, err, ErrorIncomplete)
	var adapterErr *Error
	if !errors.Is(err, errIncompleteTree) || !errors.As(err, &adapterErr) || adapterErr.Endpoint != EndpointContinuation {
		t.Fatalf("error = %v, want incomplete continuation", err)
	}
}

func TestWalkDecodedEnforcesLimits(t *testing.T) {
	t.Parallel()

	postID := "post1"
	defaults := DefaultThingLimits()
	tests := []struct {
		name    string
		limits  ThingLimits
		initial func(*testing.T) []byte
		fetch   expansionFetcher
		class   ErrorClass
	}{
		{
			name:    "invalid limits",
			limits:  ThingLimits{},
			initial: func(t *testing.T) []byte { return testInitial(t, postID) },
			fetch:   unexpectedFetch(t),
			class:   ErrorInvalidInput,
		},
		{
			name:    "things",
			limits:  withThingLimits(defaults, func(limits *ThingLimits) { limits.MaxThings = 1; limits.MaxComments = 1 }),
			initial: func(t *testing.T) []byte { return testInitial(t, postID, testComment("c1", "body", "t3_"+postID, "")) },
			fetch:   unexpectedFetch(t),
			class:   ErrorResourceLimit,
		},
		{
			name:   "comments",
			limits: withThingLimits(defaults, func(limits *ThingLimits) { limits.MaxComments = 1 }),
			initial: func(t *testing.T) []byte {
				return testInitial(t, postID, testComment("c1", "one", "t3_"+postID, ""), testComment("c2", "two", "t3_"+postID, ""))
			},
			fetch: unexpectedFetch(t),
			class: ErrorResourceLimit,
		},
		{
			name:   "more IDs",
			limits: withThingLimits(defaults, func(limits *ThingLimits) { limits.MaxMoreIDs = 1 }),
			initial: func(t *testing.T) []byte {
				return testInitial(t, postID, testMore([]string{"c1", "c2"}, 2, "t3_"+postID))
			},
			fetch: unexpectedFetch(t),
			class: ErrorResourceLimit,
		},
		{
			name:   "duplicate more IDs do not bypass limit",
			limits: withThingLimits(defaults, func(limits *ThingLimits) { limits.MaxMoreIDs = 1 }),
			initial: func(t *testing.T) []byte {
				return testInitial(t, postID, testMore([]string{"c1", "c1"}, 2, "t3_"+postID))
			},
			fetch: unexpectedFetch(t),
			class: ErrorResourceLimit,
		},
		{
			name:   "expansion requests",
			limits: withThingLimits(defaults, func(limits *ThingLimits) { limits.MaxExpansionRequests = 1 }),
			initial: func(t *testing.T) []byte {
				ids := make([]string, 2)
				for index := range ids {
					ids[index] = fmt.Sprintf("c%d", index)
				}
				return testInitial(t, postID, testMore(ids, len(ids), "t3_"+postID))
			},
			fetch: func(_ context.Context, ids []string) ([]byte, error) {
				return testFocalResponse(t, postID, testComment(ids[0], "body", "t3_"+postID, "")), nil
			},
			class: ErrorResourceLimit,
		},
		{
			name:    "body bytes",
			limits:  withThingLimits(defaults, func(limits *ThingLimits) { limits.MaxBodyBytes = 3 }),
			initial: func(t *testing.T) []byte { return testInitial(t, postID, testComment("c1", "four", "t3_"+postID, "")) },
			fetch:   unexpectedFetch(t),
			class:   ErrorResourceLimit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := walkDecodedWithLimits(context.Background(), postID, test.initial(t), test.limits, test.fetch, func(Comment) error { return nil })
			assertErrorClass(t, err, test.class)
		})
	}
}

func TestWalkDecodedDuplicateCommentsDoNotBypassLimit(t *testing.T) {
	t.Parallel()

	postID := "post1"
	limits := DefaultThingLimits()
	limits.MaxComments = 1
	duplicate := testComment("c1", "body", "t3_"+postID, "")
	_, err := walkDecodedWithLimits(context.Background(), postID, testInitial(t, postID, duplicate, duplicate), limits, unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorResourceLimit)
}

func TestThingLimitsValidationBoundaries(t *testing.T) {
	t.Parallel()

	validMaximum := ThingLimits{
		MaxThings:               absoluteMaxThings,
		MaxComments:             absoluteMaxComments,
		MaxMoreIDs:              absoluteMaxMoreIDs,
		MaxExpansionRequests:    absoluteMaxExpansionRequests,
		MaxContinuationRequests: absoluteMaxContinuationRequests,
		MaxBodyBytes:            absoluteMaxBodyBytes,
		MaxTotalBodyBytes:       absoluteMaxTotalBodyBytes,
		MaxTotalResponseBytes:   absoluteMaxTotalResponseBytes,
	}
	if err := validateThingLimits(validMaximum); err != nil {
		t.Fatalf("validateThingLimits(maximum) error = %v", err)
	}

	tests := []struct {
		name   string
		limits ThingLimits
	}{
		{name: "things zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxThings = 0 })},
		{name: "things too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxThings = absoluteMaxThings + 1 })},
		{name: "comments zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxComments = 0 })},
		{name: "comments too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxComments = absoluteMaxComments + 1 })},
		{name: "comments exceed things", limits: ThingLimits{MaxThings: 1, MaxComments: 2, MaxMoreIDs: 1, MaxExpansionRequests: 1, MaxContinuationRequests: 1, MaxBodyBytes: 1, MaxTotalBodyBytes: 1, MaxTotalResponseBytes: 1}},
		{name: "more IDs zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxMoreIDs = 0 })},
		{name: "more IDs too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxMoreIDs = absoluteMaxMoreIDs + 1 })},
		{name: "expansion requests zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxExpansionRequests = 0 })},
		{name: "expansion requests too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxExpansionRequests = absoluteMaxExpansionRequests + 1 })},
		{name: "continuation requests zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxContinuationRequests = 0 })},
		{name: "continuation requests too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxContinuationRequests = absoluteMaxContinuationRequests + 1 })},
		{name: "body zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxBodyBytes = 0 })},
		{name: "body too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxBodyBytes = absoluteMaxBodyBytes + 1 })},
		{name: "total body zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxTotalBodyBytes = 0 })},
		{name: "total body below body", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxTotalBodyBytes = int64(limits.MaxBodyBytes - 1) })},
		{name: "total body too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxTotalBodyBytes = absoluteMaxTotalBodyBytes + 1 })},
		{name: "total response zero", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxTotalResponseBytes = 0 })},
		{name: "total response too high", limits: withThingLimits(validMaximum, func(limits *ThingLimits) { limits.MaxTotalResponseBytes = absoluteMaxTotalResponseBytes + 1 })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateThingLimits(test.limits); !errors.Is(err, errInvalidThingLimits) {
				t.Fatalf("validateThingLimits() error = %v", err)
			}
		})
	}
}

func TestWalkDecodedAcceptsExactBodyLimit(t *testing.T) {
	t.Parallel()

	postID := "post1"
	limits := DefaultThingLimits()
	limits.MaxBodyBytes = len("four")
	visited := 0
	_, err := walkDecodedWithLimits(context.Background(), postID, testInitial(t, postID, testComment("c1", "four", "t3_"+postID, "")), limits, unexpectedFetch(t), func(Comment) error {
		visited++
		return nil
	})
	if err != nil || visited != 1 {
		t.Fatalf("walkDecodedWithLimits() error = %v, visited = %d", err, visited)
	}
}

func TestWalkDecodedBodyPreflightIgnoresUnconsumedMetadata(t *testing.T) {
	postID := "post1"
	post := testPost(postID)
	post["data"].(map[string]any)["body"] = strings.Repeat("x", 64)
	listing := testListing(testComment("c1", "ok", "t3_"+postID, ""))
	listing["metadata"] = map[string]any{"body": strings.Repeat("y", 64)}
	payload := mustJSON(t, []any{testListing(post), listing})
	limits := DefaultThingLimits()
	limits.MaxBodyBytes = 2
	limits.MaxTotalBodyBytes = int64(limits.MaxBodyBytes)

	visited := 0
	stats, err := walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error {
		visited++
		return nil
	})
	if err != nil || visited != 1 || stats.BodyBytes != 2 {
		t.Fatalf("stats = %#v, visited = %d, error = %v", stats, visited, err)
	}
}

func TestWalkDecodedPreflightUsesFinalDuplicateFields(t *testing.T) {
	postID := "post1"
	payload := []byte(`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"post1","name":"t3_post1"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t3","kind":"t1","data":{"body":7,"body":"ok","id":"c1","link_id":"t3_post1","name":"t1_c1","parent_id":"t3_post1","replies":""}}]}}]`)
	limits := DefaultThingLimits()
	limits.MaxComments = 1
	limits.MaxBodyBytes = 2
	limits.MaxTotalBodyBytes = int64(limits.MaxBodyBytes)

	visited := 0
	stats, err := walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error {
		visited++
		return nil
	})
	if err != nil || visited != 1 || stats.Comments != 1 || stats.BodyBytes != 2 {
		t.Fatalf("stats = %#v, visited = %d, error = %v", stats, visited, err)
	}
}

func TestWalkDecodedPreflightDoesNotCountOverwrittenCommentKind(t *testing.T) {
	payload := []byte(`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"post1","name":"t3_post1"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t1","kind":7,"data":{"body":"too long"}}]}}]`)
	limits := DefaultThingLimits()
	limits.MaxBodyBytes = 1
	limits.MaxTotalBodyBytes = int64(limits.MaxBodyBytes)

	stats, err := walkDecodedWithLimits(context.Background(), "post1", payload, limits, unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorProtocol)
	if errors.Is(err, errCommentBodyTooLarge) || errors.Is(err, errCommentBodiesTooLarge) || stats.Comments != 0 || stats.BodyBytes != 0 {
		t.Fatalf("stats = %#v, error = %v", stats, err)
	}
}

func TestWalkDecodedPreflightDoesNotCountOverwrittenCommentData(t *testing.T) {
	payload := []byte(`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"post1","name":"t3_post1"}}]}},{"kind":"Listing","data":{"children":[{"kind":"t1","data":{"body":"too long"},"data":null}]}}]`)
	limits := DefaultThingLimits()
	limits.MaxBodyBytes = 1
	limits.MaxTotalBodyBytes = int64(limits.MaxBodyBytes)

	stats, err := walkDecodedWithLimits(context.Background(), "post1", payload, limits, unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorProtocol)
	if errors.Is(err, errCommentBodyTooLarge) || errors.Is(err, errCommentBodiesTooLarge) || stats.BodyBytes != 0 {
		t.Fatalf("stats = %#v, error = %v", stats, err)
	}
}

func TestWalkDecodedEnforcesCumulativeBodyLimit(t *testing.T) {
	t.Parallel()

	postID := "post1"
	limits := DefaultThingLimits()
	limits.MaxBodyBytes = 4
	limits.MaxTotalBodyBytes = 7
	first := testComment("c1", "four", "t3_"+postID, "")
	second := testComment("c2", "four", "t3_"+postID, "")
	stats, err := walkDecodedWithLimits(context.Background(), postID, testInitial(t, postID, first, second), limits, unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorResourceLimit)
	if !errors.Is(err, errCommentBodiesTooLarge) || stats.BodyBytes != 0 {
		t.Fatalf("walkDecodedWithLimits() stats = %#v, error = %v", stats, err)
	}
}

func TestWalkDecodedEnforcesCumulativeResponseLimit(t *testing.T) {
	t.Parallel()

	postID := "post1"
	initial := testInitial(t, postID, testMore([]string{"child"}, 1, "t3_"+postID))
	response := testMoreResponse(t, nil, testComment("child", "body", "t3_"+postID, ""))
	limits := DefaultThingLimits()
	limits.MaxTotalResponseBytes = int64(len(initial) + len(response) - 1)
	stats, err := walkDecodedWithLimits(context.Background(), postID, initial, limits, func(context.Context, []string) ([]byte, error) {
		return response, nil
	}, func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorResourceLimit)
	if !errors.Is(err, errResponsesTooLarge) || stats.ResponseBytes != int64(len(initial)) {
		t.Fatalf("stats = %#v, error = %v", stats, err)
	}
}

func TestWalkDecodedPreservesCallbackErrorsAndCancellation(t *testing.T) {
	t.Parallel()

	postID := "post1"
	visitorCause := errors.New("visitor sentinel")
	_, err := walkDecoded(context.Background(), postID, testInitial(t, postID, testComment("c1", "body", "t3_"+postID, "")), unexpectedFetch(t), func(Comment) error {
		return visitorCause
	})
	assertErrorClass(t, err, ErrorVisitor)
	if !errors.Is(err, visitorCause) {
		t.Fatalf("visitor error does not unwrap cause: %v", err)
	}
	if strings.Contains(err.Error(), visitorCause.Error()) {
		t.Fatalf("formatted visitor error leaked callback text: %q", err)
	}

	fetchCause := errors.New("fetch sentinel")
	_, err = walkDecoded(context.Background(), postID, testInitial(t, postID, testMore([]string{"c1"}, 1, "t3_"+postID)), func(context.Context, []string) ([]byte, error) {
		return nil, fetchCause
	}, func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorTransport)
	if !errors.Is(err, fetchCause) {
		t.Fatalf("fetch error does not unwrap cause: %v", err)
	}
	if strings.Contains(err.Error(), fetchCause.Error()) {
		t.Fatalf("formatted fetch error leaked callback text: %q", err)
	}

	_, err = walkDecoded(context.Background(), postID, testInitial(t, postID, testMore([]string{"c1"}, 1, "t3_"+postID)), func(context.Context, []string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}, func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorCanceled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline fetch error does not unwrap cause: %v", err)
	}

	typedTransport := newError(ErrorTransport, EndpointCommentExpansion, postID, 0, context.DeadlineExceeded)
	_, err = walkDecoded(context.Background(), postID, testInitial(t, postID, testMore([]string{"c1"}, 1, "t3_"+postID)), func(context.Context, []string) ([]byte, error) {
		return nil, typedTransport
	}, func(Comment) error { return nil })
	if err != typedTransport {
		t.Fatalf("typed fetch error = %v, want original %v", err, typedTransport)
	}
	assertErrorClass(t, err, ErrorTransport)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("typed transport error does not retain deadline cause: %v", err)
	}

	fetchContext, cancelFetch := context.WithCancel(context.Background())
	typedAfterCancellation := newError(ErrorTransport, EndpointCommentExpansion, postID, 0, context.DeadlineExceeded)
	_, err = walkDecoded(fetchContext, postID, testInitial(t, postID, testMore([]string{"c1"}, 1, "t3_"+postID)), func(context.Context, []string) ([]byte, error) {
		cancelFetch()
		return nil, typedAfterCancellation
	}, func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorCanceled)
	if err == typedAfterCancellation || !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation did not override typed fetch error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err = walkDecoded(ctx, postID, testInitial(t, postID), func(context.Context, []string) ([]byte, error) {
		calls++
		return nil, nil
	}, func(Comment) error {
		calls++
		return nil
	})
	assertErrorClass(t, err, ErrorCanceled)
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled walk: error = %v, callback calls = %d", err, calls)
	}
}

func TestWalkDecodedPreflightsAllocationCardinality(t *testing.T) {
	const elements = 10_000
	postID := "post1"

	t.Run("initial listing things", func(t *testing.T) {
		payload := wideInitial(t, postID, elements)
		limits := DefaultThingLimits()
		limits.MaxThings = 2
		limits.MaxComments = 2
		var lastStats WalkStats
		var lastErr error
		allocations := testing.AllocsPerRun(5, func() {
			lastStats, lastErr = walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error { return nil })
		})
		assertErrorClass(t, lastErr, ErrorResourceLimit)
		if !errors.Is(lastErr, errThingLimit) || lastStats.Things != 0 {
			t.Fatalf("stats = %#v, error = %v", lastStats, lastErr)
		}
		// The preflight stops on the first element beyond the remaining budget;
		// decoding the complete 10k-element array would allocate orders of magnitude
		// more objects. Keep this threshold deliberately loose across Go releases.
		if allocations > 1_000 {
			t.Fatalf("allocations = %.0f, want bounded preflight below 1000", allocations)
		}
	})

	t.Run("initial comments independently", func(t *testing.T) {
		payload := wideInitial(t, postID, elements)
		limits := DefaultThingLimits()
		limits.MaxComments = 2
		var lastStats WalkStats
		var lastErr error
		allocations := testing.AllocsPerRun(5, func() {
			lastStats, lastErr = walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error { return nil })
		})
		assertErrorClass(t, lastErr, ErrorResourceLimit)
		if !errors.Is(lastErr, errCommentLimit) || lastStats.Comments != 0 {
			t.Fatalf("stats = %#v, error = %v", lastStats, lastErr)
		}
		if allocations > 1_000 {
			t.Fatalf("allocations = %.0f, want bounded preflight below 1000", allocations)
		}
	})

	t.Run("cumulative body bytes", func(t *testing.T) {
		payload := wideInitial(t, postID, elements)
		limits := DefaultThingLimits()
		limits.MaxBodyBytes = len("body")
		limits.MaxTotalBodyBytes = int64(2*len("body") - 1)
		var lastStats WalkStats
		var lastErr error
		allocations := testing.AllocsPerRun(5, func() {
			lastStats, lastErr = walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error { return nil })
		})
		assertErrorClass(t, lastErr, ErrorResourceLimit)
		if !errors.Is(lastErr, errCommentBodiesTooLarge) || lastStats.BodyBytes != 0 || lastStats.Things != 0 {
			t.Fatalf("stats = %#v, error = %v", lastStats, lastErr)
		}
		if allocations > 1_000 {
			t.Fatalf("allocations = %.0f, want bounded preflight below 1000", allocations)
		}
	})

	t.Run("more child IDs", func(t *testing.T) {
		ids := make([]string, elements)
		for index := range ids {
			ids[index] = fmt.Sprintf("c%d", index)
		}
		payload := testInitial(t, postID, testMore(ids, len(ids), "t3_"+postID))
		limits := DefaultThingLimits()
		limits.MaxThings = 2
		limits.MaxComments = 2
		limits.MaxMoreIDs = 2
		stats, err := walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error { return nil })
		assertErrorClass(t, err, ErrorResourceLimit)
		if !errors.Is(err, errMoreIDLimit) || stats.Things != 0 || stats.MoreIDs != 0 {
			t.Fatalf("stats = %#v, error = %v", stats, err)
		}
	})

	t.Run("legacy expansion envelope things", func(t *testing.T) {
		things := make([]any, elements)
		for index := range things {
			id := fmt.Sprintf("c%d", index)
			things[index] = testComment(id, "body", "t3_"+postID, "")
		}
		response := testMoreResponse(t, nil, things...)
		limits := DefaultThingLimits()
		limits.MaxThings = 3
		limits.MaxComments = 3
		stats, err := walkDecodedWithLimits(
			context.Background(), postID,
			testInitial(t, postID, testMore([]string{"c0"}, 1, "t3_"+postID)),
			limits,
			func(context.Context, []string) ([]byte, error) { return response, nil },
			func(Comment) error { return nil },
		)
		assertErrorClass(t, err, ErrorResourceLimit)
		var adapterErr *Error
		if !errors.Is(err, errThingLimit) || stats.Things != 2 || !errors.As(err, &adapterErr) || adapterErr.Endpoint != EndpointCommentExpansion {
			t.Fatalf("stats = %#v, error = %v", stats, err)
		}
	})
}

func TestPreflightRepresentativeAllocationDoesNotScalePerField(t *testing.T) {
	payload := wideInitial(t, "post1", 1_000)
	limits := DefaultThingLimits()
	var lastErr error
	allocations := testing.AllocsPerRun(10, func() {
		lastErr = preflightJSON(
			context.Background(), payload,
			limits.MaxThings, limits.MaxComments, limits.MaxMoreIDs,
			limits.MaxBodyBytes, limits.MaxTotalBodyBytes,
		)
	})
	if lastErr != nil {
		t.Fatalf("preflightJSON() error = %v", lastErr)
	}
	// The iterative scanner may allocate its bounded frame stack, but ordinary keys,
	// bodies, and scalar tokens must not allocate once per JSON field. The generous
	// ceiling leaves room for compiler/runtime changes while catching Token-based
	// regressions, which allocate thousands of values for this fixture.
	if allocations > 64 {
		t.Fatalf("preflight allocations = %.0f, want at most 64", allocations)
	}
}

func TestPreflightEscapedProtocolTokensDoNotAllocatePerToken(t *testing.T) {
	const comments = 1_000
	var payload strings.Builder
	payload.Grow(64 * comments)
	payload.WriteString(`{"child\u0072en":[`)
	for index := range comments {
		if index != 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"k\u0069nd":"t\u0031","d\u0061ta":{"b\u006fdy":"x"}}`)
	}
	payload.WriteString(`]}`)
	encoded := []byte(payload.String())

	var lastErr error
	allocations := testing.AllocsPerRun(10, func() {
		lastErr = preflightJSON(context.Background(), encoded, comments, comments, 1, 1, comments)
	})
	if lastErr != nil {
		t.Fatalf("preflightJSON() error = %v", lastErr)
	}
	if err := preflightJSON(context.Background(), encoded, comments, comments, 1, 0, comments); !errors.Is(err, errCommentBodyTooLarge) {
		t.Fatalf("escaped protocol tokens were not recognized: error = %v", err)
	}
	// Escaped recognized keys and kinds use a zero-copy ASCII comparator. A future
	// json.Unmarshal/string fallback would allocate once for each escaped token.
	if allocations > 64 {
		t.Fatalf("escaped-token preflight allocations = %.0f, want at most 64", allocations)
	}
}

func TestWalkDecodedPreflightsObjectFieldCardinality(t *testing.T) {
	const hostileFields = 10_000
	postID := "post1"
	var payload strings.Builder
	payload.Grow(64 + 20*hostileFields)
	payload.WriteString(`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{`)
	for index := range hostileFields {
		if index != 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `"field%d":null`, index)
	}
	payload.WriteString(`}}]}},{"kind":"Listing","data":{"children":[]}}]`)

	var lastStats WalkStats
	var lastErr error
	allocations := testing.AllocsPerRun(5, func() {
		lastStats, lastErr = walkDecoded(context.Background(), postID, []byte(payload.String()), unexpectedFetch(t), func(Comment) error { return nil })
	})
	assertErrorClass(t, lastErr, ErrorResourceLimit)
	if !errors.Is(lastErr, errThingLimit) || lastStats.Things != 0 {
		t.Fatalf("stats = %#v, error = %v", lastStats, lastErr)
	}
	// The preflight rejects the first field beyond the map budget before decodeOne
	// materializes the object. Keep the threshold loose across Go releases.
	if allocations > 5_000 {
		t.Fatalf("allocations = %.0f, want bounded preflight below 5000", allocations)
	}
}

func TestPreflightJSONEnforcesGlobalObjectFieldCardinality(t *testing.T) {
	var payload strings.Builder
	payload.Grow(12 * maxObjectFieldsPerResponse)
	payload.WriteByte('{')
	for branch := range 4 {
		if branch != 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `"branch%d":{`, branch)
		for leaf := range maxObjectFields {
			if leaf != 0 {
				payload.WriteByte(',')
			}
			fmt.Fprintf(&payload, `"leaf%d":{`, leaf)
			for field := range maxObjectFields {
				if field != 0 {
					payload.WriteByte(',')
				}
				fmt.Fprintf(&payload, `"field%d":null`, field)
			}
			payload.WriteByte('}')
		}
		payload.WriteByte('}')
	}
	payload.WriteByte('}')

	err := preflightJSON(
		context.Background(),
		[]byte(payload.String()),
		absoluteMaxThings,
		absoluteMaxComments,
		absoluteMaxMoreIDs,
		absoluteMaxBodyBytes,
		absoluteMaxTotalBodyBytes,
	)
	if !errors.Is(err, errThingLimit) {
		t.Fatalf("preflightJSON() error = %v, want errThingLimit", err)
	}
}

func TestWalkDecodedPreflightsJSONNesting(t *testing.T) {
	postID := "post1"
	var builder strings.Builder
	builder.Grow((maxJSONNestingDepth+1)*6 + 4)
	for range maxJSONNestingDepth + 1 {
		builder.WriteString(`{"x":`)
	}
	builder.WriteString("null")
	for range maxJSONNestingDepth + 1 {
		builder.WriteByte('}')
	}

	stats, err := walkDecoded(context.Background(), postID, []byte(builder.String()), unexpectedFetch(t), func(Comment) error { return nil })
	assertErrorClass(t, err, ErrorResourceLimit)
	if !errors.Is(err, errThingLimit) || stats.Things != 0 {
		t.Fatalf("stats = %#v, error = %v", stats, err)
	}
}

func TestPreflightJSONObservesCancellationDuringScan(t *testing.T) {
	payload := wideInitial(t, "post1", 10_000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := preflightJSON(ctx, payload, absoluteMaxThings, absoluteMaxComments, absoluteMaxMoreIDs, absoluteMaxBodyBytes, absoluteMaxTotalBodyBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("preflightJSON() error = %v, want context.Canceled", err)
	}
}

func TestPreflightNumberScanPollsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scanner := preflightScanner{ctx: ctx, payload: []byte(strings.Repeat("9", 16<<10))}
	if err := scanner.scanNumber(); !errors.Is(err, context.Canceled) {
		t.Fatalf("scanNumber() error = %v, want context.Canceled", err)
	}
}

func TestPreflightJSONAccountsDecodedEscapedBodyBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		bodyJSON  string
		bodyBytes int
	}{
		{name: "escaped ASCII", bodyJSON: `\u0061`, bodyBytes: 1},
		{name: "surrogate pair", bodyJSON: `\uD83E\uDD86`, bodyBytes: 4},
		{name: "unpaired surrogate replacement", bodyJSON: `\uD800`, bodyBytes: 3},
		{name: "control escape", bodyJSON: `line\n`, bodyBytes: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			payload := []byte(`{"children":[{"kind":"t1","data":{"body":"` + test.bodyJSON + `"}}]}`)
			if err := preflightJSON(context.Background(), payload, 1, 1, 1, test.bodyBytes, int64(test.bodyBytes)); err != nil {
				t.Fatalf("preflightJSON(exact) error = %v", err)
			}
			if err := preflightJSON(context.Background(), payload, 1, 1, 1, test.bodyBytes-1, int64(test.bodyBytes)); !errors.Is(err, errCommentBodyTooLarge) {
				t.Fatalf("preflightJSON(short) error = %v, want errCommentBodyTooLarge", err)
			}
		})
	}
}

func TestDecodeOneContextObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := decodeOneContext(ctx, []byte(`{"value":1}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeOneContext() error = %v, want context.Canceled", err)
	}
}

func TestContextAwareResponseDecodersPreserveCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	payload := testInitial(t, "post1")

	if _, err := decodeInitialContext(ctx, payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeInitialContext() error = %v, want context.Canceled", err)
	}
	if _, err := decodeExpansionResponseContext(ctx, payload, "post1", "c1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeExpansionResponseContext() error = %v, want context.Canceled", err)
	}
	if _, err := decodeContinuationResponseContext(ctx, payload, "post1", "c1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeContinuationResponseContext() error = %v, want context.Canceled", err)
	}

	for _, endpoint := range []Endpoint{EndpointComments, EndpointCommentExpansion, EndpointContinuation} {
		err := classifyTreeError(endpoint, "post1", context.Canceled)
		assertErrorClass(t, err, ErrorCanceled)
		var adapterErr *Error
		if !errors.Is(err, context.Canceled) || !errors.As(err, &adapterErr) || adapterErr.Endpoint != endpoint {
			t.Fatalf("classifyTreeError(%q) = %v", endpoint, err)
		}
	}
}

func TestWalkDecodedUsesIterativeTraversalAtDepth(t *testing.T) {
	t.Parallel()

	const depth = 512
	postID := "post1"
	payload := deepInitial(postID, depth)
	limits := DefaultThingLimits()
	limits.MaxThings = depth + 1
	limits.MaxComments = depth

	visited := 0
	stats, err := walkDecodedWithLimits(context.Background(), postID, payload, limits, unexpectedFetch(t), func(Comment) error {
		visited++
		return nil
	})
	if err != nil {
		t.Fatalf("walkDecodedWithLimits(depth=%d) error = %v", depth, err)
	}
	if visited != depth || stats.Comments != depth || stats.Things != depth+1 {
		t.Fatalf("visited = %d, stats = %#v", visited, stats)
	}
}

func TestWalkDecodedValidatesArguments(t *testing.T) {
	t.Parallel()

	valid := testInitial(t, "post1")
	tests := []struct {
		name  string
		ctx   context.Context
		post  string
		fetch expansionFetcher
		visit commentVisitor
	}{
		{name: "nil context", post: "post1", fetch: unexpectedFetch(t), visit: func(Comment) error { return nil }},
		{name: "invalid post ID", ctx: context.Background(), post: "POST-1", fetch: unexpectedFetch(t), visit: func(Comment) error { return nil }},
		{name: "nil fetch", ctx: context.Background(), post: "post1", visit: func(Comment) error { return nil }},
		{name: "nil visitor", ctx: context.Background(), post: "post1", fetch: unexpectedFetch(t)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := walkDecoded(test.ctx, test.post, valid, test.fetch, test.visit)
			assertErrorClass(t, err, ErrorInvalidInput)
		})
	}
}

func FuzzWalkDecoded(f *testing.F) {
	validInitial := testInitial(f, "post1", testComment("c1", "body", "t3_post1", ""))
	validMore := testMoreResponse(f, nil, testComment("c1", "body", "t3_post1", ""))
	depthSentinel := map[string]any{
		"kind": "more",
		"data": map[string]any{
			"id": "_", "name": "t1__", "count": 0,
			"children": []string{}, "parent_id": "t1_parent",
		},
	}
	depthInitial := testInitial(f, "post1", testComment("parent", "body", "t3_post1", testListing(depthSentinel)))
	validContinuation := testInitial(f, "post1", testComment("parent", "body", "t3_post1", testListing(
		testComment("child", "body", "t1_parent", ""),
	)))
	f.Add(validInitial, validMore, validContinuation)
	f.Add(depthInitial, validMore, validContinuation)
	f.Add([]byte("null"), []byte(`{"json":{"errors":[],"data":{"things":[]}}}`), []byte("null"))
	f.Add([]byte("["), []byte("{"), []byte("]"))

	limits := ThingLimits{MaxThings: 64, MaxComments: 32, MaxMoreIDs: 32, MaxExpansionRequests: 4, MaxContinuationRequests: 4, MaxBodyBytes: 256, MaxTotalBodyBytes: 4 << 10, MaxTotalResponseBytes: 8 << 10}
	f.Fuzz(func(t *testing.T, initial, more, continuation []byte) {
		stats, _ := walkDecodedCompleteWithLimits(context.Background(), "post1", initial, limits, func(context.Context, []string) ([]byte, error) {
			return more, nil
		}, func(context.Context, string) ([]byte, error) {
			return continuation, nil
		}, func(comment Comment) error {
			if !validCommentID(comment.ID) || comment.Fullname != "t1_"+comment.ID || len(comment.Body) > limits.MaxBodyBytes {
				t.Fatalf("invalid visited comment: %#v", comment)
			}
			return nil
		})
		if stats.Things > limits.MaxThings || stats.Comments > limits.MaxComments || stats.MoreIDs > limits.MaxMoreIDs ||
			stats.ExpansionRequests > limits.MaxExpansionRequests || stats.ContinuationRequests > limits.MaxContinuationRequests ||
			stats.BodyBytes > limits.MaxTotalBodyBytes || stats.ResponseBytes > limits.MaxTotalResponseBytes {
			t.Fatalf("stats exceed limits: %#v", stats)
		}
	})
}

func testInitial(tb testing.TB, postID string, children ...any) []byte {
	tb.Helper()
	return mustJSON(tb, []any{testListing(testPost(postID)), testListing(children...)})
}

func testPost(postID string) map[string]any {
	return map[string]any{"kind": "t3", "data": map[string]any{"id": postID, "name": "t3_" + postID}}
}

func testComment(id, body, parent string, replies any) map[string]any {
	return map[string]any{
		"kind": "t1",
		"data": map[string]any{
			"id": id, "name": "t1_" + id, "link_id": "t3_post1", "parent_id": parent,
			"body": body, "replies": replies,
		},
	}
}

func testMore(children []string, count int, parent string) map[string]any {
	id := "_"
	if len(children) > 0 {
		id = children[0]
	}
	return map[string]any{"kind": "more", "data": map[string]any{
		"id": id, "name": "t1_" + id, "children": children, "count": count, "parent_id": parent,
	}}
}

func testListing(children ...any) map[string]any {
	if children == nil {
		children = []any{}
	}
	return map[string]any{"kind": "Listing", "data": map[string]any{"children": children}}
}

func testMoreResponse(tb testing.TB, apiErrors []any, things ...any) []byte {
	tb.Helper()
	if apiErrors == nil {
		apiErrors = []any{}
	}
	return mustJSON(tb, map[string]any{"json": map[string]any{"errors": apiErrors, "data": map[string]any{"things": things}}})
}

func testFocalResponse(tb testing.TB, postID string, comment any) []byte {
	tb.Helper()
	return testInitial(tb, postID, comment)
}

func mustJSON(tb testing.TB, value any) []byte {
	tb.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		tb.Fatalf("json.Marshal() error = %v", err)
	}
	return payload
}

func cloneThing(tb testing.TB, thing map[string]any) map[string]any {
	tb.Helper()
	var cloned map[string]any
	if err := json.Unmarshal(mustJSON(tb, thing), &cloned); err != nil {
		tb.Fatalf("json.Unmarshal() error = %v", err)
	}
	return cloned
}

func unexpectedFetch(tb testing.TB) expansionFetcher {
	tb.Helper()
	return func(context.Context, []string) ([]byte, error) {
		tb.Errorf("unexpected focal expansion fetch")
		return nil, errors.New("unexpected fetch")
	}
}

func assertErrorClass(tb testing.TB, err error, want ErrorClass) {
	tb.Helper()
	var adapterErr *Error
	if !errors.As(err, &adapterErr) {
		tb.Fatalf("error = %v, want *Error", err)
	}
	if adapterErr.Class != want {
		tb.Fatalf("error class = %q, want %q (error %v)", adapterErr.Class, want, err)
	}
}

func withThingLimits(limits ThingLimits, update func(*ThingLimits)) ThingLimits {
	update(&limits)
	return limits
}

func deepInitial(postID string, depth int) []byte {
	var builder strings.Builder
	builder.Grow(depth*180 + 256)
	builder.WriteString(`[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"`)
	builder.WriteString(postID)
	builder.WriteString(`","name":"t3_`)
	builder.WriteString(postID)
	builder.WriteString(`"}}]}},{"kind":"Listing","data":{"children":[`)
	for index := 0; index < depth; index++ {
		id := fmt.Sprintf("c%d", index)
		parent := "t3_" + postID
		if index > 0 {
			parent = fmt.Sprintf("t1_c%d", index-1)
		}
		builder.WriteString(`{"kind":"t1","data":{"id":"`)
		builder.WriteString(id)
		builder.WriteString(`","name":"t1_`)
		builder.WriteString(id)
		builder.WriteString(`","link_id":"t3_`)
		builder.WriteString(postID)
		builder.WriteString(`","parent_id":"`)
		builder.WriteString(parent)
		builder.WriteString(`","body":"body","replies":`)
		if index+1 == depth {
			builder.WriteString(`""}}`)
		} else {
			builder.WriteString(`{"kind":"Listing","data":{"children":[`)
		}
	}
	for index := 1; index < depth; index++ {
		builder.WriteString(`]}}}}`)
	}
	builder.WriteString(`]}}]`)
	return []byte(builder.String())
}

func wideInitial(tb testing.TB, postID string, count int) []byte {
	tb.Helper()
	children := make([]any, count)
	for index := range children {
		children[index] = testComment(fmt.Sprintf("c%d", index), "body", "t3_"+postID, "")
	}
	return testInitial(tb, postID, children...)
}
