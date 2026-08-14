package production

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/pointerm/duckwords/internal/reddit"
)

const (
	envRedditUserAgent              = "REDDIT_USER_AGENT"
	envRedditBrowserCookie          = "REDDIT_BROWSER_COOKIE"
	envRedditBrowserAcceptLanguage  = "REDDIT_BROWSER_ACCEPT_LANGUAGE"
	envRedditBrowserSecCHUA         = "REDDIT_BROWSER_SEC_CH_UA"
	envRedditBrowserSecCHUAMobile   = "REDDIT_BROWSER_SEC_CH_UA_MOBILE"
	envRedditBrowserSecCHUAPlatform = "REDDIT_BROWSER_SEC_CH_UA_PLATFORM"

	// RedditAccessProfile identifies the fixed public-page JSON access contract.
	RedditAccessProfile = "old-reddit-public-json-v1"
	// RedditBrowserSessionAccessProfile identifies the optional personal-session
	// fallback. It is deliberately distinct from canonical anonymous evidence.
	RedditBrowserSessionAccessProfile = "old-reddit-browser-session-json-v1"
	// RedditOrigin is the only Reddit origin used by the production client.
	RedditOrigin = "old.reddit.com"
	// RedditMethod is the only HTTP method used for Reddit requests.
	RedditMethod = "GET"
	// RedditAuth records that public Reddit requests carry no authentication.
	RedditAuth = "none"
	// RedditBrowserSessionAuth records that a personal Cookie header is attached.
	RedditBrowserSessionAuth = "browser-session"

	userAgentSourceBuiltin  = "builtin"
	userAgentSourceOverride = "override"
	minUserAgentBytes       = 8
	maxUserAgentBytes       = 256
)

var errEnvironmentConfig = errors.New("invalid Reddit environment configuration")

type environmentLookup func(string) (string, bool)

type accessIdentityContextKey struct{}

// AccessIdentity binds the resolved HTTP identity to safe audit metadata. The
// actual User-Agent value is intentionally private so generic formatting cannot
// accidentally copy it into logs.
type AccessIdentity struct {
	Profile         string
	Origin          string
	Method          string
	Auth            string
	UserAgentSource string
	UserAgentSHA256 string
	userAgent       string
	browserSession  *reddit.BrowserSession
}

// String exposes only the already-sanitized audit metadata. Raw User-Agent and
// browser-session values remain redacted even under accidental generic formatting.
func (identity AccessIdentity) String() string {
	return fmt.Sprintf(
		"Reddit access profile=%s origin=%s method=%s auth=%s ua_source=%s ua_sha256=%s private=redacted",
		identity.Profile,
		identity.Origin,
		identity.Method,
		identity.Auth,
		identity.UserAgentSource,
		identity.UserAgentSHA256,
	)
}

// GoString applies the same redaction to %#v formatting.
func (identity AccessIdentity) GoString() string { return identity.String() }

// UserAgent returns the validated header value used for Reddit requests.
// Callers must not log this value; use UserAgentSource and UserAgentSHA256 instead.
func (identity AccessIdentity) UserAgent() string {
	return identity.userAgent
}

func (identity AccessIdentity) browserHeaders() *reddit.BrowserSession {
	return identity.browserSession
}

// ContextWithAccessIdentity hands one already resolved identity from the CLI to
// production composition. This prevents an environment mutation between lifecycle
// logging and request construction from changing the actual HTTP identity.
func ContextWithAccessIdentity(ctx context.Context, identity AccessIdentity) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, accessIdentityContextKey{}, identity)
}

func accessIdentityFromContext(ctx context.Context) (AccessIdentity, bool) {
	if ctx == nil {
		return AccessIdentity{}, false
	}
	identity, ok := ctx.Value(accessIdentityContextKey{}).(AccessIdentity)
	return identity, ok
}

// ResolveAccessIdentity reads the optional User-Agent and the narrow, explicit
// REDDIT_BROWSER_* allowlist. Legacy OAuth and approval variables are deliberately
// never queried or transmitted. Browser state is resolved once so later environment
// mutations cannot change either the logged identity or request headers.
func ResolveAccessIdentity(lookup func(string) (string, bool)) (AccessIdentity, error) {
	if lookup == nil {
		return AccessIdentity{}, fmt.Errorf("%w: environment lookup is required", errEnvironmentConfig)
	}
	userAgent := redditUserAgent()
	source := userAgentSourceBuiltin
	if override, present := lookup(envRedditUserAgent); present {
		if !validUserAgent(override) {
			return AccessIdentity{}, fmt.Errorf(
				"%w: %s must be %d..%d printable ASCII bytes without surrounding whitespace",
				errEnvironmentConfig,
				envRedditUserAgent,
				minUserAgentBytes,
				maxUserAgentBytes,
			)
		}
		userAgent = override
		source = userAgentSourceOverride
	}

	browserConfig, browserPresent, err := resolveBrowserSessionEnvironment(lookup)
	if err != nil {
		return AccessIdentity{}, err
	}
	profile := RedditAccessProfile
	auth := RedditAuth
	var browserSession *reddit.BrowserSession
	if browserPresent {
		browserSession, err = reddit.NewBrowserSession(browserConfig)
		if err != nil {
			return AccessIdentity{}, fmt.Errorf("%w: browser session headers are invalid", errEnvironmentConfig)
		}
		profile = RedditBrowserSessionAccessProfile
		auth = RedditBrowserSessionAuth
	}
	digest := sha256.Sum256([]byte(userAgent))
	return AccessIdentity{
		Profile:         profile,
		Origin:          RedditOrigin,
		Method:          RedditMethod,
		Auth:            auth,
		UserAgentSource: source,
		UserAgentSHA256: fmt.Sprintf("%x", digest),
		userAgent:       userAgent,
		browserSession:  browserSession,
	}, nil
}

func validAccessIdentity(identity AccessIdentity) bool {
	publicAccess := identity.Profile == RedditAccessProfile && identity.Auth == RedditAuth && identity.browserSession == nil
	browserAccess := identity.Profile == RedditBrowserSessionAccessProfile && identity.Auth == RedditBrowserSessionAuth &&
		identity.browserSession != nil && identity.browserSession.Valid()
	if (!publicAccess && !browserAccess) || identity.Origin != RedditOrigin || identity.Method != RedditMethod ||
		(identity.UserAgentSource != userAgentSourceBuiltin && identity.UserAgentSource != userAgentSourceOverride) ||
		!validUserAgent(identity.userAgent) {
		return false
	}
	digest := sha256.Sum256([]byte(identity.userAgent))
	return identity.UserAgentSHA256 == fmt.Sprintf("%x", digest)
}

func resolveBrowserSessionEnvironment(lookup environmentLookup) (reddit.BrowserSessionConfig, bool, error) {
	cookie, cookiePresent := lookup(envRedditBrowserCookie)
	config := reddit.BrowserSessionConfig{Cookie: cookie}
	optional := []struct {
		name   string
		target *string
	}{
		{name: envRedditBrowserAcceptLanguage, target: &config.AcceptLanguage},
		{name: envRedditBrowserSecCHUA, target: &config.SecCHUA},
		{name: envRedditBrowserSecCHUAMobile, target: &config.SecCHUAMobile},
		{name: envRedditBrowserSecCHUAPlatform, target: &config.SecCHUAPlatform},
	}
	optionalPresent := false
	for _, item := range optional {
		value, present := lookup(item.name)
		if !present {
			continue
		}
		optionalPresent = true
		if value == "" {
			return reddit.BrowserSessionConfig{}, false, fmt.Errorf(
				"%w: %s must not be empty when present",
				errEnvironmentConfig,
				item.name,
			)
		}
		*item.target = value
	}
	if !cookiePresent {
		if optionalPresent {
			return reddit.BrowserSessionConfig{}, false, fmt.Errorf(
				"%w: browser header variables require %s",
				errEnvironmentConfig,
				envRedditBrowserCookie,
			)
		}
		return reddit.BrowserSessionConfig{}, false, nil
	}
	return config, true, nil
}

func validUserAgent(value string) bool {
	if len(value) < minUserAgentBytes || len(value) > maxUserAgentBytes || value[0] == ' ' || value[len(value)-1] == ' ' {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	return true
}
