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
	wantLookups := []string{
		envRedditUserAgent,
		envRedditBrowserCookie,
		envRedditBrowserAcceptLanguage,
		envRedditBrowserSecCHUA,
		envRedditBrowserSecCHUAMobile,
		envRedditBrowserSecCHUAPlatform,
	}
	if fmt.Sprint(lookups) != fmt.Sprint(wantLookups) {
		t.Fatalf("lookups = %v, want %v", lookups, wantLookups)
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

func TestResolveAccessIdentityAcceptsMinimalAndFullBrowserSessions(t *testing.T) {
	t.Parallel()

	const (
		cookie    = "reddit_session=private-canary; loid=abc123"
		userAgent = "Mozilla/5.0 DuckWords browser-session test"
	)
	tests := []struct {
		name   string
		values map[string]string
	}{
		{
			name: "minimal",
			values: map[string]string{
				envRedditBrowserCookie: cookie,
			},
		},
		{
			name: "full allowlist",
			values: map[string]string{
				envRedditUserAgent:              userAgent,
				envRedditBrowserCookie:          cookie,
				envRedditBrowserAcceptLanguage:  "en-US,en;q=0.9",
				envRedditBrowserSecCHUA:         `"Chromium";v="126", "Not.A/Brand";v="24"`,
				envRedditBrowserSecCHUAMobile:   "?0",
				envRedditBrowserSecCHUAPlatform: `"macOS"`,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			identity, err := ResolveAccessIdentity(mapLookup(test.values))
			if err != nil {
				t.Fatalf("ResolveAccessIdentity() error = %v", err)
			}
			if identity.Profile != RedditBrowserSessionAccessProfile ||
				identity.Auth != RedditBrowserSessionAuth || identity.browserHeaders() == nil ||
				!identity.browserHeaders().Valid() || !validAccessIdentity(identity) {
				t.Fatalf("identity = %+v, want valid browser-session identity", identity)
			}
			formatted := fmt.Sprintf("%v %#v", identity, identity)
			for _, secret := range []string{cookie, userAgent, "Chromium", "macOS"} {
				if strings.Contains(formatted, secret) {
					t.Fatalf("formatted identity leaked %q: %q", secret, formatted)
				}
			}
		})
	}
}

func TestResolveAccessIdentityRejectsInvalidBrowserEnvironmentWithoutEchoingValues(t *testing.T) {
	t.Parallel()

	const canary = "browser-private-canary"
	tests := []struct {
		name   string
		values map[string]string
	}{
		{name: "empty cookie", values: map[string]string{envRedditBrowserCookie: ""}},
		{name: "cookie prefix", values: map[string]string{envRedditBrowserCookie: "Cookie: session=" + canary}},
		{name: "cookie injection", values: map[string]string{envRedditBrowserCookie: "session=" + canary + "\r\nAuthorization: secret"}},
		{name: "cookie unicode", values: map[string]string{envRedditBrowserCookie: "session=" + canary + "-качка"}},
		{name: "cookie too large", values: map[string]string{envRedditBrowserCookie: "session=" + strings.Repeat("a", (16<<10)+1)}},
		{name: "header without cookie", values: map[string]string{envRedditBrowserAcceptLanguage: canary}},
		{name: "empty optional header", values: map[string]string{envRedditBrowserCookie: "session=ok", envRedditBrowserSecCHUA: ""}},
		{name: "header injection", values: map[string]string{envRedditBrowserCookie: "session=ok", envRedditBrowserAcceptLanguage: "en\r\nX-Secret: " + canary}},
		{name: "invalid mobile", values: map[string]string{envRedditBrowserCookie: "session=ok", envRedditBrowserSecCHUAMobile: canary}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			identity, err := ResolveAccessIdentity(mapLookup(test.values))
			if identity != (AccessIdentity{}) || !errors.Is(err, errEnvironmentConfig) {
				t.Fatalf("identity = %+v, error = %v", identity, err)
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("error leaked browser session value: %q", err)
			}
		})
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
	tampered = identity
	tampered.Profile = RedditBrowserSessionAccessProfile
	tampered.Auth = RedditBrowserSessionAuth
	if validAccessIdentity(tampered) {
		t.Fatal("browser-session metadata without a browser session was accepted")
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
