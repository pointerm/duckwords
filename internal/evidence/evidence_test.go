package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pointerm/duckwords/internal/app"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	testPosts   = "1111111111111111111111111111111111111111111111111111111111111111"
	testPostIDs = "aa1b5251da9480bdfc7abf23702fc1123756bfb0de76341fea70fe725e3e4806"
	testWords   = "2222222222222222222222222222222222222222222222222222222222222222"
	testResult  = "88eefd45a31855d57634868edcd846e93126c6db2ffd3c1790cca38fecaaba18"
)

func TestFinalizePublishesReconciledBundle(t *testing.T) {
	t.Parallel()
	if _, err := parseLog([]byte(canonicalTestLog("completed"))); err != nil {
		t.Fatalf("parse fixture log: %+v", err)
	}
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "captured-result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "captured-application.log", canonicalTestLog("completed"))
	binaryPath := writeVersionBinary(t, directory)
	outputDir := filepath.Join(directory, "submission")

	manifest, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, outputDir, 0))
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if manifest.Schema != 1 || manifest.ExitCode != 0 || manifest.Partial || manifest.ResultWords != 2 ||
		manifest.StartedAt != "2026-08-14T10:00:00Z" || manifest.FinishedAt != "2026-08-14T10:00:03Z" ||
		manifest.Summary.PostsCompleted != assignmentPostCount || manifest.Build.Commit != testCommit || manifest.Build.BinarySHA256 == "" {
		t.Fatalf("manifest = %#v", manifest)
	}
	for _, name := range []string{"result.json", "application.log", "full-application.log", "run-manifest.json", "RUN.md"} {
		info, statErr := os.Lstat(filepath.Join(outputDir, name))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != filePermission {
			t.Fatalf("artifact %s info = %#v, error = %v", name, info, statErr)
		}
	}
	result, err := os.ReadFile(filepath.Join(outputDir, "result.json"))
	if err != nil || string(result) != canonicalTestResult() {
		t.Fatalf("published result = %q, error = %v", result, err)
	}
	fullLog, err := os.ReadFile(filepath.Join(outputDir, "full-application.log"))
	if err != nil || !strings.HasSuffix(string(fullLog), fullLogResultMarker+canonicalTestResult()) {
		t.Fatalf("full application log = %q, error = %v", fullLog, err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(outputDir, "run-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	runBytes, runErr := os.ReadFile(filepath.Join(outputDir, "RUN.md"))
	if err := json.Unmarshal(manifestBytes, &decoded); err != nil || runErr != nil || decoded.Artifacts.ResultSHA256 != digest(result) ||
		decoded.Artifacts.RunDocumentSHA256 != digest(runBytes) {
		t.Fatalf("decode manifest: %#v, decode error %v, RUN read error %v", decoded, err, runErr)
	}
	for _, forbidden := range []string{directory, "https://", "client_secret", "access_token", "raw comment"} {
		for _, name := range []string{"run-manifest.json", "RUN.md"} {
			data, readErr := os.ReadFile(filepath.Join(outputDir, name))
			if readErr != nil || strings.Contains(string(data), forbidden) {
				t.Fatalf("%s contains forbidden %q (read error %v)", name, forbidden, readErr)
			}
		}
	}
	if _, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, outputDir, 0)); !errors.Is(err, ErrPublish) {
		t.Fatalf("second Finalize() error = %v, want ErrPublish", err)
	}
}

func TestFinalizeAcceptsExplicitPartialEvidence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "log.ndjson", canonicalTestLog("failed"))
	binaryPath := writeVersionBinary(t, directory)
	manifest, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, filepath.Join(directory, "out"), 3))
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if !manifest.Partial || manifest.TerminalStatus != "partial" || manifest.Summary.PostsCompleted != assignmentPostCount-1 ||
		manifest.Summary.PostsFailed != 1 || len(manifest.Outcomes) != assignmentPostCount {
		t.Fatalf("partial manifest = %#v", manifest)
	}
}

func TestFinalizeReconcilesSourceAndRedditRetryEvents(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	log := canonicalTestLog("completed")
	sourceRetry := `{"time":"2026-08-14T10:00:00.05Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":2,"delay":"100ms"}`
	redditRetry := `{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"500ms"}`
	log = strings.Replace(log, "\n"+`{"time":"2026-08-14T10:00:00.1Z"`, "\n"+sourceRetry+"\n"+`{"time":"2026-08-14T10:00:00.1Z"`, 1)
	log = strings.Replace(log, "\n"+`{"time":"2026-08-14T10:00:01Z"`, "\n"+redditRetry+"\n"+`{"time":"2026-08-14T10:00:01Z"`, 1)
	log = strings.Replace(log, `"source_retries":0`, `"source_retries":1`, 1)
	log = strings.Replace(log, `"reddit_retries":0`, `"reddit_retries":1`, 1)
	manifest, err := Finalize(context.Background(), testConfig(
		writeTestFile(t, directory, "result.json", canonicalTestResult()),
		writeTestFile(t, directory, "log.ndjson", log),
		writeVersionBinary(t, directory), filepath.Join(directory, "out"), 0,
	))
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if manifest.Requests.SourceRetries != 1 || manifest.Requests.RedditRetries != 1 {
		t.Fatalf("request manifest = %#v", manifest.Requests)
	}
}

func TestFinalizeAcceptsOAuthRetryWithEmptyPostID(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	log := canonicalTestLog("completed")
	retry := `{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"oauth_token","post_id":"","error_class":"server","http_status":503,"attempt":2,"delay":"500ms"}`
	log = strings.Replace(log, "\n"+`{"time":"2026-08-14T10:00:01Z"`, "\n"+retry+"\n"+`{"time":"2026-08-14T10:00:01Z"`, 1)
	log = strings.Replace(log, `"reddit_retries":0`, `"reddit_retries":1`, 1)
	manifest, err := Finalize(context.Background(), testConfig(
		writeTestFile(t, directory, "result.json", canonicalTestResult()),
		writeTestFile(t, directory, "log.ndjson", log),
		writeVersionBinary(t, directory), filepath.Join(directory, "out"), 0,
	))
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if manifest.Requests.RedditRetries != 1 {
		t.Fatalf("request manifest = %#v", manifest.Requests)
	}
}

func TestValidateRetryMatchesProducerContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		json  string
		valid bool
	}{
		{name: "source status", valid: true, json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":2,"delay":"1ns"}`},
		{name: "source read", valid: true, json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"read","http_status":200,"attempt":3,"delay":"1ns"}`},
		{name: "source transport", valid: true, json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"dictionary","error_class":"transport","attempt":2,"delay":"1ns"}`},
		{name: "oauth server", valid: true, json: `{"time":"2026-08-14T10:00:00Z","operation":"oauth_token","post_id":"","error_class":"server","http_status":503,"attempt":4,"delay":"1ns"}`},
		{name: "authentication replay", valid: true, json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","post_id":"duck123","error_class":"authentication","http_status":401,"attempt":5,"delay":"0s"}`},
		{name: "source first attempt", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":1,"delay":"1ns"}`},
		{name: "source fourth attempt", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":4,"delay":"1ns"}`},
		{name: "source zero delay", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":2,"delay":"0s"}`},
		{name: "source permanent status", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":501,"attempt":2,"delay":"1ns"}`},
		{name: "source status omitted", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"http_status","attempt":2,"delay":"1ns"}`},
		{name: "source transport emitted zero", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"transport","http_status":0,"attempt":2,"delay":"1ns"}`},
		{name: "source read status omitted", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"read","attempt":2,"delay":"1ns"}`},
		{name: "source close emitted status", json: `{"time":"2026-08-14T10:00:00Z","operation":"source_download","source_kind":"posts","error_class":"close","http_status":200,"attempt":2,"delay":"1ns"}`},
		{name: "reddit status omitted", json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","post_id":"duck123","error_class":"server","attempt":2,"delay":"1ns"}`},
		{name: "authentication backoff", json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","post_id":"duck123","error_class":"authentication","http_status":401,"attempt":2,"delay":"1ns"}`},
		{name: "server zero delay", json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"0s"}`},
		{name: "server permanent status", json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","post_id":"duck123","error_class":"server","http_status":501,"attempt":2,"delay":"1ns"}`},
		{name: "oauth fifth attempt", json: `{"time":"2026-08-14T10:00:00Z","operation":"oauth_token","post_id":"","error_class":"server","http_status":503,"attempt":5,"delay":"1ns"}`},
		{name: "API sixth attempt", json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":6,"delay":"1ns"}`},
		{name: "oauth authentication", json: `{"time":"2026-08-14T10:00:00Z","operation":"oauth_token","post_id":"","error_class":"authentication","http_status":401,"attempt":2,"delay":"0s"}`},
		{name: "API post ID omitted", json: `{"time":"2026-08-14T10:00:00Z","operation":"comments","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			record, err := decodeRecord([]byte(test.json))
			if err != nil {
				t.Fatal(err)
			}
			retry, err := validateRetry(record)
			if test.valid && err != nil {
				t.Fatalf("validateRetry() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatalf("validateRetry() accepted %#v", retry)
			}
		})
	}
}

func TestParseLogRejectsImpossibleSourceRetryAttemptSequence(t *testing.T) {
	t.Parallel()
	retry := `{"time":"2026-08-14T10:00:00.05Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":2,"delay":"1ns"}`
	log := strings.Replace(canonicalTestLog("completed"), `{"time":"2026-08-14T10:00:00.1Z"`, retry+"\n"+retry+"\n"+`{"time":"2026-08-14T10:00:00.1Z"`, 1)
	log = strings.Replace(log, `"source_retries":0`, `"source_retries":2`, 1)
	if _, err := parseLog([]byte(log)); !errors.Is(err, ErrInvalidLog) {
		t.Fatalf("parseLog() error = %v, want ErrInvalidLog", err)
	}
}

func TestReconcileRejectsRedditRetryWithoutAssignmentOutcome(t *testing.T) {
	t.Parallel()
	retry := `{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"outside1","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`
	log := strings.Replace(canonicalTestLog("completed"), "\n"+`{"time":"2026-08-14T10:00:01Z"`, "\n"+retry+"\n"+`{"time":"2026-08-14T10:00:01Z"`, 1)
	log = strings.Replace(log, `"reddit_retries":0`, `"reddit_retries":1`, 1)
	if err := validateTestEvidence(canonicalTestResult(), log, 0); !errors.Is(err, ErrInvalidLog) {
		t.Fatalf("evidence validation error = %v, want ErrInvalidLog", err)
	}
}

func TestReconcileRejectsImpossibleRedditRetryAttemptSequence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		retries []string
	}{
		{
			name: "sequence starts after attempt two",
			retries: []string{
				`{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":3,"delay":"1ns"}`,
			},
		},
		{
			name: "sequence skips an attempt",
			retries: []string{
				`{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`,
				`{"time":"2026-08-14T10:00:00.6Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":4,"delay":"1ns"}`,
			},
		},
		{
			name: "second comments session",
			retries: []string{
				`{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`,
				`{"time":"2026-08-14T10:00:00.6Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`,
			},
		},
		{
			name: "morechildren session absent from outcome",
			retries: []string{
				`{"time":"2026-08-14T10:00:00.5Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"morechildren","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			log := canonicalTestLog("completed")
			log = strings.Replace(log, "\n"+`{"time":"2026-08-14T10:00:01Z"`, "\n"+strings.Join(test.retries, "\n")+"\n"+`{"time":"2026-08-14T10:00:01Z"`, 1)
			log = strings.Replace(log, `"reddit_retries":0`, fmt.Sprintf(`"reddit_retries":%d`, len(test.retries)), 1)
			if err := validateTestEvidence(canonicalTestResult(), log, 0); !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("evidence validation error = %v, want ErrInvalidLog", err)
			}
		})
	}
}

func TestReconcileAcceptsBoundedRedditRetrySessions(t *testing.T) {
	t.Parallel()
	outcomes := map[string]OutcomeManifest{
		"duck123": {PostID: "duck123", MoreRequests: 2, ContinuationRequests: 1},
	}
	retries := []retryRecord{
		{operation: "oauth_token", attempt: 2},
		{operation: "oauth_token", attempt: 3},
		{operation: "comments", postID: "duck123", attempt: 2},
		{operation: "comments", postID: "duck123", attempt: 3},
		{operation: "morechildren", postID: "duck123", attempt: 2},
		{operation: "morechildren", postID: "duck123", attempt: 2},
		{operation: "continuation", postID: "duck123", attempt: 2},
	}
	if err := reconcileRedditRetries(retries, outcomes); err != nil {
		t.Fatalf("reconcileRedditRetries() error = %v", err)
	}
}

func TestParseResultRejectsNonCanonicalOrInvalidRanks(t *testing.T) {
	t.Parallel()
	tests := []string{
		`[{"word":"duck","count":2}]`,
		"null\n",
		"[\n  {\n    \"word\": \"duck\",\n    \"count\": 0\n  }\n]\n",
		"[\n  {\n    \"word\": \"duck\",\n    \"count\": 1\n  },\n  {\n    \"word\": \"bird\",\n    \"count\": 2\n  }\n]\n",
		"[\n  {\n    \"word\": \"duck\",\n    \"count\": 2,\n    \"extra\": true\n  }\n]\n",
		"[\n  {\n    \"word\": \"client_secret\",\n    \"count\": 2\n  }\n]\n",
	}
	for _, input := range tests {
		if _, _, err := parseResult([]byte(input)); !errors.Is(err, ErrInvalidResult) {
			t.Errorf("parseResult(%q) error = %v, want ErrInvalidResult", input, err)
		}
	}
}

func TestParseResultAllowsOrdinaryWordsThatResembleSecurityTerms(t *testing.T) {
	t.Parallel()
	input := "[\n  {\n    \"word\": \"bearer\",\n    \"count\": 2\n  },\n  {\n    \"word\": \"password\",\n    \"count\": 1\n  }\n]\n"
	words, canonical, err := parseResult([]byte(input))
	if err != nil || len(words) != 2 || string(canonical) != input {
		t.Fatalf("parseResult() words=%#v canonical=%q error=%v", words, canonical, err)
	}
}

func TestParseResultEnforcesCardinalityAndWordBoundaries(t *testing.T) {
	t.Parallel()

	for _, words := range [][]resultWord{
		{},
		{{Word: "ééé", Count: 1}},
		{{Word: strings.Repeat("a", 4<<10), Count: 1}},
		{
			{Word: "alpha", Count: 10}, {Word: "bravo", Count: 9},
			{Word: "charlie", Count: 8}, {Word: "delta", Count: 7},
			{Word: "echo", Count: 6}, {Word: "foxtrot", Count: 5},
			{Word: "golf", Count: 4}, {Word: "hotel", Count: 3},
			{Word: "india", Count: 2}, {Word: "juliet", Count: 1},
		},
	} {
		input, err := marshalPretty(words)
		if err != nil {
			t.Fatal(err)
		}
		parsed, canonical, err := parseResult(input)
		if err != nil || !reflect.DeepEqual(parsed, words) || !bytes.Equal(canonical, input) {
			t.Fatalf("parseResult() parsed=%#v canonical_match=%t error=%v", parsed, bytes.Equal(canonical, input), err)
		}
	}

	invalid := [][]resultWord{
		{{Word: "aa", Count: 1}},
		{{Word: strings.Repeat("a", (4<<10)+1), Count: 1}},
		{
			{Word: "alpha", Count: 11}, {Word: "bravo", Count: 10},
			{Word: "charlie", Count: 9}, {Word: "delta", Count: 8},
			{Word: "echo", Count: 7}, {Word: "foxtrot", Count: 6},
			{Word: "golf", Count: 5}, {Word: "hotel", Count: 4},
			{Word: "india", Count: 3}, {Word: "juliet", Count: 2},
			{Word: "kilo", Count: 1},
		},
	}
	for _, words := range invalid {
		input, err := marshalPretty(words)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := parseResult(input); !errors.Is(err, ErrInvalidResult) {
			t.Fatalf("parseResult() error = %v, want ErrInvalidResult", err)
		}
	}

	overflow := "[\n  {\n    \"word\": \"duck\",\n    \"count\": 18446744073709551616\n  }\n]\n"
	if _, _, err := parseResult([]byte(overflow)); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("parseResult(uint64 overflow) error = %v, want ErrInvalidResult", err)
	}
}

func TestFinalizeRejectsLogTamperingWithoutPublishing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(string) string
	}{
		{name: "summary mismatch", edit: func(value string) string {
			return strings.Replace(value, `"counted_tokens":3`, `"counted_tokens":4`, 1)
		}},
		{name: "source mismatch", edit: func(value string) string {
			return strings.Replace(value, `"posts_sha256":"`+testPosts, `"posts_sha256":"`+testWords, 1)
		}},
		{name: "duplicate lifecycle", edit: func(value string) string { line := strings.SplitN(value, "\n", 2)[0]; return line + "\n" + value }},
		{name: "unknown event", edit: func(value string) string {
			return value + `{"time":"2026-08-14T10:00:03Z","event":"raw_payload"}` + "\n"
		}},
		{name: "secret marker", edit: func(value string) string {
			return strings.Replace(value, `"msg":"run started"`, `"msg":"client_secret exposed"`, 1)
		}},
		{name: "non json diagnostic", edit: func(value string) string { return value + "duckwords: partial result\n" }},
		{name: "missing final newline", edit: func(value string) string { return strings.TrimSuffix(value, "\n") }},
		{name: "carriage return", edit: func(value string) string { return strings.Replace(value, "\n", "\r\n", 1) }},
		{name: "duplicate key", edit: func(value string) string {
			return strings.Replace(value, `"event":"run_started"`, `"event":"run_started","event":"run_started"`, 1)
		}},
		{name: "unknown persisted field", edit: func(value string) string {
			return strings.Replace(value, `"event":"run_started"`, `"event":"run_started","raw_comment":"private"`, 1)
		}},
		{name: "filters enabled", edit: func(value string) string {
			return strings.ReplaceAll(value, `"filter_count":0`, `"filter_count":1`)
		}},
		{name: "wrong assignment origin", edit: func(value string) string {
			return strings.Replace(value, `"source_origin":"gist.githubusercontent.com"`, `"source_origin":"example.com"`, 1)
		}},
		{name: "source lifecycle order", edit: func(value string) string {
			lines := strings.Split(value, "\n")
			lines[1], lines[2] = lines[2], lines[1]
			return strings.Join(lines, "\n")
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
			logPath := writeTestFile(t, directory, "log.ndjson", test.edit(canonicalTestLog("completed")))
			binaryPath := writeVersionBinary(t, directory)
			outputDir := filepath.Join(directory, "out")
			_, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, outputDir, 0))
			if !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("Finalize() error = %v, want ErrInvalidLog", err)
			}
			if _, statErr := os.Lstat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("output unexpectedly published: %v", statErr)
			}
		})
	}
}

func TestReconcileRejectsImpossibleLifecycleEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		exitCode int
		edit     func(string) string
	}{
		{name: "summary precedes start timestamp", edit: func(value string) string {
			return strings.Replace(value, `"time":"2026-08-14T10:00:02Z","level":"INFO","msg":"processing summary"`,
				`"time":"2026-08-14T09:59:59Z","level":"INFO","msg":"processing summary"`, 1)
		}},
		{name: "source timestamp precedes run", edit: func(value string) string {
			return strings.Replace(value, `"time":"2026-08-14T10:00:00.1Z","level":"INFO","msg":"source loaded"`,
				`"time":"2026-08-14T09:59:59Z","level":"INFO","msg":"source loaded"`, 1)
		}},
		{name: "source timestamp follows summary", edit: func(value string) string {
			return strings.Replace(value, `"time":"2026-08-14T10:00:00.4Z","level":"INFO","msg":"source parsed"`,
				`"time":"2026-08-14T10:00:02.1Z","level":"INFO","msg":"source parsed"`, 1)
		}},
		{name: "outcome timestamp follows summary", edit: func(value string) string {
			return strings.Replace(value, `"time":"2026-08-14T10:00:01Z","level":"INFO","msg":"post processing completed"`,
				`"time":"2026-08-14T10:00:02.1Z","level":"INFO","msg":"post processing completed"`, 1)
		}},
		{name: "summary duration differs from timestamps", edit: func(value string) string {
			return strings.Replace(value, `"duration":"2s"`, `"duration":"20s"`, 1)
		}},
		{name: "summary duration exceeds global ceiling", edit: func(value string) string {
			return strings.Replace(value, `"duration":"2s"`, `"duration":"30m0.000000001s"`, 1)
		}},
		{name: "observed lifecycle exceeds global ceiling", edit: func(value string) string {
			value = strings.Replace(value, `"time":"2026-08-14T10:00:02Z","level":"INFO","msg":"processing summary"`,
				`"time":"2026-08-14T10:31:00Z","level":"INFO","msg":"processing summary"`, 1)
			return strings.Replace(value, `"time":"2026-08-14T10:00:03Z","level":"INFO","msg":"result written"`,
				`"time":"2026-08-14T10:31:01Z","level":"INFO","msg":"result written"`, 1)
		}},
		{name: "output precedes summary timestamp", edit: func(value string) string {
			return strings.Replace(value, `"time":"2026-08-14T10:00:03Z","level":"INFO","msg":"result written"`,
				`"time":"2026-08-14T10:00:01Z","level":"INFO","msg":"result written"`, 1)
		}},
		{name: "build follows run start", edit: func(value string) string {
			return strings.ReplaceAll(value, `"app_build_date":"2026-08-14T09:00:00Z"`, `"app_build_date":"2026-08-14T11:00:00Z"`)
		}},
		{name: "outcomes not in source order", edit: func(value string) string {
			return strings.Replace(value, `"post_id":"post002","source_line":2`, `"post_id":"post002","source_line":1`, 1)
		}},
		{name: "missing terminal outcome", edit: func(value string) string {
			return removeLogLineContaining(value, `"post_id":"post200"`)
		}},
		{name: "rank cardinality differs from distinct words", edit: func(value string) string {
			return strings.Replace(value, `"distinct_words":2`, `"distinct_words":3`, 1)
		}},
		{name: "output cardinality differs from JSON", edit: func(value string) string {
			return strings.Replace(value, `"result_words":2`, `"result_words":1`, 1)
		}},
		{name: "output digest differs from JSON", edit: func(value string) string {
			return strings.Replace(value, `"result_sha256":"`+testResult, `"result_sha256":"`+testWords, 1)
		}},
		{name: "non-assignment input profile", edit: func(value string) string {
			return strings.ReplaceAll(value, `"input_profile":"assignment-default-v1"`, `"input_profile":"custom"`)
		}},
		{name: "normalized post identity differs", edit: func(value string) string {
			return strings.ReplaceAll(value, testPostIDs, testWords)
		}},
		{name: "complete log paired with partial exit", exitCode: 3, edit: func(value string) string { return value }},
		{name: "retry after outcomes began", edit: func(value string) string {
			retry := `{"time":"2026-08-14T10:00:01Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"comments","post_id":"duck123","error_class":"server","http_status":503,"attempt":2,"delay":"1ns"}`
			return strings.Replace(value, `{"time":"2026-08-14T10:00:02Z","level":"INFO","msg":"processing summary"`,
				retry+"\n"+`{"time":"2026-08-14T10:00:02Z","level":"INFO","msg":"processing summary"`, 1)
		}},
		{name: "retry timestamp is in the future", edit: func(value string) string {
			retry := `{"time":"2026-08-14T12:00:01Z","level":"WARN","msg":"request retry scheduled","event":"request_retry","operation":"source_download","source_kind":"posts","error_class":"http_status","http_status":503,"attempt":2,"delay":"1ns"}`
			value = strings.Replace(value, `{"time":"2026-08-14T10:00:00.1Z"`, retry+"\n"+`{"time":"2026-08-14T10:00:00.1Z"`, 1)
			return strings.Replace(value, `"source_retries":0`, `"source_retries":1`, 1)
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateTestEvidence(canonicalTestResult(), test.edit(canonicalTestLog("completed")), test.exitCode)
			if !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("evidence validation error = %v, want ErrInvalidLog", err)
			}
		})
	}
}

func TestParseLogRejectsOversizedRecord(t *testing.T) {
	t.Parallel()
	input := append(bytes.Repeat([]byte{'x'}, maximumLogLineBytes+1), '\n')
	if _, err := parseLog(input); !errors.Is(err, ErrInvalidLog) {
		t.Fatalf("parseLog() error = %v, want ErrInvalidLog", err)
	}
}

func TestFinalizeRejectsMismatchedBinaryAndUnsafeTargets(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "log.ndjson", canonicalTestLog("completed"))
	binaryPath := writeVersionBinaryWithCommit(t, directory, strings.Repeat("a", 40))
	outputDir := filepath.Join(directory, "out")
	if _, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, outputDir, 0)); !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("Finalize() error = %v, want ErrInvalidBinary", err)
	}
	if err := os.Symlink(resultPath, filepath.Join(directory, "result-link")); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(filepath.Join(directory, "result-link"), logPath, binaryPath, outputDir, 0)
	if _, err := Finalize(context.Background(), cfg); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("symlink result error = %v, want ErrInvalidResult", err)
	}
}

func TestFinalizeCancellationLeavesNoPublishedOrStagedBundle(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "log.ndjson", canonicalTestLog("completed"))
	binaryPath := writeVersionBinary(t, directory)
	outputDir := filepath.Join(directory, "out")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Finalize(ctx, testConfig(resultPath, logPath, binaryPath, outputDir, 0)); err == nil {
		t.Fatal("Finalize() unexpectedly succeeded with a canceled context")
	}
	assertNoPublishedOrStagedBundle(t, directory, outputDir)
}

func TestFinalizeNeverReplacesExistingEvidence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "log.ndjson", canonicalTestLog("completed"))
	binaryPath := writeVersionBinary(t, directory)
	outputDir := filepath.Join(directory, "out")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := writeTestFile(t, outputDir, "reviewed.txt", "keep\n")

	if _, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, outputDir, 0)); !errors.Is(err, ErrPublish) {
		t.Fatalf("Finalize() error = %v, want ErrPublish", err)
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil || string(contents) != "keep\n" {
		t.Fatalf("existing evidence changed: contents=%q error=%v", contents, err)
	}
	assertNoEvidenceStages(t, directory)
}

func TestFinalizeRejectsSymlinkedPublicationParent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available without extra privileges")
	}
	directory := t.TempDir()
	realParent := filepath.Join(directory, "real-parent")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(directory, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(linkedParent, "out")
	if _, err := Finalize(context.Background(), testConfig(
		writeTestFile(t, directory, "result.json", canonicalTestResult()),
		writeTestFile(t, directory, "log.ndjson", canonicalTestLog("completed")),
		writeVersionBinary(t, directory), outputDir, 0,
	)); !errors.Is(err, ErrPublish) {
		t.Fatalf("Finalize() error = %v, want ErrPublish", err)
	}
	if _, err := os.Lstat(filepath.Join(realParent, "out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bundle unexpectedly published through symlink: %v", err)
	}
	assertNoEvidenceStages(t, realParent)
}

func TestParseVersionLineCanonicalContract(t *testing.T) {
	t.Parallel()
	valid := []string{
		"duckwords version=1.2.3 commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.26.6\n",
		"duckwords version=1.2.3-rc.1 commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.26.6 goos=" + runtime.GOOS + " goarch=" + runtime.GOARCH + "\n",
	}
	for _, line := range valid {
		build, err := parseVersionLine(line)
		if err != nil || build.Commit != testCommit || build.GoVersion != "go1.26.6" {
			t.Fatalf("parseVersionLine(%q) build=%#v error=%v", line, build, err)
		}
	}

	invalid := []string{
		"duckwords version=1.2.3 commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.26.6",
		"duckwords version=1.2.3 commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.26.6\r\n",
		"duckwords commit=" + testCommit + " version=1.2.3 built=2026-08-14T09:00:00Z go=go1.26.6\n",
		"duckwords version=1.2.3 version=1.2.3 built=2026-08-14T09:00:00Z go=go1.26.6\n",
		"duckwords version=dev commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.26.6\n",
		"duckwords version=1.2.3 commit=" + strings.ToUpper(testCommit) + " built=2026-08-14T09:00:00Z go=go1.26.6\n",
		"duckwords version=1.2.3 commit=" + testCommit + " built=2026-08-14T09:00:00.1Z go=go1.26.6\n",
		"duckwords version=1.2.3 commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.27.0\n",
	}
	for _, line := range invalid {
		if _, err := parseVersionLine(line); !errors.Is(err, ErrInvalidBinary) {
			t.Fatalf("parseVersionLine(%q) error=%v, want ErrInvalidBinary", line, err)
		}
	}
}

func TestInspectBinaryRejectsBoundedOutputOverflow(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "oversized-version")
	contents := "#!/bin/sh\nprintf '%s\\n' '" + strings.Repeat("x", maximumVersionBytes+1) + "'\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inspectBinary(context.Background(), path); !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("inspectBinary() error = %v, want ErrInvalidBinary", err)
	}
}

func TestInspectBinaryBoundsDescendantHeldPipes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("test fixture requires a POSIX shell and process signals")
	}
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "descendant.pid")
	path := filepath.Join(directory, "forking-version")
	quotedPIDPath := "'" + strings.ReplaceAll(pidPath, "'", `'"'"'`) + "'"
	contents := "#!/bin/sh\n" +
		"sleep 30 &\n" +
		"printf '%s\\n' \"$!\" > " + quotedPIDPath + "\n" +
		"printf '%s\\n' 'duckwords version=1.2.3 commit=" + testCommit + " built=2026-08-14T09:00:00Z go=go1.26.6'\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	var descendant *os.Process
	t.Cleanup(func() {
		if descendant == nil {
			pidBytes, err := os.ReadFile(pidPath)
			var pid int
			if err == nil {
				_, _ = fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &pid)
			}
			if pid > 0 {
				descendant, _ = os.FindProcess(pid)
			}
		}
		if descendant != nil {
			_ = descendant.Kill()
			_ = descendant.Release()
		}
	})
	started := time.Now()
	_, _, err := inspectBinary(context.Background(), path)
	elapsed := time.Since(started)
	if !errors.Is(err, ErrInvalidBinary) {
		t.Fatalf("inspectBinary() error = %v, want ErrInvalidBinary", err)
	}
	if elapsed >= binaryInspectionWindow {
		t.Fatalf("inspectBinary() waited %s for descendant-held pipes", elapsed)
	}
	pidBytes, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var pid int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &pid); scanErr != nil || pid <= 0 {
		t.Fatalf("descendant PID = %q, error = %v", pidBytes, scanErr)
	}
	process, findErr := os.FindProcess(pid)
	if findErr != nil {
		t.Fatal(findErr)
	}
	descendant = process
}

func TestValidateConfigRejectsUnsafeAttestation(t *testing.T) {
	t.Parallel()
	base := Config{ResultPath: "result", LogPath: "log", OutputDir: "out", BinaryPath: "bin", ExitCode: 0,
		PolicyVerifiedAt: "2026-08-14", ApprovalReference: "reddit-approval-confirmed-2026-08-14",
		Now: func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }}
	tests := []Config{
		func() Config { cfg := base; cfg.ExitCode = 2; return cfg }(),
		func() Config { cfg := base; cfg.PolicyVerifiedAt = "2026-02-30"; return cfg }(),
		func() Config { cfg := base; cfg.PolicyVerifiedAt = "2026-08-12"; return cfg }(),
		func() Config { cfg := base; cfg.PolicyVerifiedAt = "2026-08-15"; return cfg }(),
		func() Config { cfg := base; cfg.ApprovalReference = "bearer-token"; return cfg }(),
		func() Config { cfg := base; cfg.OutputDir = " bad"; return cfg }(),
	}
	for _, cfg := range tests {
		if err := validateConfig(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("validateConfig(%#v) error = %v", cfg, err)
		}
	}
}

func TestFinalizeAllowsUTCDateRolloverAfterRunStarts(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "log.ndjson", canonicalTestLog("completed"))
	binaryPath := writeVersionBinary(t, directory)
	outputDir := filepath.Join(directory, "out")
	cfg := testConfig(resultPath, logPath, binaryPath, outputDir, 0)
	cfg.Now = func() time.Time { return time.Date(2026, 8, 15, 0, 5, 0, 0, time.UTC) }

	if _, err := Finalize(context.Background(), cfg); err != nil {
		t.Fatalf("Finalize() across UTC rollover error = %v", err)
	}
}

func TestFinalizeRequiresPolicyReviewOnRunUTCDate(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	log := strings.ReplaceAll(canonicalTestLog("completed"), "2026-08-14T10:", "2026-08-15T10:")
	outputDir := filepath.Join(directory, "out")
	_, err := Finalize(context.Background(), testConfig(
		writeTestFile(t, directory, "result.json", canonicalTestResult()),
		writeTestFile(t, directory, "log.ndjson", log),
		writeVersionBinary(t, directory), outputDir, 0,
	))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Finalize() error = %v, want ErrInvalidConfig", err)
	}
	assertNoPublishedOrStagedBundle(t, directory, outputDir)
}

func FuzzParseResult(f *testing.F) {
	f.Add([]byte("[]\n"))
	f.Add([]byte(canonicalTestResult()))
	f.Add([]byte("null\n"))
	f.Add([]byte("[{\"word\":\"duck\",\"count\":1}]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maximumResultBytes {
			return
		}
		words, canonical, err := parseResult(data)
		if err != nil {
			if !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("parseResult() error = %v, want ErrInvalidResult", err)
			}
			return
		}
		if len(words) > maximumResultWords || !bytes.Equal(canonical, data) {
			t.Fatalf("accepted non-canonical result with %d words", len(words))
		}
		for index, word := range words {
			if !validWord(word.Word) || word.Count == 0 {
				t.Fatalf("accepted invalid word at %d: %#v", index, word)
			}
			if index > 0 && (words[index-1].Count < word.Count ||
				(words[index-1].Count == word.Count && words[index-1].Word >= word.Word)) {
				t.Fatalf("accepted non-deterministic ordering at %d", index)
			}
		}
	})
}

func FuzzParseLog(f *testing.F) {
	f.Add([]byte(parserOnlyTestLog()))
	f.Add([]byte("{}\n"))
	f.Add([]byte("not-json\n"))
	f.Add([]byte("\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Full capture is bounded before parseLog. This tighter parser campaign bound
		// keeps structural mutations fast while still crossing the per-record limit.
		if len(data) > 2*maximumLogLineBytes {
			return
		}
		parsed, err := parseLog(data)
		if err != nil {
			if !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("parseLog() error = %v, want ErrInvalidLog", err)
			}
			return
		}
		again, err := parseLog(append([]byte(nil), data...))
		if err != nil || !reflect.DeepEqual(parsed, again) {
			t.Fatalf("parseLog() is not deterministic: first=%#v second=%#v error=%v", parsed, again, err)
		}
		if len(parsed.sources) != 2 || parsed.sources["posts"].SHA256 == "" || parsed.sources["dictionary"].SHA256 == "" {
			t.Fatalf("accepted log without reconciled source provenance: %#v", parsed.sources)
		}
	})
}

func canonicalTestResult() string {
	return "[\n  {\n    \"word\": \"duck\",\n    \"count\": 2\n  },\n  {\n    \"word\": \"water\",\n    \"count\": 1\n  }\n]\n"
}

// canonicalTestLog builds a full 200-post capture. lastOutcome selects the terminal
// state of the final post: "completed", "failed" (a genuine server failure, which
// makes the run partial), or "skipped" (a provably absent post, which does not).
func canonicalTestLog(lastOutcome string) string {
	status, terminal := "complete", "false"
	completed, skipped, failed := assignmentPostCount, 0, 0
	switch lastOutcome {
	case "failed":
		status, terminal = "partial", "true"
		completed, failed = assignmentPostCount-1, 1
	case "skipped":
		completed, skipped = assignmentPostCount-1, 1
	}
	lines := []string{
		`{"time":"2026-08-14T10:00:00Z","level":"INFO","msg":"run started","event":"run_started","workers":4,"failure_mode":"best-effort","input_profile":"assignment-default-v1","filter_count":0,"rate_limit_rps":0.8,"request_timeout":"20s","global_timeout":"30m0s","max_retries":3,"retry_budget":"45s","source_max_retries":2,"source_retry_budget":"15s","max_distinct_words_per_post":50000,"max_in_flight_response_bytes":33554432,"max_retained_things":500000,"app_version":"1.2.3","app_commit":"` + testCommit + `","app_build_date":"2026-08-14T09:00:00Z","go_version":"go1.26.6","goos":"` + runtime.GOOS + `","goarch":"` + runtime.GOARCH + `"}`,
		`{"time":"2026-08-14T10:00:00.1Z","level":"INFO","msg":"source loaded","event":"source_loaded","source_kind":"posts","source_mode":"https","source_origin":"gist.githubusercontent.com","source_bytes":100,"source_sha256":"` + testPosts + `"}`,
		`{"time":"2026-08-14T10:00:00.2Z","level":"INFO","msg":"source parsed","event":"source_parsed","source_kind":"posts","stage":"parsed","entries":200,"source_sha256":"` + testPosts + `","posts_sha256":"` + testPostIDs + `"}`,
		`{"time":"2026-08-14T10:00:00.3Z","level":"INFO","msg":"source loaded","event":"source_loaded","source_kind":"dictionary","source_mode":"https","source_origin":"raw.githubusercontent.com","source_bytes":200,"source_sha256":"` + testWords + `"}`,
		`{"time":"2026-08-14T10:00:00.4Z","level":"INFO","msg":"source parsed","event":"source_parsed","source_kind":"dictionary","stage":"parsed","entries":3,"source_sha256":"` + testWords + `"}`,
	}
	for index := 1; index <= assignmentPostCount; index++ {
		postID, comments, bodies, tokens := fmt.Sprintf("post%03d", index), 0, 0, 0
		if index == 1 {
			postID, comments, bodies, tokens = "duck123", 1, 1, 3
		}
		if index == assignmentPostCount && lastOutcome == "failed" {
			lines = append(lines, fmt.Sprintf(`{"time":"2026-08-14T10:00:01Z","level":"INFO","msg":"post processing completed","event":"post_outcome","post_id":"%s","source_line":%d,"status":"failed","comments":0,"bodies_visited":0,"more_requests":0,"continuation_requests":0,"counted_tokens":0,"error_class":"server","operation":"comments","http_status":503}`, postID, index))
			continue
		}
		// A validated empty post listing is a 200 response, so an absent post carries
		// no HTTP status of its own.
		if index == assignmentPostCount && lastOutcome == "skipped" {
			lines = append(lines, fmt.Sprintf(`{"time":"2026-08-14T10:00:01Z","level":"INFO","msg":"post processing completed","event":"post_outcome","post_id":"%s","source_line":%d,"status":"skipped","comments":0,"bodies_visited":0,"more_requests":0,"continuation_requests":0,"counted_tokens":0,"error_class":"not_found","operation":"comments"}`, postID, index))
			continue
		}
		lines = append(lines, fmt.Sprintf(`{"time":"2026-08-14T10:00:01Z","level":"INFO","msg":"post processing completed","event":"post_outcome","post_id":"%s","source_line":%d,"status":"completed","comments":%d,"bodies_visited":%d,"more_requests":0,"continuation_requests":0,"counted_tokens":%d}`, postID, index, comments, bodies, tokens))
	}
	lines = append(lines,
		fmt.Sprintf(`{"time":"2026-08-14T10:00:02Z","level":"INFO","msg":"processing summary","event":"run_summary","terminal_status":"%s","partial":%s,"failure_mode":"best-effort","workers":4,"input_profile":"assignment-default-v1","filter_count":0,"duration":"2s","posts_total":200,"posts_completed":%d,"posts_skipped":%d,"posts_failed":%d,"posts_incomplete":0,"comments":1,"bodies_visited":1,"more_requests":0,"continuation_requests":0,"counted_tokens":3,"distinct_words":2,"dictionary_words":3,"source_retries":0,"reddit_http_attempts":201,"reddit_retries":0,"throttle_waits":200,"throttle_wait":"1s","posts_sha256":"%s","post_ids_sha256":"%s","dictionary_sha256":"%s","app_version":"1.2.3","app_commit":"%s","app_build_date":"2026-08-14T09:00:00Z","go_version":"go1.26.6","goos":"%s","goarch":"%s"}`, status, terminal, completed, skipped, failed, testPosts, testPostIDs, testWords, testCommit, runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf(`{"time":"2026-08-14T10:00:03Z","level":"INFO","msg":"result written","event":"output_written","partial":%s,"result_words":2,"result_sha256":"%s"}`, terminal, testResult),
	)
	log := strings.Join(lines, "\n") + "\n"
	return log
}

func parserOnlyTestLog() string {
	lines := strings.Split(strings.TrimSuffix(canonicalTestLog("completed"), "\n"), "\n")
	// Keep both sources, one representative outcome, and the terminal records. The
	// parser itself validates lifecycle structure; 200-outcome reconciliation is
	// exercised separately without burdening every fuzz iteration.
	lines = append(append(append([]string{}, lines[:5]...), lines[5]), lines[len(lines)-2:]...)
	return strings.Join(lines, "\n") + "\n"
}

func validateTestEvidence(result, log string, exitCode int) error {
	words, _, err := parseResult([]byte(result))
	if err != nil {
		return err
	}
	parsed, err := parseLog([]byte(log))
	if err != nil {
		return err
	}
	return reconcile(parsed, words, exitCode, time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
}

func removeLogLineContaining(log, fragment string) string {
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !strings.Contains(line, fragment) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n") + "\n"
}

func assertNoPublishedOrStagedBundle(t *testing.T, parent, outputDir string) {
	t.Helper()
	if _, err := os.Lstat(outputDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output unexpectedly published: %v", err)
	}
	assertNoEvidenceStages(t, parent)
}

func assertNoEvidenceStages(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".duckwords-evidence-") {
			t.Fatalf("staging path was not cleaned: %s", entry.Name())
		}
	}
}

func testConfig(resultPath, logPath, binaryPath, outputDir string, exitCode int) Config {
	return Config{ResultPath: resultPath, LogPath: logPath, BinaryPath: binaryPath, OutputDir: outputDir,
		ExitCode: exitCode, PolicyVerifiedAt: "2026-08-14", ApprovalReference: "reddit-approval-confirmed-2026-08-14",
		Now: func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }}
}

func writeTestFile(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeVersionBinary(t *testing.T, directory string) string {
	t.Helper()
	return writeVersionBinaryWithCommit(t, directory, testCommit)
}

func writeVersionBinaryWithCommit(t *testing.T, directory, commit string) string {
	t.Helper()
	path := filepath.Join(directory, "duckwords-"+commit[:8])
	contents := "#!/bin/sh\nprintf '%s\\n' 'duckwords version=1.2.3 commit=" + commit + " built=2026-08-14T09:00:00Z go=go1.26.6'\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFinalizeAcceptsSkippedAbsentPosts covers the supplied post list, which contains
// deleted threads. Such a post is provably absent, so the run stays complete and must
// still publish a bundle: the finalizer previously rejected the whole capture.
func TestFinalizeAcceptsSkippedAbsentPosts(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	resultPath := writeTestFile(t, directory, "result.json", canonicalTestResult())
	logPath := writeTestFile(t, directory, "log.ndjson", canonicalTestLog("skipped"))
	binaryPath := writeVersionBinary(t, directory)

	manifest, err := Finalize(context.Background(), testConfig(resultPath, logPath, binaryPath, filepath.Join(directory, "out"), 0))
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if manifest.Partial || manifest.TerminalStatus != "complete" || manifest.ExitCode != 0 {
		t.Fatalf("an absent post made the run partial: %#v", manifest)
	}
	if manifest.Summary.PostsSkipped != 1 || manifest.Summary.PostsCompleted != assignmentPostCount-1 ||
		manifest.Summary.PostsFailed != 0 || len(manifest.Outcomes) != assignmentPostCount {
		t.Fatalf("summary = %#v", manifest.Summary)
	}
}

// TestParseOutcomeSkippedContract pins the exact evidence a skipped record must carry.
// Absence is proven only by the comments endpoint; any other shape stays a failure so
// a fabricated bundle cannot downgrade one.
func TestParseOutcomeSkippedContract(t *testing.T) {
	t.Parallel()

	const base = `{"time":"2026-08-14T10:00:01Z","level":"INFO","msg":"post processing completed","event":"post_outcome","post_id":"duck123","source_line":1,"comments":0,"bodies_visited":0,"more_requests":0,"continuation_requests":0,"counted_tokens":0`
	tests := []struct {
		name  string
		extra string
		valid bool
	}{
		{name: "empty listing has no HTTP status", extra: `,"status":"skipped","error_class":"not_found","operation":"comments"`, valid: true},
		{name: "explicit 404", extra: `,"status":"skipped","error_class":"not_found","operation":"comments","http_status":404`, valid: true},
		{name: "forbidden is not absence", extra: `,"status":"skipped","error_class":"forbidden","operation":"comments","http_status":403`},
		{name: "expansion 404 is not absence", extra: `,"status":"skipped","error_class":"not_found","operation":"morechildren","http_status":404`},
		{name: "wrong status pairing", extra: `,"status":"skipped","error_class":"not_found","operation":"comments","http_status":500`},
		{name: "absent post recorded as failure", extra: `,"status":"failed","error_class":"not_found","operation":"comments","http_status":404`},
		{name: "expansion 404 stays a failure", extra: `,"status":"failed","error_class":"not_found","operation":"morechildren","http_status":404`, valid: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var record logRecord
			if err := json.Unmarshal([]byte(base+test.extra+"}"), &record); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			_, err := parseOutcome(record)
			if test.valid && err != nil {
				t.Fatalf("parseOutcome() error = %v, want accepted", err)
			}
			if !test.valid && err == nil {
				t.Fatal("parseOutcome() accepted a contradictory outcome")
			}
		})
	}
}

// TestEvidenceAcceptsEveryRunnerOutcomeStatus links the two packages so that adding a
// status to the runner without teaching the finalizer fails here rather than after a
// real capture has already been spent.
func TestEvidenceAcceptsEveryRunnerOutcomeStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []app.OutcomeStatus{app.OutcomeCompleted, app.OutcomeSkipped, app.OutcomeFailed, app.OutcomeIncomplete} {
		record := logRecord{"status": json.RawMessage(`"` + string(status) + `"`)}
		if _, err := enumField(record, "status", "completed", "skipped", "failed", "incomplete"); err != nil {
			t.Fatalf("evidence rejects runner outcome %q: %v", status, err)
		}
	}
}
