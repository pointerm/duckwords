package production

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResolveAccessIdentityUsesBuiltinWithoutCredentials(t *testing.T) {
	t.Parallel()

	var lookups []string
	identity, err := ResolveAccessIdentity(func(name string) (string, bool) {
		lookups = append(lookups, name)
		return "", false
	})
	if err != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", err)
	}
	if len(lookups) != 1 || lookups[0] != envRedditUserAgent {
		t.Fatalf("lookups = %v, want only %s", lookups, envRedditUserAgent)
	}
	wantUserAgent := redditUserAgent()
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(wantUserAgent)))
	if identity.Profile != RedditAccessProfile || identity.Origin != RedditOrigin ||
		identity.Method != RedditMethod || identity.Auth != RedditAuth ||
		identity.UserAgentSource != userAgentSourceBuiltin ||
		identity.UserAgentSHA256 != wantDigest || identity.UserAgent() != wantUserAgent {
		t.Fatalf("identity = %+v, user agent = %q", identity, identity.UserAgent())
	}
}

func TestResolveAccessIdentityAcceptsValidatedOverride(t *testing.T) {
	t.Parallel()

	const override = "duckwords-test/1.0 (+https://github.com/pointerm/duckwords)"
	identity, err := ResolveAccessIdentity(mapLookup(map[string]string{envRedditUserAgent: override}))
	if err != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(override)))
	if identity.UserAgent() != override || identity.UserAgentSource != userAgentSourceOverride ||
		identity.UserAgentSHA256 != wantDigest {
		t.Fatalf("identity = %+v, user agent = %q", identity, identity.UserAgent())
	}
}

func TestResolveAccessIdentityRejectsUnsafeOverrideWithoutEchoingIt(t *testing.T) {
	t.Parallel()

	const canary = "planted-secret"
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty"},
		{name: "too short", value: "short"},
		{name: "leading whitespace", value: " " + canary},
		{name: "trailing whitespace", value: canary + " "},
		{name: "header injection", value: canary + "\r\nAuthorization: secret"},
		{name: "unicode", value: canary + "-качка"},
		{name: "oversized", value: strings.Repeat("a", maxUserAgentBytes+1)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			identity, err := ResolveAccessIdentity(mapLookup(map[string]string{envRedditUserAgent: test.value}))
			if identity != (AccessIdentity{}) || !errors.Is(err, errEnvironmentConfig) {
				t.Fatalf("identity = %+v, error = %v", identity, err)
			}
			if !strings.Contains(err.Error(), envRedditUserAgent) || strings.Contains(err.Error(), canary) {
				t.Fatalf("unsafe or unactionable error = %q", err)
			}
		})
	}
}

func TestResolveAccessIdentityRejectsNilLookup(t *testing.T) {
	t.Parallel()

	identity, err := ResolveAccessIdentity(nil)
	if identity != (AccessIdentity{}) || !errors.Is(err, errEnvironmentConfig) {
		t.Fatalf("identity = %+v, error = %v", identity, err)
	}
}

func TestContextWithAccessIdentityPreservesValidatedIdentity(t *testing.T) {
	t.Parallel()

	identity, err := ResolveAccessIdentity(mapLookup(nil))
	if err != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", err)
	}
	ctx := ContextWithAccessIdentity(context.Background(), identity)
	got, ok := accessIdentityFromContext(ctx)
	if !ok || got != identity || !validAccessIdentity(got) {
		t.Fatalf("context identity = %+v, present=%t", got, ok)
	}
	var nilContext context.Context
	if ContextWithAccessIdentity(nilContext, identity) != nil {
		t.Fatal("nil context was not preserved")
	}

	tampered := identity
	tampered.UserAgentSHA256 = strings.Repeat("0", 64)
	if validAccessIdentity(tampered) {
		t.Fatal("tampered identity was accepted")
	}
}

func TestLegacyRedditEnvironmentVariablesAreNeverRead(t *testing.T) {
	t.Parallel()

	legacy := map[string]bool{
		"REDDIT_API_ACCESS_APPROVED": true,
		"REDDIT_CLIENT_ID":           true,
		"REDDIT_CLIENT_SECRET":       true,
	}
	_, err := ResolveAccessIdentity(func(name string) (string, bool) {
		if legacy[name] {
			t.Fatalf("legacy environment variable %s was read", name)
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ResolveAccessIdentity() error = %v", err)
	}
}

func mapLookup(values map[string]string) environmentLookup {
	return func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
}
