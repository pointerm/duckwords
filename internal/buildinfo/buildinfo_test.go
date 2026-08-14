package buildinfo

import (
	"runtime"
	"testing"
)

func TestCurrentUsesRuntimeToolchain(t *testing.T) {
	t.Parallel()

	info := Current()
	if info.GoVersion != runtime.Version() {
		t.Fatalf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if info.Version == "" || info.Commit == "" || info.BuildDate == "" {
		t.Fatalf("Current() returned empty metadata: %+v", info)
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()

	info := Info{
		Version:   "1.2.3",
		Commit:    "abc123",
		BuildDate: "2026-08-13T12:00:00Z",
		GoVersion: "go1.26.6",
	}

	const want = "duckwords version=1.2.3 commit=abc123 built=2026-08-13T12:00:00Z go=go1.26.6"
	if got := info.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
