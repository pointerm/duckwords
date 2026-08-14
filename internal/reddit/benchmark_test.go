package reddit

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func BenchmarkWalkDecoded(b *testing.B) {
	const comments = 1_000
	children := make([]any, comments)
	for index := range children {
		children[index] = testComment(fmt.Sprintf("c%d", index), "bounded benchmark body", "t3_post1", "")
	}
	payload := testInitial(b, "post1", children...)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		stats, err := walkDecoded(context.Background(), "post1", payload, unexpectedBenchmarkFetch, func(Comment) error { return nil })
		if err != nil || stats.Comments != comments {
			b.Fatalf("walkDecoded() stats = %#v, error = %v", stats, err)
		}
	}
}

func BenchmarkPreflightJSON(b *testing.B) {
	const comments = 1_000
	payload := wideInitial(b, "post1", comments)
	limits := DefaultThingLimits()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if err := preflightJSON(
			context.Background(), payload,
			limits.MaxThings, limits.MaxComments, limits.MaxMoreIDs,
			limits.MaxBodyBytes, limits.MaxTotalBodyBytes,
		); err != nil {
			b.Fatalf("preflightJSON() error = %v", err)
		}
	}
}

func BenchmarkWalkDecodedWithMore(b *testing.B) {
	const comments = 100
	descendants := make([]any, comments-1)
	for index := 1; index < comments; index++ {
		descendants[index-1] = testComment(fmt.Sprintf("c%d", index), "bounded benchmark body", "t1_c0", "")
	}
	initial := testInitial(b, "post1", testMore([]string{"c0"}, 1, "t3_post1"))
	response := testFocalResponse(b, "post1", testComment("c0", "bounded benchmark body", "t3_post1", testListing(descendants...)))
	b.ReportAllocs()
	b.SetBytes(int64(len(initial) + len(response)))
	b.ResetTimer()
	for range b.N {
		stats, err := walkDecoded(context.Background(), "post1", initial, func(context.Context, []string) ([]byte, error) {
			return response, nil
		}, func(Comment) error { return nil })
		if err != nil || stats.Comments != comments || stats.ExpansionRequests != 1 {
			b.Fatalf("walkDecoded() stats = %#v, error = %v", stats, err)
		}
	}
}

func BenchmarkWalkDecodedDeepReplies(b *testing.B) {
	const depth = 512
	payload := deepInitial("post1", depth)
	limits := DefaultThingLimits()
	limits.MaxThings = depth + 1
	limits.MaxComments = depth
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		stats, err := walkDecodedWithLimits(context.Background(), "post1", payload, limits, unexpectedBenchmarkFetch, func(Comment) error { return nil })
		if err != nil || stats.Comments != depth {
			b.Fatalf("walkDecodedWithLimits() stats = %#v, error = %v", stats, err)
		}
	}
}

func BenchmarkWalkDecodedRejectsWideResponseInPreflight(b *testing.B) {
	const comments = 10_000
	payload := wideInitial(b, "post1", comments)
	limits := DefaultThingLimits()
	limits.MaxThings = 2
	limits.MaxComments = 2
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		stats, err := walkDecodedWithLimits(context.Background(), "post1", payload, limits, unexpectedBenchmarkFetch, func(Comment) error { return nil })
		if !errors.Is(err, errThingLimit) || stats.Things != 0 {
			b.Fatalf("walkDecodedWithLimits() stats = %#v, error = %v", stats, err)
		}
	}
}

func unexpectedBenchmarkFetch(context.Context, []string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected benchmark fetch")
}
