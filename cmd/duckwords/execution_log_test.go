package main

import (
	"strings"
	"testing"

	"github.com/pointerm/duckwords/internal/buildinfo"
	"github.com/pointerm/duckwords/internal/config"
)

func TestExecutionInputProfileBindsExactAssignmentLocators(t *testing.T) {
	t.Parallel()

	assignment := config.Config{
		Posts:      config.InputSource{Kind: config.SourceURL, Location: config.DefaultPostsURL},
		Dictionary: config.InputSource{Kind: config.SourceURL, Location: config.DefaultDictionaryURL},
	}
	if got := executionInputProfile(assignment); got != inputProfileAssignment {
		t.Fatalf("executionInputProfile(assignment) = %q, want %q", got, inputProfileAssignment)
	}

	overrides := []config.Config{
		func() config.Config { cfg := assignment; cfg.Posts.Location += "?other"; return cfg }(),
		func() config.Config { cfg := assignment; cfg.Dictionary.Location += ".bak"; return cfg }(),
		func() config.Config {
			cfg := assignment
			cfg.Posts = config.InputSource{Kind: config.SourceFile, Location: "posts.txt"}
			return cfg
		}(),
	}
	for _, cfg := range overrides {
		if got := executionInputProfile(cfg); got != inputProfileCustom {
			t.Errorf("executionInputProfile(%+v) = %q, want %q", cfg, got, inputProfileCustom)
		}
	}
}

func TestSafeLogBuildIdentityPreservesReleaseMetadata(t *testing.T) {
	t.Parallel()

	identity := safeLogBuildIdentity(buildinfo.Info{
		Version:   "1.2.3-rc.1",
		Commit:    strings.Repeat("a", 40),
		BuildDate: "2026-08-13T12:00:00Z",
		GoVersion: "go1.26.6",
	}, "linux", "arm64")

	if identity.version != "1.2.3-rc.1" || identity.commit != strings.Repeat("a", 40) ||
		identity.buildDate != "2026-08-13T12:00:00Z" || identity.goVersion != "go1.26.6" ||
		identity.goos != "linux" || identity.goarch != "arm64" {
		t.Fatalf("safeLogBuildIdentity() = %+v, want preserved release metadata", identity)
	}
}

func TestSafeLogBuildIdentityRejectsUncontrolledMetadata(t *testing.T) {
	t.Parallel()

	const planted = "planted-secret"
	identity := safeLogBuildIdentity(buildinfo.Info{
		Version:   "release\n" + planted,
		Commit:    "abc123\n" + planted,
		BuildDate: "2026-08-13T12:00:00Z\n" + planted,
		GoVersion: "go1.26.6\n" + planted,
	}, "linux/"+planted, "arm64\n"+planted)

	for name, value := range map[string]string{
		"version": identity.version, "commit": identity.commit,
		"build date": identity.buildDate, "Go version": identity.goVersion,
		"GOOS": identity.goos, "GOARCH": identity.goarch,
	} {
		if value != unknownBuildLogValue {
			t.Errorf("%s = %q, want %q", name, value, unknownBuildLogValue)
		}
		if strings.Contains(value, planted) {
			t.Errorf("%s exposed planted data: %q", name, value)
		}
	}
}

func TestSafeLogBuildIdentityBoundsEveryField(t *testing.T) {
	t.Parallel()

	identity := safeLogBuildIdentity(buildinfo.Info{
		Version:   strings.Repeat("v", maxSourceVersionBytes+1),
		Commit:    strings.Repeat("a", maxCommitLogBytes+1),
		BuildDate: "2026-08-13T12:00:00+03:00",
		GoVersion: strings.Repeat("g", maxGoVersionLogBytes+1),
	}, strings.Repeat("o", maxPlatformLogBytes+1), strings.Repeat("a", maxPlatformLogBytes+1))

	for name, value := range map[string]string{
		"version": identity.version, "commit": identity.commit,
		"build date": identity.buildDate, "Go version": identity.goVersion,
		"GOOS": identity.goos, "GOARCH": identity.goarch,
	} {
		if value != unknownBuildLogValue {
			t.Errorf("%s = %q, want bounded fallback %q", name, value, unknownBuildLogValue)
		}
	}
}
