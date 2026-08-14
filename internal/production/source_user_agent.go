package production

import (
	"strings"

	"github.com/pointerm/duckwords/internal/buildinfo"
)

const (
	sourceUserAgentProduct         = "duckwords"
	sourceUserAgentFallbackVersion = "unknown"
	maxSourceVersionBytes          = 64
)

// sourceDownloadUserAgent returns a generic, versioned product identity for public
// assignment-file downloads. It deliberately excludes the Reddit contact identity,
// which belongs only on OAuth and Reddit API requests.
func sourceDownloadUserAgent() string {
	return sourceDownloadUserAgentForVersion(buildinfo.Current().Version)
}

// SourceDownloadUserAgent returns the sanitized identity used for public input downloads.
func SourceDownloadUserAgent() string {
	return sourceDownloadUserAgent()
}

func sourceDownloadUserAgentForVersion(version string) string {
	if !validSourceVersion(version) {
		version = sourceUserAgentFallbackVersion
	}
	return sourceUserAgentProduct + "/" + version
}

func validSourceVersion(version string) bool {
	if version == "" || len(version) > maxSourceVersionBytes || strings.TrimSpace(version) != version {
		return false
	}
	for _, char := range []byte(version) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || strings.ContainsRune("._-+", rune(char)) {
			continue
		}
		return false
	}
	return true
}
