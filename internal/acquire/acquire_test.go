package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testUserAgent = "duckwords/test"

func TestLoadErrorZeroValueIsSafe(t *testing.T) {
	var loadErr LoadError
	if got := loadErr.Error(); got != "load unknown unknown source" {
		t.Fatalf("zero LoadError.Error() = %q", got)
	}
	if loadErr.Unwrap() != nil || errors.Is(&loadErr, ErrRead) {
		t.Fatal("zero LoadError unexpectedly classified or unwrapped")
	}
	var nilErr *LoadError
	if got := nilErr.Error(); got != "load input" {
		t.Fatalf("nil LoadError.Error() = %q", got)
	}
}

func TestLoadFileReturnsImmutableDocumentAndSafeProvenance(t *testing.T) {
	payload := []byte("https://redd.it/abc123\r\nhttps://redd.it/def456\n")
	file := filepath.Join(t.TempDir(), "posts.txt")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	document, err := Load(context.Background(), Spec{Kind: KindPosts, File: file}, Config{MaxBytes: int64(len(payload))})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if document.Len() != len(payload) {
		t.Fatalf("Len() = %d, want %d", document.Len(), len(payload))
	}

	readBack, err := io.ReadAll(document.Reader())
	if err != nil {
		t.Fatalf("read Document.Reader(): %v", err)
	}
	if string(readBack) != string(payload) {
		t.Fatalf("Reader() = %q, want %q", readBack, payload)
	}

	copyOne := document.Bytes()
	copyOne[0] = 'X'
	if got := document.Bytes(); got[0] != payload[0] {
		t.Fatal("Bytes() exposed mutable document storage")
	}

	digest := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(digest[:])
	provenance := document.Provenance()
	if provenance != (Provenance{
		Kind:   KindPosts,
		Mode:   ModeFile,
		Origin: "local-file",
		Bytes:  int64(len(payload)),
		SHA256: wantHash,
	}) {
		t.Fatalf("Provenance() = %+v", provenance)
	}
	if strings.Contains(fmt.Sprintf("%+v", provenance), file) {
		t.Fatal("provenance retained local path")
	}
}

func TestLoadFileRejectsOversizedAndNonRegularSources(t *testing.T) {
	directory := t.TempDir()
	oversized := filepath.Join(directory, "too-large.secret")
	if err := os.WriteFile(oversized, []byte("12345"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	regular := filepath.Join(directory, "regular.txt")
	if err := os.WriteFile(regular, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	symlink := filepath.Join(directory, "input-link")
	symlinkSupported := os.Symlink(regular, symlink) == nil

	tests := []struct {
		name string
		path string
		max  int64
		want error
	}{
		{name: "size precheck", path: oversized, max: 4, want: ErrTooLarge},
		{name: "directory", path: directory, max: 4, want: ErrNotRegular},
		{name: "missing", path: filepath.Join(directory, "private-missing"), max: 4, want: ErrOpen},
	}
	if symlinkSupported {
		tests = append(tests, struct {
			name string
			path string
			max  int64
			want error
		}{name: "symlink", path: symlink, max: 4, want: ErrNotRegular})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := Load(context.Background(), Spec{Kind: KindDictionary, File: test.path}, Config{MaxBytes: test.max})
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
			assertZeroDocument(t, document)
			if strings.Contains(err.Error(), test.path) || strings.Contains(err.Error(), filepath.Base(test.path)) {
				t.Fatalf("sanitized error retained local path: %v", err)
			}
		})
	}
}

func TestLoadFileHonorsCanceledContextBeforeFilesystemAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	secretPath := filepath.Join(t.TempDir(), "never-open-this-secret")

	document, err := Load(ctx, Spec{Kind: KindPosts, File: secretPath}, Config{MaxBytes: 32})
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() error = %v, want cancellation", err)
	}
	assertZeroDocument(t, document)
	if strings.Contains(err.Error(), secretPath) || strings.Contains(err.Error(), "never-open-this-secret") {
		t.Fatalf("cancellation error retained path: %v", err)
	}
}

func TestLoadHTTPSAssignmentKinds(t *testing.T) {
	payload := []byte("alpha\nbeta\n")
	tests := []struct {
		name string
		kind Kind
		host string
	}{
		{name: "posts", kind: KindPosts, host: postsHost},
		{name: "dictionary", kind: KindDictionary, host: dictionaryHost},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server, client := newMappedTLSServer(t, test.host, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Method != http.MethodGet {
					t.Errorf("method = %q, want GET", request.Method)
				}
				if request.Host != test.host {
					t.Errorf("host = %q, want %q", request.Host, test.host)
				}
				if request.Header.Get("User-Agent") != testUserAgent {
					t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
				}
				if request.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("Accept-Encoding = %q, want identity", request.Header.Get("Accept-Encoding"))
				}
				writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = writer.Write(payload)
			}))
			defer server.Close()

			secretPath := "/owner/private-looking-token/raw/input.txt"
			document, err := Load(context.Background(), Spec{Kind: test.kind, URL: "https://" + test.host + secretPath}, Config{
				HTTPClient: client,
				UserAgent:  testUserAgent,
				MaxBytes:   int64(len(payload)),
			})
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("requests = %d, want 1", calls.Load())
			}
			if string(document.Bytes()) != string(payload) {
				t.Fatalf("document = %q, want %q", document.Bytes(), payload)
			}
			provenance := document.Provenance()
			if provenance.Kind != test.kind || provenance.Mode != ModeHTTPS || provenance.Origin != test.host || provenance.Bytes != int64(len(payload)) {
				t.Fatalf("provenance = %+v", provenance)
			}
			if strings.Contains(fmt.Sprintf("%+v", provenance), secretPath) || strings.Contains(fmt.Sprintf("%+v", provenance), "private-looking-token") {
				t.Fatal("provenance retained remote path")
			}
		})
	}
}

func TestLoadHTTPSByteLimitUsesSentinelForUnknownLength(t *testing.T) {
	server, client := newMappedTLSServer(t, dictionaryHost, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		switch request.URL.Path {
		case "/exact":
			_, _ = writer.Write([]byte("1234"))
		case "/extra":
			_, _ = writer.Write([]byte("12345"))
		default:
			t.Errorf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()

	exact, err := Load(context.Background(), Spec{Kind: KindDictionary, URL: "https://" + dictionaryHost + "/exact"}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   4,
	})
	if err != nil {
		t.Fatalf("exact Load() error = %v", err)
	}
	if string(exact.Bytes()) != "1234" {
		t.Fatalf("exact bytes = %q", exact.Bytes())
	}

	oversized, err := Load(context.Background(), Spec{Kind: KindDictionary, URL: "https://" + dictionaryHost + "/extra"}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   4,
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("extra Load() error = %v, want ErrTooLarge", err)
	}
	assertZeroDocument(t, oversized)
}

func TestLoadHTTPSRejectsBeforeReadingBody(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		contentType   []string
		encoding      string
		contentLength int64
		want          error
	}{
		{name: "status", status: http.StatusNotFound, contentType: []string{"text/plain"}, contentLength: -1, want: ErrHTTPStatus},
		{name: "content type", status: http.StatusOK, contentType: []string{"text/html"}, contentLength: -1, want: ErrContentType},
		{name: "duplicate content type", status: http.StatusOK, contentType: []string{"text/plain", "application/octet-stream"}, contentLength: -1, want: ErrContentType},
		{name: "encoding", status: http.StatusOK, contentType: []string{"text/plain"}, encoding: "gzip", contentLength: -1, want: ErrContentEncoding},
		{name: "declared size", status: http.StatusOK, contentType: []string{"text/plain"}, contentLength: 5, want: ErrTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &observedBody{reader: strings.NewReader("secret response body")}
			header := make(http.Header)
			for _, value := range test.contentType {
				header.Add("Content-Type", value)
			}
			if test.encoding != "" {
				header.Set("Content-Encoding", test.encoding)
			}
			client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    test.status,
					Header:        header,
					Body:          body,
					ContentLength: test.contentLength,
				}, nil
			}))

			document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/owner/private/raw/input"}, Config{
				HTTPClient: client,
				UserAgent:  testUserAgent,
				MaxBytes:   4,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
			assertZeroDocument(t, document)
			if body.reads.Load() != 0 {
				t.Fatalf("body reads = %d, want 0", body.reads.Load())
			}
			if body.closes.Load() != 1 {
				t.Fatalf("body closes = %d, want 1", body.closes.Load())
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret response body") {
				t.Fatalf("error leaked remote data: %v", err)
			}
			if test.status != http.StatusOK {
				var loadErr *LoadError
				if !errors.As(err, &loadErr) || loadErr.StatusCode != test.status {
					t.Fatalf("LoadError = %#v, want status %d", loadErr, test.status)
				}
			}
		})
	}
}

func TestLoadHTTPSClosesBodyAndReturnsNoDocumentOnReadOrCloseFailure(t *testing.T) {
	tests := []struct {
		name string
		body *observedBody
		want error
	}{
		{
			name: "read",
			body: &observedBody{reader: &errorReader{payload: []byte("partial"), err: errors.New("private read failure")}},
			want: ErrRead,
		},
		{
			name: "close",
			body: &observedBody{reader: strings.NewReader("valid"), closeErr: errors.New("private close failure")},
			want: ErrClose,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": {"text/plain"}},
					Body:          test.body,
					ContentLength: -1,
				}, nil
			}))
			document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, Config{
				HTTPClient: client,
				UserAgent:  testUserAgent,
				MaxBytes:   32,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
			assertZeroDocument(t, document)
			if test.body.closes.Load() != 1 {
				t.Fatalf("body closes = %d, want 1", test.body.closes.Load())
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("error retained raw cause: %v", err)
			}
		})
	}
}

func TestLoadHTTPSSanitizesTransportFailure(t *testing.T) {
	client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial private-host.example/secret-token")
	}))
	document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   32,
	})
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("Load() error = %v, want ErrTransport", err)
	}
	assertZeroDocument(t, document)
	if strings.Contains(err.Error(), "private-host") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error retained transport detail: %v", err)
	}
}

func TestLoadHTTPSPreservesContextDeadlineClassification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	document, err := Load(ctx, Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, Config{
		HTTPClient: clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("transport called after context deadline")
			return nil, nil
		})),
		UserAgent: testUserAgent,
		MaxBytes:  32,
	})
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Load() error = %v, want context deadline", err)
	}
	assertZeroDocument(t, document)
}

func TestLoadHTTPSCancellationInterruptsRequest(t *testing.T) {
	started := make(chan struct{})
	server, client := newMappedTLSServer(t, postsHost, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Load(ctx, Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, Config{
			HTTPClient: client,
			UserAgent:  testUserAgent,
			MaxBytes:   32,
		})
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Load() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Load() did not return after cancellation")
	}
}

func TestLoadHTTPSDoesNotUseCallerCookieJarOrMutateClient(t *testing.T) {
	server, client := newMappedTLSServer(t, postsHost, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if cookie := request.Header.Get("Cookie"); cookie != "" {
			t.Errorf("request forwarded caller cookies: %q", cookie)
		}
		writer.Header().Set("Content-Type", "text/plain")
		writer.Header().Set("Set-Cookie", "source-secret=must-not-store")
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()

	jar := &recordingJar{}
	originalRedirect := func(*http.Request, []*http.Request) error { return errors.New("caller redirect policy") }
	client.Jar = jar
	client.CheckRedirect = originalRedirect
	originalTransport := client.Transport
	originalTimeout := client.Timeout

	document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   8,
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if string(document.Bytes()) != "ok" {
		t.Fatalf("document = %q", document.Bytes())
	}
	if jar.reads.Load() != 0 || jar.writes.Load() != 0 {
		t.Fatalf("caller jar activity reads/writes = %d/%d, want 0/0", jar.reads.Load(), jar.writes.Load())
	}
	if client.Transport != originalTransport || client.Timeout != originalTimeout || client.Jar != jar {
		t.Fatal("Load() mutated caller HTTP client fields")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); err == nil || err.Error() != "caller redirect policy" {
		t.Fatalf("caller redirect policy changed: %v", err)
	}
}

func TestLoadHTTPSRedirectPolicy(t *testing.T) {
	t.Run("same allowlisted host", func(t *testing.T) {
		var calls atomic.Int32
		server, client := newMappedTLSServer(t, postsHost, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			switch request.URL.Path {
			case "/start":
				http.Redirect(writer, request, "https://"+postsHost+"/resolved", http.StatusFound)
			case "/resolved":
				if request.Header.Get("User-Agent") != testUserAgent || request.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("redirect headers = %v", request.Header)
				}
				writer.Header().Set("Content-Type", "text/plain; charset=us-ascii")
				_, _ = writer.Write([]byte("ok"))
			default:
				t.Errorf("unexpected path %q", request.URL.Path)
			}
		}))
		defer server.Close()

		document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/start"}, Config{
			HTTPClient: client,
			UserAgent:  testUserAgent,
			MaxBytes:   8,
		})
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if string(document.Bytes()) != "ok" || calls.Load() != 2 {
			t.Fatalf("document/calls = %q/%d", document.Bytes(), calls.Load())
		}
	})

	t.Run("host escape", func(t *testing.T) {
		server, client := newMappedTLSServer(t, postsHost, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, "https://example.com/private", http.StatusFound)
		}))
		defer server.Close()

		document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/start"}, Config{
			HTTPClient: client,
			UserAgent:  testUserAgent,
			MaxBytes:   8,
		})
		if !errors.Is(err, ErrRedirect) {
			t.Fatalf("Load() error = %v, want ErrRedirect", err)
		}
		assertZeroDocument(t, document)
	})

	t.Run("hop limit", func(t *testing.T) {
		var calls atomic.Int32
		server, client := newMappedTLSServer(t, dictionaryHost, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			var next int
			if _, err := fmt.Sscanf(request.URL.Path, "/%d", &next); err != nil {
				t.Errorf("parse path %q: %v", request.URL.Path, err)
				return
			}
			http.Redirect(writer, request, fmt.Sprintf("https://%s/%d", dictionaryHost, next+1), http.StatusFound)
		}))
		defer server.Close()

		document, err := Load(context.Background(), Spec{Kind: KindDictionary, URL: "https://" + dictionaryHost + "/0"}, Config{
			HTTPClient: client,
			UserAgent:  testUserAgent,
			MaxBytes:   8,
		})
		if !errors.Is(err, ErrRedirect) {
			t.Fatalf("Load() error = %v, want ErrRedirect", err)
		}
		assertZeroDocument(t, document)
		if calls.Load() != maxRedirects+1 {
			t.Fatalf("requests = %d, want %d", calls.Load(), maxRedirects+1)
		}
	})
}

func TestLoadValidatesSpecAndConfigBeforeTransport(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not be called")
	})
	validClient := clientWithTransport(transport)
	longUserAgent := strings.Repeat("a", maxUserAgentBytes+1)

	tests := []struct {
		name   string
		spec   Spec
		config Config
		want   error
	}{
		{name: "no source", spec: Spec{Kind: KindPosts}, config: Config{MaxBytes: 1}, want: ErrInvalidSpec},
		{name: "two sources", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input", File: "input"}, config: Config{MaxBytes: 1}, want: ErrInvalidSpec},
		{name: "unknown kind", spec: Spec{Kind: KindUnknown, File: "input"}, config: Config{MaxBytes: 1}, want: ErrInvalidSpec},
		{name: "nul file", spec: Spec{Kind: KindPosts, File: "input\x00private"}, config: Config{MaxBytes: 1}, want: ErrInvalidSpec},
		{name: "long file", spec: Spec{Kind: KindPosts, File: strings.Repeat("a", maxSourceLocatorBytes+1)}, config: Config{MaxBytes: 1}, want: ErrInvalidSpec},
		{name: "zero limit", spec: Spec{Kind: KindPosts, File: "input"}, config: Config{}, want: ErrInvalidConfig},
		{name: "excessive limit", spec: Spec{Kind: KindPosts, File: "input"}, config: Config{MaxBytes: MaximumDocumentBytes + 1}, want: ErrInvalidConfig},
		{name: "nil client", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{UserAgent: testUserAgent, MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "implicit transport", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: &http.Client{Timeout: time.Second}, UserAgent: testUserAgent, MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "zero timeout", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: &http.Client{Transport: transport}, UserAgent: testUserAgent, MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "excessive timeout", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: &http.Client{Transport: transport, Timeout: maximumHTTPTimeout + time.Second}, UserAgent: testUserAgent, MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "empty user agent", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "long user agent", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: longUserAgent, MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "control user agent", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: "duckwords\nsecret", MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "outer whitespace user agent", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: " duckwords", MaxBytes: 1}, want: ErrInvalidConfig},
		{name: "negative source retries", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: testUserAgent, MaxBytes: 1, Retry: &RetryConfig{MaxRetries: -1, MaxElapsed: time.Second}}, want: ErrInvalidConfig},
		{name: "excessive source retries", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: testUserAgent, MaxBytes: 1, Retry: &RetryConfig{MaxRetries: maxSourceRetries + 1, MaxElapsed: time.Second}}, want: ErrInvalidConfig},
		{name: "zero source retry budget", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: testUserAgent, MaxBytes: 1, Retry: &RetryConfig{MaxRetries: 0}}, want: ErrInvalidConfig},
		{name: "excessive source retry budget", spec: Spec{Kind: KindPosts, URL: "https://" + postsHost + "/input"}, config: Config{HTTPClient: validClient, UserAgent: testUserAgent, MaxBytes: 1, Retry: &RetryConfig{MaxRetries: 1, MaxElapsed: sourceRetryElapsedLimit + time.Nanosecond}}, want: ErrInvalidConfig},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := Load(context.Background(), test.spec, test.config)
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
			assertZeroDocument(t, document)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}

	file := filepath.Join(t.TempDir(), "local.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	invalidNetworkRetry := RetryConfig{MaxRetries: maxSourceRetries + 1, MaxElapsed: -time.Second}
	if _, err := Load(context.Background(), Spec{Kind: KindPosts, File: file}, Config{MaxBytes: 1, Retry: &invalidNetworkRetry}); err != nil {
		t.Fatalf("local Load() unexpectedly required HTTP config: %v", err)
	}
}

func TestValidateIsPureAndMatchesLoadPolicy(t *testing.T) {
	var calls atomic.Int32
	client := clientWithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not be called")
	}))
	validRemote := Spec{Kind: KindPosts, URL: "https://" + postsHost + "/owner/id/raw/input.txt"}
	validConfig := Config{HTTPClient: client, UserAgent: testUserAgent, MaxBytes: 32}
	if err := Validate(validRemote, validConfig); err != nil {
		t.Fatalf("Validate(valid remote) error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Validate() transport calls = %d, want 0", calls.Load())
	}
	if err := Validate(Spec{Kind: KindDictionary, File: "/path/does/not/need/to/exist"}, Config{MaxBytes: 32}); err != nil {
		t.Fatalf("Validate(valid local policy) error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Validate() transport calls after local spec = %d, want 0", calls.Load())
	}

	invalid := validRemote
	invalid.URL += "#secret"
	if err := Validate(invalid, validConfig); !errors.Is(err, ErrURLPolicy) {
		t.Fatalf("Validate(invalid remote) error = %v, want ErrURLPolicy", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("Validate(invalid) transport calls = %d, want 0", calls.Load())
	}
}

func TestValidateRemoteURLPolicy(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		raw  string
		ok   bool
	}{
		{name: "posts default", kind: KindPosts, raw: "https://" + postsHost + "/jonathan-firefly/id/raw/duck_urls_200.txt", ok: true},
		{name: "dictionary default", kind: KindDictionary, raw: "https://" + dictionaryHost + "/dwyl/english-words/master/words.txt", ok: true},
		{name: "wrong kind host", kind: KindPosts, raw: "https://" + dictionaryHost + "/input"},
		{name: "http", kind: KindPosts, raw: "http://" + postsHost + "/input"},
		{name: "userinfo", kind: KindPosts, raw: "https://user:password@" + postsHost + "/input"},
		{name: "port", kind: KindPosts, raw: "https://" + postsHost + ":443/input"},
		{name: "query", kind: KindPosts, raw: "https://" + postsHost + "/input?token=value", ok: true},
		{name: "empty query", kind: KindPosts, raw: "https://" + postsHost + "/input?", ok: true},
		{name: "oversized query", kind: KindPosts, raw: "https://" + postsHost + "/input?q=" + strings.Repeat("a", maxSourceQueryBytes)},
		{name: "fragment", kind: KindPosts, raw: "https://" + postsHost + "/input#secret"},
		{name: "encoded path", kind: KindPosts, raw: "https://" + postsHost + "/owner/%69nput"},
		{name: "encoded separator", kind: KindPosts, raw: "https://" + postsHost + "/owner%2finput"},
		{name: "dot segment", kind: KindPosts, raw: "https://" + postsHost + "/owner/../input"},
		{name: "double separator", kind: KindPosts, raw: "https://" + postsHost + "/owner//input"},
		{name: "backslash", kind: KindPosts, raw: "https://" + postsHost + "/owner\\input"},
		{name: "root", kind: KindPosts, raw: "https://" + postsHost + "/"},
		{name: "uppercase host", kind: KindPosts, raw: "https://GIST.GITHUBUSERCONTENT.COM/input"},
		{name: "trailing dot", kind: KindPosts, raw: "https://" + postsHost + "./input"},
		{name: "IP", kind: KindPosts, raw: "https://127.0.0.1/input"},
		{name: "unknown kind", kind: KindUnknown, raw: "https://" + postsHost + "/input"},
		{name: "nul", kind: KindPosts, raw: "https://" + postsHost + "/input\x00secret"},
		{name: "long", kind: KindPosts, raw: "https://" + postsHost + "/" + strings.Repeat("a", maxSourceLocatorBytes)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := validateRemoteURL(test.kind, test.raw)
			if test.ok {
				if err != nil {
					t.Fatalf("validateRemoteURL() error = %v", err)
				}
				if parsed.Scheme != "https" || parsed.Hostname() == "" {
					t.Fatalf("parsed URL = %v", parsed)
				}
				return
			}
			if !errors.Is(err, ErrURLPolicy) && !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("validateRemoteURL() error = %v", err)
			}
			if parsed != nil {
				t.Fatalf("parsed URL = %v, want nil", parsed)
			}
			if strings.Contains(fmt.Sprint(err), "secret") || strings.Contains(fmt.Sprint(err), "password") {
				t.Fatalf("error retained rejected URL detail: %v", err)
			}
		})
	}
}

func TestSupportedTextContentType(t *testing.T) {
	tests := []struct {
		values []string
		want   bool
	}{
		{values: []string{"text/plain"}, want: true},
		{values: []string{"text/plain; charset=utf-8"}, want: true},
		{values: []string{"TEXT/PLAIN; CHARSET=US-ASCII"}, want: true},
		{values: []string{"application/octet-stream"}, want: true},
		{values: nil},
		{values: []string{"text/html"}},
		{values: []string{"text/plain; charset=iso-8859-1"}},
		{values: []string{"text/plain; boundary=x"}},
		{values: []string{"application/octet-stream; charset=utf-8"}},
		{values: []string{"text/plain", "text/plain"}},
		{values: []string{"not a media type"}},
	}
	for _, test := range tests {
		if got := supportedTextContentType(test.values); got != test.want {
			t.Errorf("supportedTextContentType(%q) = %t, want %t", test.values, got, test.want)
		}
	}
}

func FuzzValidateRemoteURL(f *testing.F) {
	f.Add(uint8(KindPosts), "https://"+postsHost+"/owner/id/raw/input.txt")
	f.Add(uint8(KindDictionary), "https://"+dictionaryHost+"/owner/repository/main/words.txt")
	f.Add(uint8(KindPosts), "https://user:secret@"+postsHost+"/input?token=secret")
	f.Add(uint8(KindUnknown), "not a URL")

	f.Fuzz(func(t *testing.T, rawKind uint8, rawURL string) {
		kind := Kind(rawKind)
		parsed, err := validateRemoteURL(kind, rawURL)
		if err != nil {
			if parsed != nil {
				t.Fatal("rejected URL returned parsed state")
			}
			return
		}

		host, ok := AllowedHost(kind)
		if !ok {
			t.Fatal("accepted unknown source kind")
		}
		if parsed.Scheme != "https" || parsed.Host != host || parsed.Hostname() != host {
			t.Fatalf("accepted URL outside origin policy: scheme=%q host=%q", parsed.Scheme, parsed.Host)
		}
		if parsed.User != nil || parsed.Port() != "" || parsed.Fragment != "" || parsed.RawPath != "" {
			t.Fatal("accepted URL with ambiguous or sensitive components")
		}
		if len(parsed.RawQuery) > maxSourceQueryBytes {
			t.Fatal("accepted URL with an unbounded query string")
		}
		if parsed.Path == "" || parsed.Path == "/" || strings.Contains(parsed.EscapedPath(), "%") {
			t.Fatal("accepted invalid document path")
		}
	})
}

func newMappedTLSServer(t *testing.T, host string, handler http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		server.Close()
		t.Fatal("httptest TLS client has unexpected transport")
	}
	transport = transport.Clone()
	// The request must retain the allowlisted production hostname so URL, Host, and
	// redirect policy are exercised. The socket is deliberately mapped to the local
	// TLS fixture, whose certificate cannot name those production hosts.
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true // test-only loopback transport
	serverAddress := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, serverAddress)
	}
	return server, &http.Client{Transport: transport, Timeout: time.Second}
}

func clientWithTransport(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, Timeout: time.Second}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type observedBody struct {
	reader   io.Reader
	closeErr error
	reads    atomic.Int32
	closes   atomic.Int32
}

func (body *observedBody) Read(target []byte) (int, error) {
	body.reads.Add(1)
	return body.reader.Read(target)
}

func (body *observedBody) Close() error {
	body.closes.Add(1)
	return body.closeErr
}

type errorReader struct {
	payload []byte
	err     error
	done    bool
}

type recordingJar struct {
	reads  atomic.Int32
	writes atomic.Int32
}

func (jar *recordingJar) SetCookies(*url.URL, []*http.Cookie) {
	jar.writes.Add(1)
}

func (jar *recordingJar) Cookies(*url.URL) []*http.Cookie {
	jar.reads.Add(1)
	return []*http.Cookie{{Name: "ambient-secret", Value: "must-not-send"}}
}

func (reader *errorReader) Read(target []byte) (int, error) {
	if reader.done {
		return 0, reader.err
	}
	reader.done = true
	return copy(target, reader.payload), nil
}

func assertZeroDocument(t *testing.T, document Document) {
	t.Helper()
	if document.Len() != 0 || len(document.Bytes()) != 0 || document.Provenance() != (Provenance{}) {
		t.Fatalf("failure returned usable document: len=%d provenance=%+v", document.Len(), document.Provenance())
	}
}

// TestLoadHTTPSPreservesQueryString covers raw-gist and CDN links whose query is
// part of the resource identity. The query is forwarded verbatim but must never
// reach provenance, which records the hostname only.
func TestLoadHTTPSPreservesQueryString(t *testing.T) {
	payload := []byte("alpha\nbeta\n")
	const query = "token=private-looking-value&v=2"
	var seen atomic.Value
	server, client := newMappedTLSServer(t, postsHost, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen.Store(request.URL.RawQuery)
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	document, err := Load(context.Background(), Spec{Kind: KindPosts, URL: "https://" + postsHost + "/owner/id/raw/posts.txt?" + query}, Config{
		HTTPClient: client,
		UserAgent:  testUserAgent,
		MaxBytes:   int64(len(payload)),
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, _ := seen.Load().(string); got != query {
		t.Fatalf("server saw query %q, want %q", got, query)
	}
	provenance := document.Provenance()
	if provenance.Origin != postsHost {
		t.Fatalf("Origin = %q, want the bare hostname", provenance.Origin)
	}
	if strings.Contains(fmt.Sprintf("%+v", provenance), "private-looking-value") {
		t.Fatal("provenance retained the query string")
	}
}
