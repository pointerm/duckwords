package main

import (
	"strings"
	"testing"
)

func TestSourceDownloadUserAgentIsGenericVersionedAndHeaderSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "development", version: "dev", want: "duckwords/dev"},
		{name: "release", version: "v1.2.3+build.4", want: "duckwords/v1.2.3+build.4"},
		{name: "empty", want: "duckwords/unknown"},
		{name: "whitespace", version: "1.0 beta", want: "duckwords/unknown"},
		{name: "header injection", version: "1.0\r\nAuthorization: secret", want: "duckwords/unknown"},
		{name: "oversized", version: strings.Repeat("a", maxSourceVersionBytes+1), want: "duckwords/unknown"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := sourceDownloadUserAgentForVersion(test.version); got != test.want {
				t.Fatalf("sourceDownloadUserAgentForVersion() = %q, want %q", got, test.want)
			}
		})
	}
}
