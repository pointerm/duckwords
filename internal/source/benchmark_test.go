package source

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkLoadPostList(b *testing.B) {
	var input strings.Builder
	for i := 0; i < 10_000; i++ {
		fmt.Fprintf(&input, "https://old.reddit.com/r/duck/comments/%x/synthetic_title/%s\n", i+1, strings.Repeat("x", 16))
	}
	source := input.String()
	limits := DefaultPostListLimits()

	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list, _, err := LoadPostList(strings.NewReader(source), limits)
		if err != nil {
			b.Fatal(err)
		}
		if list.Len() != 10_000 {
			b.Fatalf("Len() = %d, want 10000", list.Len())
		}
	}
}
