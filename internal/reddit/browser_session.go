package reddit

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	maxBrowserCookieBytes   = 16 << 10
	maxAcceptLanguageBytes  = 256
	maxSecCHUABytes         = 512
	maxSecCHUAPlatformBytes = 64
	browserFetchDestination = "empty"
	browserFetchMode        = "cors"
	browserFetchSite        = "same-origin"
)

var errBrowserSessionConfig = errors.New("invalid browser session configuration")

// BrowserSessionConfig is the narrow allowlist of browser state DuckWords can add
// to a Reddit request. It deliberately has no arbitrary header map: Accept,
// Authorization, Host, forwarding, proxy, and entity headers remain owned by the
// client. Values are sensitive even though only Cookie normally carries authority.
type BrowserSessionConfig struct {
	Cookie          string
	AcceptLanguage  string
	SecCHUA         string
	SecCHUAMobile   string
	SecCHUAPlatform string
}

// String prevents accidental generic formatting from exposing session values.
func (BrowserSessionConfig) String() string { return "reddit browser session configuration (redacted)" }

// GoString prevents %#v from exposing session values.
func (BrowserSessionConfig) GoString() string { return "reddit.BrowserSessionConfig{redacted}" }

// BrowserSession is an immutable, validated browser header profile. The caller can
// pass it to any number of concurrent clients or attempts. It never owns a CookieJar
// and cannot learn or persist Set-Cookie response state.
type BrowserSession struct {
	cookie          string
	acceptLanguage  string
	secCHUA         string
	secCHUAMobile   string
	secCHUAPlatform string
}

// NewBrowserSession validates and copies the fixed browser-header allowlist.
// Errors identify only the invalid field and never include its value.
func NewBrowserSession(config BrowserSessionConfig) (*BrowserSession, error) {
	if !validBrowserCookie(config.Cookie) {
		return nil, fmt.Errorf("%w: Cookie must contain one or more bounded ASCII name=value pairs", errBrowserSessionConfig)
	}
	if !validOptionalBrowserHeader(config.AcceptLanguage, maxAcceptLanguageBytes) {
		return nil, fmt.Errorf("%w: Accept-Language must be bounded printable ASCII without surrounding whitespace", errBrowserSessionConfig)
	}
	if !validOptionalBrowserHeader(config.SecCHUA, maxSecCHUABytes) {
		return nil, fmt.Errorf("%w: Sec-CH-UA must be bounded printable ASCII without surrounding whitespace", errBrowserSessionConfig)
	}
	if config.SecCHUAMobile != "" && config.SecCHUAMobile != "?0" && config.SecCHUAMobile != "?1" {
		return nil, fmt.Errorf("%w: Sec-CH-UA-Mobile must be ?0 or ?1", errBrowserSessionConfig)
	}
	if !validOptionalBrowserHeader(config.SecCHUAPlatform, maxSecCHUAPlatformBytes) {
		return nil, fmt.Errorf("%w: Sec-CH-UA-Platform must be bounded printable ASCII without surrounding whitespace", errBrowserSessionConfig)
	}
	return &BrowserSession{
		cookie:          config.Cookie,
		acceptLanguage:  config.AcceptLanguage,
		secCHUA:         config.SecCHUA,
		secCHUAMobile:   config.SecCHUAMobile,
		secCHUAPlatform: config.SecCHUAPlatform,
	}, nil
}

// String prevents accidental generic formatting from exposing session values.
func (*BrowserSession) String() string { return "reddit browser session (redacted)" }

// GoString prevents %#v from exposing session values.
func (*BrowserSession) GoString() string { return "&reddit.BrowserSession{redacted}" }

// Valid reports whether session still satisfies the constructor invariants without
// exposing any of its values. It is intended for composition-boundary validation.
func (session *BrowserSession) Valid() bool { return session.valid() }

func (session *BrowserSession) valid() bool {
	if session == nil || !validBrowserCookie(session.cookie) ||
		!validOptionalBrowserHeader(session.acceptLanguage, maxAcceptLanguageBytes) ||
		!validOptionalBrowserHeader(session.secCHUA, maxSecCHUABytes) ||
		!validOptionalBrowserHeader(session.secCHUAPlatform, maxSecCHUAPlatformBytes) {
		return false
	}
	return session.secCHUAMobile == "" || session.secCHUAMobile == "?0" || session.secCHUAMobile == "?1"
}

func (session *BrowserSession) apply(request *http.Request) {
	if session == nil || request == nil {
		return
	}
	request.Header.Set("Cookie", session.cookie)
	if session.acceptLanguage != "" {
		request.Header.Set("Accept-Language", session.acceptLanguage)
	}
	if session.secCHUA != "" {
		request.Header.Set("Sec-CH-UA", session.secCHUA)
	}
	if session.secCHUAMobile != "" {
		request.Header.Set("Sec-CH-UA-Mobile", session.secCHUAMobile)
	}
	if session.secCHUAPlatform != "" {
		request.Header.Set("Sec-CH-UA-Platform", session.secCHUAPlatform)
	}
	request.Header.Set("Sec-Fetch-Dest", browserFetchDestination)
	request.Header.Set("Sec-Fetch-Mode", browserFetchMode)
	request.Header.Set("Sec-Fetch-Site", browserFetchSite)
}

func validBrowserCookie(value string) bool {
	if len(value) == 0 || len(value) > maxBrowserCookieBytes || strings.TrimSpace(value) != value {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	for _, rawPair := range strings.Split(value, ";") {
		pair := strings.TrimSpace(rawPair)
		name, cookieValue, found := strings.Cut(pair, "=")
		if !found || !validHTTPToken(name) || !validCookieValue(cookieValue) {
			return false
		}
	}
	return true
}

func validCookieValue(value string) bool {
	for index := range len(value) {
		character := value[index]
		// Browser Cookie request headers can contain opaque first-party values that
		// are broader than the Set-Cookie cookie-octet grammar (for example a compact
		// JSON preference value with quotes and commas). Header injection remains
		// impossible because only visible ASCII is accepted and semicolons have
		// already been consumed as pair delimiters by validBrowserCookie.
		if character < 0x20 || character > 0x7e || character == ';' {
			return false
		}
	}
	return true
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') {
			continue
		}
		switch character {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validOptionalBrowserHeader(value string, maximum int) bool {
	if value == "" {
		return true
	}
	if len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	return true
}
