package source

import (
	"errors"
	"strings"
	"testing"
)

func TestParsePostURLAcceptsSupportedPermalinks(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw      string
		wantID   string
		wantPath string
	}{
		"assignment old Reddit": {
			raw:      "https://old.reddit.com/r/duck/comments/AbC123/a_title/",
			wantID:   "abc123",
			wantPath: "/r/duck/comments/abc123/a_title/.json",
		},
		"root domain": {
			raw:      "https://reddit.com/r/duck/comments/abc123",
			wantID:   "abc123",
			wantPath: "/r/duck/comments/abc123/.json",
		},
		"new Reddit": {
			raw:      "https://new.reddit.com/r/Duck_Pictures/comments/ABC123/title",
			wantID:   "abc123",
			wantPath: "/r/duck_pictures/comments/abc123/title/.json",
		},
		"no subreddit": {
			raw:      "https://www.reddit.com/comments/abc123/title",
			wantID:   "abc123",
			wantPath: "/comments/abc123/title/.json",
		},
		"short link": {
			raw:      "https://redd.it/ABC123/",
			wantID:   "abc123",
			wantPath: "/comments/abc123/.json",
		},
		"JSON path already present": {
			raw:      "https://m.reddit.com/comments/ABC123/.json",
			wantID:   "abc123",
			wantPath: "/comments/abc123/.json",
		},
		"host and scheme case": {
			raw:      "HTTPS://OLD.REDDIT.COM/r/duck/comments/ABC123/title",
			wantID:   "abc123",
			wantPath: "/r/duck/comments/abc123/title/.json",
		},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ParsePostURL(test.raw)
			if err != nil {
				t.Fatalf("ParsePostURL() error = %v", err)
			}
			if got.ID != test.wantID || got.JSONPath != test.wantPath {
				t.Fatalf("ParsePostURL() = %#v, want ID=%q path=%q", got, test.wantID, test.wantPath)
			}
		})
	}
}

func TestParsePostURLRejectsUnsupportedOrUnsafeValues(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw    string
		reason PostURLReason
	}{
		"empty":                    {raw: "", reason: PostURLMalformed},
		"relative":                 {raw: "/r/duck/comments/abc123", reason: PostURLMalformed},
		"invalid UTF-8":            {raw: "https://reddit.com/r/duck/comments/abc\xff", reason: PostURLMalformed},
		"embedded whitespace":      {raw: "https://reddit.com/r/duck/comments/abc 123", reason: PostURLMalformed},
		"HTTP":                     {raw: "http://reddit.com/r/duck/comments/abc123", reason: PostURLUnsupportedScheme},
		"FTP":                      {raw: "ftp://reddit.com/r/duck/comments/abc123", reason: PostURLUnsupportedScheme},
		"host suffix":              {raw: "https://reddit.com.evil.test/r/duck/comments/abc123", reason: PostURLUnsupportedHost},
		"host prefix":              {raw: "https://old.reddit.com.evil/r/duck/comments/abc123", reason: PostURLUnsupportedHost},
		"trailing host dot":        {raw: "https://reddit.com./r/duck/comments/abc123", reason: PostURLUnsupportedHost},
		"unsupported subdomain":    {raw: "https://api.reddit.com/r/duck/comments/abc123", reason: PostURLUnsupportedHost},
		"credentials":              {raw: "https://user:secret@reddit.com/r/duck/comments/abc123", reason: PostURLUnsafe},
		"port":                     {raw: "https://reddit.com:443/r/duck/comments/abc123", reason: PostURLUnsafe},
		"empty port":               {raw: "https://reddit.com:/r/duck/comments/abc123", reason: PostURLUnsafe},
		"escaped slash":            {raw: "https://reddit.com/r/duck/comments%2Fabc123", reason: PostURLUnsafe},
		"escaped traversal":        {raw: "https://reddit.com/r/duck/comments/abc123/%2e%2e", reason: PostURLUnsafe},
		"query":                    {raw: "https://reddit.com/r/duck/comments/abc123/title?utm_source=share", reason: PostURLUnsafe},
		"empty query":              {raw: "https://reddit.com/r/duck/comments/abc123/title?", reason: PostURLUnsafe},
		"fragment":                 {raw: "https://reddit.com/r/duck/comments/abc123/title#context", reason: PostURLUnsafe},
		"missing path":             {raw: "https://reddit.com", reason: PostURLUnsupportedPath},
		"subreddit root":           {raw: "https://reddit.com/r/duck", reason: PostURLUnsupportedPath},
		"wrong route case":         {raw: "https://reddit.com/r/duck/COMMENTS/abc123", reason: PostURLUnsupportedPath},
		"missing subreddit":        {raw: "https://reddit.com/r//comments/abc123", reason: PostURLUnsupportedPath},
		"invalid subreddit":        {raw: "https://reddit.com/r/duck-pics/comments/abc123", reason: PostURLUnsupportedPath},
		"short link extra segment": {raw: "https://redd.it/abc123/title", reason: PostURLUnsupportedPath},
		"focal comment permalink":  {raw: "https://www.reddit.com/r/duck/comments/abc123/title/def456/", reason: PostURLUnsupportedPath},
		"empty ID":                 {raw: "https://reddit.com/r/duck/comments/", reason: PostURLUnsupportedPath},
		"non-base36 ID":            {raw: "https://reddit.com/r/duck/comments/abc-123", reason: PostURLInvalidID},
		"fullname not ID":          {raw: "https://reddit.com/r/duck/comments/t3_abc123", reason: PostURLInvalidID},
		"ID too long":              {raw: "https://redd.it/1234567890abcdefg", reason: PostURLInvalidID},
		"JSON without ID":          {raw: "https://redd.it/.json", reason: PostURLInvalidID},
		"ID dot JSON form":         {raw: "https://reddit.com/comments/abc123.json", reason: PostURLInvalidID},
		"ambiguous empty segment":  {raw: "https://reddit.com/r/duck/comments/abc123//title", reason: PostURLUnsupportedPath},
		"double leading slash":     {raw: "https://reddit.com//r/duck/comments/abc123", reason: PostURLUnsupportedPath},
		"double trailing slash":    {raw: "https://reddit.com/r/duck/comments/abc123//", reason: PostURLUnsupportedPath},
		"short link double slash":  {raw: "https://redd.it/abc123//", reason: PostURLUnsupportedPath},
	}

	for name, test := range tests {
		test := test
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := ParsePostURL(test.raw)
			if !errors.Is(err, ErrInvalidPostURL) {
				t.Fatalf("ParsePostURL() error = %v, want ErrInvalidPostURL", err)
			}
			var urlErr *PostURLError
			if !errors.As(err, &urlErr) {
				t.Fatalf("ParsePostURL() error type = %T, want *PostURLError", err)
			}
			if urlErr.Reason != test.reason {
				t.Fatalf("Reason = %v, want %v", urlErr.Reason, test.reason)
			}
			if test.raw != "" && strings.Contains(err.Error(), test.raw) {
				t.Fatalf("error leaked rejected URL: %q", err)
			}
		})
	}
}

func TestPostURLReasonStringHandlesUnknownValue(t *testing.T) {
	t.Parallel()

	if got := PostURLReason(255).String(); got != "unknown validation failure" {
		t.Fatalf("unknown reason String() = %q", got)
	}
	if got := (*PostURLError)(nil).Error(); got != ErrInvalidPostURL.Error() {
		t.Fatalf("nil PostURLError.Error() = %q", got)
	}
}

func FuzzParsePostURL(f *testing.F) {
	seeds := []string{
		"https://old.reddit.com/r/duck/comments/abc123/title/",
		"https://redd.it/ABC123?share_id=value#context",
		"https://user:secret@reddit.com/r/duck/comments/abc123",
		"https://evil.test/r/duck/comments/abc123",
		"\xff",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		parsed, err := ParsePostURL(raw)
		if err != nil {
			if !errors.Is(err, ErrInvalidPostURL) {
				t.Fatalf("unclassified error: %v", err)
			}
			return
		}
		if !validPostID(parsed.ID) || parsed.ID != strings.ToLower(parsed.ID) {
			t.Fatalf("accepted invalid normalized ID %q", parsed.ID)
		}
		if !strings.HasPrefix(parsed.JSONPath, "/") || !strings.HasSuffix(parsed.JSONPath, "/.json") ||
			strings.ContainsAny(parsed.JSONPath, "%?#\\") {
			t.Fatalf("accepted unsafe JSON path %q", parsed.JSONPath)
		}
	})
}
