package acquire

import (
	"net/url"
	"path"
	"strings"
)

const (
	postsHost      = "gist.githubusercontent.com"
	dictionaryHost = "raw.githubusercontent.com"
)

func validateRemoteURL(kind Kind, rawURL string) (*url.URL, error) {
	host, ok := AllowedHost(kind)
	if !ok {
		return nil, newLoadError(kind, ModeHTTPS, ErrInvalidSpec, 0, nil)
	}
	if len(rawURL) > maxSourceLocatorBytes || strings.IndexByte(rawURL, 0) >= 0 {
		return nil, newLoadError(kind, ModeHTTPS, ErrURLPolicy, 0, nil)
	}

	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed == nil {
		return nil, newLoadError(kind, ModeHTTPS, ErrURLPolicy, 0, nil)
	}
	if parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Host != host {
		return nil, newLoadError(kind, ModeHTTPS, ErrURLPolicy, 0, nil)
	}
	// A query string is part of a legitimate raw-gist or CDN link, so it is accepted
	// and forwarded verbatim. It is never logged or persisted: provenance records the
	// hostname only, and errors never format the requested URL. A fragment is dropped
	// by HTTP anyway, so accepting one would silently change the requested resource.
	if parsed.Port() != "" || parsed.Fragment != "" || len(parsed.RawQuery) > maxSourceQueryBytes {
		return nil, newLoadError(kind, ModeHTTPS, ErrURLPolicy, 0, nil)
	}
	if parsed.Path == "" || parsed.Path == "/" || parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
		return nil, newLoadError(kind, ModeHTTPS, ErrURLPolicy, 0, nil)
	}
	if !strings.HasPrefix(parsed.Path, "/") || strings.Contains(parsed.Path, "\\") || strings.Contains(parsed.Path, "//") || path.Clean(parsed.Path) != parsed.Path {
		return nil, newLoadError(kind, ModeHTTPS, ErrURLPolicy, 0, nil)
	}

	return parsed, nil
}

// AllowedHost returns the single HTTPS host from which a source of this kind may be
// downloaded. Configuration layers use it to reject an unusable URL with an accurate
// message instead of deferring to a generic acquisition failure.
func AllowedHost(kind Kind) (string, bool) {
	switch kind {
	case KindPosts:
		return postsHost, true
	case KindDictionary:
		return dictionaryHost, true
	default:
		return "", false
	}
}
