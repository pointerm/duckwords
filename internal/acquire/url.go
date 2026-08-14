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
	host, ok := allowedHost(kind)
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
	if parsed.Port() != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
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

func allowedHost(kind Kind) (string, bool) {
	switch kind {
	case KindPosts:
		return postsHost, true
	case KindDictionary:
		return dictionaryHost, true
	default:
		return "", false
	}
}
