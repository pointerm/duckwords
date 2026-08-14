package production

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
)

const (
	envRedditUserAgent = "REDDIT_USER_AGENT"

	// RedditAccessProfile identifies the fixed public-page JSON access contract.
	RedditAccessProfile = "old-reddit-public-json-v1"
	// RedditOrigin is the only Reddit origin used by the production client.
	RedditOrigin = "old.reddit.com"
	// RedditMethod is the only HTTP method used for Reddit requests.
	RedditMethod = "GET"
	// RedditAuth records that public Reddit requests carry no authentication.
	RedditAuth = "none"

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
}

// UserAgent returns the validated header value used for public Reddit requests.
// Callers must not log this value; use UserAgentSource and UserAgentSHA256 instead.
func (identity AccessIdentity) UserAgent() string {
	return identity.userAgent
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

// ResolveAccessIdentity reads only the optional REDDIT_USER_AGENT override. Legacy
// OAuth and approval variables are deliberately never queried or transmitted.
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
	digest := sha256.Sum256([]byte(userAgent))
	return AccessIdentity{
		Profile:         RedditAccessProfile,
		Origin:          RedditOrigin,
		Method:          RedditMethod,
		Auth:            RedditAuth,
		UserAgentSource: source,
		UserAgentSHA256: fmt.Sprintf("%x", digest),
		userAgent:       userAgent,
	}, nil
}

func validAccessIdentity(identity AccessIdentity) bool {
	if identity.Profile != RedditAccessProfile || identity.Origin != RedditOrigin ||
		identity.Method != RedditMethod || identity.Auth != RedditAuth ||
		(identity.UserAgentSource != userAgentSourceBuiltin && identity.UserAgentSource != userAgentSourceOverride) ||
		!validUserAgent(identity.userAgent) {
		return false
	}
	digest := sha256.Sum256([]byte(identity.userAgent))
	return identity.UserAgentSHA256 == fmt.Sprintf("%x", digest)
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
